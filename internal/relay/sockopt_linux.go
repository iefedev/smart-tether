package relay

import (
	"net"
	"syscall"
)

// tcpNotSentLowat is TCP_NOTSENT_LOWAT (linux/tcp.h); the syscall package has no name for it.
const tcpNotSentLowat = 25

// setNotSentLowat keeps at most n not-yet-sent bytes queued in the kernel for
// c, so the writer can put urgent data ahead of the rest.
func setNotSentLowat(c *net.TCPConn, n int) {
	rc, err := c.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) {
		syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpNotSentLowat, n)
	})
}
