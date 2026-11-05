package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mi4uu/whatsupd/internal/outbox"
)

type fakeSender struct{ reqs []outbox.Request }

func (f *fakeSender) SendText(_ context.Context, req outbox.Request) (outbox.Result, error) {
	if req.Chat == "blocked@s.whatsapp.net" {
		return outbox.Result{}, outbox.ErrNotAllowed
	}
	f.reqs = append(f.reqs, req)
	return outbox.Result{Chat: req.Chat, MessageID: "ID1"}, nil
}

func (f *fakeSender) SendFile(ctx context.Context, req outbox.Request) (outbox.Result, error) {
	if req.File == nil {
		return outbox.Result{}, errors.New("no file")
	}
	return f.SendText(ctx, req)
}

func (f *fakeSender) Policy() string { return "recipients: me" }

// TestLiveSendFile checks that a file far over the usual 1 MB JSON limit
// reaches SendFile intact.
func TestLiveSendFile(t *testing.T) {
	base, d := setup(t)
	snd := &fakeSender{}
	srv := Handler(Options{Token: testToken, MCP: http.NotFoundHandler(), Archive: base.s.opt.Archive, Daemon: d, AgentSend: snd})
	data := bytes.Repeat([]byte{0xff, 0xd8, 0x01}, 3<<20)
	body, _ := json.Marshal(outbox.Request{Chat: "a@s.whatsapp.net", Text: "caption", File: &outbox.File{Data: data, Name: "x.jpg"}})
	rec := do(srv, "POST", "/api/live/send", string(body), map[string]string{"Authorization": "Bearer " + testToken})
	if rec.Code != http.StatusOK || len(snd.reqs) != 1 {
		t.Fatalf("send file: %d %s", rec.Code, rec.Body)
	}
	if got := snd.reqs[0].File; got == nil || !bytes.Equal(got.Data, data) || got.Name != "x.jpg" {
		t.Fatal("file did not arrive intact")
	}
}

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

func TestMCPBodyIsCapped(t *testing.T) {
	base, d := setup(t)
	reader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.Write([]byte("ok"))
	})
	srv := Handler(Options{Token: testToken, MCP: reader, Archive: base.s.opt.Archive, Daemon: d})
	auth := map[string]string{"Authorization": "Bearer " + testToken}
	if rec := do(srv, "POST", "/mcp", strings.Repeat("x", 1<<20), auth); rec.Code != http.StatusOK {
		t.Fatalf("1 MB body: %d", rec.Code)
	}
	if rec := do(srv, "POST", "/mcp", strings.Repeat("x", MaxMCPBody+1), auth); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over the cap: %d", rec.Code)
	}
}
