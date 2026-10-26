package ingest

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/mi4uu/wamcp/internal/archive"
)

var (
	me      = types.NewJID("48500000001", types.DefaultUserServer)
	meLID   = types.NewJID("900000000001", types.HiddenUserServer)
	anna    = types.NewJID("48600000002", types.DefaultUserServer)
	annaLID = types.NewJID("900000000002", types.HiddenUserServer)
	bob     = types.NewJID("48700000003", types.DefaultUserServer)
	family  = types.NewJID("120363000000000001", types.GroupServer)
	t0      = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
)

func setup(t *testing.T) (*Ingestor, *archive.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := archive.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	in := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := in.SetSelf(ctx, me, meLID); err != nil {
		t.Fatal(err)
	}
	return in, store
}

func msgEvent(chat, sender types.JID, id string, at time.Time, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: sender == me, IsGroup: chat.Server == types.GroupServer},
			ID:            id, Timestamp: at, PushName: "Anna K",
		},
		Message: m,
	}
}

func text(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func mustMessages(t *testing.T, s *archive.Store, chat string) []archive.Message {
	t.Helper()
	msgs, err := s.Messages(context.Background(), archive.MessageQuery{ChatJID: chat, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestTextImageAndReply(t *testing.T) {
	ctx := context.Background()
	in, s := setup(t)
	if _, err := in.Message(ctx, msgEvent(anna, anna, "A1", t0, text("Cześć, jedziemy do Łodzi?")), "live"); err != nil {
		t.Fatal(err)
	}
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("widok"), Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(2048),
		JPEGThumbnail: make([]byte, 5000), MediaKey: []byte{1, 2, 3}, DirectPath: proto.String("/v/t62/x"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("A1"), Participant: proto.String(anna.String()),
			QuotedMessage: text("Cześć, jedziemy do Łodzi?")},
	}}
	res, err := in.Message(ctx, msgEvent(anna, me, "A2", t0.Add(time.Minute), img), "live")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.HasMedia || res.FileSize != 2048 {
		t.Fatalf("unexpected result %+v", res)
	}
	msgs := mustMessages(t, s, anna.String())
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	got := msgs[1]
	if got.Kind != "image" || got.Text != "widok" || !got.FromMe || got.SenderJID != me.String() {
		t.Fatalf("bad image row: %+v", got)
	}
	if got.QuotedID != "A1" || !strings.Contains(got.QuotedText, "Łodzi") {
		t.Fatalf("bad quote: %+v", got)
	}
	full, err := s.Message(ctx, anna.String(), "A2")
	if err != nil {
		t.Fatal(err)
	}
	var raw waE2E.Message
	if err := proto.Unmarshal(full.Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.GetImageMessage().GetJPEGThumbnail()) != 0 || raw.GetImageMessage().GetContextInfo().GetQuotedMessage() != nil {
		t.Fatal("thumbnail/quote not stripped from raw payload")
	}
	if string(raw.GetImageMessage().GetMediaKey()) != string([]byte{1, 2, 3}) {
		t.Fatal("media key lost")
	}
	// accent-insensitive search: "lodzi" finds "Łodzi"
	hits, err := s.Search(ctx, archive.SearchQuery{Text: "lodzi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "A1" {
		t.Fatalf("search: %+v", hits)
	}
}

func TestEditRevokeReaction(t *testing.T) {
	ctx := context.Background()
	in, s := setup(t)
	in.Message(ctx, msgEvent(family, anna, "G1", t0, text("spotkanie o 10")), "live")
	edit := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String("G1")},
		EditedMessage: text("spotkanie o 11"),
	}}
	in.Message(ctx, msgEvent(family, anna, "G1-edit", t0.Add(time.Minute), edit), "live")
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("G1")}, Text: proto.String("👍"), SenderTimestampMS: proto.Int64(t0.Add(2 * time.Minute).UnixMilli()),
	}}
	in.Message(ctx, msgEvent(family, bob, "R1", t0.Add(2*time.Minute), react), "live")

	msgs := mustMessages(t, s, family.String())
	if len(msgs) != 1 || msgs[0].Text != "spotkanie o 11" || msgs[0].EditedAt == 0 {
		t.Fatalf("edit not applied: %+v", msgs)
	}
	// A later history copy of the original must not undo the edit.
	in.Message(ctx, msgEvent(family, anna, "G1", t0, text("spotkanie o 10")), "history")
	if msgs = mustMessages(t, s, family.String()); msgs[0].Text != "spotkanie o 11" {
		t.Fatalf("history overwrote edit: %q", msgs[0].Text)
	}
	rs, _ := s.Reactions(ctx, family.String(), []string{"G1"})
	if len(rs["G1"]) != 1 || rs["G1"][0].Emoji != "👍" || rs["G1"][0].SenderJID != bob.String() {
		t.Fatalf("reaction: %+v", rs)
	}
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("G1"), Participant: proto.String(anna.String())},
	}}
	in.Message(ctx, msgEvent(family, anna, "G1-rev", t0.Add(3*time.Minute), revoke), "live")
	msgs = mustMessages(t, s, family.String())
	if len(msgs) != 1 || !msgs[0].Deleted() || msgs[0].Text != "" {
		t.Fatalf("revoke not applied: %+v", msgs)
	}
	if hits, _ := s.Search(ctx, archive.SearchQuery{Text: "spotkanie"}); len(hits) != 0 {
		t.Fatal("deleted message still searchable")
	}
}

