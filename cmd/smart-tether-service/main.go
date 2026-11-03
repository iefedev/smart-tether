// Command smart-tether-service is the PC side of Smart Tether. It runs as the
// "SmartTether" Windows service, elevated (wintun and routes need it):
//
//	SmartTetherService.exe install     create the keys, register and start the service (admin)
//	SmartTetherService.exe uninstall   stop and remove the service (admin)
//	SmartTetherService.exe version     print the version
//	SmartTetherService.exe [flags]     run in a console, for development (admin)
//
// It dials the phone over USB (adb forward) when the cable is in, otherwise
// over the Mobile Hotspot Wi-Fi. In "full" mode the routes are withdrawn
// whenever the phone has been unreachable for -grace, so Windows falls back to
// its other connections.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/xjasonlyu/tun2socks/v2/core"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"smarttether/internal/pc"
)

const (
	serviceName = "SmartTether"
	displayName = "Smart Tether"
	description = "Shares the phone's mobile data with this PC over USB or Wi-Fi."
	ssid        = "SmartTether" // the hotspot the phone joins (also sent in the pairing code)
)

// version is the release version; tools/build-release.ps1 sets it with -ldflags -X.
var version = "0.2.0"

var fullRoutes = []string{"0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"}

type options struct {
	token    string
	routes   string
	adb      string
	ap       string
	grace    time.Duration
	maxRun   time.Duration
	mtu      int
	apSecret string
	quic     bool
	api      string
	settings string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "install" || os.Args[1] == "uninstall") {
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		data := fs.String("data", defaultDataDir(), "data folder (keys, settings, log)")
		fs.Parse(os.Args[2:])
		var err error
		if os.Args[1] == "install" {
			err = install(*data)
		} else {
			err = uninstall()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	var o options
	data := flag.String("data", defaultDataDir(), "data folder: token, ap.secret, settings.json, service.log")
	token := flag.String("token", "", "link token (default: the token file in -data)")
	flag.StringVar(&o.routes, "routes", "full", `"full", "" (none) or comma-separated prefixes to send through the tunnel`)
	usb := flag.Bool("usb", true, "find the phone over USB through the running adb server (Smart Tether never starts one itself)")
	hotspot := flag.Bool("hotspot", true, "run the Mobile Hotspot the phone joins over Wi-Fi")
	flag.StringVar(&o.ap, "ap", "192.168.137.1/24", "this PC's address on its hotspot")
	flag.DurationVar(&o.grace, "grace", 10*time.Second, "withdraw routes after the session has been gone this long (it survives up to 30 s without a connection)")
	flag.DurationVar(&o.maxRun, "max", 0, "exit after this long (0 = run forever)")
	flag.IntVar(&o.mtu, "mtu", 1400, "TUN MTU")
	// Off by default: downloads through the tunnel are markedly slower over QUIC than over TCP,
	// and browsers fall back to TCP at once when UDP/443 is dropped.
	flag.BoolVar(&o.quic, "quic", false, "carry UDP/443 (QUIC) on first run; afterwards the tray app setting wins")
	flag.StringVar(&o.api, "api", "127.0.0.1:7780", "loopback address of the tray app's API")
	logFile := flag.String("log", "", `append the log to this file (default for the service: service.log in -data)`)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Fatal(err)
	}
	if *logFile == "" && isService {
		*logFile = filepath.Join(*data, "service.log")
	}
	if *logFile != "" {
		os.MkdirAll(filepath.Dir(*logFile), 0o755)
		f, err := openLog(*logFile)
		if err != nil {
			// A service exiting here shows up only as error 1053; leave a trace next to the log.
			os.WriteFile(*logFile+".err", []byte(err.Error()+"\n"), 0o644)
			log.Fatal(err)
		}
		defer f.Close()
		log.SetOutput(f)
	}
	o.token = *token
	if o.token == "" {
		b, err := os.ReadFile(filepath.Join(*data, tokenFile))
		if err != nil {
			log.Fatalf("no link token: %v (run `SmartTetherService.exe install`)", err)
		}
		o.token = strings.TrimSpace(string(b))
	}
	if *usb {
		o.adb = "adb"
	}
	if *hotspot {
		o.apSecret = filepath.Join(*data, apSecretFile)
	}
	o.settings = filepath.Join(*data, "settings.json")

	if isService {
		if err := svc.Run(serviceName, &service{o: o}); err != nil {
			log.Fatalf("service: %v", err)
		}
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		log.Print("interrupted")
		cancel()
	}()
	run(ctx, o)
}

