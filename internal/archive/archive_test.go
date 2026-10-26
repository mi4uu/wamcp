package archive

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMergeJIDKeepsChatState(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	lid, pn := "111@lid", "48600000001@s.whatsapp.net"
	err := s.Write(ctx, func(tx *Tx) error {
		if err := tx.TouchChat(pn, "direct", 100); err != nil {
			return err
		}
		if err := tx.TouchChat(lid, "direct", 200); err != nil {
			return err
		}
		for col, v := range map[string]int64{"pinned": 1, "unread_count": 7, "muted_until": -1, "archived": 1} {
			if err := tx.SetChatFlag(lid, "direct", col, v); err != nil {
				return err
			}
		}
		return tx.MergeJID(lid, pn)
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Chat(ctx, pn)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pinned || c.UnreadCount != 7 || c.MutedUntil != -1 || !c.Archived || c.LastMessageAt != 200 {
		t.Fatalf("merged chat lost state: %+v", c)
	}
}

func TestHistoryDoesNotResurrectUnread(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	jid := "48600000002@s.whatsapp.net"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Write(ctx, func(tx *Tx) error {
		return tx.UpsertChatMeta(ChatMeta{JID: jid, Kind: "direct", UnreadCount: 3, LastMessageAt: 100})
	}))
	must(s.Write(ctx, func(tx *Tx) error { return tx.SetChatFlag(jid, "direct", "unread_count", 0) })) // read on phone
	must(s.Write(ctx, func(tx *Tx) error {
		return tx.UpsertChatMeta(ChatMeta{JID: jid, Kind: "direct", UnreadCount: 3, LastMessageAt: 100}) // stale batch
	}))
	c, _ := s.Chat(ctx, jid)
	if c.UnreadCount != 0 {
		t.Fatalf("stale history resurrected unread badge: %d", c.UnreadCount)
	}
}

func TestLiveCallResultWins(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	up := func(c Call) {
		t.Helper()
		if err := s.Write(ctx, func(tx *Tx) error { return tx.UpsertCall(c) }); err != nil {
			t.Fatal(err)
		}
	}
	up(Call{ID: "C1", StartedAt: 10, Result: "ended", Duration: 120, Source: "live"})
	up(Call{ID: "C1", StartedAt: 10, Result: "connected", Source: "history"})
	calls, _ := s.Calls(ctx, CallQuery{})
	if len(calls) != 1 || calls[0].Result != "ended" || calls[0].Duration != 120 || calls[0].Source != "live" {
		t.Fatalf("history overrode live call: %+v", calls)
	}
	up(Call{ID: "C1", StartedAt: 10, Result: "missed", Source: "live"})
	calls, _ = s.Calls(ctx, CallQuery{})
	if calls[0].Result != "missed" {
		t.Fatalf("a later live update must still apply: %+v", calls[0])
	}
}

func TestPagingEqualTimestamps(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	chat := "g@g.us"
	err := s.Write(ctx, func(tx *Tx) error {
		for i := range 10 {
			m := &Message{ChatJID: chat, ID: string(rune('a' + i)), Timestamp: 1000, Kind: "text", Text: "x", Source: "live"}
			if err := tx.UpsertMessage(m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	page, _ := s.Messages(ctx, MessageQuery{ChatJID: chat, Limit: 4})
	for len(page) > 0 {
		for _, m := range page {
			if seen[m.ID] {
				t.Fatalf("duplicate %s", m.ID)
			}
			seen[m.ID] = true
		}
		page, _ = s.Messages(ctx, MessageQuery{ChatJID: chat, Limit: 4, Before: page[0].Timestamp, BeforePK: page[0].PK})
	}
	if len(seen) != 10 {
		t.Fatalf("backward paging saw %d of 10", len(seen))
	}
	seen = map[string]bool{}
	page, _ = s.Messages(ctx, MessageQuery{ChatJID: chat, Limit: 4, After: 1, OldestFirst: true})
	for len(page) > 0 {
		for _, m := range page {
			seen[m.ID] = true
		}
		last := page[len(page)-1]
		page, _ = s.Messages(ctx, MessageQuery{ChatJID: chat, Limit: 4, After: last.Timestamp, AfterPK: last.PK, OldestFirst: true})
	}
	if len(seen) != 10 {
		t.Fatalf("forward paging saw %d of 10", len(seen))
	}
}

func TestUnreadBaseline(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a, b := "1@s.whatsapp.net", "2@s.whatsapp.net"
	// Live messages arrive before the chat's first history snapshot.
	must(s.Write(ctx, func(tx *Tx) error {
		if err := tx.TouchChat(a, "direct", 100); err != nil {
			return err
		}
		return tx.BumpUnread(a, "direct", false)
	}))
	must(s.Write(ctx, func(tx *Tx) error { return tx.UpsertChatMeta(ChatMeta{JID: a, Kind: "direct", UnreadCount: 5}) }))
	if c, _ := s.Chat(ctx, a); c.UnreadCount != 5 {
		t.Fatalf("first snapshot should establish the count, got %d", c.UnreadCount)
	}
	must(s.Write(ctx, func(tx *Tx) error { return tx.UpsertChatMeta(ChatMeta{JID: a, Kind: "direct", UnreadCount: 9}) }))
	if c, _ := s.Chat(ctx, a); c.UnreadCount != 5 {
		t.Fatalf("later snapshots must not override, got %d", c.UnreadCount)
	}
	// Chat read on the phone before its snapshot arrives: the snapshot is stale.
	must(s.Write(ctx, func(tx *Tx) error { return tx.SetChatFlag(b, "direct", "unread_count", 0) }))
	must(s.Write(ctx, func(tx *Tx) error { return tx.UpsertChatMeta(ChatMeta{JID: b, Kind: "direct", UnreadCount: 4}) }))
	if c, _ := s.Chat(ctx, b); c.UnreadCount != 0 {
		t.Fatalf("read chat resurrected: %d", c.UnreadCount)
	}
}

func TestLateEditIsSearchableVersion(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	chat := "g@g.us"
	edit := func(text string, at int64) {
		t.Helper()
		err := s.Write(ctx, func(tx *Tx) error {
			return tx.EditMessage(&Message{ChatJID: chat, ID: "M", Kind: "text", Text: text, EditedAt: at, Timestamp: 1, Source: "live"})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Write(ctx, func(tx *Tx) error {
		return tx.UpsertMessage(&Message{ChatJID: chat, ID: "M", Kind: "text", Text: "alpha", Timestamp: 1, Source: "live"})
	}); err != nil {
		t.Fatal(err)
	}
	edit("gamma", 30) // newer edit first
	edit("beta", 20)  // older edit late
	vs, _ := s.EditHistory(ctx, chat, []string{"M"})
	if len(vs["M"]) != 2 || vs["M"][0].Text != "alpha" || vs["M"][1].Text != "beta" {
		t.Fatalf("versions out of order: %+v", vs["M"])
	}
	if m, _ := s.Message(ctx, chat, "M"); m.Text != "gamma" {
		t.Fatalf("current text changed by a late edit: %q", m.Text)
	}
	if hits, _ := s.Search(ctx, SearchQuery{Text: "beta"}); len(hits) != 1 {
		t.Fatalf("late edit text not searchable")
	}
}
