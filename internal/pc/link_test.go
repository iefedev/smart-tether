package pc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"smarttether/internal/link"
	"smarttether/internal/relay"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// source is a stand-in for an internet server: every connection gets the
// same n pseudo-random bytes.
func source(t *testing.T, n int) (addr string, sum []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	gen := func(w io.Writer) {
		r := rand.NewChaCha8([32]byte{1})
		buf := make([]byte, 32<<10)
		for left := n; left > 0; {
			k := min(left, len(buf))
			r.Read(buf[:k])
			if _, err := w.Write(buf[:k]); err != nil {
				return
			}
			left -= k
		}
	}
	h := sha256.New()
	gen(h)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { gen(c); c.Close() }()
		}
	}()
	return ln.Addr().String(), h.Sum(nil)
}

// fakePhone lets the test plug and unplug transports.
type fakePhone struct {
	usb, wifi string

	mu       sync.Mutex
	usbIn    bool
	wifiIn   bool
	conns    []net.Conn
	dialsUSB int
}

func (p *fakePhone) candidates() []Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Candidate
	if p.usbIn {
		out = append(out, Candidate{p.usb, USB})
	}
	if p.wifiIn {
		out = append(out, Candidate{p.wifi, WiFi})
	}
	return out
}

func (p *fakePhone) dial(ctx context.Context, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err == nil {
		p.mu.Lock()
		p.conns = append(p.conns, c)
		p.mu.Unlock()
	}
	return c, err
}

// set changes what is plugged in and resets every live link connection
// that is no longer reachable.
func (p *fakePhone) set(usb, wifi bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usbIn, p.wifiIn = usb, wifi
	keep := p.conns[:0]
	for _, c := range p.conns {
		ra := c.RemoteAddr().String()
		if (ra == p.usb && !usb) || (ra == p.wifi && !wifi) {
			c.Close()
			continue
		}
		keep = append(keep, c)
	}
	p.conns = keep
}

func startRelay(t *testing.T, p *fakePhone) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	srv := relay.NewServer(ctx, relay.Config{
		Token:        "tok",
		DialInternet: (&net.Dialer{}).DialContext,
		Logf:         t.Logf,
	})
	srv.SetListeners([]string{p.usb, p.wifi})
	return cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *Link) state() (up bool, t Transport, id link.SessionID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.up, l.cur, l.id
}

func openTCP(t *testing.T, l *Link, dst string) net.Conn {
	t.Helper()
	s, err := l.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := link.WriteRequest(s, link.KindTCP, dst); err != nil {
		t.Fatal(err)
	}
	var st [1]byte
	if _, err := io.ReadFull(s, st[:]); err != nil || link.Status(st[0]) != link.StatusOK {
		t.Fatalf("status %d, %v", st[0], err)
	}
	return s
}

// TestLinkFlowSurvivesTransportChanges downloads 96 MB through the real PC
// Link and phone relay while the cable is pulled (USB → Wi-Fi), plugged back
// (Wi-Fi → USB while both work), and everything drops for 2 s, and checks
// the flow completes byte-exact on one session.
func TestLinkFlowSurvivesTransportChanges(t *testing.T) {
	const n = 96 << 20
	src, want := source(t, n)
	p := &fakePhone{usb: freeAddr(t), wifi: freeAddr(t), usbIn: true, wifiIn: true}
	stop := startRelay(t, p)
	defer stop()

	l := &Link{Token: "tok", Candidates: p.candidates, Dial: p.dial}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	waitFor(t, "link up on usb", func() bool { up, tr, _ := l.state(); return up && tr == USB })
	_, _, id0 := l.state()

	s := openTCP(t, l, src)
	h := sha256.New()
	buf := make([]byte, 64<<10)
	got := 0
	steps := []struct {
		at   int
		name string
		do   func()
	}{
		{10 << 20, "unplug usb", func() {
			p.set(false, true)
			waitFor(t, "resume on wifi", func() bool { up, tr, _ := l.state(); return up && tr == WiFi })
		}},
		{30 << 20, "replug usb", func() {
			p.set(true, true)
			waitFor(t, "move to usb", func() bool { up, tr, _ := l.state(); return up && tr == USB })
		}},
		{50 << 20, "everything down 2 s", func() {
			p.set(false, false)
			time.Sleep(2 * time.Second)
			if up, _, _ := l.state(); up {
				t.Fatal("link still up with nothing plugged in")
			}
			p.set(true, true)
			waitFor(t, "resume on usb", func() bool { up, tr, _ := l.state(); return up && tr == USB })
		}},
	}
	for got < n {
		if len(steps) > 0 && got >= steps[0].at {
			t.Logf("%d MB: %s", got>>20, steps[0].name)
			steps[0].do()
			steps = steps[1:]
		}
		k, err := s.Read(buf)
		h.Write(buf[:k])
		got += k
		if err != nil {
			if err == io.EOF && got == n {
				break
			}
			t.Fatalf("read after %d bytes: %v", got, err)
		}
	}
	if !bytes.Equal(h.Sum(nil), want) {
		t.Fatal("downloaded bytes differ from the source")
	}
	if _, _, id := l.state(); id != id0 {
		t.Fatal("session was replaced; flows would have broken")
	}
}

