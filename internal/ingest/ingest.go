// Package ingest maps whatsmeow events and history sync payloads onto archive
// writes. It is independent of the live connection so it can be tested with
// synthetic events.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/mi4uu/wamcp/internal/archive"
)

// ParseFunc converts a history sync message into a message event
// (whatsmeow's Client.ParseWebMessage).
type ParseFunc func(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error)

// Resolver looks up the phone-number JID behind a LID (whatsmeow's LID store).
type Resolver interface {
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
}

// Ingestor writes WhatsApp data into the archive.
type Ingestor struct {
	store *archive.Store
	log   *slog.Logger

	mu       sync.RWMutex
	resolver Resolver
	lids     map[string]string // LID -> phone JID
	selfPN   types.JID
	selfLID  types.JID
}

// New creates an Ingestor.
func New(store *archive.Store, log *slog.Logger) *Ingestor {
	return &Ingestor{store: store, log: log, lids: map[string]string{}}
}

// LoadMappings warms the LID cache from the archive.
func (in *Ingestor) LoadMappings(ctx context.Context) error {
	m, err := in.store.LIDMappings(ctx)
	if err != nil {
		return err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	for k, v := range m {
		in.lids[k] = v
	}
	return nil
}

// SetResolver sets the live LID resolver (may be nil).
func (in *Ingestor) SetResolver(r Resolver) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.resolver = r
}

// SetSelf records the account's own phone JID and LID.
func (in *Ingestor) SetSelf(ctx context.Context, pn, lid types.JID) error {
	pn, lid = pn.ToNonAD(), lid.ToNonAD()
	in.mu.Lock()
	in.selfPN, in.selfLID = pn, lid
	in.mu.Unlock()
	return in.write(ctx, func(b *batch) error {
		if !lid.IsEmpty() {
			b.learn(lid, pn)
		}
		return b.tx.SetMeta(archive.MetaAccount, pn.String())
	})
}

func (in *Ingestor) self() types.JID {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return in.selfPN
}

// MessageResult describes the effect of a message event: the canonical chat
// it touched and, for stored messages, follow-up data like media.
type MessageResult struct {
	ChatJID  string
	ID       string // empty for reactions, edits and deletions
	Stored   bool
	HasMedia bool
	FileSize int64
}

// Message stores a message event. source is "live" for messages received
// while connected (they update unread counters), "sent" for messages sent by
// this client, and "history" for history sync copies.
func (in *Ingestor) Message(ctx context.Context, evt *events.Message, source string) (*MessageResult, error) {
	var res *MessageResult
	err := in.write(ctx, func(b *batch) error {
		m, chat, err := b.message(evt, source)
		if err != nil {
			return err
		}
		res = &MessageResult{ChatJID: chat.String()}
		if m == nil {
			return nil
		}
		res.ID, res.Stored, res.HasMedia, res.FileSize = m.ID, true, m.HasMedia, m.FileSize
		if (source == "live" || source == "sent") && m.Kind != "system" {
			return b.tx.BumpUnread(m.ChatJID, chatKind(chat), m.FromMe)
		}
		return nil
	})
	return res, err
}

// Receipt applies delivery/read receipts: ticks on own messages, and chats
// read on another device of the account.
func (in *Ingestor) Receipt(ctx context.Context, evt *events.Receipt) (string, error) {
	var chatStr string
	err := in.write(ctx, func(b *batch) error {
		chat := b.chatOf(&types.MessageInfo{MessageSource: evt.MessageSource})
		chatStr = chat.String()
		if evt.IsFromMe {
			if evt.Type == types.ReceiptTypeReadSelf || evt.Type == types.ReceiptTypePlayedSelf {
				return b.tx.SetChatFlag(chatStr, chatKind(chat), "unread_count", 0)
			}
			return nil
		}
		ack := 0
		switch evt.Type {
		case types.ReceiptTypeDelivered:
			ack = 2
		case types.ReceiptTypeRead:
			ack = 3
		case types.ReceiptTypePlayed:
			ack = 4
		}
		if ack == 0 || len(evt.MessageIDs) == 0 {
			return nil
		}
		return b.tx.SetAck(chatStr, evt.MessageIDs, ack)
	})
	return chatStr, err
}

// HistoryStats summarizes one history sync batch.
type HistoryStats struct {
	SyncType      string
	Chunk         uint32
	Progress      uint32
	Conversations int
	Messages      int
	Skipped       int
	Calls         int
}

