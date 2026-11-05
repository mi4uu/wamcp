package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mi4uu/whatsupd/internal/oauth"
)

func TestOAuthConsentLocksAfterWrongTokens(t *testing.T) {
	h, store := oauthServer(t)
	c, err := store.Register("x", []string{redirect})
	if err != nil {
		t.Fatal(err)
	}
	form := authorizeQuery(c.ID, pkce(strings.Repeat("a", 50)))
	form.Set("decision", "allow")
	form.Set("owner_token", "wrong")
	for i := range 10 {
		if rec := req(h, "POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d", i, rec.Code)
		}
	}
	form.Set("owner_token", testToken)
	if rec := req(h, "POST", "/oauth/authorize", form.Encode(), "application/x-www-form-urlencoded", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 10 wrong tokens the consent page must lock, got %d", rec.Code)
	}
}

func TestOAuthRejectsForeignResource(t *testing.T) {
	h, store := oauthServer(t)
	c, err := store.Register("x", []string{redirect})
	if err != nil {
		t.Fatal(err)
	}
	q := authorizeQuery(c.ID, pkce(strings.Repeat("a", 50)))
	q.Set("resource", "https://other.example/mcp")
	rec := req(h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || !strings.Contains(loc, "error=invalid_target") {
		t.Fatalf("foreign resource: %d %q", rec.Code, loc)
	}
	tok := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ID}, "refresh_token": {"x"}, "resource": {"https://other.example/mcp"}}
	if m := decode(t, req(h, "POST", "/oauth/token", tok.Encode(), "application/x-www-form-urlencoded", "")); m["error"] != "invalid_target" {
		t.Fatalf("token endpoint with foreign resource: %v", m)
	}
}

func TestOAuthFloodKeepsClientInConsent(t *testing.T) {
	h, store := oauthServer(t)
	real, err := store.Register("Claude", []string{redirect})
	if err != nil {
		t.Fatal(err)
	}
	// The owner opens the consent page, then an attacker floods registration.
	if rec := req(h, "GET", "/oauth/authorize?"+authorizeQuery(real.ID, pkce(strings.Repeat("a", 50))).Encode(), "", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("consent page: %d", rec.Code)
	}
	for range oauth.MaxClients * 2 {
		_, _ = store.Register("spam", []string{redirect})
	}
	found := false
	for _, c := range store.Clients() {
		found = found || c.ID == real.ID
	}
	if !found {
		t.Fatal("a client with an open consent page was evicted by a registration flood")
	}
}
