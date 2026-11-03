package pc

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// StartAccessPoint keeps Windows Mobile Hotspot on via tools\mobile-hotspot.ps1
// (next to the exe's parent dir), as a child that exits when this process does.
// A Wi-Fi Direct legacy access point is not used: some phones' Wi-Fi firmware
// is unstable with it, while the Mobile Hotspot works.
func StartAccessPoint(secretFile, ssid string) error {
	pass, err := readPassphrase(secretFile)
	if err != nil {
		return err
	}
	exe, _ := os.Executable()
	script := filepath.Join(filepath.Dir(filepath.Dir(exe)), "tools", "mobile-hotspot.ps1")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("access point script: %w", err)
	}
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Action", "keep", "-Ssid", ssid, "-Passphrase", pass, "-ParentPid", fmt.Sprint(os.Getpid()),
		"-AdapterId", strings.ToLower(strings.Trim(adapterGUID.String(), "{}")))
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("access point: started (pid %d)", cmd.Process.Pid)
	go cmd.Wait()
	return nil
}

// APPassphrase returns the hotspot passphrase from the ap.secret file in the data folder (for pairing).
func APPassphrase(file string) (string, error) { return readPassphrase(file) }

func readPassphrase(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "passphrase="); ok && v != "" {
			return v, nil
		}
	}
	return "", errors.New("no passphrase= line in " + file)
}