func TestLIDCanonicalization(t *testing.T) {
	ctx := context.Background()
	in, s := setup(t)
	// Group message whose sender is a LID; WhatsApp sends the phone number as SenderAlt.
	evt := msgEvent(family, annaLID, "L1", t0, text("hej"))
	evt.Info.SenderAlt = anna
	in.Message(ctx, evt, "live")
	// Later message with only the LID resolves through the learned mapping.
	in.Message(ctx, msgEvent(family, annaLID, "L2", t0.Add(time.Minute), text("drugi")), "live")
	for _, m := range mustMessages(t, s, family.String()) {
		if m.SenderJID != anna.String() {
			t.Fatalf("sender not canonicalized: %s", m.SenderJID)
		}
	}
	// A DM stored under an unknown LID gets merged once the mapping is learned.
	stranger := types.NewJID("900000000009", types.HiddenUserServer)
	strangerPN := types.NewJID("48900000009", types.DefaultUserServer)
	in.Message(ctx, msgEvent(stranger, stranger, "D1", t0, text("kim jestem?")), "live")
	if n := len(mustMessages(t, s, stranger.String())); n != 1 {
		t.Fatalf("want LID chat, got %d", n)
	}
	in.SetResolver(fakeResolver{stranger.String(): strangerPN})
	merged, err := in.Reconcile(ctx)
	if err != nil || merged == 0 {
		t.Fatalf("reconcile: merged=%d err=%v", merged, err)
	}
	if n := len(mustMessages(t, s, strangerPN.String())); n != 1 {
		t.Fatalf("message not moved to phone chat (got %d)", n)
	}
	if _, err := s.Chat(ctx, stranger.String()); err != archive.ErrNotFound {
		t.Fatal("LID chat still present")
	}
}

type fakeResolver map[string]types.JID

func (f fakeResolver) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f[lid.String()], nil
}

