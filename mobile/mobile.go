// Package mobile is the gomobile binding the Android app calls.
// Data never crosses JNI: Go owns every socket; Kotlin only binds internet
// sockets to the cellular Network and reports address changes.
package mobile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"smarttether/internal/link"
	"smarttether/internal/relay"
)

// Port is where the phone listens for the PC: on loopback for `adb forward`,
// and on its address in the PC's Wi-Fi network.
const Port = 7777

// Binder is implemented in Kotlin.
type Binder interface {
	// BindCellular binds the socket to the cellular network.
	BindCellular(fd int) bool
	Log(msg string)
}

var (
	mu         sync.Mutex
	cancel     context.CancelFunc
	srv        *relay.Server
	wifiIP     string
	deviceName string
	lastExit   *link.PhoneExit
)

// Start runs the relay until Stop. dns is a comma-separated list of carrier
// resolver IPs.
func Start(token, dns string, b Binder) error {
	mu.Lock()
	defer mu.Unlock()
	if cancel != nil {
		return errors.New("already running")
	}
	logf := func(format string, args ...any) { b.Log(fmt.Sprintf(format, args...)) }
	cellular := &net.Dialer{Control: bindControl(b.BindCellular), KeepAlive: 30 * time.Second}
	cellularUDP := &net.ListenConfig{Control: bindControl(b.BindCellular)}
	ctx, c := context.WithCancel(context.Background())
	cancel = c
	srv = relay.NewServer(ctx, relay.Config{
		Token:        token,
		DialInternet: cellular.DialContext,
		ListenUDP: func(ctx context.Context, network string) (*net.UDPConn, error) {
			pc, err := cellularUDP.ListenPacket(ctx, network, ":0")
			if err != nil {
				return nil, err
			}
			return pc.(*net.UDPConn), nil
		},
		DNSServers: dnsList(dns),
		Logf:       logf,
	})
	srv.SetDeviceName(deviceName)
	srv.SetLastExit(lastExit)
	srv.SetListeners(listenAddrs())
	return nil
}

// SetWifiAddress is called when the Wi-Fi IPv4 address changes ("" = no Wi-Fi).
// The phone listens only on loopback and Wi-Fi, never on the cellular interface.
func SetWifiAddress(ip string) {
	mu.Lock()
	defer mu.Unlock()
	wifiIP = ip
	if srv != nil {
		srv.SetListeners(listenAddrs())
	}
}

// SetDeviceName sets the name the PC app shows for this phone.
func SetDeviceName(name string) {
	mu.Lock()
	defer mu.Unlock()
	deviceName = name
	if srv != nil {
		srv.SetDeviceName(name)
	}
}

// SetLastExit tells the PC how the app's previous process ended, when Android
// ended it while it was relaying. Called once at startup; atMillis 0 = it
// ended normally.
func SetLastExit(atMillis int64, reason, detail string) {
	mu.Lock()
	defer mu.Unlock()
	lastExit = nil
	if atMillis != 0 {
		lastExit = &link.PhoneExit{At: atMillis, Reason: reason, Detail: detail}
	}
	if srv != nil {
		srv.SetLastExit(lastExit)
	}
}

// Stats returns relay.Stats as JSON; "{}" when the relay is not running.
func Stats() string {
	mu.Lock()
	defer mu.Unlock()
	if srv == nil {
		return "{}"
	}
	return srv.StatsJSON()
}

// SetDNS replaces the carrier resolver list.
func SetDNS(dns string) {
	mu.Lock()
	defer mu.Unlock()
	if srv != nil {
		srv.SetDNSServers(dnsList(dns))
	}
}

// CellularChanged is called when the cellular network appears, is replaced or
// is lost, so UDP sockets bound to the old one are reopened.
func CellularChanged() {
	mu.Lock()
	defer mu.Unlock()
	if srv != nil {
		srv.CellularChanged()
	}
}

// Stop ends the relay.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if cancel != nil {
		cancel()
	}
	cancel, srv = nil, nil
}

func listenAddrs() []string {
	addrs := []string{fmt.Sprintf("127.0.0.1:%d", Port)}
	if wifiIP != "" {
		addrs = append(addrs, net.JoinHostPort(wifiIP, fmt.Sprint(Port)))
	}
	return addrs
}

func dnsList(dns string) []string {
	var out []string
	for _, s := range strings.Split(dns, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, net.JoinHostPort(s, "53"))
		}
	}
	return out
}

func bindControl(bind func(int) bool) func(network, address string, rc syscall.RawConn) error {
	return func(network, address string, rc syscall.RawConn) error {
		ok := false
		if err := rc.Control(func(fd uintptr) { ok = bind(int(fd)) }); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("bind %s socket for %s: cellular unavailable", network, address)
		}
		return nil
	}
}
