package wa

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/mi4uu/whatsupd/internal/archive"
)

var (
	anna = types.NewJID("48600000101", types.DefaultUserServer)
	t0   = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
)

// newTestManager builds a Manager on a fresh archive and device store,
// without connecting to WhatsApp.
func newTestManager(t *testing.T, cfg Config) (*Manager, *archive.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	arc, err := archive.Open(ctx, filepath.Join(dir, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { arc.Close() })
	cfg.DataDir = dir
	m, err := New(ctx, cfg, arc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.container.Close() })
	m.ctx = ctx
	return m, arc
}

func message(id string, body *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: anna, Sender: anna},
			ID:            types.MessageID(id), Timestamp: t0, PushName: "Anna",
		},
		Message: body,
	}
}

func text(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func nextEvent(t *testing.T, ch <-chan Event, typ string) Event {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Type == typ {
				return e
			}
		case <-timeout:
			t.Fatalf("no %s event", typ)
		}
	}
}

// TestMessageStoredBeforeAck: the handler's true is what lets whatsmeow
// acknowledge the message (SynchronousAck), so the message must already be
// in the archive when it returns.
func TestMessageStoredBeforeAck(t *testing.T) {
	m, arc := newTestManager(t, Config{})
	ch, unsub := m.Bus().Subscribe()
	defer unsub()
	if !m.handleEvent(message("M1", text("hello"))) {
		t.Fatal("stored message not acknowledged")
	}
	got, err := arc.Message(context.Background(), anna.String(), "M1")
	if err != nil || got.Text != "hello" {
		t.Fatalf("archive after ack: %+v %v", got, err)
	}
	if e := nextEvent(t, ch, "message"); e.Chat != anna.String() || e.ID != "M1" {
		t.Fatalf("event %+v", e)
	}
}

// TestMessageNotAckedWhenArchiveFails: a failed write returns false, so
// WhatsApp keeps the message and delivers it again.
func TestMessageNotAckedWhenArchiveFails(t *testing.T) {
	m, arc := newTestManager(t, Config{})
	arc.Close()
	if m.handleEvent(message("M1", text("hello"))) {
		t.Fatal("message acknowledged although the archive write failed")
	}
}

func TestRedeliveryStoresOnce(t *testing.T) {
	m, arc := newTestManager(t, Config{})
	m.catchingUp = true
	for _, id := range []string{"M1", "M1", "M2"} {
		if !m.handleEvent(message(id, text("hi "+id))) {
			t.Fatalf("%s not acknowledged", id)
		}
	}
	msgs, err := arc.Messages(context.Background(), archive.MessageQuery{ChatJID: anna.String(), Limit: 10})
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages %d %v", len(msgs), err)
	}
	if n := m.Status().CatchUp.Received; n != 2 {
		t.Fatalf("catch-up counted %d, want 2 (the redelivery is not new)", n)
	}
	if c, err := arc.Chat(context.Background(), anna.String()); err != nil || c.UnreadCount != 2 {
		t.Fatalf("unread after a redelivery: %+v %v", c, err)
	}
}

func TestAutoMediaQueue(t *testing.T) {
	m, _ := newTestManager(t, Config{AutoMedia: true, MediaMaxBytes: 1 << 20})
	photo := func(size uint64) *waE2E.Message {
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(size),
			DirectPath: proto.String("/v/t62/x"), MediaKey: []byte{1, 2, 3}, FileSHA256: []byte{4}, FileEncSHA256: []byte{5}}}
	}
	m.handleEvent(message("SMALL", photo(1000)))
	m.handleEvent(message("SMALL", photo(1000))) // redelivered: not queued twice
	m.handleEvent(message("BIG", photo(2<<20)))
	select {
	case got := <-m.mediaQueue:
		if got != [2]string{anna.String(), "SMALL"} {
			t.Fatalf("queued %v", got)
		}
	default:
		t.Fatal("small photo not queued for download")
	}
	select {
	case got := <-m.mediaQueue:
		t.Fatalf("over the size limit but queued: %v", got)
	default:
	}
}

