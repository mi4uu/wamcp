package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mi4uu/whatsupd/internal/about"
	"github.com/mi4uu/whatsupd/internal/archive"
	"github.com/mi4uu/whatsupd/internal/demo"
	"github.com/mi4uu/whatsupd/internal/ingest"
	"github.com/mi4uu/whatsupd/internal/wa"
)

// fakeDaemon stands in for wa.Manager.
type fakeDaemon struct {
	bus       *wa.Bus
	state     string
	sent      []string
	presence  chan bool
	lastError string
}

func (f *fakeDaemon) Status() archive.DaemonStatus {
	return archive.DaemonStatus{State: f.state, Account: demo.Me.String(), PushName: "Demo", LastError: f.lastError}
}
func (f *fakeDaemon) QRPNG(int) ([]byte, bool)                      { return nil, false }
func (f *fakeDaemon) RequestPairing() bool                          { return false }
func (f *fakeDaemon) RequestPairCode(context.Context, string) error { return nil }
func (f *fakeDaemon) Logout(context.Context) error                  { return nil }
func (f *fakeDaemon) Bus() *wa.Bus                                  { return f.bus }
func (f *fakeDaemon) DownloadMedia(context.Context, string, string) (*wa.MediaFile, error) {
	return nil, errors.New("demo: media not available")
}
func (f *fakeDaemon) Avatar(context.Context, string) (string, error)          { return "", nil }
func (f *fakeDaemon) RequestOlderMessages(context.Context, string, int) error { return nil }
func (f *fakeDaemon) SendText(_ context.Context, chat, text, _ string) (*ingest.MessageResult, error) {
	f.sent = append(f.sent, chat+": "+text)
	return &ingest.MessageResult{ChatJID: chat}, nil
}
func (f *fakeDaemon) SendMedia(context.Context, string, wa.MediaUpload) (*ingest.MessageResult, error) {
	return nil, errors.New("demo")
}
func (f *fakeDaemon) React(context.Context, string, string, string) error    { return nil }
func (f *fakeDaemon) Edit(context.Context, string, string, string) error     { return nil }
func (f *fakeDaemon) Delete(context.Context, string, string) error           { return nil }
func (f *fakeDaemon) SetStarred(context.Context, string, string, bool) error { return nil }
func (f *fakeDaemon) MarkRead(context.Context, string) error                 { return nil }
func (f *fakeDaemon) SetArchived(context.Context, string, bool) error        { return nil }
func (f *fakeDaemon) SetPinned(context.Context, string, bool) error          { return nil }
func (f *fakeDaemon) SetMuted(context.Context, string, time.Duration) error  { return nil }
func (f *fakeDaemon) SetTyping(context.Context, string, bool) error          { return nil }
func (f *fakeDaemon) SetAvailable(_ context.Context, v bool) error {
	if f.presence != nil {
		f.presence <- v
	}
	return nil
}
func (f *fakeDaemon) SubscribePresence(context.Context, string) error { return nil }

const testToken = "secret-token"

