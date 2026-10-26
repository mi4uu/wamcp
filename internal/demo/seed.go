// Package demo seeds an archive with synthetic data for tests and UI previews.
package demo

import (
	"context"
	"io"
	"log/slog"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/ingest"
)

// JIDs used by the demo data.
var (
	Me     = types.NewJID("48500100200", types.DefaultUserServer)
	Anna   = types.NewJID("48600000101", types.DefaultUserServer)
	Piotr  = types.NewJID("48600000102", types.DefaultUserServer)
	Kasia  = types.NewJID("48600000103", types.DefaultUserServer)
	Family = types.NewJID("120363040000000001", types.GroupServer)
	Work   = types.NewJID("120363040000000002", types.GroupServer)
)

// Seed fills store with a small but realistic archive ending at now.
func Seed(ctx context.Context, store *archive.Store, now time.Time) error {
	in := ingest.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := in.SetSelf(ctx, Me, types.EmptyJID); err != nil {
		return err
	}
	err := in.Contacts(ctx, map[types.JID]types.ContactInfo{
		Anna:  {Found: true, FullName: "Anna Nowak", FirstName: "Anna", PushName: "Ania"},
		Piotr: {Found: true, FullName: "Piotr Kowalski", PushName: "Piotrek"},
		Kasia: {Found: true, FullName: "Kasia Wiśniewska", PushName: "Kaśka"},
	})
	if err != nil {
		return err
	}
	groups := []*types.GroupInfo{
		{JID: Family, GroupName: types.GroupName{Name: "Rodzina ❤️"}, GroupTopic: types.GroupTopic{Topic: "Sprawy rodzinne, obiady, urodziny"},
			OwnerJID: Anna, GroupCreated: now.AddDate(-3, 0, 0),
			Participants: []types.GroupParticipant{{JID: Anna, IsSuperAdmin: true}, {JID: Me, IsAdmin: true}, {JID: Kasia}}},
		{JID: Work, GroupName: types.GroupName{Name: "Projekt Łódź"}, OwnerJID: Piotr, GroupCreated: now.AddDate(0, -2, 0),
			Participants: []types.GroupParticipant{{JID: Piotr, IsSuperAdmin: true}, {JID: Me}, {JID: Anna}}},
	}
	if err := in.Groups(ctx, groups, true); err != nil {
		return err
	}
	text := func(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }
	n := 0
	msg := func(chat, sender types.JID, ago time.Duration, m *waE2E.Message, source string) error {
		n++
		_, err := in.Message(ctx, &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: sender == Me, IsGroup: chat.Server == types.GroupServer},
				ID:            types.MessageID("DEMO" + string(rune('A'+n/26)) + string(rune('A'+n%26))),
				Timestamp:     now.Add(-ago), PushName: "",
			},
			Message: m,
		}, source)
		return err
	}
	steps := []struct {
		chat, sender types.JID
		ago          time.Duration
		m            *waE2E.Message
	}{
		{Anna, Anna, 50 * time.Hour, text("Hej! Jedziemy w sobotę do Łodzi?")},
		{Anna, Me, 49 * time.Hour, text("Tak, wyjazd o 9:00 😊")},
		{Anna, Anna, 26 * time.Hour, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("Zobacz jaki widok"), Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(245_000)}}},
		{Family, Anna, 25 * time.Hour, text("Obiad w niedzielę o 14, kto będzie?")},
		{Family, Kasia, 24 * time.Hour, text("Ja będę! Przywiozę *sernik* 🍰")},
		{Family, Me, 23 * time.Hour, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("Będę z Piotrem"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("DEMOAE"), Participant: proto.String(Anna.String()), QuotedMessage: text("Obiad w niedzielę o 14, kto będzie?")}}}},
		{Work, Piotr, 5 * time.Hour, text("Spotkanie z klientem przesunięte na czwartek 10:00, sala B.")},
		{Work, Piotr, 4 * time.Hour, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("oferta_Q4.pdf"), Mimetype: proto.String("application/pdf"), FileLength: proto.Uint64(1_200_000)}}},
		{Work, Anna, 3 * time.Hour, text("Ok, przygotuję prezentację")},
		{Piotr, Piotr, 90 * time.Minute, &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(37), Mimetype: proto.String("audio/ogg; codecs=opus")}}},
		{Piotr, Piotr, 80 * time.Minute, text("Oddzwonisz jak będziesz mógł?")},
		{Kasia, Kasia, 30 * time.Minute, &waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Manufaktura"), Address: proto.String("Łódź"), DegreesLatitude: proto.Float64(51.7794), DegreesLongitude: proto.Float64(19.4485)}}},
		{Kasia, Kasia, 29 * time.Minute, text("Czekam tutaj 👋 https://example.com/mapa")},
	}
	for _, s := range steps {
		if err := msg(s.chat, s.sender, s.ago, s.m, "live"); err != nil {
			return err
		}
	}
	// A reaction from Anna to the Sunday lunch message and an edit by Piotr.
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("DEMOAE")},
		Text: proto.String("❤️"), SenderTimestampMS: proto.Int64(now.Add(-22 * time.Hour).UnixMilli())}}
	if err := msg(Family, Anna, 22*time.Hour, react, "live"); err != nil {
		return err
	}
	if err := store.SetContactMeta(ctx, archive.ContactMeta{JID: Anna.String(), Description: "Siostra. Urodziny 14 marca.",
		Priority: 5, Tags: []string{"rodzina"}, Fields: map[string]string{"urodziny": "14.03"}}); err != nil {
		return err
	}
	if err := store.SetContactMeta(ctx, archive.ContactMeta{JID: Work.String(), Priority: 4, Tags: []string{"praca"},
		Description: "Projekt dla klienta z Łodzi, deadline koniec kwartału."}); err != nil {
		return err
	}
	return store.Write(ctx, func(t *archive.Tx) error {
		return t.UpsertCall(archive.Call{ID: "CALL1", ChatJID: Piotr.String(), CreatorJID: Piotr.String(), StartedAt: now.Add(-100 * time.Minute).Unix(),
			Result: "missed", Source: "live"})
	})
}