func TestConnectionStateEvents(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	m.handleEvent(&events.StreamReplaced{})
	if st := m.Status(); st.State != StateReplaced || !strings.Contains(st.LastError, "another program") {
		t.Fatalf("replaced: %+v", st)
	}
	m.handleEvent(&events.LoggedOut{})
	if st := m.Status(); st.State != StateLoggedOut {
		t.Fatalf("logged out: %+v", st)
	}
	select {
	case reason := <-m.restart:
		if reason != "logout" {
			t.Fatalf("restart reason %q", reason)
		}
	default:
		t.Fatal("logout did not ask for a new pairing")
	}
	m.handleEvent(&events.Disconnected{})
	if st := m.Status(); st.State != StateDisconnected {
		t.Fatalf("disconnected: %+v", st)
	}
}

func TestChatStateEvents(t *testing.T) {
	m, arc := newTestManager(t, Config{})
	ctx := context.Background()
	m.handleEvent(message("M1", text("hi")))
	m.handleEvent(&events.Archive{JID: anna, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}})
	m.handleEvent(&events.Pin{JID: anna, Action: &waSyncAction.PinAction{Pinned: proto.Bool(true)}})
	until := t0.Add(8 * time.Hour)
	m.handleEvent(&events.Mute{JID: anna, Action: &waSyncAction.MuteAction{Muted: proto.Bool(true), MuteEndTimestamp: proto.Int64(until.UnixMilli())}})
	c, err := arc.Chat(ctx, anna.String())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Archived || !c.Pinned || c.MutedUntil == 0 {
		t.Fatalf("chat flags %+v", c)
	}
	m.handleEvent(&events.Mute{JID: anna, Action: &waSyncAction.MuteAction{Muted: proto.Bool(false)}})
	if c, _ = arc.Chat(ctx, anna.String()); c.MutedUntil != 0 {
		t.Fatalf("unmuted chat still muted until %d", c.MutedUntil)
	}
}

func TestTypingReachesTheBus(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	ch, unsub := m.Bus().Subscribe()
	defer unsub()
	m.handleEvent(&events.ChatPresence{MessageSource: types.MessageSource{Chat: anna, Sender: anna}, State: types.ChatPresenceComposing})
	e := nextEvent(t, ch, "typing")
	if data, _ := e.Data.(map[string]any); e.Chat != anna.String() || data["state"] != "composing" {
		t.Fatalf("typing event %+v", e)
	}
}

func TestSendNeedsConnection(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	ctx := context.Background()
	if m.NewMessageID() != "" {
		t.Fatal("message id without a connection")
	}
	if _, err := m.SendTextWithID(ctx, anna.String(), "  ", "", ""); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty text: %v", err)
	}
	if _, err := m.SendTextWithID(ctx, anna.String(), "hi", "", ""); err == nil {
		t.Fatal("sent without a connection")
	}
	if _, err := m.SendMediaWithID(ctx, anna.String(), MediaUpload{Data: []byte("x"), MimeType: "image/png"}, ""); err == nil {
		t.Fatal("sent a file without a connection")
	}
}

// TestMediaPathStaysInMediaDir: chat and message IDs come from the network
// and must not move a download outside the media directory.
func TestMediaPathStaysInMediaDir(t *testing.T) {
	m, _ := newTestManager(t, Config{})
	media := filepath.Join(m.cfg.DataDir, "media")
	for _, msg := range []archive.Message{
		{ChatJID: "..@s.whatsapp.net", ID: "../../x", FileName: "../../etc/passwd", Timestamp: t0.Unix()},
		{ChatJID: ".@g.us", ID: "..", MimeType: "image/jpeg", Timestamp: t0.Unix()},
		{ChatJID: "@s.whatsapp.net", ID: "/abs", FileName: "a/b\\c.pdf", Timestamp: t0.Unix()},
	} {
		p := m.mediaPath(&msg)
		rel, err := filepath.Rel(media, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "..") || strings.Count(rel, string(filepath.Separator)) != 2 {
			t.Errorf("%+v -> %s", msg, p)
		}
	}
}
