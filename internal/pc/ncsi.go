package pc

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Windows' Network Connectivity Status Indicator probes these. They are
// answered locally, so Windows doesn't report "No internet" while the tunnel
// is up and the probes don't use mobile data.
const (
	ncsiDNSName  = "dns.msftncsi.com."
	ncsiDNSAddr  = "131.107.255.255"
	ncsiHTTPName = "www.msftconnecttest.com."
	ncsiHTTPBody = "Microsoft Connect Test"
)

func answerLocally(q []byte) []byte {
	var m dns.Msg
	if err := m.Unpack(q); err != nil || len(m.Question) != 1 {
		return nil
	}
	question := m.Question[0]
	name := strings.ToLower(question.Name)
	var ip string
	switch name {
	case ncsiDNSName:
		ip = ncsiDNSAddr
	case ncsiHTTPName, "ipv6.msftconnecttest.com.":
		ip = NCSIAddr.String()
	default:
		return nil
	}
	r := new(dns.Msg)
	r.SetReply(&m)
	r.RecursionAvailable = true
	if question.Qtype == dns.TypeA && name != "ipv6.msftconnecttest.com." {
		r.Answer = append(r.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP(ip),
		})
	}
	out, err := r.Pack()
	if err != nil {
		return nil
	}
	return out
}

func serveNCSIHTTP(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return
	}
	req.Body.Close()
	resp := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 22\r\nConnection: close\r\n\r\n" + ncsiHTTPBody
	if req.URL.Path != "/connecttest.txt" {
		resp = "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	}
	c.Write([]byte(resp))
}
