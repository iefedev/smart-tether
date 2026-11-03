//go:build !linux

package relay

import "net"

// setNotSentLowat is Linux-only (Android); elsewhere the kernel default stays.
func setNotSentLowat(*net.TCPConn, int) {}
