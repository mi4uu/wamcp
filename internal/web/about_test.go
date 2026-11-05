package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mi4uu/whatsupd/internal/about"
)

func TestAboutNeedsSession(t *testing.T) {
	srv, _ := setup(t)
	srv.s.opt.About = about.New("v9.9.9", "foreground, MCP read-only, OAuth off, local only")
	if rec := do(srv, "GET", "/api/about", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without session: got %d", rec.Code)
	}
	if rec := do(srv, "GET", "/api/about", "", map[string]string{"Authorization": "Bearer mcp-only"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("with a bearer token: got %d", rec.Code)
	}
	rec := do(srv, "GET", "/login?code="+srv.NewLoginCode(), "", nil)
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("no session cookie")
	}
	rec = do(srv, "GET", "/api/about", "", map[string]string{"Cookie": cookie[0].Name + "=" + cookie[0].Value})
	var got about.Info
	if rec.Code != http.StatusOK || json.NewDecoder(rec.Body).Decode(&got) != nil {
		t.Fatalf("about: %d %s", rec.Code, rec.Body)
	}
	if got.Version != "v9.9.9" || got.BugURL == "" || got.Repo != about.Repo {
		t.Fatalf("about: %+v", got)
	}
}
