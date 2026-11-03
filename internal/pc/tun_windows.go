package pc

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/xjasonlyu/tun2socks/v2/core/device/iobased"
)

// A fixed GUID keeps Windows from creating a new network profile every run.
var adapterGUID = windows.GUID{Data1: 0x7074_6574, Data2: 0x6865, Data3: 0x7220, Data4: [8]byte{'p', 'h', 'o', 'n', 'e', 't', 'u', 'n'}}

// TUN wraps a wintun adapter as a gVisor link endpoint.
type TUN struct {
	*iobased.Endpoint
	nt   tun.Device
	Name string

	rMu, wMu sync.Mutex
	rSizes   []int
	rBuffs   [][]byte
	wBuffs   [][]byte

	// Up counts IP bytes Windows sent into the tunnel, Down the bytes delivered back.
	Up, Down atomic.Uint64
}

func OpenTUN(name string, mtu int) (*TUN, error) {
	nt, err := tun.CreateTUNWithRequestedGUID(name, &adapterGUID, mtu)
	if err != nil {
		return nil, fmt.Errorf("create wintun adapter (needs admin and wintun.dll next to the exe): %w", err)
	}
	t := &TUN{nt: nt, rSizes: make([]int, 1), rBuffs: make([][]byte, 1), wBuffs: make([][]byte, 1)}
	t.Name, _ = nt.Name()
	ep, err := iobased.New(t, uint32(mtu), 0)
	if err != nil {
		nt.Close()
		return nil, err
	}
	t.Endpoint = ep
	return t, nil
}

func (t *TUN) Read(p []byte) (int, error) {
	t.rMu.Lock()
	defer t.rMu.Unlock()
	t.rBuffs[0] = p
	_, err := t.nt.Read(t.rBuffs, t.rSizes, 0)
	t.Up.Add(uint64(t.rSizes[0]))
	return t.rSizes[0], err
}

func (t *TUN) Write(p []byte) (int, error) {
	t.wMu.Lock()
	defer t.wMu.Unlock()
	t.wBuffs[0] = p
	n, err := t.nt.Write(t.wBuffs, 0)
	if err == nil {
		t.Down.Add(uint64(len(p)))
	}
	return n, err
}

func (t *TUN) Close() {
	t.Endpoint.Close()
	t.nt.Close()
}

// Configure assigns the adapter's addresses and DNS server. Routes are added
// separately so they can be withdrawn while the link is down.
func (t *TUN) Configure() error {
	q := `"` + t.Name + `"`
	cmds := [][]string{
		{"interface", "ipv4", "set", "address", "name=" + q, "source=static", "address=198.18.0.1", "mask=255.255.0.0"},
		{"interface", "ipv4", "set", "interface", q, "metric=1"},
		{"interface", "ipv4", "set", "dnsservers", "name=" + q, "source=static", "address=" + DNSAddr.String(), "register=none", "validate=no"},
		{"interface", "ipv6", "add", "address", q, "fd70:7465:7468::1/64", "store=active"},
		{"interface", "ipv6", "set", "interface", q, "metric=1"},
	}
	for _, c := range cmds {
		if err := netsh(c...); err != nil {
			return err
		}
	}
	return nil
}

// AddRoutes / RemoveRoutes install on-link routes through the adapter.
func (t *TUN) AddRoutes(prefixes []string) error {
	for _, p := range prefixes {
		fam := "ipv4"
		if strings.Contains(p, ":") {
			fam = "ipv6"
		}
		if err := netsh("interface", fam, "add", "route", "prefix="+p, `interface="`+t.Name+`"`, "metric=1", "store=active"); err != nil {
			return err
		}
	}
	return nil
}

func (t *TUN) RemoveRoutes(prefixes []string) {
	for _, p := range prefixes {
		fam := "ipv4"
		if strings.Contains(p, ":") {
			fam = "ipv6"
		}
		netsh("interface", fam, "delete", "route", "prefix="+p, `interface="`+t.Name+`"`, "store=active")
	}
}

func netsh(args ...string) error {
	// netsh needs the quoted interface name passed verbatim.
	cmd := exec.Command("netsh")
	cmd.SysProcAttr = &windows.SysProcAttr{CmdLine: "netsh " + strings.Join(args, " "), HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("netsh %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		return fmt.Errorf("netsh %s: %w", args[2], err)
	}
	return nil
}
