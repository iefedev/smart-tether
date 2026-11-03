// Package relay is the phone side: it accepts the PC's link connections and
// turns each stream into an ordinary socket on the phone, so every byte that
// reaches the internet leaves from the phone's own app sockets (not Android
// tethering).
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"smarttether/internal/link"
)

// Config tells the relay how to reach the internet.
type Config struct {
	Token string
	// DialInternet opens an outbound connection for a proxied flow. In the app
	// it binds the socket to the cellular network.
	DialInternet func(ctx context.Context, network, addr string) (net.Conn, error)
	// ListenUDP opens an unconnected UDP socket ("udp4" or "udp6") for PC
	// datagrams; in the app it is bound to the cellular network too.
	ListenUDP func(ctx context.Context, network string) (*net.UDPConn, error)
	// DNSServers are host:port of the resolvers to forward PC queries to
	// (the carrier's, the same ones the phone itself uses).
	DNSServers []string
	Logf       func(format string, args ...any)
}

// Server listens for the PC on a changing set of local addresses.
type Server struct {
	ctx context.Context
	cfg Config

	mu        sync.Mutex
	listeners map[string]net.Listener
	dns       []string
	sessions  map[link.SessionID]*session
	cur       *link.Resumable // the session's stream; at most one exists

	udp *udpNAT
	dg  link.DatagramStats // across sessions
	c   counters
}

// session is the PC's yamux session on a Resumable stream; connections come
// and go underneath it.
type session struct {
	rc    *link.Resumable
	start time.Time
}

func NewServer(ctx context.Context, cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.ListenUDP == nil {
		cfg.ListenUDP = func(ctx context.Context, network string) (*net.UDPConn, error) {
			return net.ListenUDP(network, nil)
		}
	}
	s := &Server{ctx: ctx, cfg: cfg, listeners: map[string]net.Listener{}, dns: cfg.DNSServers,
		sessions: map[link.SessionID]*session{}}
	s.udp = newUDPNAT(ctx, cfg.ListenUDP, s.toPC, cfg.Logf, &s.c.up, &s.c.down)
	go func() {
		<-ctx.Done()
		s.SetListeners(nil)
	}()
	return s
}

// SetListeners opens listeners for new addresses and closes removed ones.
func (s *Server) SetListeners(addrs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]bool{}
	if s.ctx.Err() == nil {
		for _, a := range addrs {
			want[a] = true
		}
	}
	for a, ln := range s.listeners {
		if !want[a] {
			ln.Close()
			delete(s.listeners, a)
			s.cfg.Logf("stopped listening on %s", a)
		}
	}
	for a := range want {
		if _, ok := s.listeners[a]; ok {
			continue
		}
		ln, err := net.Listen("tcp", a)
		if err != nil {
			s.cfg.Logf("listen %s: %v", a, err)
			continue
		}
		s.listeners[a] = ln
		s.cfg.Logf("listening on %s", a)
		go s.acceptLoop(ln)
	}
}

// SetDNSServers replaces the resolver list (the carrier's can change).
func (s *Server) SetDNSServers(servers []string) {
	s.mu.Lock()
	s.dns = servers
	s.mu.Unlock()
}

func (s *Server) dnsServers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dns
}

func (s *Server) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go s.serveConn(c)
	}
}

// CellularChanged is called when the cellular network appears, changes or
// goes away. UDP sockets bound to the old one would fail every send, so they
// are closed; the next datagram of each PC socket opens a new one.
func (s *Server) CellularChanged() { s.udp.closeAll() }

// toPC sends a datagram to the PC over the current session.
func (s *Server) toPC(b []byte) bool {
	s.mu.Lock()
	rc := s.cur
	s.mu.Unlock()
	return rc != nil && rc.WriteDatagram(b)
}

func (s *Server) fromPC(b []byte) {
	d, err := link.ParseDatagram(b)
	if err != nil {
		return
	}
	s.udp.fromPC(d)
}

func (s *Server) serveConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetReadBuffer(4 << 20)
		// No big send buffer: what the kernel holds can't be reordered, so
		// the Resumable keeps the backlog and sends datagrams ahead of it.
		setNotSentLowat(tc, 128<<10)
	}
	if err := link.ServerHello(c, s.cfg.Token); err != nil {
		s.cfg.Logf("hello from %s: %v", c.RemoteAddr(), err)
		c.Close()
		return
	}
	// The PC may name a session we no longer have; it then starts a new one
	// on the same connection.
	for {
		id, pcRecvd, err := link.ServerReadResume(c)
		if err != nil {
			s.cfg.Logf("resume from %s: %v", c.RemoteAddr(), err)
			c.Close()
			return
		}
		sess, isNew := s.lookup(id, pcRecvd)
		if sess == nil {
			s.cfg.Logf("resume from %s: unknown session %x (pc has %d bytes)", c.RemoteAddr(), id[:4], pcRecvd)
			if link.ServerWriteResume(c, link.ResumeUnknown, 0) != nil {
				c.Close()
				return
			}
			continue
		}
		err = sess.rc.Resume(c, func(recvd uint64) (uint64, error) {
			return pcRecvd, link.ServerWriteResume(c, link.ResumeOK, recvd)
		})
		if err != nil {
			s.cfg.Logf("resume from %s: %v", c.RemoteAddr(), err)
			c.Close()
			if isNew {
				sess.rc.Close()
			}
			return
		}
		s.attached(c, isNew)
		if isNew {
			s.cfg.Logf("link up on %s from %s (new session %x)", c.LocalAddr(), c.RemoteAddr(), id[:4])
			go s.serveSession(id, sess)
		} else {
			s.cfg.Logf("link resumed on %s from %s (session %x, age %s, pc had %d bytes)",
				c.LocalAddr(), c.RemoteAddr(), id[:4], time.Since(sess.start).Round(time.Second), pcRecvd)
		}
		return
	}
}

