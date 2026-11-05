package main

import (
	"testing"

	"github.com/mi4uu/wamcp/internal/config"
)

func TestAgentTokenPrefersMCPToken(t *testing.T) {
	p := config.Paths{Dir: t.TempDir()}
	full, err := p.LoadOrCreateToken()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := agentToken(p); got != full {
		t.Fatal("without an MCP token the stdio server must fall back to the full token")
	}
	mcpTok, err := p.LoadOrCreateMCPToken()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := agentToken(p); got != mcpTok {
		t.Fatal("stdio server must use the MCP token when it exists")
	}
}
