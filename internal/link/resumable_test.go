package link

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// tcpPair returns both ends of a loopback TCP connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	if b == nil {
		t.Fatal("accept failed")
	}
	return a, b
}

// swapCounts is the minimal handshake: each side sends its received count.
func swapCounts(c net.Conn) func(uint64) (uint64, error) {
	return func(recvd uint64) (uint64, error) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], recvd)
		if _, err := c.Write(b[:]); err != nil {
			return 0, err
		}
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return 0, err
		}
		return binary.BigEndian.Uint64(b[:]), nil
	}
}

// attach moves both Resumables onto a fresh connection.
func attach(t *testing.T, a, b *Resumable, ca, cb net.Conn) {
	t.Helper()
	errs := make(chan error, 2)
	go func() { errs <- a.Resume(ca, swapCounts(ca)) }()
	go func() { errs <- b.Resume(cb, swapCounts(cb)) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
}

// pump writes n random bytes to w in random-sized pieces and returns their hash.
func pump(w io.Writer, n int) ([]byte, error) {
	h := sha256.New()
	buf := make([]byte, 200<<10)
	for n > 0 {
		var sz [2]byte
		rand.Read(sz[:])
		k := min(n, 1+int(binary.BigEndian.Uint16(sz[:]))*3) // up to ~196 KB
		rand.Read(buf[:k])
		h.Write(buf[:k])
		if _, err := w.Write(buf[:k]); err != nil {
			return nil, err
		}
		n -= k
	}
	return h.Sum(nil), nil
}

func drain(r io.Reader, n int) ([]byte, error) {
	h := sha256.New()
	_, err := io.CopyN(h, r, int64(n))
	return h.Sum(nil), err
}

type transfer struct {
	sent, got []byte
	err       error
}

// transferBoth sends n bytes each way at once and reports the hashes.
func transferBoth(a, b io.ReadWriter, n int) (ab, ba chan transfer) {
	ab, ba = make(chan transfer, 1), make(chan transfer, 1)
	run := func(src, dst io.ReadWriter, out chan transfer) {
		var tr transfer
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			tr.sent, err = pump(src, n)
			if err != nil {
				tr.err = err
			}
		}()
		got, err := drain(dst, n)
		wg.Wait()
		tr.got = got
		if err != nil {
			tr.err = err
		}
		out <- tr
	}
	go run(a, b, ab)
	go run(b, a, ba)
	return ab, ba
}

func checkTransfer(t *testing.T, name string, ch chan transfer) {
	t.Helper()
	select {
	case tr := <-ch:
		if tr.err != nil {
			t.Fatalf("%s: %v", name, tr.err)
		}
		if !bytes.Equal(tr.sent, tr.got) {
			t.Fatalf("%s: received bytes differ from sent bytes", name)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("%s: timed out", name)
	}
}

// TestResumableSurvivesDrops sends 48 MB each way while the transport is
// repeatedly killed (reset on one side, or both) or swapped while healthy,
// and checks every byte arrives exactly once, in order.
func TestResumableSurvivesDrops(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ResumableConfig
	}{
		{"default", ResumableConfig{}},
		{"tiny-buffers", ResumableConfig{MaxUnacked: 200 << 10, MaxQueued: 64 << 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := NewResumable(tc.cfg), NewResumable(tc.cfg)
			defer a.Close()
			defer b.Close()
			ca, cb := tcpPair(t)
			attach(t, a, b, ca, cb)

			const n = 48 << 20
			ab, ba := transferBoth(a, b, n)
			for i := range 12 {
				time.Sleep(40 * time.Millisecond)
				switch i % 3 {
				case 0: // one side resets; the other learns from EOF
					ca.Close()
				case 1: // both ends gone, then a pause like a cable replug
					ca.Close()
					cb.Close()
					time.Sleep(150 * time.Millisecond)
				case 2: // swap to a new transport while the old one still works
				}
				ca, cb = tcpPair(t)
				attach(t, a, b, ca, cb)
			}
			checkTransfer(t, "a->b", ab)
			checkTransfer(t, "b->a", ba)
			if a.Received() != n || b.Received() != n {
				t.Fatalf("received counts %d / %d, want %d", a.Received(), b.Received(), n)
			}
		})
	}
}

