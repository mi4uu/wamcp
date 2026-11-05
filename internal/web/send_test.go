package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mi4uu/wamcp/internal/outbox"
)

type fakeSender struct{ reqs []outbox.Request }

func (f *fakeSender) SendText(_ context.Context, req outbox.Request) (outbox.Result, error) {
	if req.Chat == "blocked@s.whatsapp.net" {
		return outbox.Result{}, outbox.ErrNotAllowed
	}
	f.reqs = append(f.reqs, req)
	return outbox.Result{Chat: req.Chat, MessageID: "ID1"}, nil
}

func (f *fakeSender) Policy() string { return "recipients: me" }

func TestLiveSendDisabledByDefault(t *testing.T) {
	srv, _ := setup(t)
	auth := map[string]string{"Authorization": "Bearer " + testToken}
	if rec := do(srv, "POST", "/api/live/send", `{"chat":"a@s.whatsapp.net","text":"hi"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", rec.Code)
	}
	if rec := do(srv, "POST", "/api/live/send", `{"chat":"a@s.whatsapp.net","text":"hi"}`, auth); rec.Code != http.StatusForbidden {
		t.Fatalf("send without --mcp-allow-send: got %d", rec.Code)
	}
	if rec := do(srv, "GET", "/api/live/send", "", auth); rec.Code != http.StatusForbidden {
		t.Fatalf("policy without --mcp-allow-send: got %d", rec.Code)
	}
}

func TestLiveSendWithSwitch(t *testing.T) {
	base, d := setup(t)
	snd := &fakeSender{}
	srv := Handler(Options{Token: testToken, MCP: http.NotFoundHandler(), Archive: base.s.opt.Archive, Daemon: d,
		AgentSend: snd})
	auth := map[string]string{"Authorization": "Bearer " + testToken}

	rec := do(srv, "GET", "/api/live/send", "", auth)
	if rec.Code != http.StatusOK || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body)
	}
	rec = do(srv, "POST", "/api/live/send", `{"Chat":"a@s.whatsapp.net","Text":"hi","IdempotencyKey":"k"}`, auth)
	var res outbox.Result
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil || res.MessageID != "ID1" {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	if len(snd.reqs) != 1 || snd.reqs[0].IdempotencyKey != "k" || !strings.HasPrefix(snd.reqs[0].Client, "stdio via daemon API") {
		t.Fatalf("request not passed through: %+v", snd.reqs)
	}
	if rec := do(srv, "POST", "/api/live/send", `{"Chat":"blocked@s.whatsapp.net","Text":"hi"}`, auth); rec.Code != http.StatusConflict {
		t.Fatalf("refusal: got %d", rec.Code)
	}
	if d.sent != nil {
		t.Fatalf("live send must go through the policy, not the daemon directly: %v", d.sent)
	}
}
