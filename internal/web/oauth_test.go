package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mi4uu/whatsupd/internal/oauth"
)

func oauthServer(t *testing.T) (http.Handler, *oauth.Server) {
	t.Helper()
	base, d := setup(t)
	store, err := oauth.Open(filepath.Join(t.TempDir(), "oauth.json"))
	if err != nil {
		t.Fatal(err)
	}
	mcpStub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("mcp-ok")) })
	h := Handler(Options{Token: testToken, MCP: mcpStub, Archive: base.s.opt.Archive, Daemon: d,
		PublicHosts: []string{"wa.example.com"},
		OAuth:       &oauth.Handler{S: store, OwnerToken: testToken, Access: "read your chats"}})
	return h, store
}

// req sends a request as a remote client would, through the public host.
func req(h http.Handler, method, path, body, contentType, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://wa.example.com"+path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	return m
}

func pkce(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

const redirect = "https://claude.ai/api/mcp/auth_callback"

func authorizeQuery(clientID, challenge string) url.Values {
	return url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"state": {"st-1"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {"https://wa.example.com/mcp"}}
}

func TestOAuthFlow(t *testing.T) {
	h, _ := oauthServer(t)

	// Discovery starts from a 401 on /mcp.
	rec := req(h, "POST", "/mcp", "{}", "", "")
	if rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Header().Get("WWW-Authenticate"), `resource_metadata="https://wa.example.com/.well-known/oauth-protected-resource"`) {
		t.Fatalf("401 challenge: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	prm := decode(t, req(h, "GET", "/.well-known/oauth-protected-resource", "", "", ""))
	if prm["resource"] != "https://wa.example.com/mcp" {
		t.Fatalf("protected resource metadata: %v", prm)
	}
	asm := decode(t, req(h, "GET", "/.well-known/oauth-authorization-server", "", "", ""))
	if asm["issuer"] != "https://wa.example.com" || asm["token_endpoint"] != "https://wa.example.com/oauth/token" {
		t.Fatalf("server metadata: %v", asm)
	}

	reg := decode(t, req(h, "POST", "/oauth/register", `{"client_name":"Claude","redirect_uris":["`+redirect+`"]}`, "application/json", ""))
	clientID, _ := reg["client_id"].(string)
	if clientID == "" {
		t.Fatalf("registration: %v", reg)
	}

	verifier := strings.Repeat("v", 50)
	q := authorizeQuery(clientID, pkce(verifier))
	rec = req(h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Allow Claude") {
		t.Fatalf("consent page: %d %s", rec.Code, rec.Body)
	}

	form := url.Values{}
	for k, v := range q {
		form[k] = v
	}
	form.Set("decision", "allow")
	form.Set("owner_token", "wrong")
	if rec = req(h, "POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong owner token: %d", rec.Code)
	}
	form.Set("owner_token", testToken)
	rec = req(h, "POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("approval: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")
	if !strings.HasPrefix(loc.String(), redirect) || code == "" || loc.Query().Get("state") != "st-1" {
		t.Fatalf("redirect: %s", loc)
	}

	exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code},
		"redirect_uri": {redirect}, "code_verifier": {verifier}}
	tok := decode(t, req(h, "POST", "/oauth/token", exchange.Encode(), "application/x-www-form-urlencoded", ""))
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("token response: %v", tok)
	}
	if rec = req(h, "POST", "/oauth/token", exchange.Encode(), "application/x-www-form-urlencoded", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("a code must work once: %d", rec.Code)
	}

	if rec = req(h, "POST", "/mcp", "{}", "", access); rec.Code != http.StatusOK || rec.Body.String() != "mcp-ok" {
		t.Fatalf("/mcp with OAuth token: %d", rec.Code)
	}
	if rec = req(h, "GET", "/api/chats", "", "", access); rec.Code != http.StatusUnauthorized {
		t.Fatalf("OAuth token must not open the web API: %d", rec.Code)
	}
	if rec = req(h, "POST", "/api/login-code", "", "", access); rec.Code != http.StatusUnauthorized {
		t.Fatalf("OAuth token must not mint login codes: %d", rec.Code)
	}

	ref := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}}
	tok2 := decode(t, req(h, "POST", "/oauth/token", ref.Encode(), "application/x-www-form-urlencoded", ""))
	if tok2["access_token"] == nil || tok2["refresh_token"] == refresh {
		t.Fatalf("refresh must rotate: %v", tok2)
	}
	if rec = req(h, "POST", "/oauth/token", ref.Encode(), "application/x-www-form-urlencoded", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("an old refresh token must not work again: %d", rec.Code)
	}

	if rec = req(h, "POST", "/api/oauth/revoke", `{"id":"`+clientID+`"}`, "application/json", testToken); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec = req(h, "POST", "/mcp", "{}", "", tok2["access_token"].(string)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token of a revoked client still works: %d", rec.Code)
	}
}

func TestOAuthRejectsBadRequests(t *testing.T) {
	h, store := oauthServer(t)
	if rec := req(h, "POST", "/oauth/register", `{"redirect_uris":["http://evil.example/cb"]}`, "application/json", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("plain http redirect off loopback must be refused: %d", rec.Code)
	}
	c, err := store.Register("x", []string{redirect})
	if err != nil {
		t.Fatal(err)
	}
	q := authorizeQuery(c.ID, pkce(strings.Repeat("a", 50)))
	q.Set("redirect_uri", "https://evil.example/cb")
	if rec := req(h, "GET", "/oauth/authorize?"+q.Encode(), "", "", ""); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("unregistered redirect must not redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	q = authorizeQuery(c.ID, "")
	q.Del("code_challenge_method")
	rec := req(h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || !strings.Contains(loc, "error=invalid_request") {
		t.Fatalf("missing PKCE: %d %q", rec.Code, loc)
	}

	// A code with the wrong verifier is useless.
	q = authorizeQuery(c.ID, pkce(strings.Repeat("r", 50)))
	areq, err := store.CheckAuthorize(q)
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.Approve(areq)
	if err != nil {
		t.Fatal(err)
	}
	bad := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.ID}, "code": {code}, "redirect_uri": {redirect},
		"code_verifier": {strings.Repeat("x", 50)}}
	if rec := req(h, "POST", "/oauth/token", bad.Encode(), "application/x-www-form-urlencoded", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong PKCE verifier: %d", rec.Code)
	}
}

func TestOAuthOffByDefault(t *testing.T) {
	srv, _ := setup(t)
	if rec := do(srv, "GET", "/.well-known/oauth-authorization-server", "", nil); rec.Code == http.StatusOK {
		t.Fatal("OAuth metadata served without --oauth")
	}
	if rec := do(srv, "POST", "/mcp", "{}", nil); strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatal("401 advertises OAuth without --oauth")
	}
}

func TestOAuthRegistrationCannotLockOut(t *testing.T) {
	_, store := oauthServer(t)
	for range oauth.MaxClients + 5 {
		if _, err := store.Register("spam", []string{redirect}); err != nil {
			t.Fatalf("unapproved clients must be evicted, not block registration: %v", err)
		}
	}
	if n := len(store.Clients()); n != oauth.MaxClients {
		t.Fatalf("want %d clients, got %d", oauth.MaxClients, n)
	}
}
