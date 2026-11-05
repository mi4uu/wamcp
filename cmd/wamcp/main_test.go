package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/demo"
	"github.com/mi4uu/wamcp/internal/outbox"
	"github.com/mi4uu/wamcp/internal/web"
)

func TestSendNeedsAllowlistOffLoopback(t *testing.T) {
	f := sendFlags{allow: true}
	if _, err := f.outbox("0.0.0.0:8765", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("non-loopback without allowlist: want refusal, got %v", err)
	}
	f.to = "+48 600 000 101"
	if ob, err := f.outbox("0.0.0.0:8765", nil, nil, nil); err != nil || ob == nil {
		t.Fatalf("non-loopback with allowlist: %v", err)
	}
	if ob, err := (&sendFlags{}).outbox("127.0.0.1:8765", nil, nil, nil); err != nil || ob != nil {
		t.Fatalf("switch off must give no outbox, got %v %v", ob, err)
	}
	if _, err := (&sendFlags{allow: true, to: "Anna"}).outbox("127.0.0.1:8765", nil, nil, nil); err == nil {
		t.Fatal("names in the allowlist must be rejected")
	}
}

func TestServiceArgsKeepSendFlags(t *testing.T) {
	if a := (&sendFlags{}).args(); a != nil {
		t.Fatalf("switch off must add no flags, got %v", a)
	}
	a := strings.Join((&sendFlags{allow: true, to: "me", perHour: 5}).args(), " ")
	if a != "--mcp-allow-send --mcp-send-per-hour 5 --mcp-send-to me" {
		t.Fatalf("service args: %q", a)
	}
}

// The stdio server forwards send_message to the daemon, which applies the policy.
func TestRemoteSenderUsesDaemonPolicy(t *testing.T) {
	ctx := context.Background()
	store, err := archive.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := demo.Seed(ctx, store, time.Now()); err != nil {
		t.Fatal(err)
	}
	sent := 0
	ob, err := outbox.New(store, outbox.Config{Allow: []string{"me"}},
		func(_ context.Context, _, _, _, id string) (string, error) { sent++; return id, nil },
		func() string { return "REMOTE1" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := web.Handler(web.Options{Token: "tok", MCP: http.NotFoundHandler(), Archive: store, AgentSend: ob})
	ts := httptest.NewServer(h)
	defer ts.Close()
	rs := &remoteSender{remoteLive: &remoteLive{base: ts.URL, token: "tok", arc: store, http: ts.Client()}}

	res, err := rs.SendText(ctx, outbox.Request{Chat: demo.Me.String(), Text: "note to self", IdempotencyKey: "x"})
	if err != nil || res.MessageID != "REMOTE1" {
		t.Fatalf("send via daemon: %+v %v", res, err)
	}
	if res, err = rs.SendText(ctx, outbox.Request{Chat: demo.Me.String(), Text: "note to self", IdempotencyKey: "x"}); err != nil || !res.Duplicate {
		t.Fatalf("retry via daemon: %+v %v", res, err)
	}
	if _, err := rs.SendText(ctx, outbox.Request{Chat: demo.Anna.String(), Text: "hi"}); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("daemon allowlist not applied to stdio: %v", err)
	}
	if sent != 1 {
		t.Fatalf("want 1 delivery, got %d", sent)
	}
}
