// Command linktest measures raw TCP throughput between the phone and the PC
// over a candidate link (Wi-Fi, adb forward); server and client touch no
// internet. fetch and stun measure the internet path instead.
//
//	linktest server -l :5201
//	linktest client -c 192.168.137.1:5201 -dir down -streams 4 -t 10s
//	linktest fetch -url <large file>
//	linktest stun -t 30s -i 20ms
//
// "down" means server→client (phone→PC when the phone is the server).
// stun sends STUN binding requests from one UDP socket to several servers and
// reports round-trip times, loss and the public address each server saw.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const bufSize = 256 << 10

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: linktest server|client [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		fs := flag.NewFlagSet("server", flag.ExitOnError)
		addr := fs.String("l", ":5201", "listen address")
		fs.Parse(os.Args[2:])
		serve(*addr)
	case "client":
		fs := flag.NewFlagSet("client", flag.ExitOnError)
		addr := fs.String("c", "", "server address host:port")
		dir := fs.String("dir", "down", "down (server sends) or up (client sends)")
		streams := fs.Int("streams", 1, "parallel TCP connections")
		dur := fs.Duration("t", 10*time.Second, "test duration")
		fs.Parse(os.Args[2:])
		client(*addr, *dir, *streams, *dur)
	case "fetch":
		// Measures the phone's own internet: run on the phone, egress is the shell
		// uid's normal sockets. -ip skips DNS (Android has no /etc/resolv.conf).
		fs := flag.NewFlagSet("fetch", flag.ExitOnError)
		url := fs.String("url", "https://speed.cloudflare.com/__down?bytes=50000000", "URL to download")
		ip := fs.String("ip", "", "dial this IP instead of resolving the host")
		streams := fs.Int("streams", 1, "parallel downloads")
		fs.Parse(os.Args[2:])
		fetch(*url, *ip, *streams)
	case "stun":
		fs := flag.NewFlagSet("stun", flag.ExitOnError)
		servers := fs.String("s", "stun.l.google.com:19302,stun.cloudflare.com:3478", "comma-separated STUN servers")
		every := fs.Duration("i", 20*time.Millisecond, "interval between requests (round-robin over servers)")
		dur := fs.Duration("t", 30*time.Second, "test duration")
		fs.Parse(os.Args[2:])
		stun(strings.Split(*servers, ","), *every, *dur)
	default:
		log.Fatalf("unknown mode %q", os.Args[1])
	}
}

// Protocol: client sends 1 byte mode ('D' or 'U') + 8 byte duration in ms.
// 'D': server writes for the duration, then closes. 'U': client writes; server
// discards and replies with the byte count it received as 8 bytes.
func serve(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go handle(c)
	}
}

func handle(c net.Conn) {
	defer c.Close()
	var hdr [9]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return
	}
	dur := time.Duration(binary.BigEndian.Uint64(hdr[1:])) * time.Millisecond
	switch hdr[0] {
	case 'D':
		buf := make([]byte, bufSize)
		deadline := time.Now().Add(dur)
		var n int64
		for time.Now().Before(deadline) {
			w, err := c.Write(buf)
			n += int64(w)
			if err != nil {
				break
			}
		}
		log.Printf("%s down: sent %d bytes", c.RemoteAddr(), n)
	case 'U':
		// adb forward does not carry half-close, so stop on a deadline as well as EOF.
		c.SetReadDeadline(time.Now().Add(dur + 500*time.Millisecond))
		n, _ := io.Copy(io.Discard, c)
		c.SetReadDeadline(time.Time{})
		var out [8]byte
		binary.BigEndian.PutUint64(out[:], uint64(n))
		c.Write(out[:])
		log.Printf("%s up: received %d bytes", c.RemoteAddr(), n)
	}
}

func fetch(url, ip string, streams int) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if ip != "" {
				_, port, _ := net.SplitHostPort(addr)
				addr = net.JoinHostPort(ip, port)
			}
			return d.DialContext(ctx, network, addr)
		},
		DisableCompression: true,
	}
	hc := &http.Client{Transport: tr}
	var total atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := hc.Get(url)
			if err != nil {
				log.Print(err)
				return
			}
			defer resp.Body.Close()
			n, _ := io.Copy(io.Discard, resp.Body)
			total.Add(n)
		}()
	}
	wg.Wait()
	el := time.Since(start).Seconds()
	fmt.Printf("fetch streams=%d bytes=%d time=%.2fs throughput=%.1f Mbit/s\n",
		streams, total.Load(), el, float64(total.Load())*8/1e6/el)
}

const stunMagic = 0x2112A442

type stunServer struct {
	name    string
	addr    netip.AddrPort
	sent    int
	rtts    []time.Duration
	mapped  []string // distinct public addresses, in the order first seen
	changes []string // "15:04:05 old -> new"
}

