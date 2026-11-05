package service

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRun records service manager commands instead of running them.
func fakeRun(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := run
	run = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	t.Cleanup(func() { run = orig })
	return &calls
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchdRemoveLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	calls := fakeRun(t)
	if path, err := launchdRemoveLegacy(); err != nil || path != "" || len(*calls) != 0 {
		t.Fatalf("nothing installed: %q %v %v", path, err, *calls)
	}
	old := filepath.Join(home, "Library", "LaunchAgents", "dev.wamcp.daemon.plist")
	current := filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
	touch(t, old)
	touch(t, current)
	path, err := launchdRemoveLegacy()
	if err != nil || path != old {
		t.Fatalf("removed %q, %v", path, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old plist still there")
	}
	if _, err := os.Stat(current); err != nil {
		t.Error("current plist removed")
	}
	want := fmt.Sprintf("launchctl bootout gui/%d/dev.wamcp.daemon", os.Getuid())
	if !slices.Equal(*calls, []string{want}) {
		t.Errorf("commands %v, want %q", *calls, want)
	}
}

func TestSystemdRemoveLegacy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	calls := fakeRun(t)
	if path, err := systemdRemoveLegacy(); err != nil || path != "" || len(*calls) != 0 {
		t.Fatalf("nothing installed: %q %v %v", path, err, *calls)
	}
	old := filepath.Join(cfg, "systemd", "user", "wamcp.service")
	current := filepath.Join(cfg, "systemd", "user", UnitName)
	touch(t, old)
	touch(t, current)
	path, err := systemdRemoveLegacy()
	if err != nil || path != old {
		t.Fatalf("removed %q, %v", path, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old unit still there")
	}
	if _, err := os.Stat(current); err != nil {
		t.Error("current unit removed")
	}
	want := []string{"systemctl --user disable --now wamcp.service", "systemctl --user daemon-reload"}
	if !slices.Equal(*calls, want) {
		t.Errorf("commands %v, want %v", *calls, want)
	}
}
