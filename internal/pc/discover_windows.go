package pc

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net/netip"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// PhonePort is where the phone app listens (mobile.Port).
const PhonePort = 7777

// usbLocalPort is the PC end of `adb forward`.
const usbLocalPort = 7778

// Discovery finds the phone over USB (adb) and over the PC's Mobile Hotspot.
type Discovery struct {
	ADB    string       // non-empty enables USB; commands go to the adb server over its wire protocol (adbwire.go)
	APNet  netip.Prefix // the access point's subnet, 192.168.137.0/24
	APHost netip.Addr   // the PC's own address there

	run func(name string, args ...string) ([]byte, error) // nil means run; tests fake adb here

	lastUSB string       // last logged USB state, so the log shows changes only
	serial  atomic.Value // string: the authorized USB device seen last, "" if none
}

// USBSerial is the phone adb currently lists as authorized, or "".
func (d *Discovery) USBSerial() string {
	s, _ := d.serial.Load().(string)
	return s
}

// PushPairing sends the link token and hotspot passphrase to the phone over adb
// and starts the relay, for a phone that is plugged in but not answering: the
// app is not running, or it was never paired. The phone's UsbPairingActivity
// accepts these extras only from the adb shell, starts the relay and closes.
func (d *Discovery) PushPairing(serial, token, passphrase string) error {
	if d.run != nil {
		return nil // tests
	}
	cmd := pairingCommand(token, passphrase)
	out, err := adbShell(serial, cmd)
	if err != nil {
		return err
	}
	if bytes.Contains(out, []byte("Error")) {
		return fmt.Errorf("am start: %s", bytes.TrimSpace(out))
	}
	return nil
}

// pairingCommand is the adb shell command that pairs the phone app and starts its relay.
func pairingCommand(token, passphrase string) string {
	return fmt.Sprintf("am start -n dev.smarttether/.UsbPairingActivity --es token %s --es appass %s",
		shellQuote(token), shellQuote(passphrase))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (d *Discovery) noteUSB(state string) {
	if state != d.lastUSB {
		d.lastUSB = state
		log.Printf("usb: %s", state)
	}
}

func (d *Discovery) Candidates() []Candidate {
	var out []Candidate
	if serial := d.usbDevice(); serial != "" {
		if d.ensureForward(serial) {
			out = append(out, Candidate{Addr: fmt.Sprintf("127.0.0.1:%d", usbLocalPort), Transport: USB})
		}
	}
	for _, ip := range d.apNeighbors() {
		out = append(out, Candidate{Addr: netip.AddrPortFrom(ip, PhonePort).String(), Transport: WiFi})
	}
	return out
}

func (d *Discovery) usbDevice() string {
	if d.ADB == "" {
		return ""
	}
	out, err := d.exec(d.ADB, "devices")
	if err != nil {
		d.noteUSB(fmt.Sprintf("adb devices failed: %v %s", err, bytes.TrimSpace(out)))
		d.serial.Store("")
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == "device" {
			d.noteUSB("device " + f[0])
			d.serial.Store(f[0])
			return f[0]
		}
	}
	d.noteUSB("no authorized device: " + strings.Join(strings.Fields(string(out)), " "))
	d.serial.Store("")
	return ""
}

// ensureForward asks the adb server every time instead of remembering that it
// added the forward: a restarted adb server (kill-server, a crash, another
// tool) forgets its forwards while the phone stays listed.
func (d *Discovery) ensureForward(serial string) bool {
	local, remote := fmt.Sprintf("tcp:%d", usbLocalPort), fmt.Sprintf("tcp:%d", PhonePort)
	if out, err := d.exec(d.ADB, "forward", "--list"); err == nil && hasForward(out, serial, local, remote) {
		return true
	}
	out, err := d.exec(d.ADB, "-s", serial, "forward", local, remote)
	if err != nil {
		d.noteUSB(fmt.Sprintf("adb forward failed: %v %s", err, bytes.TrimSpace(out)))
	}
	return err == nil
}

// hasForward reports whether `adb forward --list` output ("serial local remote"
// per line) contains this forward.
func hasForward(list []byte, serial, local, remote string) bool {
	sc := bufio.NewScanner(bytes.NewReader(list))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 && f[0] == serial && f[1] == local && f[2] == remote {
			return true
		}
	}
	return false
}

func (d *Discovery) exec(name string, args ...string) ([]byte, error) {
	if d.run != nil {
		return d.run(name, args...)
	}
	if name == d.ADB {
		return adbWire(args...)
	}
	return run(name, args...)
}

// apNeighbors lists hosts the PC has seen on the access point's subnet.
func (d *Discovery) apNeighbors() []netip.Addr {
	if !d.APHost.IsValid() {
		return nil
	}
	out, err := d.exec("arp", "-a", "-N", d.APHost.String())
	if err != nil {
		return nil
	}
	var ips []netip.Addr
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// Hotspot clients can be listed as "static" as well as "dynamic".
		if len(f) != 3 || (f[2] != "dynamic" && f[2] != "static") {
			continue
		}
		ip, err := netip.ParseAddr(f[0])
		if err != nil || !d.APNet.Contains(ip) || ip == d.APHost || ip == broadcast(d.APNet) {
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	for i := 3; i >= 0 && host > 0; i-- {
		n := min(host, 8)
		b[i] |= byte(1<<n - 1)
		host -= n
	}
	return netip.AddrFrom4(b)
}

func run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	done := make(chan struct{})
	var out []byte
	var err error
	// Combined, so adb's stderr ("daemon not running", auth errors) reaches the log.
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return out, err
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		return nil, fmt.Errorf("%s timed out", name)
	}
}
