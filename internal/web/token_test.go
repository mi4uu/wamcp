package web

import (
	"net/http"
	"testing"
)

func TestMCPTokenScope(t *testing.T) {
	base, d := setup(t)
	mcpStub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("mcp-ok")) })
	srv := Handler(Options{Token: testToken, MCPToken: "mcp-only", MCP: mcpStub, Archive: base.s.opt.Archive, Daemon: d})
	agent := map[string]string{"Authorization": "Bearer mcp-only"}
	if rec := do(srv, "POST", "/mcp", "{}", agent); rec.Code != http.StatusOK {
		t.Fatalf("MCP token on /mcp: got %d", rec.Code)
	}
	if rec := do(srv, "GET", "/api/live/status", "", agent); rec.Code != http.StatusOK {
		t.Fatalf("MCP token on the live API: got %d", rec.Code)
	}
	if rec := do(srv, "POST", "/api/login-code", "", agent); rec.Code != http.StatusUnauthorized {
		t.Fatalf("MCP token must not mint web login codes: got %d", rec.Code)
	}
	if rec := do(srv, "POST", "/api/login", `{"token":"mcp-only"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("MCP token must not sign in to the web client: got %d", rec.Code)
	}
	if rec := do(srv, "GET", "/api/chats", "", agent); rec.Code != http.StatusUnauthorized {
		t.Fatalf("MCP token must not open the web API: got %d", rec.Code)
	}
	if rec := do(srv, "POST", "/mcp", "{}", map[string]string{"Authorization": "Bearer " + testToken}); rec.Code != http.StatusOK {
		t.Fatalf("full token on /mcp still works: got %d", rec.Code)
	}
}