func TestHistorySync(t *testing.T) {
	ctx := context.Background()
	in, s := setup(t)
	parse := func(chat types.JID, wm *waWeb.WebMessageInfo) (*events.Message, error) {
		info := types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, IsFromMe: wm.GetKey().GetFromMe()},
			ID: wm.GetKey().GetID(), Timestamp: time.Unix(int64(wm.GetMessageTimestamp()), 0), PushName: wm.GetPushName()}
		switch {
		case info.IsFromMe:
			info.Sender = me
		case wm.GetParticipant() != "":
			info.Sender, _ = types.ParseJID(wm.GetParticipant())
		default:
			info.Sender = chat
		}
		return (&events.Message{Info: info, RawMessage: wm.GetMessage()}).UnwrapRaw(), nil
	}
	hs := &waHistorySync.HistorySync{
		SyncType:                 waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String(anna.String()), LidJID: proto.String(annaLID.String())}},
		Pushnames:                []*waHistorySync.Pushname{{ID: proto.String(bob.String()), Pushname: proto.String("Bobby")}},
		Conversations: []*waHistorySync.Conversation{{
			ID:          proto.String(family.String()),
			Name:        proto.String("Rodzina"),
			UnreadCount: proto.Uint32(2),
			Participant: []*waHistorySync.GroupParticipant{
				{UserJID: proto.String(annaLID.String()), Rank: waHistorySync.GroupParticipant_ADMIN.Enum()},
				{UserJID: proto.String(bob.String())},
			},
			Messages: []*waHistorySync.HistorySyncMsg{
				{Message: &waWeb.WebMessageInfo{
					Key:              &waCommon.MessageKey{RemoteJID: proto.String(family.String()), ID: proto.String("H1")},
					Participant:      proto.String(annaLID.String()),
					MessageTimestamp: proto.Uint64(uint64(t0.Unix())),
					Message:          &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: text("obiad o 14")}},
					Reactions: []*waWeb.Reaction{{Key: &waCommon.MessageKey{FromMe: proto.Bool(true)}, Text: proto.String("❤️"),
						SenderTimestampMS: proto.Int64(t0.UnixMilli())}},
				}},
				{Message: &waWeb.WebMessageInfo{
					Key:                   &waCommon.MessageKey{RemoteJID: proto.String(family.String()), ID: proto.String("H2")},
					Participant:           proto.String(bob.String()),
					MessageTimestamp:      proto.Uint64(uint64(t0.Add(time.Hour).Unix())),
					MessageStubType:       waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD.Enum(),
					MessageStubParameters: []string{annaLID.String()},
				}},
				{Message: &waWeb.WebMessageInfo{
					Key:              &waCommon.MessageKey{RemoteJID: proto.String(family.String()), ID: proto.String("H3")},
					MessageTimestamp: proto.Uint64(uint64(t0.Unix())),
					MessageStubType:  waWeb.WebMessageInfo_E2E_ENCRYPTED.Enum(),
				}},
			},
		}},
	}
	st, err := in.HistorySync(ctx, hs, parse)
	if err != nil {
		t.Fatal(err)
	}
	if st.Messages != 2 || st.Conversations != 1 {
		t.Fatalf("stats: %+v", st)
	}
	msgs := mustMessages(t, s, family.String())
	if len(msgs) != 2 || msgs[0].SenderJID != anna.String() || msgs[0].Text != "obiad o 14" {
		t.Fatalf("history messages: %+v", msgs)
	}
	if msgs[1].Kind != "system" || !strings.Contains(msgs[1].Text, anna.String()) {
		t.Fatalf("stub: %+v", msgs[1])
	}
	rs, _ := s.Reactions(ctx, family.String(), []string{"H1"})
	if len(rs["H1"]) != 1 || rs["H1"][0].SenderJID != me.String() {
		t.Fatalf("history reaction: %+v", rs)
	}
	dir, err := s.Directory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dir.Name(family.String()) != "Rodzina" || dir.Name(bob.String()) != "Bobby" {
		t.Fatalf("names: %q %q", dir.Name(family.String()), dir.Name(bob.String()))
	}
	ps, _ := s.Participants(ctx, family.String())
	if len(ps) != 2 || ps[0].JID != anna.String() || ps[0].Role != "admin" {
		t.Fatalf("participants: %+v", ps)
	}
	chat, err := s.Chat(ctx, family.String())
	if err != nil || chat.UnreadCount != 2 || chat.Kind != "group" {
		t.Fatalf("chat meta: %+v %v", chat, err)
	}
}

func TestExtractKinds(t *testing.T) {
	cases := []struct {
		msg  *waE2E.Message
		kind string
		text string
	}{
		{&waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(42)}}, "voice", ""},
		{&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("umowa.pdf")}}, "document", ""},
		{&waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Dom"), DegreesLatitude: proto.Float64(52.1), DegreesLongitude: proto.Float64(21.2)}}, "location", "Dom (52.10000, 21.20000)"},
		{&waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("Pizza?"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("tak")}, {OptionName: proto.String("nie")}}}}, "poll", "Pizza?\noptions: tak | nie"},
		{&waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Ola"), Vcard: proto.String("BEGIN:VCARD\nTEL;type=CELL;waid=48111:+48 111\nEND:VCARD")}}, "contact", "Ola (+48 111)"},
		{&waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}}, "", ""},
		{&waE2E.Message{SpoilerMessage: &waE2E.FutureProofMessage{Message: text("sekret")}}, "text", "sekret"},
	}
	for _, c := range cases {
		got := Extract(c.msg)
		if got.Kind != c.kind || got.Text != c.text {
			t.Errorf("Extract(%v) = %q/%q, want %q/%q", c.msg, got.Kind, got.Text, c.kind, c.text)
		}
	}
}

