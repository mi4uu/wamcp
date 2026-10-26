package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"
)

var messageColNames = []string{"pk", "chat_jid", "id", "sender_jid", "from_me", "timestamp", "kind", "text",
	"push_name", "quoted_id", "quoted_sender", "quoted_text", "forwarded", "view_once", "starred",
	"edited_at", "revoked_at", "mime_type", "file_name", "file_size", "duration",
	"has_media", "ack", "media_path", "media_error", "source", "revoked_by"}

// messageColumns lists the columns read by scanMessage, prefixed with alias.
func messageColumns(alias string) string {
	cols := make([]string, len(messageColNames))
	for i, c := range messageColNames {
		c = alias + c
		if strings.HasSuffix(c, "edited_at") || strings.HasSuffix(c, "revoked_at") {
			c = "COALESCE(" + c + ", 0)"
		}
		cols[i] = c
	}
	return strings.Join(cols, ", ")
}

var messageCols = messageColumns("")

func scanMessage(sc interface{ Scan(...any) error }, withRaw bool) (Message, error) {
	var m Message
	dest := []any{&m.PK, &m.ChatJID, &m.ID, &m.SenderJID, &m.FromMe, &m.Timestamp, &m.Kind, &m.Text, &m.PushName,
		&m.QuotedID, &m.QuotedSender, &m.QuotedText, &m.Forwarded, &m.ViewOnce, &m.Starred,
		&m.EditedAt, &m.RevokedAt, &m.MimeType, &m.FileName, &m.FileSize, &m.Duration,
		&m.HasMedia, &m.Ack, &m.MediaPath, &m.MediaError, &m.Source, &m.RevokedBy}
	if withRaw {
		dest = append(dest, &m.Raw)
	}
	return m, sc.Scan(dest...)
}

