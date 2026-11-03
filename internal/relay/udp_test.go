package relay

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smarttether/internal/link"
)

// echoServer answers every datagram with the sender's address, so a test can
// see which public port the NAT used.
func echoServer(t *testing.T) (netip.AddrPort, *net.UDPConn) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			_, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			c.WriteToUDPAddrPort([]byte(from.String()), from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).AddrPort(), c
}

type pcSide struct {
	mu  sync.Mutex
	got []link.Datagram
}

func (p *pcSide) toPC(b []byte) bool {
	d, err := link.ParseDatagram(b)
	if err != nil {
		panic(err)
	}
	p.mu.Lock()
	p.got = append(p.got, d)
	p.mu.Unlock()
	return true
}

// await returns the first reply from from to PC port port.
func (p *pcSide) await(t *testing.T, port uint16, from netip.AddrPort) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for i, d := range p.got {
			if d.Port == port && d.Addr == from {
				p.got = append(p.got[:i], p.got[i+1:]...)
				p.mu.Unlock()
				return string(d.Data)
			}
		}
		p.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no reply from %s to pc port %d", from, port)
	return ""
}

func newTestNAT(t *testing.T, idle time.Duration) (*udpNAT, *pcSide) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pc := &pcSide{}
	var up, down atomic.Uint64
	listen := func(ctx context.Context, network string) (*net.UDPConn, error) {
		return net.ListenUDP(network, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	}
	return newUDPNATIdle(ctx, listen, pc.toPC, t.Logf, &up, &down, idle), pc
}

// TestNATMapping: one PC socket gets one phone port for every destination;
// another PC socket gets another; a refused destination doesn't break the flow.
func TestNATMapping(t *testing.T) {
	n, pc := newTestNAT(t, time.Minute)
	e1, _ := echoServer(t)
	e2, _ := echoServer(t)
	n.fromPC(link.Datagram{Port: 5000, Addr: e1, Data: []byte("a")})
	n.fromPC(link.Datagram{Port: 5000, Addr: e2, Data: []byte("b")})
	n.fromPC(link.Datagram{Port: 6000, Addr: e1, Data: []byte("c")})
	m1, m2, m3 := pc.await(t, 5000, e1), pc.await(t, 5000, e2), pc.await(t, 6000, e1)
	if m1 != m2 {
		t.Fatalf("pc port 5000 mapped to %s and %s: not endpoint-independent", m1, m2)
	}
	if m1 == m3 {
		t.Fatalf("pc ports 5000 and 6000 share mapping %s", m1)
	}
	// A closed port answers with ICMP port unreachable; the flow carries on.
	closed, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	dead := closed.LocalAddr().(*net.UDPAddr).AddrPort()
	closed.Close()
	for range 3 {
		n.fromPC(link.Datagram{Port: 5000, Addr: dead, Data: []byte("x")})
	}
	time.Sleep(100 * time.Millisecond)
	n.fromPC(link.Datagram{Port: 5000, Addr: e1, Data: []byte("again")})
	if m := pc.await(t, 5000, e1); m != m1 {
		t.Fatalf("after a refused destination pc port 5000 maps to %s, was %s", m, m1)
	}
	if got := n.sockets(); got != 2 {
		t.Fatalf("%d sockets, want 2", got)
	}
}

// TestNATFiltering: a remote the PC socket never sent to can't reach it.
func TestNATFiltering(t *testing.T) {
	n, pc := newTestNAT(t, time.Minute)
	e1, _ := echoServer(t)
	n.fromPC(link.Datagram{Port: 5000, Addr: e1, Data: []byte("a")})
	mapped := netip.MustParseAddrPort(pc.await(t, 5000, e1))
	stranger, sc := echoServer(t)
	sc.WriteToUDPAddrPort([]byte("unsolicited"), mapped)
	time.Sleep(100 * time.Millisecond)
	pc.mu.Lock()
	for _, d := range pc.got {
		if d.Addr == stranger {
			t.Fatal("unsolicited datagram reached the pc")
		}
	}
	pc.mu.Unlock()
	if n.dropFilter.Load() != 1 {
		t.Fatalf("filter drops = %d, want 1", n.dropFilter.Load())
	}
}

// TestNATIdleAndReopen: idle sockets close; closeAll (cellular change) reopens
// on the next datagram.
func TestNATIdleAndReopen(t *testing.T) {
	n, pc := newTestNAT(t, 200*time.Millisecond)
	e1, _ := echoServer(t)
	n.fromPC(link.Datagram{Port: 5000, Addr: e1, Data: []byte("a")})
	pc.await(t, 5000, e1)
	deadline := time.Now().Add(3 * time.Second)
	for n.sockets() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle socket not closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	n.fromPC(link.Datagram{Port: 5000, Addr: e1, Data: []byte("b")})
	pc.await(t, 5000, e1)
	n.closeAll()
	if n.sockets() != 0 {
		t.Fatal("closeAll left sockets")
	}
	n.fromPC(link.Datagram{Port: 5000, Addr: e1, Data: []byte("c")})
	pc.await(t, 5000, e1)
}