func TestRevokeKeepsContent(t *testing.T) {
	ctx := context.Background()
	in, s := setup(t)
	s.SetKeepDeleted(true)
	in.Message(ctx, msgEvent(family, anna, "K1", t0, text("tajny plan na sobotę")), "live")
	revoke := func(target string, by types.JID, at time.Time) {
		ev := msgEvent(family, by, target+"-rev", at, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String(target), Participant: proto.String(anna.String())},
		}})
		if _, err := in.Message(ctx, ev, "live"); err != nil {
			t.Fatal(err)
		}
	}
	revoke("K1", anna, t0.Add(time.Minute))
	m, err := s.Message(ctx, family.String(), "K1")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Deleted() || m.Text != "tajny plan na sobotę" || m.RevokedBy != anna.String() {
		t.Fatalf("content must be kept and marked: %+v", m)
	}
	if hits, _ := s.Search(ctx, archive.SearchQuery{Text: "tajny"}); len(hits) != 1 {
		t.Fatalf("kept deleted message should stay searchable, got %d hits", len(hits))
	}
	// Deletion by an admin arriving before the original (out-of-order history).
	revoke("K2", bob, t0.Add(3*time.Minute))
	in.Message(ctx, msgEvent(family, anna, "K2", t0.Add(2*time.Minute), text("druga wiadomość")), "history")
	m, err = s.Message(ctx, family.String(), "K2")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Deleted() || m.Kind != "text" || m.Text != "druga wiadomość" || m.RevokedBy != bob.String() || m.SenderJID != anna.String() {
		t.Fatalf("tombstone not filled with the original: %+v", m)
	}
	if m.Timestamp != t0.Add(2*time.Minute).Unix() {
		t.Fatalf("timestamp should be the original's, got %v", time.Unix(m.Timestamp, 0))
	}
}

func TestEditHistory(t *testing.T) {
	ctx := context.Background()
	in, s := setup(t)
	edit := func(id, body string, at time.Time) {
		ev := msgEvent(family, anna, id+"-e"+body, at, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id)}, EditedMessage: text(body),
		}})
		if _, err := in.Message(ctx, ev, "live"); err != nil {
			t.Fatal(err)
		}
	}
	in.Message(ctx, msgEvent(family, anna, "E1", t0, text("pociąg o 10:15")), "live")
	edit("E1", "pociąg o 10:45", t0.Add(time.Minute))
	edit("E1", "pociąg o 11:05", t0.Add(2*time.Minute))
	edit("E1", "pociąg o 11:05", t0.Add(2*time.Minute)) // duplicate delivery
	vs, err := s.EditHistory(ctx, family.String(), []string{"E1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := vs["E1"]; len(got) != 2 || got[0].Text != "pociąg o 10:15" || got[1].Text != "pociąg o 10:45" {
		t.Fatalf("versions: %+v", got)
	}
	m, _ := s.Message(ctx, family.String(), "E1")
	if m.Text != "pociąg o 11:05" {
		t.Fatalf("current text: %q", m.Text)
	}
	// The original wording stays searchable.
	if hits, _ := s.Search(ctx, archive.SearchQuery{Text: "10:15"}); len(hits) != 1 {
		t.Fatalf("original version not searchable: %d hits", len(hits))
	}
	// Edit arriving before the original: the original becomes an earlier version.
	edit("E2", "poprawione", t0.Add(5*time.Minute))
	in.Message(ctx, msgEvent(family, anna, "E2", t0.Add(4*time.Minute), text("oryginał")), "history")
	vs, _ = s.EditHistory(ctx, family.String(), []string{"E2"})
	m, _ = s.Message(ctx, family.String(), "E2")
	if m.Text != "poprawione" || len(vs["E2"]) != 1 || vs["E2"][0].Text != "oryginał" {
		t.Fatalf("late original: text=%q versions=%+v", m.Text, vs["E2"])
	}
}
