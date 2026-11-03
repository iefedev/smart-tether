package relay

import (
	"encoding/json"
	"net"
	"sync/atomic"
	"time"

	"smarttether/internal/link"
)

// Stats is what the phone app shows.
type Stats struct {
	Connected      bool   `json:"connected"`      // a PC connection carries the session now
	Via            string `json:"via,omitempty"`  // "usb" (adb forward to loopback) or "wifi"
	SessionSeconds int64  `json:"sessionSeconds"` // 0 without a session
	Down           uint64 `json:"down"`           // bytes from the internet to the PC, since start
	Up             uint64 `json:"up"`             // bytes from the PC to the internet, since start
	SessionBytes   uint64 `json:"sessionBytes"`   // down+up since the current session began
	UDPSockets     int    `json:"udpSockets"`     // cellular UDP sockets open for PC sockets
	UDPDropped     uint64 `json:"udpDropped"`     // datagrams dropped on the phone, since start
	UDPSendErrors  uint64 `json:"udpSendErrors"`  // datagrams the cellular network refused, since start
}

type counters struct {
	down, up    atomic.Uint64
	sessionBase atomic.Uint64 // down+up when the current session began
	connected   atomic.Bool
	via         atomic.Value // string
	sessionFrom atomic.Int64 // unix nanos; 0 = no session
	deviceName  atomic.Value // string
	lastExit    atomic.Pointer[link.PhoneExit]
}

// SetDeviceName sets the name the PC shows for this phone.
func (s *Server) SetDeviceName(name string) { s.c.deviceName.Store(name) }

func (s *Server) DeviceName() string {
	n, _ := s.c.deviceName.Load().(string)
	return n
}

// SetLastExit records how the app's previous process ended; nil when it ended
// normally. The PC reads it with KindInfo.
func (s *Server) SetLastExit(e *link.PhoneExit) { s.c.lastExit.Store(e) }

func (s *Server) info() link.PhoneInfo {
	return link.PhoneInfo{Name: s.DeviceName(), LastExit: s.c.lastExit.Load()}
}

// StatsJSON is the current Stats as JSON (for the gomobile binding).
func (s *Server) StatsJSON() string {
	n := s.udp
	st := Stats{Connected: s.c.connected.Load(), Down: s.c.down.Load(), Up: s.c.up.Load(),
		UDPSockets: n.sockets(),
		UDPDropped: n.dropNoSock.Load() + n.dropQueue.Load() + n.dropFilter.Load() + n.dropNoPC.Load() +
			s.dg.DropNoLink.Load() + s.dg.DropQueue.Load() + s.dg.DropAge.Load() + s.dg.DropBig.Load(),
		UDPSendErrors: n.errRefused.Load() + n.errMsgSize.Load() + n.errUnreach.Load() + n.errOther.Load(),
	}
	st.Via, _ = s.c.via.Load().(string)
	if from := s.c.sessionFrom.Load(); from != 0 {
		st.SessionSeconds = int64(time.Since(time.Unix(0, from)) / time.Second)
		st.SessionBytes = st.Down + st.Up - s.c.sessionBase.Load()
	}
	b, _ := json.Marshal(st)
	return string(b)
}

// attached records that conn now carries the session.
func (s *Server) attached(conn net.Conn, newSession bool) {
	via := "wifi"
	if a, ok := conn.LocalAddr().(*net.TCPAddr); ok && a.IP.IsLoopback() {
		via = "usb"
	}
	s.c.via.Store(via)
	s.c.connected.Store(true)
	if newSession || s.c.sessionFrom.Load() == 0 {
		s.c.sessionBase.Store(s.c.down.Load() + s.c.up.Load())
		s.c.sessionFrom.Store(time.Now().UnixNano())
	}
}

func (s *Server) transportDown() { s.c.connected.Store(false) }

func (s *Server) sessionEnded() {
	s.c.connected.Store(false)
	s.c.sessionFrom.Store(0)
}

// counted wraps an internet-side connection so its bytes show up in Stats.
func (s *Server) counted(c net.Conn) net.Conn { return &countConn{Conn: c, c: &s.c} }

type countConn struct {
	net.Conn
	c *counters
}

func (cc *countConn) Read(p []byte) (int, error) {
	n, err := cc.Conn.Read(p)
	cc.c.down.Add(uint64(n))
	return n, err
}

func (cc *countConn) Write(p []byte) (int, error) {
	n, err := cc.Conn.Write(p)
	cc.c.up.Add(uint64(n))
	return n, err
}

// CloseWrite keeps half-close working through the wrapper (link.Pipe uses it).
func (cc *countConn) CloseWrite() error {
	if cw, ok := cc.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return cc.Conn.Close()
}
