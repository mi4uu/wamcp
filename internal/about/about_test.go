package about

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBugURLPrefills(t *testing.T) {
	i := New("v1.2.3", "MCP read-only, OAuth off")
	u, err := url.Parse(i.BugURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(i.BugURL, Repo+"/issues/new?") {
		t.Fatalf("bug URL %q", i.BugURL)
	}
	q := u.Query()
	want := map[string]string{
		"template": "bug_report.yml",
		"version":  "v1.2.3",
		"platform": runtime.GOOS + "/" + runtime.GOARCH + ", " + runtime.Version(),
		"setup":    "MCP read-only, OAuth off",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("unexpected fields in %v", q)
	}
}

// TestTemplatesHaveFields keeps the pre-filled ids in step with the issue form.
func TestTemplatesHaveFields(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "ISSUE_TEMPLATE")
	bug, err := os.ReadFile(filepath.Join(dir, "bug_report.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"version", "platform", "setup"} {
		if !strings.Contains(string(bug), "id: "+id+"\n") {
			t.Errorf("bug_report.yml has no field id %q", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "feature_request.yml")); err != nil {
		t.Error(err)
	}
}