// service adapts run to the Windows service control manager.
type service struct{ o options }

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		run(ctx, s.o)
		close(done)
	}()
	accepts := svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				log.Print("service stop requested")
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					log.Print("service: shutdown timed out")
				}
				return false, 0
			}
		case <-done:
			// run only returns on its own after -max or a fatal error; let the
			// service manager's recovery policy restart us.
			cancel()
			return false, 1
		}
	}
}

// run brings the tunnel up and keeps it until ctx ends.
func run(ctx context.Context, o options) {
	prefixes := strings.Split(o.routes, ",")
	if o.routes == "full" {
		prefixes = fullRoutes
	} else if o.routes == "" {
		prefixes = nil
	}
	apPrefix, err := netip.ParsePrefix(o.ap)
	if err != nil {
		log.Printf("-ap: %v", err)
		return
	}

	t, err := pc.OpenTUN("SmartTether", o.mtu)
	if err != nil {
		log.Print(err)
		return
	}
	defer t.Close()
	if err := t.Configure(); err != nil {
		log.Print(err)
		return
	}

	var routesMu sync.Mutex
	routesUp := false
	setRoutes := func(up bool) {
		routesMu.Lock()
		defer routesMu.Unlock()
		if up == routesUp || len(prefixes) == 0 {
			return
		}
		if up {
			if err := t.AddRoutes(prefixes); err != nil {
				log.Printf("routes: %v", err)
				t.RemoveRoutes(prefixes)
				return
			}
		} else {
			t.RemoveRoutes(prefixes)
		}
		routesUp = up
		log.Printf("routes %s: %s", map[bool]string{true: "added", false: "withdrawn"}[up], strings.Join(prefixes, ","))
	}
	defer setRoutes(false)

	if o.apSecret != "" {
		if err := pc.StartAccessPoint(o.apSecret, ssid); err != nil {
			log.Printf("access point: %v", err)
		}
	}

	mon := pc.NewMonitor(o.settings)
	if _, err := os.Stat(o.settings); err != nil {
		s := mon.Settings()
		s.QUIC = o.quic // first run: the flag seeds the saved setting
		mon.Restore(s)
	}

	disc := &pc.Discovery{ADB: o.adb, APNet: apPrefix.Masked(), APHost: apPrefix.Addr()}
	l := &pc.Link{
		Token: o.token,
		// The tray app's preference and pause filter what Discovery finds.
		Candidates: func() []pc.Candidate {
			s := mon.Settings()
			if s.Paused {
				return nil
			}
			var out []pc.Candidate
			for _, c := range disc.Candidates() {
				if s.Preference == pc.PreferUSB && c.Transport != pc.USB || s.Preference == pc.PreferWiFi && c.Transport != pc.WiFi {
					continue
				}
				out = append(out, c)
			}
			return out
		},
	}
	var lastMu sync.Mutex
	var lastTransport string
	var reportedExit atomic.Int64 // the phone app kill already reported, unix millis
	l.OnChange = func(up bool, tr pc.Transport) {
		lastMu.Lock()
		defer lastMu.Unlock()
		if up {
			setRoutes(true)
			go func() {
				info, err := pc.PhoneInfo(l)
				if err != nil {
					return
				}
				if info.Name != "" {
					mon.SetPhone(info.Name)
				}
				// A killed phone app comes back with new sockets: every UDP
				// flow (a voice call, for example) got a new public port.
				if e := info.LastExit; e != nil && reportedExit.Swap(e.At) != e.At {
					at := time.UnixMilli(e.At)
					log.Printf("phone: Android ended the app at %s (%s): %s", at.Format(time.DateTime), e.Reason, e.Detail)
					when := at.Format("15:04")
					if now := time.Now(); at.YearDay() != now.YearDay() || at.Year() != now.Year() {
						when = at.Format("Jan 2 15:04")
					}
					mon.Event("The phone stopped the app at %s (%s); calls had to reconnect", when, e.Reason)
				}
			}()
			if lastTransport != "" && lastTransport != tr.String() {
				mon.Event("Moved to %s, connections kept", label(tr))
			} else {
				mon.Event("Connected over %s", label(tr))
			}
			lastTransport = tr.String()
		} else {
			mon.Event("%s connection lost, looking for another path", label(tr))
		}
	}
	h := &pc.Handler{Link: l}
	l.OnDatagram = h.FromPhone // set before Run starts a session
	h.SetQUIC(mon.Settings().QUIC)
	if _, err := core.CreateStack(&core.Config{LinkEndpoint: t, TransportHandler: h}); err != nil {
		log.Print(err)
		return
	}
	mon.Link, mon.Handler, mon.TUN = l, h, t
	controls := &pc.Controls{
		Apply: func(old, next pc.Settings) {
			h.SetQUIC(next.QUIC)
			switch {
			case next.Paused && !old.Paused:
				l.Drop()
				setRoutes(false)
				mon.Event("Paused; Windows uses its other connections")
			case !next.Paused && old.Paused:
				mon.Event("Resumed")
				l.Reselect()
			case next.Preference != old.Preference:
				mon.Event("Connection preference: %s", next.Preference)
				l.Reselect()
			}
		},
		StartWithWindows:    serviceAutoStart,
		SetStartWithWindows: setServiceAutoStart,
		Pairing: func() (any, error) {
			pass, err := pc.APPassphrase(o.apSecret)
			if err != nil {
				return nil, err
			}
			payload := fmt.Sprintf(`{"v":1,"token":%q,"ssid":%q,"pass":%q}`, o.token, ssid, pass)
			png, err := qrcode.Encode(payload, qrcode.Medium, 320)
			if err != nil {
				return nil, err
			}
			return map[string]string{"qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)}, nil
		},
	}
	if err := mon.ServeAPI(o.api, o.token, controls); err != nil {
		log.Printf("api: %v", err)
	}
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	go l.Run(lctx)
	go autoPair(lctx, disc, l, mon, o)
	log.Printf("ready: Smart Tether %s, tun %q, routes %q", version, t.Name, o.routes)
	mon.Event("Started; looking for the phone")

	var deadline <-chan time.Time
	if o.maxRun > 0 {
		deadline = time.After(o.maxRun)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	statsAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			log.Print("max run time reached")
			return
		case <-tick.C:
			mon.Sample()
			if l.DownFor() > o.grace {
				setRoutes(false)
			}
			if time.Since(statsAt) >= 30*time.Second {
				statsAt = time.Now()
				log.Printf("stats: %s", h.Stats())
			}
		}
	}
}

