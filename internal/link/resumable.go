package link

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Resumable is one reliable byte stream that outlives the transport
// connections carrying it. yamux runs on top of it; underneath, a USB or
// Wi-Fi connection can drop and be replaced (Resume) without yamux noticing.
//
// Every byte written is kept until the peer acknowledges it; on Resume both
// sides exchange how many bytes they have received and resend the rest.
// "Received" means appended to this side's read queue, which yamux drains;
// those bytes are lost only if the process dies, and then the session is gone
// anyway.
//
// On the transport, everything is a frame: type (1 byte), length (4 bytes),
// payload. Data frames carry no sequence number: after a Resume both sides
// know the exact offset the other will continue from.
//
// Datagram frames ride the same transport outside the byte stream: they are
// never acknowledged or resent, they are dropped while no transport is
// attached, and they are sent ahead of stream data (with a bound, so stream
// data keeps a share of the link). That keeps UDP out of the queue behind bulk
// bytes and out of the replay after a Resume.
type Resumable struct {
	cfg ResumableConfig

	hs sync.Mutex // serializes Resume calls

	mu   sync.Mutex
	cond *sync.Cond

	// Send side. acked <= sent <= written are stream offsets; blocks hold
	// [bufStart, written), each block exactly blockSize except the last.
	blocks   [][]byte
	bufStart uint64
	acked    uint64
	sent     uint64
	written  uint64

	// Receive side.
	queue   [][]byte
	queued  int
	recvd   uint64
	ackSent uint64

	// Datagrams waiting for the attached transport.
	dq      []queuedDatagram
	dqBytes int

	// Wire counts: data and datagram frame bytes (headers included) on the
	// current transport, for pacing. They restart with every transport.
	pace        pacer // cap on wire bytes in flight
	wireSent    uint64
	wireAcked   uint64
	wireRecvd   uint64
	wireAckSent uint64

	cur       *transport // attached transport, nil while down
	last      *transport // most recent transport; its goroutines may still be exiting
	gen       int
	expiry    *time.Timer
	closed    bool
	closeErr  error
	done      chan struct{}
	localAddr net.Addr
	peerAddr  net.Addr
}

// ResumableConfig tunes a Resumable; zero fields take the defaults.
type ResumableConfig struct {
	MaxUnacked int // retransmit buffer; Write blocks when full (32 MB)
	// MaxInFlight fixes the cap on bytes (stream data and datagrams) sent
	// but not yet acknowledged.
	// 0 (the default) adapts it to the link (see pacer): beyond the socket,
	// adbd and the adb server buffer generously, and without a cap the backlog
	// would wait there, where datagrams can't overtake it.
	MaxInFlight  int
	MaxQueued    int           // received bytes waiting for Read (4 MB)
	Heartbeat    time.Duration // send a ping after this long without writing (1 s)
	IdleTimeout  time.Duration // no bytes at all for this long = transport dead (5 s)
	ResumeWindow time.Duration // close if no transport is attached for this long (30 s)
	// OnDown is called (in its own goroutine) when an attached transport
	// fails. It is not called for Detach-by-Resume or Close.
	OnDown func(err error)

	// OnDatagram receives each datagram frame's payload (which it may keep).
	// It runs on the transport's reader, so it must not block.
	OnDatagram       func(b []byte)
	MaxDatagramQueue int           // bytes of datagrams waiting to be sent; more are dropped (512 KB)
	MaxDatagramAge   time.Duration // queued datagrams older than this are dropped (300 ms)
	// Datagrams counts datagram traffic; pass one to keep counting across
	// Resumables. nil uses a private one.
	Datagrams *DatagramStats
}

// DatagramStats counts datagram frames.
type DatagramStats struct {
	Sent, Received atomic.Uint64
	DropNoLink     atomic.Uint64 // no transport attached (or it went away with them queued)
	DropQueue      atomic.Uint64 // send queue full
	DropAge        atomic.Uint64 // waited longer than MaxDatagramAge
	DropBig        atomic.Uint64 // larger than a frame
}

