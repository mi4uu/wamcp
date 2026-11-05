package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdArg(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/wamcp":         "/usr/bin/wamcp",
		"--listen":               "--listen",
		"me,+48600100200":        "me,+48600100200",
		"/home/a b/.wamcp":       `"/home/a b/.wamcp"`,
		`say "hi"`:               `"say \"hi\""`,
		`back\slash`:             `"back\\slash"`,
		"100%":                   "100%%",
		"$HOME":                  "$$HOME",
		"":                       `""`,
		"a;b":                    `"a;b"`,
		"/data/50% off $x y":     `"/data/50%% off $$x y"`,
		"it's":                   `"it's"`,
		"tab\there":              "\"tab\there\"",
		"wa.example.com,a.b.com": "wa.example.com,a.b.com",
	}
	for in, want := range cases {
		got, err := systemdArg(in)
		if err != nil || got != want {
			t.Errorf("systemdArg(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := systemdArg("a\nb"); err == nil {
		t.Error("line break accepted")
	}
}

func TestUnit(t *testing.T) {
	unit, err := Unit([]string{"/usr/bin/wamcp", "serve", "--service", "--data-dir", "/home/a b/.wamcp"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nExecStart=/usr/bin/wamcp serve --service --data-dir \"/home/a b/.wamcp\"\n",
		"\nRestart=on-failure\n",
		"\nWantedBy=default.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if _, err := Unit([]string{"/usr/bin/wamcp", "x\ny"}); err == nil {
		t.Error("line break accepted")
	}
}

func TestUnitPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if p, _ := UnitPath(); p != "/cfg/systemd/user/wamcp.service" {
		t.Errorf("UnitPath = %q", p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if p, _ := UnitPath(); p != "/home/u/.config/systemd/user/wamcp.service" {
		t.Errorf("UnitPath = %q", p)
	}
}

// TestUnitVerify checks the unit with systemd's own parser where it exists.
func TestUnitVerify(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not available")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	unit, err := Unit([]string{sh, "-c", `echo "50% $HOME" ok`, "a b"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), UnitName)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(analyze, "verify", "--man=no", path).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify: %v\n%s\n%s", err, out, unit)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, UnitName) {
			t.Errorf("systemd-analyze: %s", line)
		}
	}
}
