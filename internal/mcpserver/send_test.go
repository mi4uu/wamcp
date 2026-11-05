package mcpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/demo"
	"github.com/mi4uu/wamcp/internal/outbox"
)

func seeded(t *testing.T) *archive.Store {
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
	return store
}

func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "9"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func TestSendToolHiddenByDefault(t *testing.T) {
	cs := connect(t, New(seeded(t), nil, "test"))
	if slices.Contains(toolNames(t, cs), "send_message") {
		t.Fatal("send_message is listed without the switch")
	}
	if ins := cs.InitializeResult().Instructions; !strings.HasPrefix(ins, "Read-only access") {
		t.Errorf("default instructions must say read-only: %q", ins)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message",
		Arguments: map[string]any{"chat": "anna", "text": "hi"}})
	if err == nil && !res.IsError {
		t.Fatal("calling an unlisted send_message succeeded")
	}
}

type countingWA struct {
	mu   sync.Mutex
	sent int
	n    int
}

func (c *countingWA) send(_ context.Context, _, _, _, id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	return id, nil
}

func (c *countingWA) id() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return fmt.Sprintf("ID%d", c.n)
}

func TestSendMessageWithSwitch(t *testing.T) {
	ctx := context.Background()
	store := seeded(t)
	wa := &countingWA{}
	ob, err := outbox.New(store, outbox.Config{Allow: []string{demo.Family.String()}}, wa.send, wa.id,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, New(store, nil, "test", WithSender(ob)))

	if !slices.Contains(toolNames(t, cs), "send_message") {
		t.Fatal("send_message not listed with the switch")
	}
	mustContain(t, cs.InitializeResult().Instructions, "Sending is enabled", "untrusted")

	args := map[string]any{"chat": "Rodzina", "text": "Podsumowanie dnia", "idempotency_key": "daily-1"}
	mustContain(t, call(t, cs, "send_message", args), "Sent to Rodzina ❤️", demo.Family.String())
	mustContain(t, call(t, cs, "send_message", args), "Already sent", "not sent again")
	if wa.sent != 1 {
		t.Fatalf("retry through MCP sent %d times", wa.sent)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"chat": "Anna Nowak", "text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "allowlist") {
		t.Fatalf("recipient off the allowlist was not refused: %+v", res.Content)
	}
	if wa.sent != 1 {
		t.Fatal("refused message was sent")
	}
	recs, err := store.AgentSends(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].Status != archive.SendRefused || recs[1].Status != archive.SendDuplicate ||
		recs[2].Status != archive.SendSent || recs[2].Client != "test-client 9" {
		t.Fatalf("audit log: %+v", recs)
	}
}