func (s *DatagramStats) String() string {
	return fmt.Sprintf("dg_tx=%d dg_rx=%d dg_drop_nolink=%d dg_drop_queue=%d dg_drop_age=%d dg_drop_big=%d",
		s.Sent.Load(), s.Received.Load(), s.DropNoLink.Load(), s.DropQueue.Load(), s.DropAge.Load(), s.DropBig.Load())
}

type queuedDatagram struct {
	b  []byte
	at time.Time
}

const (
	frameData     = 1
	frameAck      = 2
	framePing     = 3
	frameDatagram = 4
	// frameWireAck acknowledges data and datagram frame bytes received on
	// this transport (8-byte count), for the sender's pacer.
	frameWireAck = 5

	frameHeader  = 5
	maxFrameData = 64 << 10
	blockSize    = 64 << 10
	ackBatch     = 64 << 10         // unacknowledged bytes that make the receiver ack at once
	ackDelay     = time.Millisecond // otherwise it acks this soon after a frame

	// smallChunk is the data frame size while datagrams wait, so a datagram
	// waits behind at most this much stream data.
	smallChunk = 16 << 10
	// dgReserve is how far datagrams may exceed the in-flight cap; a few
	// dozen datagrams, so they don't wait for acks, but a flood still can't
	// queue up beyond the cap.
	dgReserve = 64 << 10
	// maxSkip bounds frames a reader discards (unknown types, oversize
	// datagrams); anything longer means the framing is lost.
	maxSkip = 1 << 20
)

var (
	ErrResumeTimeout = errors.New("link: no transport within the resume window")
	ErrResumeOffset  = errors.New("link: peer resumes from an offset outside the retransmit buffer")
	errIdle          = errors.New("link: no bytes from peer")
)

var blockPool = sync.Pool{New: func() any { return make([]byte, 0, blockSize) }}

func NewResumable(cfg ResumableConfig) *Resumable {
	if cfg.MaxUnacked <= 0 {
		cfg.MaxUnacked = 32 << 20
	}
	if cfg.MaxQueued <= 0 {
		cfg.MaxQueued = 4 << 20
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Second
	}
	if cfg.ResumeWindow <= 0 {
		cfg.ResumeWindow = 30 * time.Second
	}
	if cfg.MaxDatagramQueue <= 0 {
		cfg.MaxDatagramQueue = 512 << 10
	}
	if cfg.MaxDatagramAge <= 0 {
		cfg.MaxDatagramAge = 300 * time.Millisecond
	}
	if cfg.Datagrams == nil {
		cfg.Datagrams = new(DatagramStats)
	}
	r := &Resumable{cfg: cfg, done: make(chan struct{}), pace: newPacer(cfg.MaxInFlight)}
	r.cond = sync.NewCond(&r.mu)
	r.mu.Lock()
	r.startExpiry()
	r.mu.Unlock()
	return r
}

type transport struct {
	c        net.Conn
	gen      int
	kick     chan struct{}
	dead     bool
	ackArmed bool // an ackSoonLocked timer is pending (r.mu)
	stop     chan struct{}
	wg       sync.WaitGroup
	closer   sync.Once
}

func (t *transport) close() {
	t.closer.Do(func() {
		close(t.stop)
		t.c.Close()
	})
}

func (t *transport) wake() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// Received is the number of bytes received so far.
func (r *Resumable) Received() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recvd
}

// Accepts reports whether the peer may resume from peerRecvd: everything it
// has not received must still be in the retransmit buffer.
func (r *Resumable) Accepts(peerRecvd uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && peerRecvd >= r.acked && peerRecvd <= r.written
}

// Attached reports whether a transport is currently carrying the stream.
func (r *Resumable) Attached() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur != nil
}

// Pace describes the in-flight cap and the round trips it is based on.
func (r *Resumable) Pace() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pace.String()
}

// Done is closed when the stream is closed for good.
func (r *Resumable) Done() <-chan struct{} { return r.done }

// Err is why the stream closed.
func (r *Resumable) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}

