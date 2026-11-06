package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mi4uu/whatsupd/internal/config"
)

// Label identifies the LaunchAgent.
const Label = "dev.whatsupd.daemon"

// legacyLabel is the LaunchAgent installed before the rename.
const legacyLabel = "dev." + config.LegacyName + ".daemon"

// PlistPath returns ~/Library/LaunchAgents/<Label>.plist.
func PlistPath() (string, error) { return plistPath(Label) }

func plistPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func launchdRemoveLegacy() (string, error) {
	path, err := plistPath(legacyLabel)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	_, _ = launchctl("bootout", domain()+"/"+legacyLabel)
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
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

// run executes a service manager command; tests replace it.
var run = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func launchctl(args ...string) (string, error) {
	return run("launchctl", args...)
}

func launchdInstall(args []string, logFile string) (string, error) {
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

func launchdStop() {
	_, _ = launchctl("bootout", domain()+"/"+Label)
}

func launchdRestart() error {
	if out, err := launchctl("kickstart", "-k", domain()+"/"+Label); err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, out)
	}
	return nil
}

func launchdUninstall() error {
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

func launchdStatus() string {
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