func setup(t *testing.T) (*Server, *fakeDaemon) {
	t.Helper()
	ctx := context.Background()
	store, err := archive.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := demo.Seed(ctx, store, time.Now()); err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{bus: wa.NewBus(), state: wa.StateConnected}
	mcpStub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("mcp-ok")) })
	return Handler(Options{Token: testToken, MCP: mcpStub, Archive: store, Daemon: d}), d
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:8765"+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMCPRequiresToken(t *testing.T) {
	srv, _ := setup(t)
	if rec := do(srv, "POST", "/mcp", "{}", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", rec.Code)
	}
	if rec := do(srv, "POST", "/mcp", "{}", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d", rec.Code)
	}
	rec := do(srv, "POST", "/mcp", "{}", map[string]string{"Authorization": "Bearer " + testToken})
	if rec.Code != http.StatusOK || rec.Body.String() != "mcp-ok" {
		t.Fatalf("valid token: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestHostCheckBlocksRebinding(t *testing.T) {
	srv, _ := setup(t)
	req := httptest.NewRequest("GET", "http://evil.example:8765/api/session", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign Host header: got %d", rec.Code)
	}
}

func TestPublicHostAllowed(t *testing.T) {
	srv, _ := setup(t)
	srv.s.opt.PublicHosts = []string{"wa.example.com"}
	for host, want := range map[string]int{"wa.example.com": http.StatusOK, "WA.example.com:443": http.StatusOK, "evil.example": http.StatusForbidden} {
		req := httptest.NewRequest("GET", "http://"+host+"/api/session", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: got %d, want %d", host, rec.Code, want)
		}
	}
}

func TestPairingNeedsSignInWhenExposed(t *testing.T) {
	srv, _ := setup(t)
	srv.s.opt.PublicHosts = []string{"wa.example.com"}
	for _, ep := range []struct{ method, path string }{{"GET", "/qr.png"}, {"POST", "/api/pairing/restart"}, {"POST", "/api/pairing/phone"}} {
		req := httptest.NewRequest(ep.method, "http://wa.example.com"+ep.path, strings.NewReader(`{"phone":"+48600000000"}`))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s from a public host: got %d", ep.method, ep.path, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "http://wa.example.com/api/session", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["pairing_locked"] != true || out["pair_code"] != nil || out["has_qr"] != false {
		t.Fatalf("session leaks pairing details to a public visitor: %v", out)
	}
}

func TestProxyProcessNames(t *testing.T) {
	for cmd, want := range map[string]bool{"cloudflared": true, "/usr/local/bin/ngrok": true, "ssh": true, "nginx": true,
		"Google Chrome Helper": false, "firefox": false, "com.apple.WebKit.Networking": false, "curl": false} {
		if got := proxyProcess(cmd); got != want {
			t.Errorf("proxyProcess(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestSessionFlow(t *testing.T) {
	srv, d := setup(t)
	if rec := do(srv, "GET", "/api/chats", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("chats without session: %d", rec.Code)
	}
	// One-time code from the CLI (bearer), then the browser redeems it.
	rec := do(srv, "POST", "/api/login-code", "", map[string]string{"Authorization": "Bearer " + testToken})
	var out struct{ Code string }
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil || out.Code == "" {
		t.Fatalf("login code: %d %v", rec.Code, err)
	}
	rec = do(srv, "GET", "/login?code="+out.Code, "", nil)
	cookie := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || len(cookie) == 0 {
		t.Fatalf("login: %d cookies=%d", rec.Code, len(cookie))
	}
	if rec := do(srv, "GET", "/login?code="+out.Code, "", nil); len(rec.Result().Cookies()) != 0 {
		t.Fatal("login code reused")
	}
	sess := map[string]string{"Cookie": cookie[0].Name + "=" + cookie[0].Value}
	rec = do(srv, "GET", "/api/chats", "", sess)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Rodzina") {
		t.Fatalf("chats: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(srv, "GET", "/api/chats/"+demo.Family.String()+"/messages", "", sess)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "sernik") || !strings.Contains(rec.Body.String(), `"reactions"`) {
		t.Fatalf("messages: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(srv, "PUT", "/api/meta/"+demo.Kasia.String(), `{"priority":4,"tags":["Rodzina"," przyjaciele "],"description":"x"}`,
		map[string]string{"Cookie": sess["Cookie"], "Content-Type": "application/json"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tags":["rodzina","przyjaciele"]`) {
		t.Fatalf("meta: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(srv, "POST", "/api/chats/"+demo.Anna.String()+"/messages", `{"text":"hej"}`,
		map[string]string{"Cookie": sess["Cookie"], "Content-Type": "application/json"})
	if rec.Code != 200 || len(d.sent) != 1 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	// A cross-site form post must be refused even with the cookie.
	rec = do(srv, "POST", "/api/chats/"+demo.Anna.String()+"/messages", `{"text":"csrf"}`,
		map[string]string{"Cookie": sess["Cookie"], "Content-Type": "application/json", "Sec-Fetch-Site": "cross-site",
			"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden || len(d.sent) != 1 {
		t.Fatalf("cross-site post: %d sent=%v", rec.Code, d.sent)
	}
}

// TestDemoServer serves the web client with demo data for manual previews:
// WHATSUPD_DEMO_ADDR=127.0.0.1:8799 go test ./internal/web -run TestDemoServer -timeout 0
func TestDemoServer(t *testing.T) {
	addr := os.Getenv("WHATSUPD_DEMO_ADDR")
	if addr == "" {
		t.Skip("set WHATSUPD_DEMO_ADDR to run the demo server")
	}
	srv, _ := setup(t)
	srv.s.opt.About = about.New("dev", "demo data")
	t.Logf("demo at http://%s/login?code=%s (token %s)", addr, srv.NewLoginCode(), testToken)
	if err := http.ListenAndServe(addr, srv); err != nil {
		t.Fatal(err)
	}
}

func TestLocalBrowserGetsCookieButAPINeedsIt(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not available")
	}
	srv, _ := setup(t)
	srv.s.opt.TrustLocal = true
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// Host header must be a loopback name for the rebinding check.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/api/chats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API without cookie must be refused even from loopback, got %d", resp.StatusCode)
	}
	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("same-user browser opening the page should get a session cookie")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/chats", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("API with cookie: %d", resp.StatusCode)
	}
}

func TestProxiedRequestIsNotSignedInAutomatically(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not available")
	}
	srv, _ := setup(t)
	srv.s.opt.TrustLocal = true
	ts := httptest.NewServer(srv)
	defer ts.Close()
	for _, h := range []string{"X-Forwarded-For", "Cf-Connecting-Ip", "Forwarded"} {
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.Header.Set(h, "203.0.113.7")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == cookieName {
				t.Fatalf("request through a proxy (%s) got a session cookie", h)
			}
		}
	}
}

func TestPresenceFollowsVisibleTabs(t *testing.T) {
	srv, d := setup(t)
	d.presence = make(chan bool, 10)
	next := func() bool {
		t.Helper()
		select {
		case v := <-d.presence:
			return v
		case <-time.After(2 * time.Second):
			t.Fatal("no presence update")
		}
		return false
	}
	quiet := func() {
		t.Helper()
		select {
		case v := <-d.presence:
			t.Fatalf("unexpected presence update %v", v)
		case <-time.After(200 * time.Millisecond):
		}
	}
	srv.s.setTabPresence("a", true, false)
	if !next() {
		t.Fatal("first visible tab should go online")
	}
	srv.s.setTabPresence("b", true, false)
	srv.s.setTabPresence("a", false, true) // tab a closed while b is visible
	quiet()
	srv.s.setTabPresence("b", false, false)
	if next() {
		t.Fatal("no visible tab left: should go offline")
	}
}

func TestSessionHidesErrorsAfterPairing(t *testing.T) {
	srv, d := setup(t)
	d.lastError = "secret internal detail"
	rec := do(srv, "GET", "/api/session", "", nil)
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Fatalf("unauthenticated session leaked last_error: %s", rec.Body.String())
	}
}

func TestPresenceSurvivesStreamReconnect(t *testing.T) {
	srv, d := setup(t)
	d.presence = make(chan bool, 10)
	srv.s.tabConnected("a")
	srv.s.setTabPresence("a", true, false)
	if v := <-d.presence; !v {
		t.Fatal("expected online")
	}
	srv.s.tabDisconnected("a") // network blip
	srv.s.tabConnected("a")    // EventSource reconnects
	select {
	case v := <-d.presence:
		t.Fatalf("a reconnect must not change presence, got %v", v)
	case <-time.After(300 * time.Millisecond):
	}
}
