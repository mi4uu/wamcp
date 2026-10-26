// Package service installs wamcp as a macOS LaunchAgent so the daemon runs in
// the background, starts at login and restarts after crashes.
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Label identifies the LaunchAgent.
const Label = "dev.wamcp.daemon"

// PlistPath returns ~/Library/LaunchAgents/<Label>.plist.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Plist renders the LaunchAgent definition.
func Plist(args []string, logFile string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range args {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
	<key>ThrottleInterval</key><integer>30</integer>
	<key>ProcessType</key><string>Standard</string>
	<key>StandardOutPath</key><string>` + esc(logFile) + `</string>
	<key>StandardErrorPath</key><string>` + esc(logFile) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Install writes the LaunchAgent and (re)starts it.
func Install(args []string, logFile string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("service install is only implemented for macOS; use systemd or another supervisor to run `wamcp serve`")
	}
	path, err := PlistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(Plist(args, logFile)), 0o644); err != nil {
		return "", err
	}
	_, _ = launchctl("bootout", domain()+"/"+Label)
	if out, err := launchctl("bootstrap", domain(), path); err != nil {
		return "", fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	return path, nil
}

// Stop unloads the LaunchAgent without removing its definition.
func Stop() {
	_, _ = launchctl("bootout", domain()+"/"+Label)
}

// Restart restarts a loaded LaunchAgent (e.g. after installing a new binary).
func Restart() error {
	if out, err := launchctl("kickstart", "-k", domain()+"/"+Label); err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, out)
	}
	return nil
}

// Uninstall stops the LaunchAgent and removes its definition.
func Uninstall() error {
	path, err := PlistPath()
	if err != nil {
		return err
	}
	_, _ = launchctl("bootout", domain()+"/"+Label)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Status reports whether the LaunchAgent is loaded and running.
func Status() string {
	if runtime.GOOS != "darwin" {
		return "not supported on " + runtime.GOOS
	}
	out, err := launchctl("print", domain()+"/"+Label)
	if err != nil {
		return "not installed"
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") {
			return strings.TrimPrefix(line, "state = ")
		}
	}
	return "installed"
}
