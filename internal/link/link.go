// Package link is the wire protocol shared by the PC and the phone.
//
// The phone listens (on loopback for `adb forward`, and on its address in the
// PC's Wi-Fi network); the PC dials it, so Windows needs no inbound firewall
// rule. The PC authenticates with a pre-shared token, then names its session
// (Resume) so a new connection continues the same Resumable byte stream, and
// yamux runs on that stream with one stream per proxied TCP flow or DNS
// client. Each stream starts with a Request; TCP streams get a one-byte Status
// back before data flows. UDP does not use streams: each datagram is a
// Resumable datagram frame (see Datagram), outside the reliable byte stream.
package link

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/hashicorp/yamux"
)

// magic opens every connection: resume handshake, then a Resumable stream
// with datagram frames. Other protocol versions are rejected; the PC and the
// phone must run the same release.
const magic = "PTL3"

// ErrBadMagic is returned by ServerHello for a PC speaking another protocol version.
var ErrBadMagic = errors.New("pc speaks another protocol version (update both sides)")

// SessionID names a Resumable stream across connections.
type SessionID [16]byte

// Resume statuses sent by the phone.
const (
	ResumeOK      byte = 1 // attached: a new session, or the named one continues
	ResumeUnknown byte = 2 // no such session (phone restarted, expired, or offsets mismatch)
)

// Kind is the type of flow a stream carries.
type Kind byte

const (
	KindTCP Kind = 1 // Addr is the destination host:port; Status follows, then raw bytes.
	KindDNS Kind = 2 // Addr unused; then length-prefixed queries and answers.
	// 3 is reserved and unused: UDP travels in datagram frames, not streams.
	// KindInfo asks the phone about itself; it answers with one JSON object
	// (PhoneInfo) and closes the stream.
	KindInfo Kind = 4
)

// PhoneInfo is the phone's answer to KindInfo.
type PhoneInfo struct {
	Name string `json:"name"`
	// LastExit is set when Android ended the app's previous process while it
	// was relaying (an OEM battery kill, low memory, a crash): the new process
	// has new sockets, so every UDP flow got a new public port.
	LastExit *PhoneExit `json:"lastExit,omitempty"`
}

// PhoneExit describes how the phone app's previous process ended.
type PhoneExit struct {
	At     int64  `json:"at"`     // unix millis
	Reason string `json:"reason"` // short, e.g. "ScreenOffCPUCheckKill" or "low memory"
	Detail string `json:"detail"` // Android's full description
}

// Status is the phone's answer to a KindTCP request.
type Status byte

const (
	StatusOK          Status = 0
	StatusRefused     Status = 1
	StatusUnreachable Status = 2
	StatusTimeout     Status = 3
	StatusFailed      Status = 4
)

// YamuxConfig is used by both ends on a Resumable stream. Windows are sized
// for ~300 Mbit/s on a link with a few ms RTT while keeping per-stream memory
// bounded. yamux's own liveness checks are off: the stream may legitimately
// stall for up to the resume window while the transport is replaced, and the
// Resumable detects dead transports itself.
func YamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.MaxStreamWindowSize = 2 << 20
	c.EnableKeepAlive = false
	c.ConnectionWriteTimeout = 45 * time.Second // > resume window
	c.StreamOpenTimeout = 0                     // a timeout here closes the whole session
	c.LogOutput = io.Discard
	return c
}

// ClientHello is sent by the PC right after dialing the phone; Resume must follow.
func ClientHello(c net.Conn, token string) error {
	if len(token) > 255 {
		return errors.New("token too long")
	}
	b := append([]byte(magic), byte(len(token)))
	b = append(b, token...)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	if _, err := c.Write(b); err != nil {
		return err
	}
	var ok [1]byte
	if _, err := io.ReadFull(c, ok[:]); err != nil {
		if errors.Is(err, io.EOF) {
			// A phone app speaking another protocol version closes here; so does
			// adb when no app listens.
			return fmt.Errorf("phone closed the hello (app not running, or a different app version): %w", err)
		}
		return err
	}
	if ok[0] != 1 {
		return errors.New("phone rejected token")
	}
	return nil
}

// ServerHello is run by the phone on an accepted connection.
func ServerHello(c net.Conn, token string) error {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	hdr := make([]byte, len(magic)+1)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if string(hdr[:len(magic)]) != magic {
		return fmt.Errorf("%w: got %q", ErrBadMagic, hdr[:len(magic)])
	}
	got := make([]byte, hdr[len(magic)])
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(got, []byte(token)) != 1 {
		c.Write([]byte{0})
		return errors.New("bad token")
	}
	_, err := c.Write([]byte{1})
	return err
}