// Resume moves the stream onto c. Any current transport is closed first, so
// the received count passed to exchange is final; exchange runs the handshake
// on c (telling the peer that count) and returns how much the peer has
// received. Unacknowledged bytes beyond that are resent on c.
func (r *Resumable) Resume(c net.Conn, exchange func(recvd uint64) (peerRecvd uint64, err error)) error {
	r.hs.Lock()
	defer r.hs.Unlock()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return r.closeErr
	}
	old, last := r.cur, r.last
	r.cur = nil
	if old != nil {
		old.dead = true
		r.dropDatagramsLocked()
		r.startExpiry()
		r.cond.Broadcast()
	}
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
	if last != nil {
		last.wg.Wait()
	}

	peer, err := exchange(r.Received())
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	if peer < r.acked || peer > r.written {
		r.closeLocked(fmt.Errorf("%w: peer has %d, buffer holds [%d, %d)", ErrResumeOffset, peer, r.acked, r.written))
		return r.closeErr
	}
	r.ackLocked(peer)
	r.sent = peer
	r.pace.reset() // a new transport has its own delay and capacity
	r.wireSent, r.wireAcked, r.wireRecvd, r.wireAckSent = 0, 0, 0, 0
	r.ackSent = r.recvd // the handshake told the peer
	if r.expiry != nil {
		r.expiry.Stop()
		r.expiry = nil
	}
	r.gen++
	t := &transport{c: c, gen: r.gen, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	r.cur, r.last = t, t
	r.localAddr, r.peerAddr = c.LocalAddr(), c.RemoteAddr()
	t.wg.Add(2)
	go r.readLoop(t)
	go r.writeLoop(t)
	t.wake()
	r.cond.Broadcast()
	return nil
}

// startExpiry arms the resume window; r.mu must be held.
func (r *Resumable) startExpiry() {
	if r.expiry != nil || r.closed {
		return
	}
	gen := r.gen
	r.expiry = time.AfterFunc(r.cfg.ResumeWindow, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.cur == nil && r.gen == gen {
			r.closeLocked(ErrResumeTimeout)
		}
	})
}

// fail detaches t after an I/O error, unless it was already replaced.
func (r *Resumable) fail(t *transport, err error) {
	r.mu.Lock()
	current := r.cur == t
	if current {
		r.cur = nil
		r.dropDatagramsLocked()
		r.startExpiry()
	}
	t.dead = true
	r.cond.Broadcast()
	r.mu.Unlock()
	t.close()
	if current && r.cfg.OnDown != nil {
		go r.cfg.OnDown(err)
	}
}

// dropDatagramsLocked discards queued datagrams when their transport goes away:
// the next one may come much later, and UDP would rather lose them than get them late.
func (r *Resumable) dropDatagramsLocked() {
	if n := len(r.dq); n > 0 {
		r.cfg.Datagrams.DropNoLink.Add(uint64(n))
	}
	r.dq, r.dqBytes = nil, 0
}

// WriteDatagram queues b (copied) as one datagram frame. It never blocks: it
// reports false and drops b when no transport is attached, b does not fit a
// frame, or the queue is full.
func (r *Resumable) WriteDatagram(b []byte) bool {
	st := r.cfg.Datagrams
	if len(b) == 0 || len(b) > maxFrameData {
		st.DropBig.Add(1)
		return false
	}
	r.mu.Lock()
	t := r.cur
	switch {
	case r.closed || t == nil:
		r.mu.Unlock()
		st.DropNoLink.Add(1)
		return false
	case r.dqBytes+len(b) > r.cfg.MaxDatagramQueue:
		r.mu.Unlock()
		st.DropQueue.Add(1)
		return false
	}
	r.dq = append(r.dq, queuedDatagram{append([]byte(nil), b...), time.Now()})
	r.dqBytes += len(b)
	r.mu.Unlock()
	t.wake()
	return true
}

// takeDatagramsLocked removes the datagrams to send in this round, within
// room wire bytes: all that fit when no stream data waits, otherwise half the
// room (and at most one full data frame's worth), so stream data keeps a share
// of a link that datagrams alone could fill. Stale ones are dropped here. It
// returns the wire bytes the datagrams take.
func (r *Resumable) takeDatagramsLocked(now time.Time, room int) ([][]byte, int) {
	budget := room
	if r.sent < r.written {
		budget = min(room/2, maxFrameData+frameHeader)
	}
	var out [][]byte
	n := 0
	for len(r.dq) > 0 {
		q := r.dq[0]
		stale := now.Sub(q.at) > r.cfg.MaxDatagramAge
		if !stale && n+frameHeader+len(q.b) > budget {
			break
		}
		r.dq[0] = queuedDatagram{}
		r.dq = r.dq[1:]
		r.dqBytes -= len(q.b)
		if stale {
			r.cfg.Datagrams.DropAge.Add(1)
			continue
		}
		out = append(out, q.b)
		n += frameHeader + len(q.b)
	}
	if len(r.dq) == 0 {
		r.dq = nil
	}
	return out, n
}

