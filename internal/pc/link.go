// Package pc is the PC side: a TUN device feeding a userspace TCP/IP stack,
// whose flows are carried over the link to the phone.
package pc

import (
	"context"
	"crypto/rand"
	"errors"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"smarttether/internal/link"
)

var errNoLink = errors.New("phone not connected")

// Transport ranks the ways to reach the phone; lower is preferred.
type Transport int

const (
	USB  Transport = 0 // adb forward to the phone's loopback listener
	WiFi Transport = 1 // the phone's address on the PC's Mobile Hotspot
)

func (t Transport) String() string { return [...]string{"usb", "wifi"}[t] }

// Candidate is one address the phone may be listening on.
type Candidate struct {
	Addr      string
	Transport Transport
}

// Link dials the phone and keeps one yamux session alive across connections:
// the session runs on a link.Resumable, and each successful dial (a reconnect
// after a drop, or the switch between USB and Wi-Fi) resumes it, so flows in
// flight survive. Only when the phone no longer has the session (app
// restarted, or no connection for the resume window) does a new one start.
type Link struct {
	Token string
	// Candidates returns the addresses to try, best first.
	Candidates func() []Candidate
	OnChange   func(up bool, t Transport)
	// Dial connects to a candidate address; nil uses TCP.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// OnDatagram receives each datagram from the phone, on the transport's
	// reader: it must not block.
	OnDatagram func(b []byte)

	dg link.DatagramStats // across sessions

	mu      sync.Mutex
	sess    *yamux.Session
	rc      *link.Resumable
	id      link.SessionID
	started time.Time
	up      bool // a connection currently carries the session
	cur     Transport
	curAddr string
	lostAt  time.Time
	kick    chan struct{}

	reselect bool      // dial the first candidate even if it is not better (preference changed)
	upSince  time.Time // when up last changed
}

// setUpLocked records whether a connection carries the session; l.mu must be held.
func (l *Link) setUpLocked(up bool) {
	if l.up != up {
		l.up = up
		l.upSince = time.Now()
	}
}

// NotUpFor is how long no connection has carried a session (0 while one does).
func (l *Link) NotUpFor() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.up {
		return 0
	}
	if l.upSince.IsZero() {
		return time.Since(l.lostAt)
	}
	return time.Since(l.upSince)
}

// LinkState is a snapshot for the tray app.
type LinkState struct {
	Session    bool // a session exists (it may be between connections)
	Up         bool // a connection carries it right now
	Transport  Transport
	Addr       string
	SessionAge time.Duration
}

func (l *Link) State() LinkState {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := LinkState{Session: l.sess != nil, Up: l.up, Transport: l.cur, Addr: l.curAddr}
	if l.sess != nil {
		st.SessionAge = time.Since(l.started)
	}
	return st
}

// Reselect makes the next step move to the best allowed candidate even if it
// ranks below the current transport (the user changed the preference).
func (l *Link) Reselect() {
	l.mu.Lock()
	l.reselect = true
	l.mu.Unlock()
	l.wake()
}

// Drop ends the session (the user paused the tunnel). Flows in flight break.
func (l *Link) Drop() {
	l.mu.Lock()
	rc := l.rc
	l.mu.Unlock()
	if rc != nil {
		l.endSession(rc)
	}
}

func (l *Link) wake() {
	select {
	case l.kicked() <- struct{}{}:
	default:
	}
}

// Run tries to connect while down, and upgrades Wi-Fi to USB when the cable appears.
func (l *Link) Run(ctx context.Context) {
	l.mu.Lock()
	l.lostAt = time.Now()
	l.mu.Unlock()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		l.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.kicked():
		}
	}
}

func (l *Link) kicked() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.kick == nil {
		l.kick = make(chan struct{}, 1)
	}
	return l.kick
}

func (l *Link) step(ctx context.Context) {
	l.mu.Lock()
	up, cur, curAddr, re := l.up, l.cur, l.curAddr, l.reselect
	l.mu.Unlock()
	settled := false // reselect is done: we moved, or already use the best allowed candidate
	for _, c := range l.Candidates() {
		if up && !re && c.Transport >= cur {
			return // already on this transport or a better one
		}
		if up && re && c.Transport == cur && c.Addr == curAddr {
			settled = true
			break
		}
		if err := l.dial(ctx, c); err != nil {
			continue
		}
		settled = true
		break
	}
	if re && settled {
		l.mu.Lock()
		l.reselect = false
		l.mu.Unlock()
	}
}

func (l *Link) dial(ctx context.Context, cand Candidate) error {
	var c net.Conn
	var err error
	if l.Dial != nil {
		c, err = l.Dial(ctx, cand.Addr)
	} else {
		d := net.Dialer{Timeout: 2 * time.Second}
		c, err = d.DialContext(ctx, "tcp", cand.Addr)
	}
	if err != nil {
		return err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetReadBuffer(4 << 20)
		// The send buffer stays at Windows' auto-tuned size: bytes queued in
		// the kernel can't be overtaken by datagrams.
	}
	if err := link.ClientHello(c, l.Token); err != nil {
		log.Printf("link: hello via %s %s: %v", cand.Transport, cand.Addr, err)
		c.Close()
		return err
	}
	resumed, err := l.resume(c)
	if err != nil {
		log.Printf("link: resume via %s %s: %v", cand.Transport, cand.Addr, err)
		c.Close()
		return err
	}
	l.mu.Lock()
	prev, wasUp := l.cur, l.up
	l.setUpLocked(true)
	l.cur, l.curAddr = cand.Transport, cand.Addr
	age := time.Since(l.started).Round(time.Second)
	l.mu.Unlock()
	switch {
	case !resumed:
		log.Printf("link: up via %s %s (new session)", cand.Transport, cand.Addr)
	case wasUp:
		log.Printf("link: moved from %s to %s %s (session age %s)", prev, cand.Transport, cand.Addr, age)
	default:
		log.Printf("link: resumed via %s %s (session age %s)", cand.Transport, cand.Addr, age)
	}
	if l.OnChange != nil {
		l.OnChange(true, cand.Transport)
	}
	return nil
}