// udpEcho answers each datagram with the sender's address as text.
func udpEcho(t *testing.T) netip.AddrPort {
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
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

// TestLinkDatagramsKeepPort: a PC UDP socket keeps its phone-side port across
// a USB→Wi-Fi switch and across a whole new session, and sees the same port
// from two destinations.
func TestLinkDatagramsKeepPort(t *testing.T) {
	e1, e2 := udpEcho(t), udpEcho(t)
	p := &fakePhone{usb: freeAddr(t), wifi: freeAddr(t), usbIn: true, wifiIn: true}
	stop := startRelay(t, p)
	defer stop()
	replies := make(chan link.Datagram, 16)
	l := &Link{Token: "tok", Candidates: p.candidates, Dial: p.dial, OnDatagram: func(b []byte) {
		d, err := link.ParseDatagram(b)
		if err == nil {
			replies <- d
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	waitFor(t, "link up on usb", func() bool { up, tr, _ := l.state(); return up && tr == USB })

	ask := func(dst netip.AddrPort) string {
		t.Helper()
		for range 20 { // datagrams may be dropped while a transport is coming up
			l.WriteDatagram(link.AppendDatagram(nil, link.Datagram{Port: 5000, Addr: dst, Data: []byte("?")}))
			select {
			case d := <-replies:
				if d.Port != 5000 || d.Addr != dst {
					t.Fatalf("reply for %d from %s, want 5000 from %s", d.Port, d.Addr, dst)
				}
				return string(d.Data)
			case <-time.After(250 * time.Millisecond):
			}
		}
		t.Fatalf("no reply from %s", dst)
		return ""
	}
	m := ask(e1)
	if m2 := ask(e2); m2 != m {
		t.Fatalf("two destinations saw %s and %s", m, m2)
	}
	p.set(false, true)
	waitFor(t, "resume on wifi", func() bool { up, tr, _ := l.state(); return up && tr == WiFi })
	if got := ask(e1); got != m {
		t.Fatalf("after usb→wifi the port is %s, was %s", got, m)
	}
	_, _, id0 := l.state()
	l.Drop()
	waitFor(t, "new session", func() bool { up, _, id := l.state(); return up && id != id0 })
	if got := ask(e1); got != m {
		t.Fatalf("on a new session the port is %s, was %s", got, m)
	}
}

// TestLinkNewSessionWhenPhoneRestarts: if the phone app restarts it no
// longer knows the session; the PC starts a new one and new flows work.
func TestLinkNewSessionWhenPhoneRestarts(t *testing.T) {
	src, want := source(t, 1<<20)
	p := &fakePhone{usb: freeAddr(t), wifi: freeAddr(t), usbIn: true}
	stop := startRelay(t, p)

	l := &Link{Token: "tok", Candidates: p.candidates, Dial: p.dial}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	waitFor(t, "link up", func() bool { up, _, _ := l.state(); return up })
	_, _, id0 := l.state()
	io.Copy(io.Discard, openTCP(t, l, src))

	stop()
	p.set(false, false) // the app's sockets die with it
	time.Sleep(200 * time.Millisecond)
	stop = startRelay(t, p)
	defer stop()
	p.set(true, false)
	waitFor(t, "new session", func() bool { up, _, id := l.state(); return up && id != id0 })

	h := sha256.New()
	if _, err := io.Copy(h, openTCP(t, l, src)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.Sum(nil), want) {
		t.Fatal("bytes differ on the new session")
	}
}
