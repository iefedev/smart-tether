// Command phone-relay runs the phone side from an adb shell, for testing the
// tunnel without the Android app. Use with `adb forward tcp:7778 tcp:7777`.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"strings"

	"smarttether/internal/relay"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7777", "comma-separated listen addresses")
	token := flag.String("token", "", "pre-shared link token")
	dns := flag.String("dns", "", "comma-separated carrier DNS servers (ip or ip:port)")
	flag.Parse()

	var servers []string
	for _, s := range strings.Split(*dns, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, "53")
		}
		servers = append(servers, s)
	}
	d := &net.Dialer{}
	srv := relay.NewServer(context.Background(), relay.Config{
		Token:        *token,
		DialInternet: d.DialContext,
		DNSServers:   servers,
		Logf:         log.Printf,
	})
	srv.SetListeners(strings.Split(*listen, ","))
	select {}
}