// resume continues the current session on c, or starts a new one if there
// is none or the phone does not know it.
func (l *Link) resume(c net.Conn) (resumed bool, err error) {
	l.mu.Lock()
	rc, id := l.rc, l.id
	l.mu.Unlock()
	if rc != nil {
		var status byte
		err := rc.Resume(c, func(recvd uint64) (uint64, error) {
			var peer uint64
			var err error
			status, peer, err = link.ClientResume(c, id, recvd)
			if err == nil && status != link.ResumeOK {
				err = errUnknownSession
			}
			return peer, err
		})
		if err == nil {
			return true, nil
		}
		if errors.Is(err, link.ErrResumeOffset) {
			// The phone already attached; start over on a fresh connection.
			log.Printf("link: session %x: %v; flows in flight break", id[:4], err)
			l.endSession(rc)
			return false, err
		}
		if status != link.ResumeUnknown {
			return false, err
		}
		log.Printf("link: phone lost session %x; flows in flight break", id[:4])
		l.endSession(rc)
	}
	rc, id = l.newSession()
	err = rc.Resume(c, func(uint64) (uint64, error) {
		status, peer, err := link.ClientResume(c, id, 0)
		if err == nil && status != link.ResumeOK {
			err = errUnknownSession
		}
		return peer, err
	})
	return false, err
}

var errUnknownSession = errors.New("phone does not know the session")

func (l *Link) newSession() (*link.Resumable, link.SessionID) {
	var id link.SessionID
	rand.Read(id[:])
	rc := link.NewResumable(link.ResumableConfig{OnDown: l.transportDown, OnDatagram: l.OnDatagram, Datagrams: &l.dg})
	sess, err := yamux.Client(rc, link.YamuxConfig())
	if err != nil {
		panic(err) // only fails on an invalid config
	}
	l.mu.Lock()
	l.sess, l.rc, l.id, l.started = sess, rc, id, time.Now()
	l.mu.Unlock()
	go l.watch(sess, rc, id)
	return rc, id
}

func (l *Link) endSession(rc *link.Resumable) {
	l.mu.Lock()
	sess := l.sess
	current := l.rc == rc
	if current {
		l.sess, l.rc = nil, nil
		l.setUpLocked(false)
		l.lostAt = time.Now()
	}
	l.mu.Unlock()
	if current {
		sess.Close()
	}
	rc.Close()
}

// transportDown is called when the connection under the session fails; the
// session waits for a new one.
func (l *Link) transportDown(err error) {
	l.mu.Lock()
	wasUp, cur, addr := l.up, l.cur, l.curAddr
	l.setUpLocked(false)
	if l.kick == nil {
		l.kick = make(chan struct{}, 1)
	}
	kick := l.kick
	l.mu.Unlock()
	if wasUp {
		log.Printf("link: lost %s %s: %v", cur, addr, err)
		if l.OnChange != nil {
			l.OnChange(false, cur)
		}
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

func (l *Link) watch(sess *yamux.Session, rc *link.Resumable, id link.SessionID) {
	<-sess.CloseChan()
	l.mu.Lock()
	current := l.sess == sess
	age := time.Since(l.started).Round(time.Second)
	if current {
		l.sess, l.rc = nil, nil
		l.setUpLocked(false)
		l.lostAt = time.Now()
	}
	l.mu.Unlock()
	rc.Close()
	if current {
		why := rc.Err()
		log.Printf("link: session %x ended after %s: %v", id[:4], age, why)
	}
}

// Open starts a new stream to the phone. While the session is between
// connections the stream is queued and starts once it resumes.
func (l *Link) Open() (*yamux.Stream, error) {
	l.mu.Lock()
	s := l.sess
	l.mu.Unlock()
	if s == nil {
		return nil, errNoLink
	}
	return s.OpenStream()
}

// WriteDatagram sends one encoded link.Datagram to the phone. It never
// blocks; while no connection carries the session the datagram is dropped.
func (l *Link) WriteDatagram(b []byte) bool {
	l.mu.Lock()
	rc := l.rc
	l.mu.Unlock()
	if rc == nil {
		l.dg.DropNoLink.Add(1)
		return false
	}
	return rc.WriteDatagram(b)
}

// Pace describes the current session's in-flight cap ("" without a session).
func (l *Link) Pace() string {
	l.mu.Lock()
	rc := l.rc
	l.mu.Unlock()
	if rc == nil {
		return ""
	}
	return rc.Pace()
}

// DatagramStats counts datagrams over all sessions.
func (l *Link) DatagramStats() *link.DatagramStats { return &l.dg }

// DownFor reports how long there has been no session (0 while one exists,
// even between connections).
func (l *Link) DownFor() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess != nil {
		return 0
	}
	return time.Since(l.lostAt)
}
