package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/demo"
)

func session(t *testing.T) *mcp.ClientSession {
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
	srv := New(store, nil, "test")
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		t.Fatalf("%s returned error: %s", name, b.String())
	}
	return b.String()
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestTools(t *testing.T) {
	cs := session(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 12 { // no live tools without a daemon
		t.Errorf("want 12 tools, got %d", len(tools.Tools))
	}

	mustContain(t, call(t, cs, "list_chats", nil), "Rodzina ❤️", "Projekt Łódź", "Anna Nowak", "priority high (4/5)")
	mustContain(t, call(t, cs, "list_chats", map[string]any{"min_priority": 5}), "Anna Nowak", "1 chats match")
	mustContain(t, call(t, cs, "get_messages", map[string]any{"chat": "rodzina"}),
		"Kasia Wiśniewska: Ja będę!", "↪ reply to Anna Nowak", "reactions: ❤️ Anna Nowak", "me: Będę z Piotrem")
	mustContain(t, call(t, cs, "get_messages", map[string]any{"chat": "+48 600 000 102"}), "[voice note 0:37", "⟨id:")
	mustContain(t, call(t, cs, "search_messages", map[string]any{"query": "lodzi"}), "Jedziemy w sobotę do Łodzi", "chat: 48600000101@s.whatsapp.net")
	mustContain(t, call(t, cs, "search_messages", map[string]any{"query": "oferta"}), "oferta_Q4.pdf")
	mustContain(t, call(t, cs, "get_recent_messages", map[string]any{"after": "6h"}), "## Projekt Łódź", "Spotkanie z klientem")
	mustContain(t, call(t, cs, "get_contact", map[string]any{"contact": "anna"}),
		"Anna Nowak (+48600000101)", "priority: top (5/5)", "urodziny: 14.03", "Shared groups (2)")
	mustContain(t, call(t, cs, "get_chat", map[string]any{"chat": "Projekt"}), "Members:", "Piotr Kowalski (+48600000102) [superadmin]", "description: Projekt dla klienta")
	mustContain(t, call(t, cs, "search_contacts", map[string]any{"tag": "rodzina"}), "Anna Nowak")
	mustContain(t, call(t, cs, "list_groups", nil), "2 groups match")
	mustContain(t, call(t, cs, "list_calls", nil), "incoming voice call with Piotr Kowalski", "missed")
	mustContain(t, call(t, cs, "whatsapp_status", nil), "Archive: 13 messages")

	out := call(t, cs, "update_contact_meta", map[string]any{"contact": "Kasia", "priority": "high", "add_tags": []string{"Rodzina"},
		"description": "Kuzynka", "fields": map[string]string{"miasto": "Łódź"}})
	mustContain(t, out, "priority: high (4/5)", "tags: rodzina", "miasto: Łódź")
	mustContain(t, call(t, cs, "search_contacts", map[string]any{"query": "kuzynka"}), "Kasia Wiśniewska")
	out = call(t, cs, "update_contact_meta", map[string]any{"contact": "Kasia", "tags": []string{}, "priority": "0"})
	if strings.Contains(out, "rodzina") || strings.Contains(out, "priority:") {
		t.Errorf("tags/priority not cleared:\n%s", out)
	}
}

func TestAmbiguousChat(t *testing.T) {
	cs := session(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_messages", Arguments: map[string]any{"chat": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected an ambiguity error")
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "call again with one of these JIDs") {
		t.Fatalf("unexpected error text: %s", text)
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 9, 26, 15, 30, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"2026-09-20":           time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
		"2026-09-20 08:15":     time.Date(2026, 9, 20, 8, 15, 0, 0, time.UTC),
		"2026-09-20t08:15:00z": time.Date(2026, 9, 20, 8, 15, 0, 0, time.UTC),
		"3h":                   now.Add(-3 * time.Hour),
		"2d":                   now.AddDate(0, 0, -2),
		"today":                time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseTime(in, now, time.UTC)
		if err != nil || got != want.Unix() {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, time.Unix(got, 0).UTC(), err, want)
		}
	}
	if _, err := parseTime("next tuesday", now, time.UTC); err == nil {
		t.Error("expected error")
	}
}

func TestUpdateMetaKeepsTagsWhenFieldNamedTags(t *testing.T) {
	cs := session(t)
	call(t, cs, "update_contact_meta", map[string]any{"contact": "Anna", "fields": map[string]string{"tags": "client"}})
	out := call(t, cs, "get_contact", map[string]any{"contact": "Anna"})
	mustContain(t, out, "tags: rodzina", "tags: client")
}

func TestSignedRelativeTimeRejected(t *testing.T) {
	if _, err := parseTime("-3h", time.Now(), time.UTC); err == nil {
		t.Fatal("-3h must be rejected")
	}
}

func TestRecentMessagesPriorityFilterBeforeLimit(t *testing.T) {
	cs := session(t)
	// Only Anna's chat (priority 5) qualifies; with limit 1 the newest message
	// overall (Kasia, lower priority) must not consume the budget.
	out := call(t, cs, "get_recent_messages", map[string]any{"after": "7d", "min_priority": 5, "limit": 1})
	mustContain(t, out, "1 messages in 1 chats", "Anna Nowak")
	if strings.Contains(out, "Kasia") {
		t.Fatalf("low-priority chat leaked into results:\n%s", out)
	}
}
