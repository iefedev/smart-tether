package pc

import (
	"context"
	"net"
	"testing"

	"smarttether/internal/link"
	"smarttether/internal/relay"
)

// TestPhoneInfoReportsLastExit: the phone's name and how its previous process
// ended reach the PC through the real link.
func TestPhoneInfoReportsLastExit(t *testing.T) {
	p := &fakePhone{usb: freeAddr(t), wifi: freeAddr(t), usbIn: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := relay.NewServer(ctx, relay.Config{Token: "tok", DialInternet: (&net.Dialer{}).DialContext, Logf: t.Logf})
	srv.SetListeners([]string{p.usb, p.wifi})
	srv.SetDeviceName("Test phone")

	l := &Link{Token: "tok", Candidates: p.candidates, Dial: p.dial}
	go l.Run(ctx)
	waitFor(t, "link up on usb", func() bool { up, _, _ := l.state(); return up })

	info, err := PhoneInfo(l)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "Test phone" || info.LastExit != nil {
		t.Fatalf("got %+v, want the name and no exit", info)
	}

	// Synthetic values: 2023-11-14 22:13:20 UTC and a made-up reason.
	want := link.PhoneExit{At: 1700000000000, Reason: "ExampleKill", Detail: "ExampleKill synthetic test detail"}
	srv.SetLastExit(&want)
	info, err = PhoneInfo(l)
	if err != nil {
		t.Fatal(err)
	}
	if info.LastExit == nil || *info.LastExit != want {
		t.Fatalf("last exit %+v, want %+v", info.LastExit, want)
	}
}
