package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"smarttether/internal/link"
)

// udpIdle closes a phone socket, or forgets one of its remotes, after this
// long without a datagram.
const udpIdle = 2 * time.Minute

// natKey names a PC UDP socket: its family and source port on the tunnel.
type natKey struct {
	v6   bool
	port uint16
}

// udpNAT gives each PC UDP socket one unconnected cellular socket, like a home
// router: the same public port for every destination (endpoint-independent
// mapping), and replies only from remotes that socket has sent to
// (address-and-port-dependent filtering). It belongs to the Server, not to a
// session, so a socket keeps its public port across transport switches and
// session replacement.
type udpNAT struct {
	ctx    context.Context
	listen func(ctx context.Context, network string) (*net.UDPConn, error)
	// toPC hands a reply to the current session; false when there is none.
	toPC func(b []byte) bool
	logf func(format string, args ...any)
	// up and down count payload bytes like the Server's TCP counters.
	up, down *atomic.Uint64
	idle     time.Duration // udpIdle; shorter in tests

	mu    sync.Mutex
	socks map[natKey]*natSock

	dropNoSock, dropQueue, dropFilter, dropNoPC  atomic.Uint64
	errRefused, errMsgSize, errUnreach, errOther atomic.Uint64
}

type natSock struct {
	key  natKey
	c    *net.UDPConn
	q    chan outDatagram
	last atomic.Int64 // unix nanos of the last datagram either way

	mu      sync.Mutex
	remotes map[netip.AddrPort]time.Time // allowed senders, by when traffic last went either way
	closed  bool
	unreach int // consecutive "network gone" send errors
}

type outDatagram struct {
	dst  netip.AddrPort
	data []byte
}

func newUDPNAT(ctx context.Context, listen func(context.Context, string) (*net.UDPConn, error),
	toPC func([]byte) bool, logf func(string, ...any), up, down *atomic.Uint64) *udpNAT {
	return newUDPNATIdle(ctx, listen, toPC, logf, up, down, udpIdle)
}

func newUDPNATIdle(ctx context.Context, listen func(context.Context, string) (*net.UDPConn, error),
	toPC func([]byte) bool, logf func(string, ...any), up, down *atomic.Uint64, idle time.Duration) *udpNAT {
	n := &udpNAT{ctx: ctx, listen: listen, toPC: toPC, logf: logf, up: up, down: down, idle: idle,
		socks: map[natKey]*natSock{}}
	go n.sweep()
	return n
}

// fromPC sends one PC datagram. It runs on the link's reader, so it never
// blocks on the network: the socket's own goroutine does the write.
func (n *udpNAT) fromPC(d link.Datagram) {
	dst := netip.AddrPortFrom(d.Addr.Addr().Unmap(), d.Addr.Port())
	k := natKey{v6: !dst.Addr().Is4(), port: d.Port}
	s := n.sock(k)
	if s == nil {
		n.dropNoSock.Add(1)
		return
	}
	now := time.Now()
	s.last.Store(now.UnixNano())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed { // closed since sock returned it
		n.dropNoSock.Add(1)
		return
	}
	s.remotes[dst] = now
	select {
	case s.q <- outDatagram{dst, d.Data}:
	default:
		n.dropQueue.Add(1)
	}
}

// sock returns the socket for k, opening it on first use.
func (n *udpNAT) sock(k natKey) *natSock {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := n.socks[k]; s != nil {
		return s
	}
	if n.ctx.Err() != nil {
		return nil
	}
	network := "udp4"
	if k.v6 {
		network = "udp6"
	}
	c, err := n.listen(n.ctx, network)
	if err != nil {
		n.logf("udp: open %s socket for pc port %d: %v", network, k.port, err)
		return nil
	}
	s := &natSock{key: k, c: c, q: make(chan outDatagram, 128), remotes: map[netip.AddrPort]time.Time{}}
	s.last.Store(time.Now().UnixNano())
	n.socks[k] = s
	go n.send(s)
	go n.receive(s)
	return s
}

func (n *udpNAT) send(s *natSock) {
	for d := range s.q {
		_, err := s.c.WriteToUDPAddrPort(d.data, d.dst)
		if err == nil {
			n.up.Add(uint64(len(d.data)))
			s.unreach = 0
			continue
		}
		if errors.Is(err, net.ErrClosed) {
			return
		}
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			n.errRefused.Add(1)
		case errors.Is(err, syscall.EMSGSIZE):
			n.errMsgSize.Add(1)
		case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH),
			errors.Is(err, syscall.ENONET), errors.Is(err, syscall.EPERM):
			n.errUnreach.Add(1)
			// A socket bound to a cellular network that went away fails
			// every send; normally CellularChanged closes it first.
			if s.unreach++; s.unreach == 50 {
				n.logf("udp: pc port %d: %v; reopening", s.key.port, err)
				n.close(s)
				return
			}
		default:
			n.errOther.Add(1)
		}
	}
}

func (n *udpNAT) receive(s *natSock) {
	defer n.close(s)
	buf := make([]byte, 65535)
	for {
		m, from, err := s.c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				n.logf("udp: pc port %d: read: %v", s.key.port, err)
			}
			return
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		now := time.Now()
		s.mu.Lock()
		_, ok := s.remotes[from]
		if ok {
			s.remotes[from] = now
		}
		s.mu.Unlock()
		if !ok {
			n.dropFilter.Add(1)
			continue
		}
		s.last.Store(now.UnixNano())
		n.down.Add(uint64(m))
		if !n.toPC(link.AppendDatagram(nil, link.Datagram{Port: s.key.port, Addr: from, Data: buf[:m]})) {
			n.dropNoPC.Add(1)
		}
	}
}

// close removes s from the table (if it is still there) and closes it.
func (n *udpNAT) close(s *natSock) {
	n.mu.Lock()
	if n.socks[s.key] == s {
		delete(n.socks, s.key)
	}
	n.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.c.Close()
	close(s.q) // fromPC no longer finds s, so nothing sends on q after this
}

// closeAll closes every socket; the next datagram opens a new one (with a
// new public port).
func (n *udpNAT) closeAll() {
	n.mu.Lock()
	all := make([]*natSock, 0, len(n.socks))
	for _, s := range n.socks {
		all = append(all, s)
	}
	n.mu.Unlock()
	for _, s := range all {
		n.close(s)
	}
}

// sweep expires idle sockets and remotes, and closes everything when the
// relay stops.
func (n *udpNAT) sweep() {
	t := time.NewTicker(min(10*time.Second, n.idle/4))
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			n.closeAll()
			return
		case <-t.C:
		}
		now := time.Now()
		n.mu.Lock()
		all := make([]*natSock, 0, len(n.socks))
		for _, s := range n.socks {
			all = append(all, s)
		}
		n.mu.Unlock()
		for _, s := range all {
			if now.Sub(time.Unix(0, s.last.Load())) > n.idle {
				n.close(s)
				continue
			}
			s.mu.Lock()
			for r, at := range s.remotes {
				if now.Sub(at) > n.idle {
					delete(s.remotes, r)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (n *udpNAT) sockets() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.socks)
}

func (n *udpNAT) String() string {
	return fmt.Sprintf("udp_sockets=%d drop_nosock=%d drop_queue=%d drop_filter=%d drop_nopc=%d send_err_refused=%d send_err_msgsize=%d send_err_unreach=%d send_err_other=%d",
		n.sockets(), n.dropNoSock.Load(), n.dropQueue.Load(), n.dropFilter.Load(), n.dropNoPC.Load(),
		n.errRefused.Load(), n.errMsgSize.Load(), n.errUnreach.Load(), n.errOther.Load())
}