func (s *Store) queryMessages(ctx context.Context, query string, args ...any) ([]Message, error) {
	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Message returns one message including its raw payload.
func (s *Store) Message(ctx context.Context, chat, id string) (*Message, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+messageCols+`, raw FROM messages WHERE chat_jid = ? AND id = ?`, chat, id)
	m, err := scanMessage(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// MessageQuery selects messages; zero values mean "no filter".
type MessageQuery struct {
	ChatJID   string
	SenderJID string
	ChatKind  string // direct, group, ... (joins chats)
	After     int64  // timestamp >= After
	Before    int64  // timestamp < Before
	// BeforePK combined with Before pages strictly before the message
	// (Before, BeforePK), so equal timestamps are neither skipped nor repeated.
	BeforePK int64
	// AfterPK combined with After pages strictly after the message (After, AfterPK).
	AfterPK       int64
	ChatJIDs      []string // restrict to these chats
	ExcludeFromMe bool
	MediaOnly     bool
	Starred       bool
	Limit         int
	// OldestFirst selects the oldest messages of the range instead of the
	// newest ones. Results are always returned in chronological order.
	OldestFirst bool
}

// Messages returns messages matching q in chronological order.
func (s *Store) Messages(ctx context.Context, q MessageQuery) ([]Message, error) {
	var where []string
	var args []any
	if q.ChatJID != "" {
		where = append(where, "chat_jid = ?")
		args = append(args, q.ChatJID)
	}
	if q.SenderJID != "" {
		where = append(where, "sender_jid = ?")
		args = append(args, q.SenderJID)
	}
	if q.ChatKind != "" {
		where = append(where, "chat_jid IN (SELECT jid FROM chats WHERE kind = ?)")
		args = append(args, q.ChatKind)
	}
	if len(q.ChatJIDs) > 0 {
		list, _ := json.Marshal(q.ChatJIDs)
		where = append(where, "chat_jid IN (SELECT value FROM json_each(?))")
		args = append(args, string(list))
	}
	if q.ExcludeFromMe {
		where = append(where, "from_me = 0")
	}
	switch {
	case q.After != 0 && q.AfterPK != 0:
		where = append(where, "(timestamp > ? OR (timestamp = ? AND pk > ?))")
		args = append(args, q.After, q.After, q.AfterPK)
	case q.After != 0:
		where = append(where, "timestamp >= ?")
		args = append(args, q.After)
	}
	switch {
	case q.Before != 0 && q.BeforePK != 0:
		where = append(where, "(timestamp < ? OR (timestamp = ? AND pk < ?))")
		args = append(args, q.Before, q.Before, q.BeforePK)
	case q.Before != 0:
		where = append(where, "timestamp < ?")
		args = append(args, q.Before)
	}
	if q.MediaOnly {
		where = append(where, "has_media = 1")
	}
	if q.Starred {
		where = append(where, "starred = 1")
	}
	query := `SELECT ` + messageCols + ` FROM messages`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if q.OldestFirst {
		query += " ORDER BY timestamp ASC, pk ASC"
	} else {
		query += " ORDER BY timestamp DESC, pk DESC"
	}
	query += " LIMIT ?"
	args = append(args, clamp(q.Limit, 50, 5000))
	msgs, err := s.queryMessages(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if !q.OldestFirst {
		slices.Reverse(msgs)
	}
	return msgs, nil
}

// SearchQuery is a full-text search over message text, captions and file names.
type SearchQuery struct {
	Text      string
	ChatJID   string
	SenderJID string
	After     int64
	Before    int64
	Limit     int
	Offset    int
}

// Search returns matching messages, newest first.
func (s *Store) Search(ctx context.Context, q SearchQuery) ([]Message, error) {
	match, err := MatchQuery(q.Text)
	if err != nil {
		return nil, err
	}
	where := []string{"messages_fts MATCH ?"}
	args := []any{match}
	if q.ChatJID != "" {
		where = append(where, "m.chat_jid = ?")
		args = append(args, q.ChatJID)
	}
	if q.SenderJID != "" {
		where = append(where, "m.sender_jid = ?")
		args = append(args, q.SenderJID)
	}
	if q.After != 0 {
		where = append(where, "m.timestamp >= ?")
		args = append(args, q.After)
	}
	if q.Before != 0 {
		where = append(where, "m.timestamp < ?")
		args = append(args, q.Before)
	}
	cols := messageColumns("m.")
	query := `SELECT ` + cols + ` FROM messages_fts JOIN messages m ON m.pk = messages_fts.rowid WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY m.timestamp DESC, m.pk DESC LIMIT ? OFFSET ?`
	args = append(args, clamp(q.Limit, 30, 1000), max(q.Offset, 0))
	return s.queryMessages(ctx, query, args...)
}

// MessageContext returns up to before messages preceding and after messages
// following the given message, including the message itself.
func (s *Store) MessageContext(ctx context.Context, chat, id string, before, after int) ([]Message, error) {
	target, err := s.Message(ctx, chat, id)
	if err != nil {
		return nil, err
	}
	target.Raw = nil
	older, err := s.queryMessages(ctx, `SELECT `+messageCols+` FROM messages
WHERE chat_jid = ? AND (timestamp < ? OR (timestamp = ? AND pk < ?))
ORDER BY timestamp DESC, pk DESC LIMIT ?`, chat, target.Timestamp, target.Timestamp, target.PK, clamp(before, 10, 200))
	if err != nil {
		return nil, err
	}
	newer, err := s.queryMessages(ctx, `SELECT `+messageCols+` FROM messages
WHERE chat_jid = ? AND (timestamp > ? OR (timestamp = ? AND pk > ?))
ORDER BY timestamp ASC, pk ASC LIMIT ?`, chat, target.Timestamp, target.Timestamp, target.PK, clamp(after, 10, 200))
	if err != nil {
		return nil, err
	}
	slices.Reverse(older)
	out := append(older, *target)
	return append(out, newer...), nil
}

// OldestMessage returns the oldest archived message of a chat.
func (s *Store) OldestMessage(ctx context.Context, chat string) (*Message, error) {
	msgs, err := s.Messages(ctx, MessageQuery{ChatJID: chat, Limit: 1, OldestFirst: true})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, ErrNotFound
	}
	return &msgs[0], nil
}

// Reaction is an emoji reaction to a message.
type Reaction struct {
	MessageID string
	SenderJID string
	Emoji     string
	Timestamp int64
}

// Reactions returns reactions for the given messages of one chat, keyed by message ID.
func (s *Store) Reactions(ctx context.Context, chat string, ids []string) (map[string][]Reaction, error) {
	out := map[string][]Reaction{}
	if len(ids) == 0 {
		return out, nil
	}
	list, _ := json.Marshal(ids)
	rows, err := s.read.QueryContext(ctx, `SELECT message_id, sender_jid, emoji, timestamp FROM reactions
WHERE chat_jid = ? AND message_id IN (SELECT value FROM json_each(?)) ORDER BY timestamp`, chat, string(list))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Reaction
		if err := rows.Scan(&r.MessageID, &r.SenderJID, &r.Emoji, &r.Timestamp); err != nil {
			return nil, err
		}
		out[r.MessageID] = append(out[r.MessageID], r)
	}
	return out, rows.Err()
}

// PendingMedia lists live messages since `since` whose media was never
// downloaded (nor failed), up to maxBytes each, as (chat, id) pairs.
func (s *Store) PendingMedia(ctx context.Context, since, maxBytes int64, limit int) ([][2]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT chat_jid, id FROM messages
WHERE has_media = 1 AND media_path = '' AND media_error = '' AND source = 'live' AND timestamp >= ? AND file_size <= ?
ORDER BY timestamp DESC LIMIT ?`, since, maxBytes, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Version is an earlier text of an edited message.
type Version struct {
	Text       string
	ReplacedAt int64
}

// EditHistory returns earlier versions of the given messages, oldest first.
func (s *Store) EditHistory(ctx context.Context, chat string, ids []string) (map[string][]Version, error) {
	out := map[string][]Version{}
	if len(ids) == 0 {
		return out, nil
	}
	list, _ := json.Marshal(ids)
	rows, err := s.read.QueryContext(ctx, `SELECT message_id, text, replaced_at FROM message_edits
WHERE chat_jid = ? AND message_id IN (SELECT value FROM json_each(?)) ORDER BY replaced_at, pk`, chat, string(list))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var v Version
		if err := rows.Scan(&id, &v.Text, &v.ReplacedAt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], v)
	}
	return out, rows.Err()
}

// Chat is a conversation with its state and activity summary.
type Chat struct {
	JID              string
	Kind             string
	Name             string // name reported by WhatsApp; see Directory for display names
	LastMessageAt    int64
	UnreadCount      int64
	Archived         bool
	Pinned           bool
	MarkedUnread     bool
	MutedUntil       int64
	EphemeralSeconds int64
	MessageCount     int64
	LastText         string
	LastKind         string
	LastSender       string
	LastFromMe       bool
	LastDeleted      bool
}

// Chats returns all chats with message counts and a preview of the last message.
func (s *Store) Chats(ctx context.Context) ([]Chat, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT c.jid, c.kind, COALESCE(c.name, ''), COALESCE(c.last_message_at, 0), COALESCE(c.unread_count, 0),
	c.archived, c.pinned, c.marked_unread, COALESCE(c.muted_until, 0), COALESCE(c.ephemeral_seconds, 0),
	(SELECT COUNT(*) FROM messages WHERE chat_jid = c.jid),
	COALESCE(l.text, ''), COALESCE(l.kind, ''), COALESCE(l.sender_jid, ''), COALESCE(l.from_me, 0),
	COALESCE(l.revoked_at IS NOT NULL, 0)
FROM chats c
LEFT JOIN messages l ON l.pk = (SELECT pk FROM messages WHERE chat_jid = c.jid ORDER BY timestamp DESC, pk DESC LIMIT 1)
ORDER BY COALESCE(c.last_message_at, 0) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.JID, &c.Kind, &c.Name, &c.LastMessageAt, &c.UnreadCount, &c.Archived, &c.Pinned,
			&c.MarkedUnread, &c.MutedUntil, &c.EphemeralSeconds, &c.MessageCount,
			&c.LastText, &c.LastKind, &c.LastSender, &c.LastFromMe, &c.LastDeleted); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Chat returns one chat.
func (s *Store) Chat(ctx context.Context, jid string) (*Chat, error) {
	chats, err := s.Chats(ctx)
	if err != nil {
		return nil, err
	}
	for i := range chats {
		if chats[i].JID == jid {
			return &chats[i], nil
		}
	}
	return nil, ErrNotFound
}

// SenderCount is the number of messages one participant sent.
type SenderCount struct {
	JID   string
	Count int64
}

// Activity summarizes a chat (or a time range of it).
type Activity struct {
	Messages int64
	First    int64
	Last     int64
	Media    int64
	Senders  []SenderCount
}

// ChatActivity returns message counts and the most active senders of a chat.
func (s *Store) ChatActivity(ctx context.Context, chat string, after, before int64, topSenders int) (*Activity, error) {
	cond := "chat_jid = ?"
	args := []any{chat}
	if after != 0 {
		cond += " AND timestamp >= ?"
		args = append(args, after)
	}
	if before != 0 {
		cond += " AND timestamp < ?"
		args = append(args, before)
	}
	var a Activity
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(timestamp), 0), COALESCE(MAX(timestamp), 0),
	COALESCE(SUM(has_media), 0) FROM messages WHERE `+cond+` AND kind NOT IN ('system', 'revoked')`, args...).
		Scan(&a.Messages, &a.First, &a.Last, &a.Media)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx, `SELECT sender_jid, COUNT(*) AS n FROM messages WHERE `+cond+
		` AND kind NOT IN ('system', 'revoked') GROUP BY sender_jid ORDER BY n DESC LIMIT ?`, append(args, clamp(topSenders, 10, 1000))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc SenderCount
		if err := rows.Scan(&sc.JID, &sc.Count); err != nil {
			return nil, err
		}
		a.Senders = append(a.Senders, sc)
	}
	return &a, rows.Err()
}