// HistorySync stores a history sync batch sent by the phone.
func (in *Ingestor) HistorySync(ctx context.Context, data *waHistorySync.HistorySync, parse ParseFunc) (HistoryStats, error) {
	st := HistoryStats{SyncType: data.GetSyncType().String(), Chunk: data.GetChunkOrder(), Progress: data.GetProgress()}
	err := in.write(ctx, func(b *batch) error {
		for _, m := range data.GetPhoneNumberToLidMappings() {
			b.learnStrings(m.GetLidJID(), m.GetPnJID())
		}
		for _, c := range data.GetInlineContacts() {
			b.learnStrings(c.GetLidJID(), c.GetPnJID())
			jid := b.canonString(firstNonEmpty(c.GetPnJID(), c.GetLidJID()))
			if err := b.tx.UpsertContact(archive.Contact{JID: jid, FullName: c.GetFullName(), FirstName: c.GetFirstName()}); err != nil {
				return err
			}
		}
		for _, p := range data.GetPushnames() {
			if err := b.tx.UpsertContact(archive.Contact{JID: b.canonString(p.GetID()), PushName: p.GetPushname()}); err != nil {
				return err
			}
		}
		for _, conv := range data.GetConversations() {
			if err := b.conversation(conv, parse, &st); err != nil {
				return err
			}
		}
		for _, wm := range data.GetStatusV3Messages() {
			if err := b.webMessage(types.StatusBroadcastJID, wm, parse, &st); err != nil {
				return err
			}
		}
		for _, rec := range data.GetCallLogRecords() {
			if err := b.callRecord(rec); err != nil {
				return err
			}
			st.Calls++
		}
		return nil
	})
	return st, err
}

// PushName stores a changed push name.
func (in *Ingestor) PushName(ctx context.Context, jid, alt types.JID, name string) error {
	return in.write(ctx, func(b *batch) error {
		return b.tx.UpsertContact(archive.Contact{JID: b.canon(jid, alt).String(), PushName: name})
	})
}

// BusinessName stores a changed verified business name.
func (in *Ingestor) BusinessName(ctx context.Context, jid types.JID, name string) error {
	return in.write(ctx, func(b *batch) error {
		return b.tx.UpsertContact(archive.Contact{JID: b.canon(jid, types.EmptyJID).String(), BusinessName: name})
	})
}

// ContactAction stores an address book change synced from the phone.
func (in *Ingestor) ContactAction(ctx context.Context, jid types.JID, act *waSyncAction.ContactAction) error {
	return in.write(ctx, func(b *batch) error {
		b.learnStrings(act.GetLidJID(), act.GetPnJID())
		if jid.Server == types.HiddenUserServer && act.GetLidJID() == "" && act.GetPnJID() != "" {
			b.learnStrings(jid.String(), act.GetPnJID())
		}
		return b.tx.UpsertContact(archive.Contact{
			JID:       b.canon(jid, types.EmptyJID).String(),
			FullName:  act.GetFullName(),
			FirstName: act.GetFirstName(),
		})
	})
}