// stun probes all servers from one socket, so the mapped addresses show whether
// the NAT path keeps one public port per local socket (endpoint-independent
// mapping) and whether that port survives link events during the run.
func stun(names []string, every, dur time.Duration) {
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	var srvs []*stunServer
	for _, n := range names {
		ua, err := net.ResolveUDPAddr("udp4", strings.TrimSpace(n))
		if err != nil {
			log.Fatalf("resolve %s: %v", n, err)
		}
		srvs = append(srvs, &stunServer{name: n, addr: ua.AddrPort()})
	}
	type pending struct {
		s  *stunServer
		at time.Time
	}
	var mu sync.Mutex
	inflight := map[[12]byte]pending{}
	// Silences longer than gapAt between answers (any server), and answers
	// that took over a second: what a link switch costs a UDP flow.
	const gapAt = 250 * time.Millisecond
	var lastReply time.Time
	var gaps []string
	late := 0
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			now := time.Now()
			if n < 20 || binary.BigEndian.Uint16(buf) != 0x0101 || binary.BigEndian.Uint32(buf[4:]) != stunMagic {
				continue
			}
			var id [12]byte
			copy(id[:], buf[8:20])
			mu.Lock()
			p, ok := inflight[id]
			delete(inflight, id)
			if ok {
				if !lastReply.IsZero() && now.Sub(lastReply) > gapAt {
					gaps = append(gaps, fmt.Sprintf("%s silent %s", lastReply.Format("15:04:05.000"), now.Sub(lastReply).Round(time.Millisecond)))
				}
				lastReply = now
				if now.Sub(p.at) > time.Second {
					late++
				}
				p.s.rtts = append(p.s.rtts, now.Sub(p.at))
				if m := stunMapped(buf[:n]); m != "" && !slices.Contains(p.s.mapped, m) {
					if len(p.s.mapped) > 0 {
						p.s.changes = append(p.s.changes, fmt.Sprintf("%s %s -> %s", now.Format("15:04:05"), p.s.mapped[len(p.s.mapped)-1], m))
					}
					p.s.mapped = append(p.s.mapped, m)
				}
			}
			mu.Unlock()
		}
	}()
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req, 0x0001)
	binary.BigEndian.PutUint32(req[4:], stunMagic)
	end := time.Now().Add(dur)
	t := time.NewTicker(every)
	defer t.Stop()
	for i := 0; time.Now().Before(end); i++ {
		<-t.C
		s := srvs[i%len(srvs)]
		var id [12]byte
		rand.Read(id[:])
		copy(req[8:], id[:])
		mu.Lock()
		inflight[id] = pending{s, time.Now()}
		s.sent++
		mu.Unlock()
		if _, err := c.WriteToUDPAddrPort(req, s.addr); err != nil {
			log.Printf("send %s: %v", s.name, err)
		}
	}
	time.Sleep(2 * time.Second) // late answers still count; later ones are lost
	mu.Lock()
	defer mu.Unlock()
	for _, s := range srvs {
		r := slices.Clone(s.rtts)
		slices.Sort(r)
		pct := func(p float64) string {
			if len(r) == 0 {
				return "-"
			}
			return r[min(len(r)-1, int(p*float64(len(r))))].Round(100 * time.Microsecond).String()
		}
		loss := 100.0
		if s.sent > 0 {
			loss = 100 * float64(s.sent-len(r)) / float64(s.sent)
		}
		fmt.Printf("stun %s sent=%d recv=%d loss=%.1f%% p50=%s p95=%s p99=%s max=%s mapped=%s\n",
			s.name, s.sent, len(r), loss, pct(0.50), pct(0.95), pct(0.99), pct(1), strings.Join(s.mapped, ","))
		for _, ch := range s.changes {
			fmt.Printf("  mapped changed %s\n", ch)
		}
	}
	fmt.Printf("answers over 1 s late: %d\n", late)
	for _, g := range gaps {
		fmt.Printf("  gap: %s\n", g)
	}
}

// stunMapped returns the XOR-MAPPED-ADDRESS (or MAPPED-ADDRESS) of an IPv4
// binding response, "" if there is none.
func stunMapped(m []byte) string {
	for b := m[20:]; len(b) >= 4; {
		typ, l := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+l {
			return ""
		}
		v := b[4 : 4+l]
		if (typ == 0x0020 || typ == 0x0001) && l >= 8 && v[1] == 1 {
			port := binary.BigEndian.Uint16(v[2:])
			ip := [4]byte(v[4:8])
			if typ == 0x0020 {
				port ^= stunMagic >> 16
				binary.BigEndian.PutUint32(ip[:], binary.BigEndian.Uint32(ip[:])^stunMagic)
			}
			return netip.AddrPortFrom(netip.AddrFrom4(ip), port).String()
		}
		b = b[4+(l+3)&^3:]
	}
	return ""
}

func client(addr, dir string, streams int, dur time.Duration) {
	mode := byte('D')
	if dir == "up" {
		mode = 'U'
	}
	var total atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				log.Print(err)
				return
			}
			defer c.Close()
			var hdr [9]byte
			hdr[0] = mode
			binary.BigEndian.PutUint64(hdr[1:], uint64(dur.Milliseconds()))
			if _, err := c.Write(hdr[:]); err != nil {
				log.Print(err)
				return
			}
			if mode == 'D' {
				n, _ := io.Copy(io.Discard, c)
				total.Add(n)
				return
			}
			buf := make([]byte, bufSize)
			deadline := time.Now().Add(dur)
			c.SetWriteDeadline(deadline)
			for time.Now().Before(deadline) {
				if _, err := c.Write(buf); err != nil {
					if time.Now().Before(deadline) {
						log.Print(err)
					}
					break
				}
			}
			c.SetWriteDeadline(time.Time{})
			// No CloseWrite: adb forward turns a half-close into a full close.
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			var out [8]byte
			if _, err := io.ReadFull(c, out[:]); err == nil {
				total.Add(int64(binary.BigEndian.Uint64(out[:])))
			}
		}()
	}
	wg.Wait()
	el := time.Since(start).Seconds()
	fmt.Printf("%s streams=%d bytes=%d time=%.2fs throughput=%.1f Mbit/s\n",
		dir, streams, total.Load(), el, float64(total.Load())*8/1e6/el)
}
