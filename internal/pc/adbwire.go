package pc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// adbServer is the adb server's host port. The service talks the server's wire
// protocol instead of running adb.exe: an adb server spawned by the
// LocalSystem service runs in session 0, where it fails to start ("failed to
// create fdevent interrupt socketpair") yet holds the port, refusing every
// client. The tray app keeps the server running in the user's session
// instead; without it there is simply no USB candidate.
const adbServer = "127.0.0.1:5037"

var errNoADBServer = errors.New("adb server not running (it starts at user logon)")

// adbWire answers the few adb commands Discovery uses, with output shaped like adb.exe's.
func adbWire(args ...string) ([]byte, error) {
	switch {
	case len(args) == 1 && args[0] == "devices":
		out, err := adbQuery("host:devices", true)
		return append([]byte("List of devices attached\n"), out...), err
	case len(args) == 2 && args[0] == "forward" && args[1] == "--list":
		return adbQuery("host:list-forward", true)
	case len(args) == 5 && args[0] == "-s" && args[2] == "forward":
		// The server answers a forward with two OKAYs: request accepted, forward set up.
		_, err := adbQuery(fmt.Sprintf("host-serial:%s:forward:%s;%s", args[1], args[3], args[4]), false, true)
		return nil, err
	}
	return nil, fmt.Errorf("adbwire: unsupported %q", args)
}

// adbShell runs a shell command on the device and returns its output.
func adbShell(serial, cmd string) ([]byte, error) {
	c, err := net.DialTimeout("tcp", adbServer, 2*time.Second)
	if err != nil {
		return nil, errNoADBServer
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	for _, svc := range []string{"host:transport:" + serial, "shell:" + cmd} {
		if _, err := fmt.Fprintf(c, "%04x%s", len(svc), svc); err != nil {
			return nil, err
		}
		if err := adbStatus(c); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(c)
}

// adbQuery sends one host service request. withPayload reads a length-prefixed
// reply; secondStatus reads a further OKAY/FAIL after the first.
func adbQuery(service string, withPayload bool, secondStatus ...bool) ([]byte, error) {
	c, err := net.DialTimeout("tcp", adbServer, 2*time.Second)
	if err != nil {
		return nil, errNoADBServer
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, "%04x%s", len(service), service); err != nil {
		return nil, err
	}
	if err := adbStatus(c); err != nil {
		return nil, err
	}
	if len(secondStatus) > 0 && secondStatus[0] {
		if err := adbStatus(c); err != nil {
			return nil, err
		}
	}
	if !withPayload {
		return nil, nil
	}
	return adbBlock(c)
}

func adbStatus(r io.Reader) error {
	var st [4]byte
	if _, err := io.ReadFull(r, st[:]); err != nil {
		return fmt.Errorf("adb server: %w", err)
	}
	switch string(st[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, _ := adbBlock(r)
		return fmt.Errorf("adb: %s", bytes.TrimSpace(msg))
	}
	return fmt.Errorf("adb server: bad status %q", st[:])
}

func adbBlock(r io.Reader) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	if _, err := hex.DecodeString(string(l[:])); err != nil {
		return nil, fmt.Errorf("adb server: bad length %q", l[:])
	}
	n, _ := strconv.ParseUint(string(l[:]), 16, 16)
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