// lookup finds the session the PC names, or creates it when the PC starts a
// new one (nothing received yet). A new session replaces all others: there
// is one PC, and its old flows are gone.
func (s *Server) lookup(id link.SessionID, pcRecvd uint64) (sess *session, isNew bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[id]; sess != nil {
		if sess.rc.Accepts(pcRecvd) {
			return sess, false
		}
		s.cfg.Logf("session %x: pc resumes from %d, outside our buffer; dropping it", id[:4], pcRecvd)
		sess.rc.Close()
		delete(s.sessions, id)
		if s.cur == sess.rc {
			s.cur = nil
		}
		return nil, false
	}
	if pcRecvd != 0 || s.ctx.Err() != nil {
		return nil, false
	}
	for old, o := range s.sessions {
		o.rc.Close()
		delete(s.sessions, old)
	}
	sess = &session{
		rc: link.NewResumable(link.ResumableConfig{
			OnDown: func(err error) {
				s.cfg.Logf("session %x: transport lost: %v", id[:4], err)
				s.transportDown()
			},
			OnDatagram: s.fromPC,
			Datagrams:  &s.dg,
		}),
		start: time.Now(),
	}
	s.sessions[id] = sess
	s.cur = sess.rc
	return sess, true
}

func (s *Server) serveSession(id link.SessionID, sess *session) {
	defer func() {
		s.mu.Lock()
		last := s.sessions[id] == sess
		if last {
			delete(s.sessions, id)
		}
		if s.cur == sess.rc {
			s.cur = nil
		}
		s.mu.Unlock()
		if last {
			s.sessionEnded()
		}
	}()
	ys, err := yamux.Server(sess.rc, link.YamuxConfig())
	if err != nil {
		sess.rc.Close()
		return
	}
	stop := context.AfterFunc(s.ctx, func() { ys.Close(); sess.rc.Close() })
	defer stop()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sess.rc.Done():
				return
			case <-t.C:
				if sess.rc.Attached() {
					s.cfg.Logf("stats: %s %s %s", sess.rc.Pace(), &s.dg, s.udp)
				}
			}
		}
	}()
	for {
		st, err := ys.AcceptStream()
		if err != nil {
			why := sess.rc.Err()
			if why == nil {
				why = err
			}
			s.cfg.Logf("session %x ended after %s: %v (%s; %s)", id[:4], time.Since(sess.start).Round(time.Second), why, &s.dg, s.udp)
			sess.rc.Close()
			return
		}
		go s.handle(st)
	}
}

func (s *Server) handle(st *yamux.Stream) {
	kind, addr, err := link.ReadRequest(st)
	if err != nil {
		st.Close()
		return
	}
	switch kind {
	case link.KindTCP:
		dctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
		out, err := s.cfg.DialInternet(dctx, "tcp", addr)
		cancel()
		st.Write([]byte{byte(statusFor(err))})
		if err != nil {
			s.cfg.Logf("dial %s: %v", addr, err)
			st.Close()
			return
		}
		link.Pipe(st, s.counted(out))
	case link.KindDNS:
		s.serveDNS(st)
	case link.KindInfo:
		json.NewEncoder(st).Encode(s.info())
		st.Close()
	default:
		st.Close()
	}
}

func statusFor(err error) link.Status {
	switch {
	case err == nil:
		return link.StatusOK
	case errors.Is(err, syscall.ECONNREFUSED):
		return link.StatusRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return link.StatusUnreachable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT):
		return link.StatusTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return link.StatusTimeout
	}
	return link.StatusFailed
}

// serveDNS answers length-prefixed queries on one stream until it closes.
func (s *Server) serveDNS(st *yamux.Stream) {
	defer st.Close()
	for {
		q, err := link.ReadMsg(st)
		if err != nil {
			return
		}
		a, err := s.exchange(q)
		if err != nil {
			s.cfg.Logf("dns: %v", err)
			continue // the PC side times out
		}
		if err := link.WriteMsg(st, a); err != nil {
			return
		}
	}
}

func (s *Server) exchange(q []byte) ([]byte, error) {
	var last error
	for _, srv := range s.dnsServers() {
		dctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		c, err := s.cfg.DialInternet(dctx, "udp", srv)
		cancel()
		if err != nil {
			last = err
			continue
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = c.Write(q)
		buf := make([]byte, 4096)
		n := 0
		if err == nil {
			n, err = c.Read(buf)
		}
		c.Close()
		if err == nil {
			return buf[:n], nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no dns servers configured")
	}
	return nil, last
}
