package pc

import (
	"fmt"
	"strings"
	"testing"
)

// fakeADB is an adb server with one phone attached that keeps its forwards
// until it is restarted.
type fakeADB struct {
	serial   string
	forwards map[string]string // local -> remote
	added    int
}

func (a *fakeADB) run(name string, args ...string) ([]byte, error) {
	switch cmd := strings.Join(args, " "); {
	case cmd == "devices":
		return []byte("List of devices attached\n" + a.serial + "\tdevice\n\n"), nil
	case cmd == "forward --list":
		var b strings.Builder
		for l, r := range a.forwards {
			fmt.Fprintf(&b, "%s %s %s\n", a.serial, l, r)
		}
		return []byte(b.String()), nil
	case len(args) == 5 && args[0] == "-s" && args[1] == a.serial && args[2] == "forward":
		a.forwards[args[3]] = args[4]
		a.added++
		return nil, nil
	}
	return nil, fmt.Errorf("fake adb: unexpected %s %q", name, args)
}

func usbCandidate(cs []Candidate) bool {
	for _, c := range cs {
		if c.Transport == USB && c.Addr == fmt.Sprintf("127.0.0.1:%d", usbLocalPort) {
			return true
		}
	}
	return false
}

// TestDiscoveryReaddsForwardAfterADBRestart: `adb kill-server` drops every
// forward while the phone stays listed; the next tick must add it back
// instead of trusting that it was added before.
func TestDiscoveryReaddsForwardAfterADBRestart(t *testing.T) {
	a := &fakeADB{serial: "emulator-5554", forwards: map[string]string{}}
	d := &Discovery{ADB: "adb", run: a.run}

	if !usbCandidate(d.Candidates()) || a.added != 1 {
		t.Fatalf("first tick: forward added %d times, want 1", a.added)
	}
	if !usbCandidate(d.Candidates()) || a.added != 1 {
		t.Fatalf("second tick re-added a live forward (%d adds)", a.added)
	}

	a.forwards = map[string]string{} // adb server restarted

	if !usbCandidate(d.Candidates()) || a.added != 2 {
		t.Fatalf("after adb restart: forward added %d times, want 2", a.added)
	}
	if got := a.forwards[fmt.Sprintf("tcp:%d", usbLocalPort)]; got != fmt.Sprintf("tcp:%d", PhonePort) {
		t.Fatalf("forward after restart = %q", got)
	}
}

// TestPairingCommand: USB pairing goes to the shell-only pairing activity,
// never to the launcher, and the values are quoted for the device shell.
func TestPairingCommand(t *testing.T) {
	got := pairingCommand("tok'en", "pass word")
	want := `am start -n dev.smarttether/.UsbPairingActivity --es token 'tok'\''en' --es appass 'pass word'`
	if got != want {
		t.Fatalf("pairingCommand = %s\nwant %s", got, want)
	}
}

func TestHasForward(t *testing.T) {
	list := []byte("other tcp:7778 tcp:7777\nemulator-5554 tcp:9000 tcp:7777\r\nemulator-5554 tcp:7778 tcp:7777\r\n")
	if !hasForward(list, "emulator-5554", "tcp:7778", "tcp:7777") {
		t.Fatal("missed the forward")
	}
	if hasForward(list, "emulator-5554", "tcp:7778", "tcp:7000") || hasForward(nil, "x", "tcp:7778", "tcp:7777") {
		t.Fatal("matched a forward that is not there")
	}
}