// ackLocked drops acknowledged blocks.
func (r *Resumable) ackLocked(a uint64) {
	if a <= r.acked {
		return
	}
	r.acked = a
	for len(r.blocks) > 0 && r.bufStart+blockSize <= a {
		blockPool.Put(r.blocks[0][:0])
		r.blocks[0] = nil
		r.blocks = r.blocks[1:]
		r.bufStart += blockSize
	}
	r.cond.Broadcast()
}

// chunkLocked returns up to max contiguous unsent bytes starting at r.sent.
// The slice stays valid without the lock: Write only appends past r.written
// and blocks are freed only once acknowledged, which cannot happen before
// they were handed to the transport.
func (r *Resumable) chunkLocked(max int) []byte {
	if r.sent >= r.written {
		return nil
	}
	off := r.sent - r.bufStart
	b := r.blocks[off/blockSize]
	i := int(off % blockSize)
	end := len(b)
	if end-i > max {
		end = i + max
	}
	return b[i:end]
}

func (r *Resumable) readLoop(t *transport) {
	defer t.wg.Done()
	var hdr [frameHeader]byte
	for {
		t.c.SetReadDeadline(time.Now().Add(r.cfg.IdleTimeout))
		if _, err := io.ReadFull(t.c, hdr[:]); err != nil {
			r.fail(t, readErr(err))
			return
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		switch hdr[0] {
		case frameData:
			if n == 0 || n > maxFrameData {
				r.fail(t, fmt.Errorf("link: bad data frame length %d", n))
				return
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(t.c, buf); err != nil {
				r.fail(t, readErr(err))
				return
			}
			r.mu.Lock()
			for r.queued >= r.cfg.MaxQueued && !r.closed && !t.dead {
				r.cond.Wait()
			}
			if r.closed || t.dead {
				r.mu.Unlock()
				return
			}
			r.queue = append(r.queue, buf)
			r.queued += len(buf)
			r.recvd += uint64(n)
			r.wireRecvd += frameHeader + uint64(n)
			r.ackSoonLocked(t)
			r.cond.Broadcast()
			r.mu.Unlock()
		case frameAck:
			var v [8]byte
			if n != 8 {
				r.fail(t, fmt.Errorf("link: bad ack frame length %d", n))
				return
			}
			if _, err := io.ReadFull(t.c, v[:]); err != nil {
				r.fail(t, readErr(err))
				return
			}
			a := binary.BigEndian.Uint64(v[:])
			r.mu.Lock()
			bad := a > r.sent
			if !bad {
				r.ackLocked(a)
			}
			r.mu.Unlock()
			if bad {
				r.fail(t, fmt.Errorf("link: peer acked %d, beyond what was sent", a))
				return
			}
		case frameWireAck:
			var v [8]byte
			if n != 8 {
				r.fail(t, fmt.Errorf("link: bad wire ack frame length %d", n))
				return
			}
			if _, err := io.ReadFull(t.c, v[:]); err != nil {
				r.fail(t, readErr(err))
				return
			}
			a := binary.BigEndian.Uint64(v[:])
			r.mu.Lock()
			if a > r.wireAcked && a <= r.wireSent {
				r.wireAcked = a
				r.pace.acked(a, time.Now())
			}
			r.mu.Unlock()
			t.wake() // the in-flight cap has room again
		case framePing:
			if n != 0 {
				r.fail(t, fmt.Errorf("link: bad ping frame length %d", n))
				return
			}
		case frameDatagram:
			var buf []byte
			if n == 0 || n > maxFrameData {
				if err := skip(t.c, n); err != nil {
					r.fail(t, err)
					return
				}
				r.cfg.Datagrams.DropBig.Add(1)
			} else {
				buf = make([]byte, n)
				if _, err := io.ReadFull(t.c, buf); err != nil {
					r.fail(t, readErr(err))
					return
				}
			}
			r.mu.Lock()
			r.wireRecvd += frameHeader + uint64(n)
			r.ackSoonLocked(t)
			r.mu.Unlock()
			if buf != nil {
				r.cfg.Datagrams.Received.Add(1)
				if r.cfg.OnDatagram != nil {
					r.cfg.OnDatagram(buf)
				}
			}
		default:
			// A later frame type this side doesn't know: skip it.
			if err := skip(t.c, n); err != nil {
				r.fail(t, fmt.Errorf("link: frame type %d: %w", hdr[0], err))
				return
			}
		}
	}
}

// ackSoonLocked has the writer acknowledge what t received: at once when
// ackBatch bytes wait for it, otherwise within ackDelay, so a burst of frames
// costs one ack (on Wi-Fi every small packet takes airtime from the burst)
// while the sender's pacer still times acks to about a millisecond.
func (r *Resumable) ackSoonLocked(t *transport) {
	if r.wireRecvd-r.wireAckSent >= ackBatch {
		t.wake()
		return
	}
	if t.ackArmed {
		return
	}
	t.ackArmed = true
	time.AfterFunc(ackDelay, func() {
		r.mu.Lock()
		t.ackArmed = false
		r.mu.Unlock()
		t.wake()
	})
}

// skip discards a frame payload of n bytes.
func skip(c net.Conn, n uint32) error {
	if n > maxSkip {
		return fmt.Errorf("link: frame length %d", n)
	}
	if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
		return readErr(err)
	}
	return nil
}

func readErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errIdle
	}
	return err
}

