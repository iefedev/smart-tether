package pc

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Preference limits which transports the link may use.
type Preference string

const (
	PreferAuto Preference = "auto"
	PreferUSB  Preference = "usb"
	PreferWiFi Preference = "wifi"
)

// Settings are changed from the tray app and kept across restarts.
type Settings struct {
	Preference Preference `json:"preference"`
	QUIC       bool       `json:"quic"`
	Paused     bool       `json:"paused"`
}

// Controls is how the API acts on the running tunnel; main wires it up.
type Controls struct {
	Apply               func(old, new Settings) // called after a settings change
	StartWithWindows    func() (bool, error)
	SetStartWithWindows func(bool) error
	Pairing             func() (any, error)
}

// Sample is one second of traffic.
type Sample struct {
	Down uint64 `json:"d"` // bytes/s into Windows
	Up   uint64 `json:"u"` // bytes/s out of Windows
}

type Event struct {
	Time time.Time `json:"t"`
	Text string    `json:"text"`
}

// Monitor keeps what the tray app shows: link state, traffic history, recent events.
type Monitor struct {
	Link    *Link
	Handler *Handler
	TUN     *TUN

	mu        sync.Mutex
	settings  Settings
	file      string
	history   []Sample // last 120 s, oldest first
	events    []Event  // last 30, newest last
	lastUp    uint64
	lastDown  uint64
	baseUp    uint64 // TUN counters when the current session started
	baseDown  uint64
	sessionOn bool
	phone     string
}

func NewMonitor(settingsFile string) *Monitor {
	m := &Monitor{file: settingsFile, settings: Settings{Preference: PreferAuto}}
	if b, err := os.ReadFile(settingsFile); err == nil {
		json.Unmarshal(b, &m.settings)
	}
	if m.settings.Preference == "" {
		m.settings.Preference = PreferAuto
	}
	return m
}

// Restore replaces the settings without applying them (startup only).
func (m *Monitor) Restore(s Settings) {
	m.mu.Lock()
	m.settings = s
	m.mu.Unlock()
}

func (m *Monitor) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

func (m *Monitor) Event(format string, args ...any) {
	e := Event{Time: time.Now(), Text: fmt.Sprintf(format, args...)}
	m.mu.Lock()
	m.events = append(m.events, e)
	if len(m.events) > 30 {
		m.events = m.events[len(m.events)-30:]
	}
	m.mu.Unlock()
}

// SetPhone records the phone's display name (from the phone or adb).
func (m *Monitor) SetPhone(name string) {
	m.mu.Lock()
	m.phone = name
	m.mu.Unlock()
}

// Sample runs once a second.
func (m *Monitor) Sample() {
	up, down := m.TUN.Up.Load(), m.TUN.Down.Load()
	st := m.Link.State()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = append(m.history, Sample{Down: down - m.lastDown, Up: up - m.lastUp})
	if len(m.history) > 120 {
		m.history = m.history[len(m.history)-120:]
	}
	m.lastUp, m.lastDown = up, down
	if st.Session && !m.sessionOn {
		m.baseUp, m.baseDown = up, down
	}
	m.sessionOn = st.Session
}

type statusJSON struct {
	State            string   `json:"state"` // connected | reconnecting | searching | paused
	Transport        string   `json:"transport,omitempty"`
	Addr             string   `json:"addr,omitempty"`
	Phone            string   `json:"phone,omitempty"`
	SessionSeconds   int64    `json:"sessionSeconds"`
	SessionDown      uint64   `json:"sessionDown"`
	SessionUp        uint64   `json:"sessionUp"`
	TotalDown        uint64   `json:"totalDown"`
	TotalUp          uint64   `json:"totalUp"`
	History          []Sample `json:"history"`
	Events           []Event  `json:"events"`
	Settings         Settings `json:"settings"`
	StartWithWindows bool     `json:"startWithWindows"`
	TCP              int64    `json:"tcp"`
	UDP              int64    `json:"udp"`
}

func (m *Monitor) status(c *Controls) statusJSON {
	st := m.Link.State()
	up, down := m.TUN.Up.Load(), m.TUN.Down.Load()
	m.mu.Lock()
	s := statusJSON{
		Phone:     m.phone,
		TotalDown: down, TotalUp: up,
		// Never nil: an empty history must encode as [], not null, for API clients.
		History:  append(make([]Sample, 0, len(m.history)), m.history...),
		Events:   append(make([]Event, 0, len(m.events)), m.events...),
		Settings: m.settings,
		TCP:      m.Handler.tcpActive.Load(),
		UDP:      m.Handler.udpActive.Load(),
	}
	if st.Session {
		s.SessionDown, s.SessionUp = down-m.baseDown, up-m.baseUp
	}
	m.mu.Unlock()
	switch {
	case s.Settings.Paused:
		s.State = "paused"
	case st.Up:
		s.State = "connected"
	case st.Session:
		s.State = "reconnecting"
	default:
		s.State = "searching"
	}
	if st.Session {
		s.Transport, s.Addr = st.Transport.String(), st.Addr
		s.SessionSeconds = int64(st.SessionAge / time.Second)
	}
	if c.StartWithWindows != nil {
		s.StartWithWindows, _ = c.StartWithWindows()
	}
	return s
}

// ServeAPI serves the tray app's JSON API on a loopback address. Every
// request needs the link token in the X-PT-Key header. The token file is
// readable by users signed in to this PC (the tray app reads it), so the key
// keeps out web pages and non-interactive accounts; any process of a user
// signed in to this PC can read the token and call the API.
func (m *Monitor) ServeAPI(addr, key string, c *Controls) error {
	authed := func(r *http.Request) bool {
		got := r.Header.Get("X-PT-Key")
		return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, m.status(c))
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) || r.Method != http.MethodPost {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Preference       *Preference `json:"preference"`
			QUIC             *bool       `json:"quic"`
			Paused           *bool       `json:"paused"`
			StartWithWindows *bool       `json:"startWithWindows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.StartWithWindows != nil && c.SetStartWithWindows != nil {
			if err := c.SetStartWithWindows(*req.StartWithWindows); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		m.mu.Lock()
		old := m.settings
		next := old
		if p := req.Preference; p != nil && (*p == PreferAuto || *p == PreferUSB || *p == PreferWiFi) {
			next.Preference = *p
		}
		if req.QUIC != nil {
			next.QUIC = *req.QUIC
		}
		if req.Paused != nil {
			next.Paused = *req.Paused
		}
		m.settings = next
		b, _ := json.MarshalIndent(next, "", "  ")
		m.mu.Unlock()
		if err := os.WriteFile(m.file, b, 0o644); err != nil {
			log.Printf("settings: %v", err)
		}
		if c.Apply != nil && next != old {
			c.Apply(old, next)
		}
		writeJSON(w, m.status(c))
	})
	mux.HandleFunc("/api/pairing", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) || c.Pairing == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p, err := c.Pairing()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, p)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	log.Printf("api: http://%s/api/", addr)
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