// Contacts returns all known contacts.
func (s *Store) Contacts(ctx context.Context) ([]Contact, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT jid, COALESCE(full_name, ''), COALESCE(first_name, ''),
	COALESCE(push_name, ''), COALESCE(business_name, ''), updated_at FROM contacts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.JID, &c.FullName, &c.FirstName, &c.PushName, &c.BusinessName, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LIDMappings returns all known LID → phone JID mappings.
func (s *Store) LIDMappings(ctx context.Context) (map[string]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT lid, pn FROM lid_map`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var lid, pn string
		if err := rows.Scan(&lid, &pn); err != nil {
			return nil, err
		}
		out[lid] = pn
	}
	return out, rows.Err()
}

// UnresolvedLIDs lists LIDs still used as identifiers somewhere in the archive.
func (s *Store) UnresolvedLIDs(ctx context.Context) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT jid FROM chats WHERE jid LIKE '%@lid'
UNION SELECT sender_jid FROM messages WHERE sender_jid LIKE '%@lid'
UNION SELECT jid FROM contacts WHERE jid LIKE '%@lid'
UNION SELECT jid FROM group_participants WHERE jid LIKE '%@lid'
UNION SELECT sender_jid FROM reactions WHERE sender_jid LIKE '%@lid'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Groups returns all known groups, including ones the account has left.
func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT jid, COALESCE(name, ''), COALESCE(topic, ''), COALESCE(owner_jid, ''),
	COALESCE(created_at, 0), announce, locked, is_community, COALESCE(parent_jid, ''), participant_count,
	is_member, updated_at FROM groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.JID, &g.Name, &g.Topic, &g.OwnerJID, &g.CreatedAt, &g.Announce, &g.Locked,
			&g.IsCommunity, &g.ParentJID, &g.ParticipantCount, &g.IsMember, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Participants returns the members of a group, admins first.
func (s *Store) Participants(ctx context.Context, group string) ([]Participant, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT group_jid, jid, role FROM group_participants WHERE group_jid = ?
ORDER BY CASE role WHEN 'superadmin' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, jid`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.GroupJID, &p.JID, &p.Role); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupsOf returns the JIDs of groups in which jid is a participant.
func (s *Store) GroupsOf(ctx context.Context, jid string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT group_jid FROM group_participants WHERE jid = ?`, jid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CallQuery selects call log entries.
type CallQuery struct {
	ChatJID string
	After   int64
	Limit   int
}

// Calls returns call log entries, newest first.
func (s *Store) Calls(ctx context.Context, q CallQuery) ([]Call, error) {
	query := `SELECT id, chat_jid, creator_jid, from_me, video, is_group, started_at, COALESCE(accepted_at, 0),
	duration, result, participants, source FROM calls WHERE 1 = 1`
	var args []any
	if q.ChatJID != "" {
		query += " AND (chat_jid = ? OR creator_jid = ?)"
		args = append(args, q.ChatJID, q.ChatJID)
	}
	if q.After != 0 {
		query += " AND started_at >= ?"
		args = append(args, q.After)
	}
	query += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, clamp(q.Limit, 30, 1000))
	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Call
	for rows.Next() {
		var c Call
		var parts string
		if err := rows.Scan(&c.ID, &c.ChatJID, &c.CreatorJID, &c.FromMe, &c.Video, &c.IsGroup, &c.StartedAt,
			&c.AcceptedAt, &c.Duration, &c.Result, &parts, &c.Source); err != nil {
			return nil, err
		}
		if parts != "" {
			c.Participants = strings.Split(parts, ",")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Meta returns a stored value, or "" when missing.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Stats are archive-wide totals.
type Stats struct {
	Messages   int64
	Chats      int64
	Groups     int64
	Contacts   int64
	Calls      int64
	Media      int64 // messages with media
	Downloaded int64 // media saved locally
	Oldest     int64
	Newest     int64
	DBBytes    int64
}

// Stats returns archive-wide totals.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	var st Stats
	err := s.read.QueryRowContext(ctx, `SELECT
	(SELECT COUNT(*) FROM messages),
	(SELECT COUNT(*) FROM chats),
	(SELECT COUNT(*) FROM groups WHERE is_member = 1),
	(SELECT COUNT(*) FROM contacts),
	(SELECT COUNT(*) FROM calls),
	(SELECT COUNT(*) FROM messages WHERE has_media = 1),
	(SELECT COUNT(*) FROM messages WHERE media_path <> ''),
	(SELECT COALESCE(MIN(timestamp), 0) FROM messages),
	(SELECT COALESCE(MAX(timestamp), 0) FROM messages)`).
		Scan(&st.Messages, &st.Chats, &st.Groups, &st.Contacts, &st.Calls, &st.Media, &st.Downloaded, &st.Oldest, &st.Newest)
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			st.DBBytes += fi.Size()
		}
	}
	return &st, nil
}

// DaemonStatus is the live state of the sync daemon. The daemon persists it
// in the meta table so the stdio MCP server can report it too.
type DaemonStatus struct {
	State       string        `json:"state"`
	Account     string        `json:"account,omitempty"`
	PushName    string        `json:"push_name,omitempty"`
	PairCode    string        `json:"pair_code,omitempty"`
	LastError   string        `json:"last_error,omitempty"`
	StartedAt   time.Time     `json:"started_at,omitzero"`
	ConnectedAt time.Time     `json:"connected_at,omitzero"`
	LastEventAt time.Time     `json:"last_event_at,omitzero"`
	UpdatedAt   time.Time     `json:"updated_at,omitzero"`
	History     HistoryStatus `json:"history"`
	CatchUp     CatchUpStatus `json:"catch_up"`
}

// CatchUpStatus describes the last reconnect: how long the daemon was offline
// and how many queued messages the server delivered afterwards.
type CatchUpStatus struct {
	OfflineSeconds int64     `json:"offline_seconds"`
	Expected       int       `json:"expected"` // messages the server announced
	Received       int       `json:"received"` // messages stored while catching up
	Done           bool      `json:"done"`
	CompletedAt    time.Time `json:"completed_at,omitzero"`
}

// HistoryStatus tracks history sync batches received from the phone.
type HistoryStatus struct {
	Batches      int       `json:"batches"`
	Messages     int       `json:"messages"`
	LastType     string    `json:"last_type,omitempty"`
	LastProgress int       `json:"last_progress"`
	LastAt       time.Time `json:"last_at,omitzero"`
}

// MetaDaemonStatus is the meta key holding the JSON-encoded DaemonStatus.
const MetaDaemonStatus = "daemon.status"

// MetaAccount is the meta key holding the paired account JID.
const MetaAccount = "account.jid"

// MetaLastOnline is the meta key holding the last time (unix) the daemon was connected.
const MetaLastOnline = "daemon.last_online"

// DaemonStatus reads the status last persisted by the daemon.
func (s *Store) DaemonStatus(ctx context.Context) (*DaemonStatus, error) {
	raw, err := s.Meta(ctx, MetaDaemonStatus)
	if err != nil || raw == "" {
		return nil, err
	}
	var st DaemonStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func clamp(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(v, maxV)
}
