package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mi4uu/whatsupd/internal/archive"
	"github.com/mi4uu/whatsupd/internal/demo"
)

// fakeWA records deliveries instead of talking to WhatsApp.
type fakeWA struct {
	mu   sync.Mutex
	sent []string // "chat|id|text"
	fail error
	ids  int
}

func (f *fakeWA) send(_ context.Context, chat, text, _ string, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.sent = append(f.sent, chat+"|"+id+"|"+text)
	return id, nil
}

func (f *fakeWA) newID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids++
	return fmt.Sprintf("TESTID%04d", f.ids)
}

func (f *fakeWA) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func setup(t *testing.T, cfg Config) (*Outbox, *fakeWA, *archive.Store) {
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
	wa := &fakeWA{}
	o, err := New(store, cfg, wa.send, wa.newID, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return o, wa, store
}

func audit(t *testing.T, store *archive.Store) []archive.AgentSend {
	t.Helper()
	recs, err := store.AgentSends(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestSendAndAudit(t *testing.T) {
	o, wa, store := setup(t, Config{})
	res, err := o.SendText(context.Background(), Request{Chat: demo.Anna.String(), Text: " hello ", Client: "test 1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate || res.MessageID == "" || wa.count() != 1 {
		t.Fatalf("want one fresh send, got %+v, sent %v", res, wa.sent)
	}
	recs := audit(t, store)
	if len(recs) != 1 {
		t.Fatalf("want 1 audit record, got %d", len(recs))
	}
	a := recs[0]
	if a.Status != archive.SendSent || a.ChatJID != demo.Anna.String() || a.Text != "hello" ||
		a.MessageID != res.MessageID || a.Client != "test 1.0" {
		t.Fatalf("audit record: %+v", a)
	}
}

func TestAllowlistRefusal(t *testing.T) {
	o, wa, store := setup(t, Config{Allow: []string{"+48 600 000 101", "me"}})
	ctx := context.Background()
	_, err := o.SendText(ctx, Request{Chat: demo.Piotr.String(), Text: "hi"})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("want ErrNotAllowed, got %v", err)
	}
	if wa.count() != 0 {
		t.Fatal("a refused message was sent")
	}
	if recs := audit(t, store); len(recs) != 1 || recs[0].Status != archive.SendRefused {
		t.Fatalf("refusal not audited: %+v", recs)
	}
	for _, chat := range []string{demo.Anna.String(), demo.Me.String()} {
		if _, err := o.SendText(ctx, Request{Chat: chat, Text: "hi"}); err != nil {
			t.Fatalf("%s is allowed: %v", chat, err)
		}
	}
	if _, err := o.SendText(ctx, Request{Chat: demo.Family.String(), Text: "hi"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("group not on the list: got %v", err)
	}
}

func TestInvalidAllowlist(t *testing.T) {
	if _, err := New(nil, Config{Allow: []string{"Anna Nowak"}}, nil, nil, nil); err == nil {
		t.Fatal("names must not be accepted in the allowlist")
	}
}

func TestRetryWithKeyDoesNotDoubleSend(t *testing.T) {
	o, wa, store := setup(t, Config{})
	ctx := context.Background()
	req := Request{Chat: demo.Family.String(), Text: "Obiad o 14", IdempotencyKey: "summary-2026-11-05"}
	first, err := o.SendText(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := o.SendText(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if wa.count() != 1 || !second.Duplicate || second.MessageID != first.MessageID {
		t.Fatalf("retry sent again: sent=%v second=%+v", wa.sent, second)
	}
	if recs := audit(t, store); len(recs) != 2 || recs[0].Status != archive.SendDuplicate || recs[1].Status != archive.SendSent {
		t.Fatalf("want sent then duplicate in the audit, got %+v", recs)
	}
	req.Text = "something else"
	if _, err := o.SendText(ctx, req); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("same key, different text: want ErrKeyReused, got %v", err)
	}
}

func TestRetryWithoutKeyDoesNotDoubleSend(t *testing.T) {
	o, wa, _ := setup(t, Config{})
	ctx := context.Background()
	req := Request{Chat: demo.Anna.String(), Text: "ok"}
	if _, err := o.SendText(ctx, req); err != nil {
		t.Fatal(err)
	}
	if res, err := o.SendText(ctx, req); err != nil || !res.Duplicate {
		t.Fatalf("identical text within the window: %+v %v", res, err)
	}
	o.now = func() time.Time { return time.Now().Add(DefaultRetryWindow + time.Minute) }
	if res, err := o.SendText(ctx, req); err != nil || res.Duplicate {
		t.Fatalf("after the window the same text is a new message: %+v %v", res, err)
	}
	if wa.count() != 2 {
		t.Fatalf("want 2 sends, got %v", wa.sent)
	}
}

func TestConcurrentRetriesSendOnce(t *testing.T) {
	o, wa, _ := setup(t, Config{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := o.SendText(context.Background(), Request{Chat: demo.Anna.String(), Text: "once", IdempotencyKey: "k1"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wa.count() != 1 {
		t.Fatalf("want exactly one send, got %v", wa.sent)
	}
}

func TestFailedSendRetryReusesID(t *testing.T) {
	o, wa, _ := setup(t, Config{})
	ctx := context.Background()
	req := Request{Chat: demo.Anna.String(), Text: "retry me", IdempotencyKey: "k2"}
	wa.fail = errors.New("timeout")
	if _, err := o.SendText(ctx, req); err == nil {
		t.Fatal("want the send error")
	}
	wa.fail = nil
	res, err := o.SendText(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.MessageID != "TESTID0001" || wa.ids != 1 {
		t.Fatalf("retry after failure must reuse the message id, got %s (ids generated %d)", res.MessageID, wa.ids)
	}
}

func TestInterruptedSendIsNotRepeated(t *testing.T) {
	o, wa, store := setup(t, Config{})
	ctx := context.Background()
	req := Request{Chat: demo.Anna.String(), Text: "crash", IdempotencyKey: "k3"}
	// A pending record without an outcome: the daemon stopped mid-send.
	if _, err := store.AddAgentSend(ctx, archive.AgentSend{CreatedAt: time.Now(), DedupeKey: dedupeKey(req),
		ChatJID: req.Chat, Text: req.Text, Status: archive.SendPending, MessageID: "LOSTID"}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SendText(ctx, req); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("want ErrUnknownOutcome, got %v", err)
	}
	if wa.count() != 0 {
		t.Fatal("an interrupted send was repeated")
	}
}

func TestRateLimit(t *testing.T) {
	o, wa, store := setup(t, Config{PerHour: 2})
	ctx := context.Background()
	for i := range 2 {
		if _, err := o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: fmt.Sprint("m", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: "m3"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if wa.count() != 2 {
		t.Fatalf("want 2 sends, got %d", wa.count())
	}
	if recs := audit(t, store); recs[0].Status != archive.SendRefused {
		t.Fatalf("rate limit refusal not audited: %+v", recs[0])
	}
	o.now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	if _, err := o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: "m4"}); err != nil {
		t.Fatalf("an hour later: %v", err)
	}
}

func TestDisconnectedIsAudited(t *testing.T) {
	o, wa, store := setup(t, Config{})
	o.newID = func() string { return "" }
	if _, err := o.SendText(context.Background(), Request{Chat: demo.Anna.String(), Text: "hi"}); err == nil {
		t.Fatal("want an error while disconnected")
	}
	if recs := audit(t, store); len(recs) != 1 || recs[0].Status != archive.SendRefused || wa.count() != 0 {
		t.Fatalf("disconnected attempt not audited: %+v", recs)
	}
}

func TestFailedSendsCountTowardsLimit(t *testing.T) {
	o, wa, _ := setup(t, Config{PerHour: 1})
	ctx := context.Background()
	wa.fail = errors.New("ack timeout")
	if _, err := o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: "maybe delivered"}); err == nil {
		t.Fatal("want the send error")
	}
	wa.fail = nil
	if _, err := o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: "another"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a failed send must use up the budget: got %v", err)
	}
}

func TestEveryCallIsAudited(t *testing.T) {
	o, _, store := setup(t, Config{})
	ctx := context.Background()
	req := Request{Chat: demo.Anna.String(), Text: "once", IdempotencyKey: "k"}
	if _, err := o.SendText(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SendText(ctx, req); err != nil {
		t.Fatal(err)
	}
	_, _ = o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: " "})
	_, _ = o.SendText(ctx, Request{Text: "no chat"})
	recs := audit(t, store)
	got := []string{}
	for _, a := range recs {
		got = append(got, a.Status)
	}
	if fmt.Sprint(got) != "[refused refused duplicate sent]" {
		t.Fatalf("audit statuses (newest first): %v", got)
	}
}

func TestRefusalNotesAreCapped(t *testing.T) {
	o, _, store := setup(t, Config{Allow: []string{"me"}})
	ctx := context.Background()
	for i := range maxNotesPerHour + 50 {
		_, _ = o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: fmt.Sprint("spam ", i)})
	}
	recs, err := store.AgentSends(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != maxNotesPerHour {
		t.Fatalf("want %d refusal records, got %d", maxNotesPerHour, len(recs))
	}
}

func TestRejectsEmptyAndLongText(t *testing.T) {
	o, wa, _ := setup(t, Config{})
	ctx := context.Background()
	if _, err := o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: "   "}); err == nil {
		t.Fatal("empty text accepted")
	}
	long := make([]rune, MaxTextLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := o.SendText(ctx, Request{Chat: demo.Anna.String(), Text: string(long)}); err == nil {
		t.Fatal("over-long text accepted")
	}
	if wa.count() != 0 {
		t.Fatal("invalid text was sent")
	}
}