// ClientResume names the PC's session and how many bytes the PC has
// received on it; the phone answers with a status and its own count. It may
// be repeated on the same connection after ResumeUnknown.
func ClientResume(c net.Conn, id SessionID, recvd uint64) (status byte, peerRecvd uint64, err error) {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	var b [24]byte
	copy(b[:16], id[:])
	binary.BigEndian.PutUint64(b[16:], recvd)
	if _, err := c.Write(b[:]); err != nil {
		return 0, 0, err
	}
	var a [9]byte
	if _, err := io.ReadFull(c, a[:]); err != nil {
		return 0, 0, err
	}
	return a[0], binary.BigEndian.Uint64(a[1:]), nil
}

// ServerReadResume reads the PC's ClientResume request.
func ServerReadResume(c net.Conn) (id SessionID, peerRecvd uint64, err error) {
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	var b [24]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return id, 0, err
	}
	copy(id[:], b[:16])
	return id, binary.BigEndian.Uint64(b[16:]), nil
}

// ServerWriteResume answers ServerReadResume.
func ServerWriteResume(c net.Conn, status byte, recvd uint64) error {
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer c.SetWriteDeadline(time.Time{})
	var a [9]byte
	a[0] = status
	binary.BigEndian.PutUint64(a[1:], recvd)
	_, err := c.Write(a[:])
	return err
}

// WriteRequest starts a stream.
func WriteRequest(w io.Writer, k Kind, addr string) error {
	if len(addr) > 255 {
		return fmt.Errorf("address too long: %q", addr)
	}
	b := []byte{byte(k), byte(len(addr))}
	_, err := w.Write(append(b, addr...))
	return err
}

// ReadRequest reads the header written by WriteRequest.
func ReadRequest(r io.Reader) (Kind, string, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, "", err
	}
	addr := make([]byte, h[1])
	if _, err := io.ReadFull(r, addr); err != nil {
		return 0, "", err
	}
	return Kind(h[0]), string(addr), nil
}

// Datagram is one UDP datagram between a PC socket and the internet. Port is
// the PC socket's source port (on the PC's tunnel address of Addr's family);
// Addr is the internet end: the destination on the way out, the sender on the
// way back. Each datagram names both ends, so neither side needs per-flow
// state from the other, and a restarted side carries on.
type Datagram struct {
	Port uint16
	Addr netip.AddrPort
	Data []byte
}

// MaxDatagram is the largest encoded datagram a frame may carry; larger ones
// are dropped by the sender.
const MaxDatagram = maxFrameData

// AppendDatagram encodes d after b: family (4 or 6), Port, Addr's IP and port, Data.
func AppendDatagram(b []byte, d Datagram) []byte {
	a := d.Addr.Addr().Unmap()
	fam := byte(6)
	if a.Is4() {
		fam = 4
	}
	b = append(b, fam)
	b = binary.BigEndian.AppendUint16(b, d.Port)
	b = append(b, a.AsSlice()...)
	b = binary.BigEndian.AppendUint16(b, d.Addr.Port())
	return append(b, d.Data...)
}

// ParseDatagram decodes AppendDatagram's output. Data aliases b.
func ParseDatagram(b []byte) (Datagram, error) {
	if len(b) < 1 {
		return Datagram{}, errors.New("empty datagram")
	}
	n := 16
	if b[0] == 4 {
		n = 4
	} else if b[0] != 6 {
		return Datagram{}, fmt.Errorf("datagram family %d", b[0])
	}
	if len(b) < 1+2+n+2 {
		return Datagram{}, errors.New("short datagram")
	}
	ip, _ := netip.AddrFromSlice(b[3 : 3+n])
	return Datagram{
		Port: binary.BigEndian.Uint16(b[1:]),
		Addr: netip.AddrPortFrom(ip, binary.BigEndian.Uint16(b[3+n:])),
		Data: b[5+n:],
	}, nil
}

// WriteMsg and ReadMsg frame DNS messages with a 2-byte length, as in DNS over TCP.
func WriteMsg(w io.Writer, m []byte) error {
	if len(m) > 0xffff {
		return errors.New("message too large")
	}
	b := make([]byte, 2+len(m))
	binary.BigEndian.PutUint16(b, uint16(len(m)))
	copy(b[2:], m)
	_, err := w.Write(b)
	return err
}

func ReadMsg(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	m := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err := io.ReadFull(r, m)
	return m, err
}

// Pipe copies both directions and propagates half-close: when one side
// reaches EOF, the other side's write half is closed, and Pipe returns once
// both directions are done.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		closeWrite(dst)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	// yamux.Stream.Close is a half-close (sends FIN, reads continue).
	if s, ok := c.(*yamux.Stream); ok {
		s.Close()
		return
	}
	c.Close()
}