func (r *Resumable) writeLoop(t *transport) {
	defer t.wg.Done()
	tick := time.NewTicker(r.cfg.Heartbeat / 4)
	defer tick.Stop()
	lastWrite := time.Now()
	ackBuf := make([]byte, frameHeader+8)
	wireAckBuf := make([]byte, frameHeader+8)
	dataHdr := make([]byte, frameHeader)
	ping := []byte{framePing, 0, 0, 0, 0}
	for {
		select {
		case <-t.kick:
		case <-tick.C:
		case <-t.stop:
			return
		}
		for {
			r.mu.Lock()
			if t.dead || r.closed || r.cur != t {
				r.mu.Unlock()
				return
			}
			var bufs net.Buffers
			if r.recvd > r.ackSent {
				r.ackSent = r.recvd
				ackBuf[0] = frameAck
				binary.BigEndian.PutUint32(ackBuf[1:], 8)
				binary.BigEndian.PutUint64(ackBuf[5:], r.recvd)
				bufs = append(bufs, ackBuf)
			}
			if r.wireRecvd > r.wireAckSent {
				r.wireAckSent = r.wireRecvd
				wireAckBuf[0] = frameWireAck
				binary.BigEndian.PutUint32(wireAckBuf[1:], 8)
				binary.BigEndian.PutUint64(wireAckBuf[5:], r.wireRecvd)
				bufs = append(bufs, wireAckBuf)
			}
			now := time.Now()
			room := r.pace.limit(now) - int(r.wireSent-r.wireAcked)
			// Datagrams may go dgReserve beyond the cap: with the cap full of
			// stream data they would otherwise wait a round trip for room. Not
			// while more than that waits (a flood): then they share the cap.
			dgRoom := room
			if r.dqBytes <= dgReserve {
				dgRoom += dgReserve
			}
			dgs, dgBytes := r.takeDatagramsLocked(now, dgRoom)
			room -= dgBytes
			chunkMax := maxFrameData
			if len(r.dq) > 0 {
				chunkMax = smallChunk
			}
			if room-frameHeader < chunkMax {
				chunkMax = room - frameHeader
			}
			var chunk []byte
			if chunkMax > 0 {
				chunk = r.chunkLocked(chunkMax)
			}
			r.sent += uint64(len(chunk))
			if chunk != nil {
				dgBytes += frameHeader + len(chunk)
			}
			if dgBytes > 0 {
				r.wireSent += uint64(dgBytes)
				r.pace.sent(r.wireSent, now)
			}
			moreDatagrams := len(r.dq) > 0
			r.mu.Unlock()
			if len(dgs) > 0 {
				hdrs := make([]byte, frameHeader*len(dgs))
				for i, d := range dgs {
					h := hdrs[i*frameHeader : (i+1)*frameHeader]
					h[0] = frameDatagram
					binary.BigEndian.PutUint32(h[1:], uint32(len(d)))
					bufs = append(bufs, h, d)
				}
			}
			if chunk != nil {
				dataHdr[0] = frameData
				binary.BigEndian.PutUint32(dataHdr[1:], uint32(len(chunk)))
				bufs = append(bufs, dataHdr, chunk)
			}
			if bufs == nil {
				if time.Since(lastWrite) < r.cfg.Heartbeat {
					break
				}
				bufs = net.Buffers{ping}
			}
			if _, err := bufs.WriteTo(t.c); err != nil {
				r.fail(t, err)
				return
			}
			r.cfg.Datagrams.Sent.Add(uint64(len(dgs)))
			lastWrite = time.Now()
			if chunk == nil && !moreDatagrams {
				break
			}
		}
	}
}

