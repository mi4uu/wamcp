package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mi4uu/whatsupd/internal/config"
)

// UnitName is the systemd user unit. Packages ship the same name under
// /usr/lib/systemd/user; the copy `whatsupd service install` writes to the user's
// config directory takes precedence over it.
const UnitName = "whatsupd.service"

// UnitPath returns $XDG_CONFIG_HOME/systemd/user/whatsupd.service.
func UnitPath() (string, error) { return unitPath(UnitName) }

func unitPath(name string) (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", name), nil
}

// systemdArg quotes one ExecStart word. systemd expands % specifiers and $
// variables even inside quotes, so both are doubled.
func systemdArg(a string) (string, error) {
	if strings.ContainsAny(a, "\n\r\x00") {
		return "", fmt.Errorf("argument %q contains a line break", a)
	}
	a = strings.ReplaceAll(a, "%", "%%")
	a = strings.ReplaceAll(a, "$", "$$")
	if a != "" && !strings.ContainsAny(a, " \t\"'\\;") {
		return a, nil
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`, nil
}

// Unit renders the systemd user unit. Restart=on-failure matches the daemon's
// crash-loop guard, which exits 0 to stop restarts.
func Unit(args []string) (string, error) {
	words := make([]string, len(args))
	for i, a := range args {
		w, err := systemdArg(a)
		if err != nil {
			return "", err
		}
		words[i] = w
	}
	return `[Unit]
Description=whatsupd, WhatsApp archive and MCP server
Documentation=https://github.com/mi4uu/whatsupd

[Service]
ExecStart=` + strings.Join(words, " ") + `
Restart=on-failure
RestartSec=30

[Install]
WantedBy=default.target
`, nil
}

func systemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func systemdAvailable() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found; run `whatsupd serve` under your own supervisor")
	}
	if out, err := systemctl("show-environment"); err != nil {
		return fmt.Errorf("no systemd user session (%s); this is common in containers and WSL without systemd; run `whatsupd serve` under your own supervisor", out)
	}
	return nil
}

func systemdInstall(args []string) (string, error) {
	if err := systemdAvailable(); err != nil {
		return "", err
	}
	unit, err := Unit(args)
	if err != nil {
		return "", err
	}
	path, err := UnitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return "", err
	}
	for _, step := range [][]string{{"daemon-reload"}, {"enable", UnitName}, {"restart", UnitName}} {
		if out, err := systemctl(step...); err != nil {
			return "", fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(step, " "), err, out)
		}
	}
	return path, nil
}

func systemdRemoveLegacy() (string, error) {
	legacy := config.LegacyName + ".service"
	path, err := unitPath(legacy)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	_, _ = systemctl("disable", "--now", legacy)
	if err := os.Remove(path); err != nil {
		return "", err
	}
	_, _ = systemctl("daemon-reload")
	return path, nil
}

func systemdStop() {
	_, _ = systemctl("stop", UnitName)
}

func systemdRestart() error {
	if out, err := systemctl("restart", UnitName); err != nil {
		return fmt.Errorf("systemctl --user restart: %v: %s", err, out)
	}
	return nil
}

func systemdUninstall() error {
	path, err := UnitPath()
	if err != nil {
		return err
	}
	_, _ = systemctl("disable", "--now", UnitName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = systemctl("daemon-reload")
	return nil
}

func systemdStatus() string {
	path, err := UnitPath()
	if err != nil {
		return "not installed"
	}
	if _, err := os.Stat(path); err != nil {
		if _, err := exec.LookPath("systemctl"); err == nil {
			if out, _ := systemctl("is-enabled", UnitName); out == "enabled" {
				state, _ := systemctl("is-active", UnitName)
				return state + " (packaged unit)"
			}
		}
		return "not installed"
	}
	state, _ := systemctl("is-active", UnitName)
	if state == "" {
		return "installed"
	}
	return state
}

// systemdNotes warns when the user manager stops at logout, which on a server
// reached over SSH would stop the daemon with the last session.
func systemdNotes() string {
	out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value").Output()
	if err != nil || strings.TrimSpace(string(out)) != "no" {
		return ""
	}
	return "systemd stops user services when you log out. To keep whatsupd running, run: loginctl enable-linger"
}