// Contacts stores the contact list kept by whatsmeow.
func (in *Ingestor) Contacts(ctx context.Context, all map[types.JID]types.ContactInfo) error {
	return in.write(ctx, func(b *batch) error {
		for jid, ci := range all {
			j := b.canon(jid, types.EmptyJID)
			if j.Server != types.DefaultUserServer && j.Server != types.HiddenUserServer {
				continue
			}
			err := b.tx.UpsertContact(archive.Contact{JID: j.String(), FullName: ci.FullName, FirstName: ci.FirstName,
				PushName: ci.PushName, BusinessName: ci.BusinessName})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Groups stores group metadata fetched from WhatsApp. When complete is true the
// list is every group the account belongs to, and all others are marked as left.
func (in *Ingestor) Groups(ctx context.Context, groups []*types.GroupInfo, complete bool) error {
	return in.write(ctx, func(b *batch) error {
		ids := make([]string, 0, len(groups))
		for _, g := range groups {
			if err := b.group(g); err != nil {
				return err
			}
			ids = append(ids, g.JID.String())
		}
		if complete {
			return b.tx.SetGroupMembership(ids)
		}
		return nil
	})
}

// ChatFlag updates chat state synced from the phone (archived, pinned, muted_until, ...).
func (in *Ingestor) ChatFlag(ctx context.Context, jid types.JID, column string, value int64) error {
	return in.write(ctx, func(b *batch) error {
		chat := b.canon(jid, types.EmptyJID)
		return b.tx.SetChatFlag(chat.String(), chatKind(chat), column, value)
	})
}

// ChatRead applies "mark chat as read/unread".
func (in *Ingestor) ChatRead(ctx context.Context, jid types.JID, read bool) error {
	return in.write(ctx, func(b *batch) error {
		chat := b.canon(jid, types.EmptyJID)
		if read {
			if err := b.tx.SetChatFlag(chat.String(), chatKind(chat), "unread_count", 0); err != nil {
				return err
			}
		}
		return b.tx.SetChatFlag(chat.String(), chatKind(chat), "marked_unread", boolInt(!read))
	})
}

// Star applies a starred/unstarred message change.
func (in *Ingestor) Star(ctx context.Context, chat types.JID, id string, starred bool) error {
	return in.write(ctx, func(b *batch) error {
		return b.tx.SetStarred(b.canon(chat, types.EmptyJID).String(), id, starred)
	})
}

// CallEvent is a live call signal.
type CallEvent struct {
	Meta   types.BasicCallMeta
	Kind   string // offer, accept, reject, terminate
	Video  bool
	Group  bool
	Reason string
}

// Call stores a live call signal in the call log.
func (in *Ingestor) Call(ctx context.Context, e CallEvent) error {
	return in.write(ctx, func(b *batch) error {
		creator := b.canon(e.Meta.CallCreator, e.Meta.CallCreatorAlt)
		peer := creator
		if !e.Meta.GroupJID.IsEmpty() {
			peer = e.Meta.GroupJID
		} else if !e.Meta.From.IsEmpty() && e.Meta.From.User != b.in.self().User {
			peer = b.canon(e.Meta.From, types.EmptyJID)
		}
		ts := e.Meta.Timestamp.Unix()
		c := archive.Call{ID: e.Meta.CallID, ChatJID: peer.String(), CreatorJID: creator.String(),
			FromMe: creator.User == b.in.self().User, Video: e.Video, IsGroup: e.Group || !e.Meta.GroupJID.IsEmpty(),
			StartedAt: ts, Source: "live"}
		prev, err := b.tx.CallByID(e.Meta.CallID)
		if err != nil && err != archive.ErrNotFound {
			return err
		}
		if prev != nil {
			c.StartedAt = prev.StartedAt
		}
		switch e.Kind {
		case "offer":
			c.Result = "ringing"
		case "accept":
			c.Result, c.AcceptedAt = "accepted", ts
		case "reject":
			c.Result = "rejected"
		case "terminate":
			c.Result = "missed"
			if prev != nil && prev.AcceptedAt != 0 {
				c.Result, c.Duration = "ended", ts-prev.AcceptedAt
			}
			if e.Reason != "" && c.Result == "missed" && prev != nil && prev.Result == "rejected" {
				c.Result = "rejected"
			}
		}
		return b.tx.UpsertCall(c)
	})
}

// Reconcile rewrites LIDs that have become resolvable to phone JIDs.
func (in *Ingestor) Reconcile(ctx context.Context) (int, error) {
	lids, err := in.store.UnresolvedLIDs(ctx)
	if err != nil || len(lids) == 0 {
		return 0, err
	}
	merged := 0
	err = in.write(ctx, func(b *batch) error {
		for _, l := range lids {
			lid, err := types.ParseJID(l)
			if err != nil {
				continue
			}
			pn, ok := b.lookup(lid)
			if !ok {
				continue
			}
			if err := b.tx.MergeJID(l, pn.String()); err != nil {
				return err
			}
			merged++
		}
		return nil
	})
	return merged, err
}

// write runs fn in one archive transaction and removes media files of
// messages deleted for everyone once the transaction commits.
func (in *Ingestor) write(ctx context.Context, fn func(*batch) error) error {
	var removed []string
	err := in.store.Write(ctx, func(tx *archive.Tx) error {
		b := &batch{in: in, ctx: ctx, tx: tx}
		if err := fn(b); err != nil {
			return err
		}
		removed = b.removed
		return nil
	})
	if err == nil {
		for _, p := range removed {
			if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
				in.log.Warn("remove media of deleted message", "path", p, "err", rmErr)
			}
		}
	}
	return err
}

// batch carries one transaction plus JID canonicalization.
type batch struct {
	in      *Ingestor
	ctx     context.Context
	tx      *archive.Tx
	removed []string
}

// learn records that lid belongs to phone JID pn.
func (b *batch) learn(lid, pn types.JID) {
	lid, pn = lid.ToNonAD(), pn.ToNonAD()
	if lid.Server != types.HiddenUserServer || pn.Server != types.DefaultUserServer {
		return
	}
	ls, ps := lid.String(), pn.String()
	b.in.mu.Lock()
	old := b.in.lids[ls]
	b.in.lids[ls] = ps
	b.in.mu.Unlock()
	if old != ps {
		if err := b.tx.PutLIDMapping(ls, ps); err != nil {
			b.in.log.Warn("store LID mapping", "lid", ls, "err", err)
		}
	}
}

func (b *batch) learnStrings(lid, pn string) {
	if lid == "" || pn == "" {
		return
	}
	l, err1 := types.ParseJID(lid)
	p, err2 := types.ParseJID(pn)
	if err1 == nil && err2 == nil {
		b.learn(l, p)
	}
}

// lookup returns the phone JID for a LID from the cache or the live store.
func (b *batch) lookup(lid types.JID) (types.JID, bool) {
	b.in.mu.RLock()
	pn, ok := b.in.lids[lid.String()]
	resolver := b.in.resolver
	b.in.mu.RUnlock()
	if ok {
		j, err := types.ParseJID(pn)
		return j, err == nil
	}
	if resolver == nil {
		return types.EmptyJID, false
	}
	j, err := resolver.GetPNForLID(b.ctx, lid)
	if err != nil || j.IsEmpty() {
		return types.EmptyJID, false
	}
	b.learn(lid, j)
	return j.ToNonAD(), true
}

// canon returns the canonical JID for a user: the phone-number JID when known,
// otherwise the input. alt is the alternate address WhatsApp sent along.
func (b *batch) canon(j, alt types.JID) types.JID {
	if j.IsEmpty() {
		return j
	}
	j = j.ToNonAD()
	if j.Server == types.LegacyUserServer {
		j.Server = types.DefaultUserServer
	}
	if !alt.IsEmpty() {
		alt = alt.ToNonAD()
		if alt.Server == types.LegacyUserServer {
			alt.Server = types.DefaultUserServer
		}
	}
	switch j.Server {
	case types.HiddenUserServer:
		if alt.Server == types.DefaultUserServer {
			b.learn(j, alt)
			return alt
		}
		if pn, ok := b.lookup(j); ok {
			return pn
		}
	case types.DefaultUserServer:
		if alt.Server == types.HiddenUserServer {
			b.learn(alt, j)
		}
	}
	return j
}

func (b *batch) canonString(s string) string {
	if s == "" {
		return ""
	}
	j, err := types.ParseJID(s)
	if err != nil {
		return s
	}
	return b.canon(j, types.EmptyJID).String()
}

func (b *batch) chatOf(info *types.MessageInfo) types.JID {
	if info.Chat.Server != types.HiddenUserServer {
		return b.canon(info.Chat, types.EmptyJID)
	}
	alt := info.SenderAlt
	if info.IsFromMe {
		alt = info.RecipientAlt
	}
	return b.canon(info.Chat, alt)
}

func (b *batch) senderOf(info *types.MessageInfo) types.JID {
	if info.IsFromMe {
		if self := b.in.self(); !self.IsEmpty() {
			return self
		}
	}
	return b.canon(info.Sender, info.SenderAlt)
}

// message stores one message event. It returns the stored row (nil for
// reactions, edits, deletions and bookkeeping payloads) and the canonical chat.
func (b *batch) message(evt *events.Message, source string) (*archive.Message, types.JID, error) {
	info := &evt.Info
	chat := b.chatOf(info)
	msg := evt.Message
	if msg == nil || info.ID == "" {
		return nil, chat, nil
	}
	sender := b.senderOf(info)
	ts := info.Timestamp.Unix()
	chatStr, kind := chat.String(), chatKind(chat)

	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			target := pm.GetKey().GetID()
			if target == "" {
				return nil, chat, nil
			}
			var origSender string
			if p := pm.GetKey().GetParticipant(); p != "" {
				origSender = b.canonString(p)
			} else if pm.GetKey().GetFromMe() {
				origSender = b.in.self().String()
			} else if kind == "direct" {
				origSender = chatStr
			}
			path, err := b.tx.RevokeMessage(chatStr, target, origSender, pm.GetKey().GetFromMe(), ts, source, sender.String())
			if path != "" {
				b.removed = append(b.removed, path)
			}
			return nil, chat, err
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			c := Extract(pm.GetEditedMessage())
			target := pm.GetKey().GetID()
			if c.Kind == "" || target == "" {
				return nil, chat, nil
			}
			return nil, chat, b.tx.EditMessage(&archive.Message{ChatJID: chatStr, ID: target, SenderJID: sender.String(),
				FromMe: info.IsFromMe, Timestamp: ts, Kind: c.Kind, Text: c.Text, EditedAt: ts, Source: source})
		case waE2E.ProtocolMessage_EPHEMERAL_SETTING:
			secs := int64(pm.GetEphemeralExpiration())
			if err := b.tx.SetChatFlag(chatStr, kind, "ephemeral_seconds", secs); err != nil {
				return nil, chat, err
			}
			text := "disappearing messages turned off"
			if secs > 0 {
				text = "disappearing messages set to " + humanDuration(secs)
			}
			m := &archive.Message{ChatJID: chatStr, ID: info.ID, SenderJID: sender.String(), FromMe: info.IsFromMe,
				Timestamp: ts, Kind: "system", Text: text, Source: source}
			return m, chat, b.store(m, kind)
		}
		return nil, chat, nil
	}
	if r := msg.GetReactionMessage(); r != nil {
		rts := r.GetSenderTimestampMS() / 1000
		if rts == 0 {
			rts = ts
		}
		return nil, chat, b.tx.SetReaction(chatStr, r.GetKey().GetID(), sender.String(), r.GetText(), rts)
	}

	c := Extract(msg)
	if c.Kind == "" {
		return nil, chat, nil
	}
	m := &archive.Message{
		ChatJID: chatStr, ID: info.ID, SenderJID: sender.String(), FromMe: info.IsFromMe, Timestamp: ts,
		Kind: c.Kind, Text: c.Text, PushName: info.PushName,
		QuotedID: c.QuotedID, QuotedSender: b.canonString(c.QuotedSender), QuotedText: c.QuotedText,
		Forwarded: c.Forwarded, ViewOnce: evt.IsViewOnce,
		MimeType: c.MimeType, FileName: c.FileName, FileSize: c.FileSize, Duration: c.Duration, HasMedia: c.HasMedia,
		Source: source,
	}
	if source == "sent" {
		m.Ack = 1
	}
	if c.KeepRaw() {
		m.Raw = RawForArchive(msg)
	}
	if err := b.store(m, kind); err != nil {
		return nil, chat, err
	}
	if !info.IsFromMe && info.PushName != "" && (sender.Server == types.DefaultUserServer || sender.Server == types.HiddenUserServer) {
		if err := b.tx.UpsertContact(archive.Contact{JID: sender.String(), PushName: info.PushName}); err != nil {
			return nil, chat, err
		}
	}
	if vn := info.VerifiedName; vn != nil && vn.Details.GetVerifiedName() != "" {
		if err := b.tx.UpsertContact(archive.Contact{JID: sender.String(), BusinessName: vn.Details.GetVerifiedName()}); err != nil {
			return nil, chat, err
		}
	}
	return m, chat, nil
}

func (b *batch) store(m *archive.Message, kind string) error {
	if err := b.tx.UpsertMessage(m); err != nil {
		return fmt.Errorf("store message %s/%s: %w", m.ChatJID, m.ID, err)
	}
	return b.tx.TouchChat(m.ChatJID, kind, m.Timestamp)
}

func (b *batch) conversation(conv *waHistorySync.Conversation, parse ParseFunc, st *HistoryStats) error {
	raw, err := types.ParseJID(conv.GetID())
	if err != nil {
		st.Skipped++
		return nil
	}
	b.learnStrings(conv.GetLidJID(), conv.GetPnJID())
	var alt types.JID
	if raw.Server == types.HiddenUserServer && conv.GetPnJID() != "" {
		alt, _ = types.ParseJID(conv.GetPnJID())
	}
	chat := b.canon(raw, alt)
	kind := chatKind(chat)
	st.Conversations++
	err = b.tx.UpsertChatMeta(archive.ChatMeta{
		JID: chat.String(), Kind: kind,
		Name:             firstNonEmpty(conv.GetName(), conv.GetDisplayName(), conv.GetUsername()),
		LastMessageAt:    int64(max(conv.GetConversationTimestamp(), conv.GetLastMsgTimestamp())),
		UnreadCount:      int64(conv.GetUnreadCount()),
		Archived:         conv.GetArchived(),
		Pinned:           conv.GetPinned() > 0,
		MarkedUnread:     conv.GetMarkedAsUnread(),
		MutedUntil:       MuteUntil(int64(min(conv.GetMuteEndTime(), math.MaxInt64))),
		EphemeralSeconds: int64(conv.GetEphemeralExpiration()),
	})
	if err != nil {
		return err
	}
	if chat.Server == types.GroupServer {
		g := archive.Group{JID: chat.String(), Name: conv.GetName(), Topic: conv.GetDescription(),
			CreatedAt: int64(conv.GetCreatedAt()), OwnerJID: b.canonString(conv.GetCreatedBy()),
			IsCommunity: conv.GetIsParentGroup(), ParentJID: conv.GetParentGroupID()}
		var ps []archive.Participant
		for _, p := range conv.GetParticipant() {
			if j := b.canonString(p.GetUserJID()); j != "" {
				ps = append(ps, archive.Participant{GroupJID: g.JID, JID: j, Role: rankRole(p.GetRank())})
			}
		}
		g.ParticipantCount = len(ps)
		if err := b.tx.MergeGroupFromHistory(g, ps); err != nil {
			return err
		}
	}
	for _, hm := range conv.GetMessages() {
		if err := b.webMessage(raw, hm.GetMessage(), parse, st); err != nil {
			return err
		}
	}
	return nil
}

func (b *batch) webMessage(chat types.JID, wm *waWeb.WebMessageInfo, parse ParseFunc, st *HistoryStats) error {
	if wm == nil || wm.GetKey().GetID() == "" {
		return nil
	}
	if wm.GetMessage() == nil {
		return b.stub(chat, wm, st)
	}
	evt, err := parse(chat, wm)
	if err != nil {
		st.Skipped++
		b.in.log.Debug("skip history message", "chat", chat, "id", wm.GetKey().GetID(), "err", err)
		return nil
	}
	m, canonChat, err := b.message(evt, "history")
	if err != nil {
		return err
	}
	if m == nil {
		return nil
	}
	st.Messages++
	for _, r := range wm.GetReactions() {
		sender := b.reactionSender(r.GetKey().GetFromMe(), r.GetKey().GetParticipant(), canonChat)
		if sender == "" {
			continue
		}
		if err := b.tx.SetReaction(canonChat.String(), evt.Info.ID, sender, r.GetText(), r.GetSenderTimestampMS()/1000); err != nil {
			return err
		}
	}
	if wm.GetStarred() {
		return b.tx.SetStarred(canonChat.String(), evt.Info.ID, true)
	}
	return nil
}

func (b *batch) reactionSender(fromMe bool, participant string, chat types.JID) string {
	switch {
	case fromMe:
		return b.in.self().String()
	case participant != "":
		return b.canonString(participant)
	case chatKind(chat) == "direct":
		return chat.String()
	}
	return ""
}

// stub stores a system message ("X added Y", "missed voice call", deletions).
func (b *batch) stub(chat types.JID, wm *waWeb.WebMessageInfo, st *HistoryStats) error {
	t := wm.GetMessageStubType()
	name := t.String()
	if !keepStub(name) {
		st.Skipped++
		return nil
	}
	key := wm.GetKey()
	c := b.canon(chat, types.EmptyJID)
	kind := chatKind(c)
	var sender string
	switch {
	case key.GetFromMe():
		sender = b.in.self().String()
	case wm.GetParticipant() != "":
		sender = b.canonString(wm.GetParticipant())
	case key.GetParticipant() != "":
		sender = b.canonString(key.GetParticipant())
	case kind == "direct":
		sender = c.String()
	}
	ts := int64(wm.GetMessageTimestamp())
	st.Messages++
	if t == waWeb.WebMessageInfo_REVOKE {
		path, err := b.tx.RevokeMessage(c.String(), key.GetID(), sender, key.GetFromMe(), ts, "history", sender)
		if path != "" {
			b.removed = append(b.removed, path)
		}
		if err != nil {
			return err
		}
		return b.tx.TouchChat(c.String(), kind, ts)
	}
	params := make([]string, 0, len(wm.GetMessageStubParameters()))
	for _, p := range wm.GetMessageStubParameters() {
		if strings.Contains(p, "@") {
			p = b.canonString(p)
		}
		params = append(params, p)
	}
	text := strings.ToLower(strings.ReplaceAll(name, "_", " "))
	if len(params) > 0 {
		text += ": " + strings.Join(params, ", ")
	}
	return b.store(&archive.Message{ChatJID: c.String(), ID: key.GetID(), SenderJID: sender, FromMe: key.GetFromMe(),
		Timestamp: ts, Kind: "system", Text: text, Source: "history"}, kind)
}

func keepStub(name string) bool {
	switch name {
	case "REVOKE", "INDIVIDUAL_CHANGE_NUMBER", "CHANGE_EPHEMERAL_SETTING", "BLOCK_CONTACT":
		return true
	}
	for _, p := range []string{"GROUP_", "COMMUNITY_", "CALL_MISSED", "BROADCAST_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (b *batch) group(g *types.GroupInfo) error {
	ag := archive.Group{
		JID: g.JID.String(), Name: g.Name, Topic: g.Topic,
		OwnerJID:    b.canon(g.OwnerJID, g.OwnerPN).String(),
		CreatedAt:   unixOrZero(g.GroupCreated),
		Announce:    g.IsAnnounce,
		Locked:      g.IsLocked,
		IsCommunity: g.IsParent,
		ParentJID:   jidString(g.LinkedParentJID),
		IsMember:    true,
	}
	ps := make([]archive.Participant, 0, len(g.Participants))
	for _, p := range g.Participants {
		if !p.LID.IsEmpty() && !p.PhoneNumber.IsEmpty() {
			b.learn(p.LID, p.PhoneNumber)
		}
		role := "member"
		if p.IsSuperAdmin {
			role = "superadmin"
		} else if p.IsAdmin {
			role = "admin"
		}
		ps = append(ps, archive.Participant{GroupJID: ag.JID, JID: b.canon(p.JID, p.PhoneNumber).String(), Role: role})
	}
	ag.ParticipantCount = max(g.ParticipantCount, len(ps))
	return b.tx.UpsertGroup(ag, ps)
}

func (b *batch) callRecord(r *waSyncAction.CallLogRecord) error {
	if r.GetCallID() == "" {
		return nil
	}
	creator := b.canonString(r.GetCallCreatorJID())
	var parts []string
	for _, p := range r.GetParticipants() {
		if j := b.canonString(p.GetUserJID()); j != "" {
			parts = append(parts, j)
		}
	}
	peer := r.GetGroupJID()
	if peer == "" {
		if r.GetIsIncoming() {
			peer = creator
		} else if len(parts) > 0 {
			peer = parts[0]
		}
	}
	start := r.GetStartTime()
	if start > 1e12 { // milliseconds
		start /= 1000
	}
	dur := r.GetDuration()
	if dur > 86400 { // milliseconds
		dur /= 1000
	}
	return b.tx.UpsertCall(archive.Call{
		ID: r.GetCallID(), ChatJID: peer, CreatorJID: creator, FromMe: !r.GetIsIncoming(), Video: r.GetIsVideo(),
		IsGroup: r.GetGroupJID() != "" || len(parts) > 1, StartedAt: start, Duration: dur,
		Result: strings.ToLower(r.GetCallResult().String()), Participants: parts, Source: "history",
	})
}

// ChatKind classifies a chat JID.
func ChatKind(j types.JID) string { return chatKind(j) }

func chatKind(j types.JID) string {
	switch j.Server {
	case types.GroupServer:
		return "group"
	case types.BroadcastServer:
		if j.User == types.StatusBroadcastJID.User {
			return "status"
		}
		return "broadcast"
	case types.NewsletterServer:
		return "channel"
	case types.BotServer:
		return "bot"
	}
	return "direct"
}

// MuteUntil normalizes a mute end timestamp to seconds; -1 means forever.
func MuteUntil(v int64) int64 {
	switch {
	case v < 0 || v > 1e15:
		return -1
	case v > 1e12:
		return v / 1000
	}
	return v
}

func rankRole(r waHistorySync.GroupParticipant_Rank) string {
	switch r {
	case waHistorySync.GroupParticipant_SUPERADMIN:
		return "superadmin"
	case waHistorySync.GroupParticipant_ADMIN:
		return "admin"
	}
	return "member"
}

func humanDuration(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d hours", d/time.Hour)
	}
	return d.String()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func jidString(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return j.String()
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