// Read returns received bytes in order. While no transport is attached it
// blocks; it fails only once the stream is closed.
func (r *Resumable) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.queue) == 0 && !r.closed {
		r.cond.Wait()
	}
	if len(r.queue) == 0 {
		return 0, r.closeErr
	}
	n := 0
	for n < len(p) && len(r.queue) > 0 {
		c := copy(p[n:], r.queue[0])
		n += c
		if c == len(r.queue[0]) {
			r.queue[0] = nil
			r.queue = r.queue[1:]
		} else {
			r.queue[0] = r.queue[0][c:]
		}
	}
	r.queued -= n
	r.cond.Broadcast()
	return n, nil
}

// Write queues p for sending. It blocks only while the retransmit buffer is full.
func (r *Resumable) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		for !r.closed && r.written-r.acked >= uint64(r.cfg.MaxUnacked) {
			r.cond.Wait()
		}
		if r.closed {
			return n, r.closeErr
		}
		room := uint64(r.cfg.MaxUnacked) - (r.written - r.acked)
		for room > 0 && n < len(p) {
			if len(r.blocks) == 0 || len(r.blocks[len(r.blocks)-1]) == blockSize {
				r.blocks = append(r.blocks, blockPool.Get().([]byte))
			}
			last := &r.blocks[len(r.blocks)-1]
			k := min(blockSize-len(*last), len(p)-n, int(room))
			*last = append(*last, p[n:n+k]...)
			n += k
			room -= uint64(k)
			r.written += uint64(k)
		}
		if r.cur != nil {
			r.cur.wake()
		}
	}
	return n, nil
}

// Close ends the stream for good.
func (r *Resumable) Close() error {
	r.mu.Lock()
	r.closeLocked(net.ErrClosed)
	r.mu.Unlock()
	return nil
}

func (r *Resumable) closeLocked(err error) {
	if r.closed {
		return
	}
	r.closed, r.closeErr = true, err
	r.dropDatagramsLocked()
	if r.expiry != nil {
		r.expiry.Stop()
		r.expiry = nil
	}
	if r.cur != nil {
		r.cur.close()
		r.cur = nil
	}
	close(r.done)
	r.cond.Broadcast()
}

type resumableAddr struct{}

func (resumableAddr) Network() string { return "resumable" }
func (resumableAddr) String() string  { return "resumable" }

func (r *Resumable) LocalAddr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.localAddr == nil {
		return resumableAddr{}
	}
	return r.localAddr
}

func (r *Resumable) RemoteAddr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.peerAddr == nil {
		return resumableAddr{}
	}
	return r.peerAddr
}

// Deadlines are not supported: liveness is the transport's business.
func (r *Resumable) SetDeadline(time.Time) error      { return nil }
func (r *Resumable) SetReadDeadline(time.Time) error  { return nil }
func (r *Resumable) SetWriteDeadline(time.Time) error { return nil }
