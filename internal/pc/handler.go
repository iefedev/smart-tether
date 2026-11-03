package pc

import (
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/core/adapter"

	"smarttether/internal/link"
)

// Virtual addresses inside the TUN subnet.
var (
	DNSAddr  = netip.MustParseAddr("198.18.0.2")     // the PC's DNS server
	NCSIAddr = netip.MustParseAddr("198.18.255.254") // answers Windows' connectivity probe
)

// Handler turns stack flows into link streams.
type Handler struct {
	Link *Link
	// noQUIC drops UDP/443 so browsers use TCP instead of QUIC (SetQUIC).
	noQUIC atomic.Bool

	tcpActive  atomic.Int64
	tcpOpenErr atomic.Int64 // no link / stream open failed
	tcpRefused atomic.Int64 // phone could not reach the destination
	udpActive  atomic.Int64
	udpNoMatch atomic.Int64 // datagrams from the phone with no PC flow to deliver them to

	udpMu sync.Mutex
	flows map[udpKey]*udpFlow
}

// udpKey names a PC UDP flow the way the phone's datagrams do: the PC
// socket's source port and the internet end.
type udpKey struct {
	port   uint16
	remote netip.AddrPort
}

type udpFlow struct {
	c    adapter.UDPConn
	last atomic.Int64 // unix nanos of the last datagram either way
}

var _ adapter.TransportHandler = (*Handler)(nil)

func (h *Handler) HandleTCP(c adapter.TCPConn) {
	id := c.ID()
	dst := netip.AddrPortFrom(addrOf(id.LocalAddress.AsSlice()), id.LocalPort)
	go h.tcp(c, dst)
}

func (h *Handler) tcp(c net.Conn, dst netip.AddrPort) {
	h.tcpActive.Add(1)
	defer h.tcpActive.Add(-1)
	switch {
	case dst.Addr() == NCSIAddr && dst.Port() == 80:
		serveNCSIHTTP(c)
		return
	case dst.Addr() == DNSAddr && dst.Port() == 53:
		h.dnsOverTCP(c)
		return
	case isVirtual(dst.Addr()):
		c.Close()
		return
	}
	s, err := h.Link.Open()
	if err != nil {
		h.tcpOpenErr.Add(1)
		c.Close()
		return
	}
	if err := link.WriteRequest(s, link.KindTCP, dst.String()); err != nil {
		h.tcpOpenErr.Add(1)
		s.Close()
		c.Close()
		return
	}
	s.SetReadDeadline(time.Now().Add(15 * time.Second))
	var st [1]byte
	if _, err := io.ReadFull(s, st[:]); err != nil || link.Status(st[0]) != link.StatusOK {
		h.tcpRefused.Add(1)
		if err == nil {
			log.Printf("tcp %s: phone status %d", dst, st[0])
		} else {
			log.Printf("tcp %s: no status: %v", dst, err)
		}
		s.Close()
		c.Close()
		return
	}
	s.SetReadDeadline(time.Time{})
	link.Pipe(c, s)
}

func (h *Handler) HandleUDP(c adapter.UDPConn) {
	id := c.ID()
	dst := netip.AddrPortFrom(addrOf(id.LocalAddress.AsSlice()), id.LocalPort)
	srcPort := id.RemotePort
	switch {
	case dst.Addr() == DNSAddr && dst.Port() == 53, dst.Port() == 53 && !isVirtual(dst.Addr()):
		// DNS to any server is answered by the phone's carrier resolvers.
		go h.dnsOverUDP(c)
	case isVirtual(dst.Addr()):
		c.Close()
	case h.noQUIC.Load() && dst.Port() == 443:
		c.Close() // browsers fall back to TCP
	default:
		go h.udp(c, srcPort, dst)
	}
}

// SetQUIC turns carrying UDP/443 on or off for new flows.
func (h *Handler) SetQUIC(on bool) { h.noQUIC.Store(!on) }

// udpIdle closes a UDP flow after this long without a datagram either way.
const udpIdle = 2 * time.Minute

