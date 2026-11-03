package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	tokenFile    = "token"
	apSecretFile = "ap.secret"
)

// defaultDataDir is %ProgramData%\Smart Tether. install restricts it to
// SYSTEM and Administrators (full control) and interactively signed-in users
// (read): the tray app runs as the signed-in user and reads the token there.
func defaultDataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "Smart Tether")
}

// ensureKeys creates the link token and the hotspot passphrase on first install.
// Existing keys are kept, so reinstalling doesn't unpair the phone.
func ensureKeys(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tp := filepath.Join(dir, tokenFile)
	if _, err := os.Stat(tp); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(tp, []byte(randomString(32)), 0o644); err != nil {
			return err
		}
	}
	ap := filepath.Join(dir, apSecretFile)
	if _, err := os.Stat(ap); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ap, []byte("passphrase="+randomString(16)+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func randomString(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // no look-alikes: it may be typed by hand
	b := make([]byte, n)
	for i := range b {
		x, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[x.Int64()]
	}
	return string(b)
}

// install registers the service for this executable (or updates it), lets
// interactive users start and stop it (the tray app's Pause doesn't need it,
// but "Start with Windows" and manual restarts do), and starts it.
func install(dataDir string) error {
	if err := ensureKeys(dataDir); err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	if err := restrictDataDir(dataDir); err != nil {
		// Not fatal: the folder keeps the default %ProgramData% permissions.
		fmt.Fprintf(os.Stderr, "data folder permissions: %v\n", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()

	args := []string{"-data", dataDir}
	s, err := m.OpenService(serviceName)
	if err == nil {
		stop(s)
		cfg, err := s.Config()
		if err != nil {
			s.Close()
			return err
		}
		cfg.BinaryPathName = windows.EscapeArg(exe) + " " + strings.Join(escapeAll(args), " ")
		cfg.DisplayName, cfg.Description = displayName, description
		if err := s.UpdateConfig(cfg); err != nil {
			s.Close()
			return err
		}
	} else {
		s, err = m.CreateService(serviceName, exe, mgr.Config{
			DisplayName: displayName,
			Description: description,
			StartType:   mgr.StartAutomatic,
		}, args...)
		if err != nil {
			return fmt.Errorf("create service: %w", err)
		}
	}
	defer s.Close()
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 300); err != nil {
		return fmt.Errorf("recovery actions: %w", err)
	}
	if err := allowInteractiveControl(); err != nil {
		return fmt.Errorf("service permissions: %w", err)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil // not installed
	}
	defer s.Close()
	stop(s)
	return s.Delete()
}

func stop(s *mgr.Service) {
	st, err := s.Control(svc.Stop)
	if err != nil {
		return
	}
	for i := 0; i < 40 && st.State != svc.Stopped; i++ {
		time.Sleep(500 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return
		}
	}
}

// allowInteractiveControl adds an ACE for Interactive Users (IU): query,
// start, stop, interrogate. sc.exe is the simplest way to edit the SDDL.
func allowInteractiveControl() error {
	out, err := exec.Command("sc.exe", "sdshow", serviceName).Output()
	if err != nil {
		return err
	}
	sddl := strings.TrimSpace(string(out))
	const ace = "(A;;CCLCSWRPWPLORC;;;IU)"
	if strings.Contains(sddl, ace) {
		return nil
	}
	if i := strings.Index(sddl, "S:"); i >= 0 {
		sddl = sddl[:i] + ace + sddl[i:]
	} else {
		sddl += ace
	}
	if out, err := exec.Command("sc.exe", "sdset", serviceName, sddl).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// restrictDataDir replaces the folder's inherited %ProgramData% permissions
// (which let every local user create files in it) with: SYSTEM and
// Administrators full control, INTERACTIVE read. Files inside inherit this.
func restrictDataDir(dir string) error {
	out, err := exec.Command("icacls.exe", dir, "/inheritance:r",
		"/grant:r", "*S-1-5-18:(OI)(CI)F", // SYSTEM (the service)
		"/grant:r", "*S-1-5-32-544:(OI)(CI)F", // Administrators
		"/grant:r", "*S-1-5-4:(OI)(CI)RX", // INTERACTIVE (the tray app reads the token)
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func escapeAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = windows.EscapeArg(a)
	}
	return out
}
