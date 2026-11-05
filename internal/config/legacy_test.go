package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvFallsBackToLegacyName(t *testing.T) {
	t.Setenv("WHATSUPD_X_TEST", "")
	t.Setenv("WAMCP_X_TEST", "old")
	if got := Env("X_TEST"); got != "old" {
		t.Fatalf("legacy fallback: %q", got)
	}
	t.Setenv("WHATSUPD_X_TEST", "new")
	if got := Env("X_TEST"); got != "new" {
		t.Fatalf("new name must win: %q", got)
	}
}

func TestDataDirReusesLegacy(t *testing.T) {
	home := t.TempDir()
	if got := dataDirIn(home); got != filepath.Join(home, ".whatsupd") {
		t.Fatalf("fresh install: %s", got)
	}
	if err := os.Mkdir(filepath.Join(home, ".wamcp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := dataDirIn(home); got != filepath.Join(home, ".wamcp") {
		t.Fatalf("old install must keep its data directory: %s", got)
	}
	if err := os.Mkdir(filepath.Join(home, ".whatsupd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := dataDirIn(home); got != filepath.Join(home, ".whatsupd") {
		t.Fatalf("moved install: %s", got)
	}
}

// TestLockSeesLegacyDaemon: an old daemon holding wamcp.lock in the same
// directory must keep a new one from starting.
func TestLockSeesLegacyDaemon(t *testing.T) {
	p := Paths{Dir: t.TempDir()}
	old, err := lockFile(p.LegacyLockFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Lock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock with an old daemon running: %v", err)
	}
	old()
	unlock, err := p.Lock()
	if err != nil {
		t.Fatalf("lock after the old daemon stopped: %v", err)
	}
	if _, err := p.Lock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
}
