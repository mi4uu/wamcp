package config

import (
	"os"
	"testing"
)

func TestMCPTokenIsSeparateAndRotates(t *testing.T) {
	p := Paths{Dir: t.TempDir()}
	full, err := p.LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := p.LoadOrCreateMCPToken()
	if err != nil {
		t.Fatal(err)
	}
	if mcp == full || len(mcp) != 64 {
		t.Fatalf("MCP token must be a separate 32-byte token: %q vs %q", mcp, full)
	}
	if again, _ := p.LoadOrCreateMCPToken(); again != mcp {
		t.Fatal("MCP token changed without rotation")
	}
	rotated, err := p.RotateMCPToken()
	if err != nil {
		t.Fatal(err)
	}
	if rotated == mcp {
		t.Fatal("rotation kept the old token")
	}
	if got, _ := p.ReadMCPToken(); got != rotated {
		t.Fatalf("read %q after rotation, want %q", got, rotated)
	}
	if got, _ := p.ReadToken(); got != full {
		t.Fatal("rotating the MCP token changed the full-access token")
	}
	fi, err := os.Stat(p.MCPToken())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mcp-token mode %v, want 0600", fi.Mode().Perm())
	}
}