// autoPair pushes the pairing to a phone that is plugged in (adb lists it) while
// no connection has been up for 10 s: the app isn't running or has no token yet.
// At most once a minute, so a phone that keeps refusing isn't spammed.
func autoPair(ctx context.Context, disc *pc.Discovery, l *pc.Link, mon *pc.Monitor, o options) {
	var last time.Time
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s := mon.Settings()
		serial := disc.USBSerial()
		if serial == "" || s.Paused || s.Preference == pc.PreferWiFi || l.NotUpFor() < 10*time.Second || time.Since(last) < time.Minute {
			continue
		}
		last = time.Now()
		pass, err := pc.APPassphrase(o.apSecret)
		if err != nil {
			continue
		}
		if err := disc.PushPairing(serial, o.token, pass); err != nil {
			log.Printf("auto-pair: %v", err)
			continue
		}
		log.Printf("auto-pair: sent pairing to %s and started the app", serial)
		mon.Event("Paired the phone over USB")
	}
}

func label(t pc.Transport) string {
	if t == pc.USB {
		return "USB"
	}
	return "Wi-Fi"
}

// serviceAutoStart and setServiceAutoStart read and set the service's start
// type; the service runs as LocalSystem, so it may change its own config.
func serviceAutoStart() (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return false, err
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return false, err
	}
	return c.StartType == mgr.StartAutomatic, nil
}

func setServiceAutoStart(on bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return err
	}
	c.StartType = mgr.StartManual
	if on {
		c.StartType = mgr.StartAutomatic
	}
	return s.UpdateConfig(c)
}

// openLog appends to path, moving it to path+".old" first once it passes 10 MB.
func openLog(path string) (io.WriteCloser, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		os.Rename(path, path+".old")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