// udp sends one UDP 5-tuple's datagrams to the phone as link datagrams. The
// flow doesn't depend on the session: while the link is down its datagrams
// are dropped, as a router would, and it ends only when idle.
func (h *Handler) udp(c adapter.UDPConn, srcPort uint16, dst netip.AddrPort) {
	h.udpActive.Add(1)
	defer h.udpActive.Add(-1)
	k := udpKey{srcPort, dst}
	f := &udpFlow{c: c}
	f.last.Store(time.Now().UnixNano())
	h.udpMu.Lock()
	if h.flows == nil {
		h.flows = map[udpKey]*udpFlow{}
	}
	old := h.flows[k]
	h.flows[k] = f
	h.udpMu.Unlock()
	if old != nil {
		old.c.Close()
	}
	done := make(chan struct{})
	defer func() {
		close(done)
		h.udpMu.Lock()
		if h.flows[k] == f {
			delete(h.flows, k)
		}
		h.udpMu.Unlock()
		c.Close()
	}()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, f.last.Load())) > udpIdle {
					c.Close() // ends the Read below
					return
				}
			}
		}
	}()
	buf := make([]byte, 65535)
	out := make([]byte, 0, len(buf)+32)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		f.last.Store(time.Now().UnixNano())
		out = link.AppendDatagram(out[:0], link.Datagram{Port: srcPort, Addr: dst, Data: buf[:n]})
		h.Link.WriteDatagram(out)
	}
}

// FromPhone delivers a datagram from the phone to the PC flow it answers.
// Only flows the PC opened receive (address-and-port-dependent filtering).
// It runs on the link's reader, so the write must not wait.
func (h *Handler) FromPhone(b []byte) {
	d, err := link.ParseDatagram(b)
	if err != nil {
		return
	}
	h.udpMu.Lock()
	f := h.flows[udpKey{d.Port, d.Addr}]
	h.udpMu.Unlock()
	if f == nil {
		h.udpNoMatch.Add(1)
		return
	}
	f.last.Store(time.Now().UnixNano())
	f.c.SetWriteDeadline(time.Now().Add(5 * time.Millisecond))
	f.c.Write(d.Data)
}

// dnsOverUDP serves one client 5-tuple until it goes idle.
func (h *Handler) dnsOverUDP(c adapter.UDPConn) {
	defer c.Close()
	buf := make([]byte, 4096)
	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if a := h.resolve(q); a != nil {
				c.Write(a)
			}
		}()
	}
}

func (h *Handler) dnsOverTCP(c net.Conn) {
	defer c.Close()
	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		q, err := link.ReadMsg(c)
		if err != nil {
			return
		}
		a := h.resolve(q)
		if a == nil {
			return
		}
		if err := link.WriteMsg(c, a); err != nil {
			return
		}
	}
}

// resolve answers NCSI names locally and forwards everything else to the phone.
func (h *Handler) resolve(q []byte) []byte {
	if a := answerLocally(q); a != nil {
		return a
	}
	s, err := h.Link.Open()
	if err != nil {
		return nil
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(8 * time.Second))
	if link.WriteRequest(s, link.KindDNS, "") != nil || link.WriteMsg(s, q) != nil {
		return nil
	}
	a, err := link.ReadMsg(s)
	if err != nil {
		log.Printf("dns: %v", err)
		return nil
	}
	return a
}

func addrOf(b []byte) netip.Addr {
	a, _ := netip.AddrFromSlice(b)
	return a.Unmap()
}

var virtualNet = netip.MustParsePrefix("198.18.0.0/16")

func isVirtual(a netip.Addr) bool { return virtualNet.Contains(a) }

func (h *Handler) Stats() string {
	return "tcp=" + strconv.FormatInt(h.tcpActive.Load(), 10) +
		" open_err=" + strconv.FormatInt(h.tcpOpenErr.Load(), 10) +
		" refused=" + strconv.FormatInt(h.tcpRefused.Load(), 10) +
		" udp=" + strconv.FormatInt(h.udpActive.Load(), 10) +
		" udp_nomatch=" + strconv.FormatInt(h.udpNoMatch.Load(), 10) +
		" " + h.Link.DatagramStats().String() +
		" " + h.Link.Pace()
}