// stallConn forwards nothing once frozen: a Wi-Fi link that went silent
// without a reset.
type stallProxy struct {
	mu     sync.Mutex
	frozen bool
}

func (p *stallProxy) pipe(t *testing.T) (net.Conn, net.Conn) {
	a, a2 := tcpPair(t)
	b2, b := tcpPair(t)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if err != nil {
				dst.Close()
				return
			}
			p.mu.Lock()
			frozen := p.frozen
			p.mu.Unlock()
			if frozen {
				continue // swallow, never close
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				src.Close()
				return
			}
		}
	}
	go cp(b2, a2)
	go cp(a2, b2)
	return a, b
}

// TestResumableIdleTimeout: a transport that silently stops delivering is
// declared dead after IdleTimeout on both sides, and the stream resumes on a
// new one without losing data.
func TestResumableIdleTimeout(t *testing.T) {
	downs := make(chan error, 4)
	cfg := ResumableConfig{Heartbeat: 50 * time.Millisecond, IdleTimeout: 300 * time.Millisecond,
		OnDown: func(err error) { downs <- err }}
	a, b := NewResumable(cfg), NewResumable(cfg)
	defer a.Close()
	defer b.Close()
	p := &stallProxy{}
	ca, cb := p.pipe(t)
	attach(t, a, b, ca, cb)

	const n = 16 << 20
	ab, ba := transferBoth(a, b, n)
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.frozen = true
	p.mu.Unlock()
	start := time.Now()
	for i := range 2 {
		select {
		case err := <-downs:
			// The first side to time out closes its end; the proxy passes
			// that on, so the other side may see EOF instead.
			if i == 0 && !errors.Is(err, errIdle) {
				t.Fatalf("down error = %v, want idle", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("stalled transport was not declared dead")
		}
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("declared dead after %v, before the idle timeout", d)
	}
	ca, cb = tcpPair(t)
	attach(t, a, b, ca, cb)
	checkTransfer(t, "a->b", ab)
	checkTransfer(t, "b->a", ba)
}

// TestResumableIdleHeartbeat: an idle but healthy transport is not declared dead.
func TestResumableIdleHeartbeat(t *testing.T) {
	downs := make(chan error, 2)
	cfg := ResumableConfig{Heartbeat: 50 * time.Millisecond, IdleTimeout: 200 * time.Millisecond,
		OnDown: func(err error) { downs <- err }}
	a, b := NewResumable(cfg), NewResumable(cfg)
	defer a.Close()
	defer b.Close()
	ca, cb := tcpPair(t)
	attach(t, a, b, ca, cb)
	select {
	case err := <-downs:
		t.Fatalf("idle transport declared dead: %v", err)
	case <-time.After(time.Second):
	}
}

// TestResumableExpires: without a new transport within the resume window the
// stream closes, and Read and Write fail.
func TestResumableExpires(t *testing.T) {
	cfg := ResumableConfig{ResumeWindow: 200 * time.Millisecond}
	a, b := NewResumable(cfg), NewResumable(cfg)
	defer b.Close()
	ca, cb := tcpPair(t)
	attach(t, a, b, ca, cb)
	ca.Close()
	start := time.Now()
	_, err := a.Read(make([]byte, 1))
	if !errors.Is(err, ErrResumeTimeout) {
		t.Fatalf("read error = %v, want resume timeout", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("closed after %v, before the window", d)
	}
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("write after expiry succeeded")
	}
}

// TestResumableWriteBlocksWhenFull: with no transport, Write accepts up to
// MaxUnacked and then waits; attaching drains it.
func TestResumableWriteBlocksWhenFull(t *testing.T) {
	cfg := ResumableConfig{MaxUnacked: 100 << 10}
	a, b := NewResumable(cfg), NewResumable(cfg)
	defer a.Close()
	defer b.Close()
	wrote := make(chan int, 1)
	go func() {
		n, _ := a.Write(make([]byte, 300<<10))
		wrote <- n
	}()
	select {
	case n := <-wrote:
		t.Fatalf("write of 300 KB into a 100 KB buffer returned %d with no transport", n)
	case <-time.After(200 * time.Millisecond):
	}
	ca, cb := tcpPair(t)
	attach(t, a, b, ca, cb)
	if _, err := io.CopyN(io.Discard, b, 300<<10); err != nil {
		t.Fatal(err)
	}
	if n := <-wrote; n != 300<<10 {
		t.Fatalf("wrote %d", n)
	}
}

// TestResumableRejectsBadOffset: a peer claiming more than was written, or
// less than it already acknowledged, cannot resume.
func TestResumableRejectsBadOffset(t *testing.T) {
	a, b := NewResumable(ResumableConfig{}), NewResumable(ResumableConfig{})
	defer b.Close()
	ca, cb := tcpPair(t)
	attach(t, a, b, ca, cb)
	a.Write(make([]byte, 1000))
	io.ReadFull(b, make([]byte, 1000))
	deadline := time.Now().Add(2 * time.Second)
	for !a.Accepts(1000) || a.Accepts(999) { // wait for b's ack
		if time.Now().After(deadline) {
			t.Fatal("ack never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.Accepts(1001) {
		t.Fatal("accepts an offset beyond what was written")
	}
	c1, c2 := tcpPair(t)
	go func() { // a peer that claims 5000 bytes
		var buf [8]byte
		io.ReadFull(c2, buf[:])
		binary.BigEndian.PutUint64(buf[:], 5000)
		c2.Write(buf[:])
	}()
	if err := a.Resume(c1, swapCounts(c1)); !errors.Is(err, ErrResumeOffset) {
		t.Fatalf("resume error = %v, want offset error", err)
	}
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("stream still open after an offset mismatch")
	}
}

// TestResumableUnderYamux: yamux streams opened on top keep working across
// transport drops, which is what the tunnel relies on.
func TestResumableUnderYamux(t *testing.T) {
	a, b := NewResumable(ResumableConfig{}), NewResumable(ResumableConfig{})
	ca, cb := tcpPair(t)
	attach(t, a, b, ca, cb)
	cli, err := yamux.Client(a, YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv, err := yamux.Server(b, YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	const streams, n = 4, 8 << 20
	type result struct {
		chs [2]chan transfer
	}
	results := make([]result, streams)
	for i := range streams {
		s1, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		s1.Write([]byte{byte(i)}) // make the stream visible to Accept
		s2, err := srv.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		io.ReadFull(s2, make([]byte, 1))
		ab, ba := transferBoth(s1, s2, n)
		results[i] = result{[2]chan transfer{ab, ba}}
	}
	for i := range 6 {
		time.Sleep(60 * time.Millisecond)
		ca.Close()
		if i%2 == 0 {
			cb.Close()
			time.Sleep(300 * time.Millisecond)
		}
		ca, cb = tcpPair(t)
		attach(t, a, b, ca, cb)
	}
	for i, r := range results {
		checkTransfer(t, "stream "+string(rune('0'+i))+" a->b", r.chs[0])
		checkTransfer(t, "stream "+string(rune('0'+i))+" b->a", r.chs[1])
	}
	if cli.IsClosed() || srv.IsClosed() {
		t.Fatal("yamux session closed across drops")
	}
}

// BenchmarkResumable measures one-way throughput over loopback; the link it
// carries tops out near 300 Mbit/s (37.5 MB/s).
func BenchmarkResumable(b *testing.B) {
	t := &testing.T{}
	x, y := NewResumable(ResumableConfig{}), NewResumable(ResumableConfig{})
	defer x.Close()
	defer y.Close()
	cx, cy := tcpPair(t)
	attach(t, x, y, cx, cy)
	buf := make([]byte, 64<<10)
	b.SetBytes(int64(len(buf)))
	done := make(chan struct{})
	go func() {
		io.CopyN(io.Discard, y, int64(b.N)*int64(len(buf)))
		close(done)
	}()
	b.ResetTimer()
	for range b.N {
		x.Write(buf)
	}
	<-done
}

// TestHello: the phone accepts PTL3 with the right token and rejects older
// versions and bad tokens.
func TestHello(t *testing.T) {
	for _, tc := range []struct {
		magic, token string
		ok, badMagic bool
	}{
		{magic, "tok", true, false},
		{magic, "bad", false, false},
		{"PTL2", "tok", false, true},
		{"PTL1", "tok", false, true},
	} {
		c1, c2 := tcpPair(t)
		go func() {
			c1.Write(append([]byte(tc.magic), append([]byte{byte(len(tc.token))}, tc.token...)...))
		}()
		err := ServerHello(c2, "tok")
		if (err == nil) != tc.ok || errors.Is(err, ErrBadMagic) != tc.badMagic {
			t.Errorf("%s/%s: err %v", tc.magic, tc.token, err)
		}
		c1.Close()
		c2.Close()
	}
}

// dgSink collects datagrams delivered by OnDatagram.
type dgSink struct {
	mu  sync.Mutex
	got [][]byte
}

func (s *dgSink) add(b []byte) {
	s.mu.Lock()
	s.got = append(s.got, b)
	s.mu.Unlock()
}

func (s *dgSink) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDatagramsArrive: datagrams go both ways, in order, intact.
func TestDatagramsArrive(t *testing.T) {
	var sa, sb dgSink
	a, b := NewResumable(ResumableConfig{OnDatagram: sa.add}), NewResumable(ResumableConfig{OnDatagram: sb.add})
	defer a.Close()
	defer b.Close()
	ca, cb := tcpPair(t)
	attach(t, a, b, ca, cb)
	for i := range 100 {
		if !a.WriteDatagram([]byte{byte(i), 1}) || !b.WriteDatagram([]byte{byte(i), 2}) {
			t.Fatalf("datagram %d not queued", i)
		}
	}
	waitFor(t, "datagrams", func() bool { return sa.n() == 100 && sb.n() == 100 })
	for i := range 100 {
		if !bytes.Equal(sb.got[i], []byte{byte(i), 1}) || !bytes.Equal(sa.got[i], []byte{byte(i), 2}) {
			t.Fatalf("datagram %d out of order or changed", i)
		}
	}
}

// TestDatagramsNotResent: datagrams sent while detached are dropped at once,
// and nothing queued before a drop shows up after the Resume; the byte
// stream still arrives whole.
func TestDatagramsNotResent(t *testing.T) {
	var sb dgSink
	st := new(DatagramStats)
	a, b := NewResumable(ResumableConfig{Datagrams: st}), NewResumable(ResumableConfig{OnDatagram: sb.add})
	defer a.Close()
	defer b.Close()
	ca, cb := tcpPair(t)
	attach(t, a, b, ca, cb)
	ca.Close()
	cb.Close()
	waitFor(t, "detach", func() bool { return !a.Attached() })
	if a.WriteDatagram([]byte("late")) {
		t.Fatal("datagram queued with no transport")
	}
	a.Write([]byte("stream"))
	ca, cb = tcpPair(t)
	attach(t, a, b, ca, cb)
	got := make([]byte, 6)
	if _, err := io.ReadFull(b, got); err != nil || string(got) != "stream" {
		t.Fatalf("stream after resume: %q %v", got, err)
	}
	a.WriteDatagram([]byte("fresh"))
	waitFor(t, "fresh datagram", func() bool { return sb.n() == 1 })
	if string(sb.got[0]) != "fresh" {
		t.Fatalf("got %q, want only the datagram sent after the resume", sb.got[0])
	}
	if st.DropNoLink.Load() != 1 {
		t.Fatalf("no-link drops = %d, want 1", st.DropNoLink.Load())
	}
}

// TestDatagramsAheadOfBulk: while megabytes of stream data are queued, a
// datagram still arrives quickly, and sustained datagrams don't stop the stream.
// latencyBudget widens a wall-clock latency limit on shared CI runners, where
// scheduling and the Windows timer (about 15 ms) add delay the code does not cause.
func latencyBudget(d time.Duration) time.Duration {
	if os.Getenv("CI") != "" {
		return 3 * d
	}
	return d
}

func TestDatagramsAheadOfBulk(t *testing.T) {
	var sb dgSink
	at := make(chan time.Time, 1)
	b := NewResumable(ResumableConfig{OnDatagram: func(p []byte) {
		sb.add(p)
		if string(p) == "probe" {
			at <- time.Now()
		}
	}})
	a := NewResumable(ResumableConfig{})
	defer a.Close()
	defer b.Close()
	// A slow transport, so the bulk backlog sits in the Resumable.
	ca, cb := throttledPair(t, 4<<20) // 4 MB/s
	attach(t, a, b, ca, cb)
	go a.Write(make([]byte, 32<<20)) // 8 s of backlog
	go io.Copy(io.Discard, b)
	time.Sleep(200 * time.Millisecond)
	sent := time.Now()
	a.WriteDatagram([]byte("probe"))
	select {
	case got := <-at:
		t.Logf("datagram behind 16 MB of backlog: %v", got.Sub(sent))
		if d := got.Sub(sent); d > latencyBudget(150*time.Millisecond) {
			t.Fatalf("datagram took %v behind bulk data", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("datagram never arrived")
	}
	// Once the pacer has settled (after its first probe for the base delay,
	// 2 s in), the queue ahead of a datagram is short.
	time.Sleep(2500 * time.Millisecond)
	sent = time.Now()
	a.WriteDatagram([]byte("probe"))
	select {
	case got := <-at:
		t.Logf("datagram behind backlog, pacer settled: %v (in flight: %s)", got.Sub(sent), a.Pace())
		if d := got.Sub(sent); d > latencyBudget(40*time.Millisecond) {
			t.Fatalf("datagram took %v once the pacer settled", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second datagram never arrived")
	}
	// Datagrams faster than the link: the stream must keep moving.
	before := b.Received()
	stop := time.After(time.Second)
	payload := make([]byte, 1200)
loop:
	for {
		select {
		case <-stop:
			break loop
		default:
			a.WriteDatagram(payload)
		}
	}
	moved := b.Received() - before
	t.Logf("stream bytes during 1 s of datagram flood: %d", moved)
	if moved < 200<<10 {
		t.Fatalf("stream moved %d bytes in 1 s of datagram flood", moved)
	}
}

// TestUnknownAndOversizeFrames: frames this side doesn't know, and datagrams
// larger than a frame, are skipped without failing the transport.
func TestUnknownAndOversizeFrames(t *testing.T) {
	var sb dgSink
	downs := make(chan error, 1)
	b := NewResumable(ResumableConfig{OnDatagram: sb.add, OnDown: func(err error) { downs <- err }})
	defer b.Close()
	ca, cb := tcpPair(t)
	go b.Resume(cb, swapCounts(cb))
	swapCounts(ca)(0)
	frame := func(typ byte, n int) []byte {
		h := []byte{typ, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(h[1:], uint32(n))
		return append(h, make([]byte, n)...)
	}
	ca.Write(frame(9, 100))                // unknown type
	ca.Write(frame(frameDatagram, 70<<10)) // oversize datagram
	ca.Write(frame(frameDatagram, 0))      // empty datagram
	ok := frame(frameDatagram, 3)
	copy(ok[5:], "end")
	ca.Write(ok)
	waitFor(t, "datagram after skipped frames", func() bool { return sb.n() == 1 })
	select {
	case err := <-downs:
		t.Fatalf("transport failed: %v", err)
	default:
	}
}

// throttledPair is a loopback connection whose a→b direction carries at most
// rate bytes per second.
func throttledPair(t *testing.T, rate int) (net.Conn, net.Conn) {
	a, a2 := tcpPair(t)
	b2, b := tcpPair(t)
	go func() {
		buf := make([]byte, 16<<10)
		for {
			n, err := a2.Read(buf)
			if err != nil {
				b2.Close()
				return
			}
			time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
			if _, err := b2.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	go func() { io.Copy(a2, b2); a2.Close() }()
	return a, b
}

func TestDatagramCodec(t *testing.T) {
	for _, d := range []Datagram{
		{Port: 5353, Addr: netip.MustParseAddrPort("1.2.3.4:443"), Data: []byte("hi")},
		{Port: 1, Addr: netip.MustParseAddrPort("[2001:db8::1]:19302"), Data: nil},
		{Port: 7, Addr: netip.MustParseAddrPort("[::ffff:8.8.8.8]:53"), Data: []byte{0}},
	} {
		got, err := ParseDatagram(AppendDatagram(nil, d))
		want := netip.AddrPortFrom(d.Addr.Addr().Unmap(), d.Addr.Port())
		if err != nil || got.Port != d.Port || got.Addr != want || !bytes.Equal(got.Data, d.Data) {
			t.Errorf("%v: got %+v, %v", d, got, err)
		}
	}
	if _, err := ParseDatagram([]byte{4, 0}); err == nil {
		t.Error("short datagram parsed")
	}
}
