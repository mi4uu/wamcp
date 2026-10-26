// Package archive stores everything synced from WhatsApp in a local SQLite
// database with full-text search, and answers the queries behind the MCP tools
// and the web viewer.
package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite-backed archive.
//
// Writes go through a single-connection pool so they are serialized; reads use
// a separate pool and never block on writers thanks to WAL mode.
type Store struct {
	path  string
	read  *sql.DB
	write *sql.DB // nil when opened read-only
	// keepDeleted keeps the content of messages deleted for everyone and only
	// marks them (revoked_at/revoked_by); otherwise their content is wiped.
	keepDeleted bool
}

// SetKeepDeleted chooses whether messages deleted for everyone keep their
// content (true) or are wiped like in WhatsApp (false).
func (s *Store) SetKeepDeleted(keep bool) { s.keepDeleted = keep }

const basePragmas = "_pragma=busy_timeout(10000)"

// Open opens (creating if needed) the archive at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w, err := openWriter(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, write: w}
	if err := s.migrate(ctx); err != nil {
		_ = w.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	if s.read, err = openReader(path); err != nil {
		_ = w.Close()
		return nil, err
	}
	return s, nil
}

// OpenExisting opens an archive created by the daemon without running
// migrations. The stdio MCP server uses it; writes (contact annotations) are
// rare and coordinate with the daemon through SQLite locking.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("archive %s not found; start the daemon with `wamcp serve` first", path)
	}
	w, err := openWriter(path)
	if err != nil {
		return nil, err
	}
	var v int
	if err := w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		_ = w.Close()
		return nil, err
	}
	if v != len(migrations) {
		_ = w.Close()
		return nil, fmt.Errorf("archive schema is v%d but this binary expects v%d; run the matching `wamcp serve` once", v, len(migrations))
	}
	r, err := openReader(path)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	return &Store{path: path, read: r, write: w}, nil
}

func openWriter(path string) (*sql.DB, error) {
	w, err := sql.Open("sqlite", "file:"+path+"?"+basePragmas+
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	return w, nil
}

func openReader(path string) (*sql.DB, error) {
	r, err := sql.Open("sqlite", "file:"+path+"?"+basePragmas+"&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	r.SetMaxOpenConns(4)
	return r, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes both connection pools.
func (s *Store) Close() error {
	var errs []error
	if s.read != nil {
		errs = append(errs, s.read.Close())
	}
	if s.write != nil {
		errs = append(errs, s.write.Close())
	}
	return errors.Join(errs...)
}

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("archive schema v%d is newer than this binary supports (v%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Tx is a write transaction on the archive.
type Tx struct {
	tx   *sql.Tx
	ctx  context.Context
	now  int64
	keep bool // keep content of deleted messages
}

// Write runs fn inside one IMMEDIATE transaction and commits if fn succeeds.
func (s *Store) Write(ctx context.Context, fn func(*Tx) error) error {
	if s.write == nil {
		return errors.New("archive is open read-only")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Tx{tx: tx, ctx: ctx, now: time.Now().Unix(), keep: s.keepDeleted}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (t *Tx) exec(query string, args ...any) error {
	_, err := t.tx.ExecContext(t.ctx, query, args...)
	return err
}

// Message is one archived WhatsApp message.
type Message struct {
	PK           int64
	ChatJID      string
	ID           string
	SenderJID    string
	FromMe       bool
	Timestamp    int64
	Kind         string
	Text         string
	PushName     string
	QuotedID     string
	QuotedSender string
	QuotedText   string
	Forwarded    bool
	ViewOnce     bool
	Starred      bool
	EditedAt     int64
	RevokedAt    int64
	RevokedBy    string // who deleted it for everyone (sender or a group admin)
	MimeType     string
	FileName     string
	FileSize     int64
	Duration     int64
	HasMedia     bool
	Ack          int // own messages: 1 sent, 2 delivered, 3 read, 4 played
	MediaPath    string
	MediaError   string
	Raw          []byte
	Source       string
}

// Deleted reports whether the sender deleted the message for everyone.
func (m *Message) Deleted() bool { return m.RevokedAt != 0 }

const upsertMessageSQL = `
INSERT INTO messages (chat_jid, id, sender_jid, from_me, timestamp, kind, text, push_name,
	quoted_id, quoted_sender, quoted_text, forwarded, view_once, starred,
	mime_type, file_name, file_size, duration, has_media, ack, raw, source, inserted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (chat_jid, id) DO UPDATE SET
	sender_jid    = CASE WHEN excluded.sender_jid <> '' THEN excluded.sender_jid ELSE messages.sender_jid END,
	timestamp     = CASE WHEN excluded.timestamp > 0 AND excluded.timestamp < messages.timestamp
	                     THEN excluded.timestamp ELSE messages.timestamp END,
	push_name     = CASE WHEN excluded.push_name <> '' THEN excluded.push_name ELSE messages.push_name END,
	kind          = CASE WHEN messages.edited_at IS NULL AND messages.revoked_at IS NULL
	                     THEN excluded.kind ELSE messages.kind END,
	text          = CASE WHEN messages.edited_at IS NULL AND messages.revoked_at IS NULL
	                     THEN excluded.text ELSE messages.text END,
	quoted_id     = CASE WHEN excluded.quoted_id <> '' THEN excluded.quoted_id ELSE messages.quoted_id END,
	quoted_sender = CASE WHEN excluded.quoted_sender <> '' THEN excluded.quoted_sender ELSE messages.quoted_sender END,
	quoted_text   = CASE WHEN excluded.quoted_text <> '' AND messages.revoked_at IS NULL
	                     THEN excluded.quoted_text ELSE messages.quoted_text END,
	forwarded     = MAX(messages.forwarded, excluded.forwarded),
	view_once     = MAX(messages.view_once, excluded.view_once),
	starred       = MAX(messages.starred, excluded.starred),
	mime_type     = CASE WHEN excluded.mime_type <> '' THEN excluded.mime_type ELSE messages.mime_type END,
	file_name     = CASE WHEN excluded.file_name <> '' AND messages.revoked_at IS NULL
	                     THEN excluded.file_name ELSE messages.file_name END,
	file_size     = MAX(messages.file_size, excluded.file_size),
	duration      = MAX(messages.duration, excluded.duration),
	has_media     = CASE WHEN messages.revoked_at IS NULL THEN MAX(messages.has_media, excluded.has_media) ELSE messages.has_media END,
	ack           = MAX(messages.ack, excluded.ack),
	raw           = CASE WHEN messages.revoked_at IS NULL AND excluded.raw IS NOT NULL
	                     THEN excluded.raw ELSE messages.raw END
RETURNING pk, text, file_name, revoked_at IS NOT NULL, kind, COALESCE(edited_at, 0)`

// UpsertMessage inserts a message or merges it into the stored copy. Content of
// edited or deleted messages is never overwritten by older copies (for example
// from a later history sync).
func (t *Tx) UpsertMessage(m *Message) error {
	if m.ChatJID == "" || m.ID == "" {
		return errors.New("message without chat or id")
	}
	var raw any
	if len(m.Raw) > 0 {
		raw = m.Raw
	}
	var pk, editedAt int64
	var text, fileName, kind string
	var revoked bool
	err := t.tx.QueryRowContext(t.ctx, upsertMessageSQL,
		m.ChatJID, m.ID, m.SenderJID, m.FromMe, m.Timestamp, m.Kind, m.Text, m.PushName,
		m.QuotedID, m.QuotedSender, m.QuotedText, m.Forwarded, m.ViewOnce, m.Starred,
		m.MimeType, m.FileName, m.FileSize, m.Duration, m.HasMedia, m.Ack, raw, m.Source, t.now,
	).Scan(&pk, &text, &fileName, &revoked, &kind, &editedAt)
	if err != nil {
		return err
	}
	m.PK = pk
	if editedAt != 0 && m.Text != "" && m.Text != text && m.Kind != "revoked" {
		// A copy that differs from the edited text is an earlier version
		// (typically the original arriving after the edit).
		if err := t.addVersion(m.ChatJID, m.ID, m.Text, editedAt); err != nil {
			return err
		}
	}
	if t.keep && kind == "revoked" && m.Kind != "revoked" {
		// The deletion arrived before the content (tombstone); fill it in.
		err := t.exec(`UPDATE messages SET kind = ?, text = ?, quoted_id = ?, quoted_sender = ?, quoted_text = ?,
	mime_type = ?, file_name = ?, file_size = ?, duration = ?, has_media = ?, raw = ?,
	sender_jid = CASE WHEN sender_jid = '' THEN ? ELSE sender_jid END WHERE pk = ?`,
			m.Kind, m.Text, m.QuotedID, m.QuotedSender, m.QuotedText, m.MimeType, m.FileName, m.FileSize,
			m.Duration, m.HasMedia, raw, m.SenderJID, pk)
		if err != nil {
			return err
		}
		text, fileName = m.Text, m.FileName
	}
	return t.index(pk, m.ChatJID, m.ID, text, fileName, revoked && !t.keep, editedAt != 0)
}

// index refreshes the full-text entry of one message. Earlier versions of
// edited messages stay searchable.
func (t *Tx) index(pk int64, chat, id, text, fileName string, revoked, edited bool) error {
	if err := t.exec(`DELETE FROM messages_fts WHERE rowid = ?`, pk); err != nil {
		return err
	}
	if revoked {
		return nil
	}
	if edited {
		var versions sql.NullString
		err := t.tx.QueryRowContext(t.ctx, `SELECT group_concat(text, ' ') FROM message_edits WHERE chat_jid = ? AND message_id = ?`,
			chat, id).Scan(&versions)
		if err != nil {
			return err
		}
		text += " " + versions.String
	}
	body := strings.TrimSpace(SearchText(text + " " + fileName))
	if body == "" {
		return nil
	}
	return t.exec(`INSERT INTO messages_fts (rowid, body) VALUES (?, ?)`, pk, body)
}

// addVersion records an earlier text of an edited message (once per distinct text).
func (t *Tx) addVersion(chat, id, text string, replacedAt int64) error {
	return t.exec(`INSERT INTO message_edits (chat_jid, message_id, text, replaced_at)
SELECT ?, ?, ?, ? WHERE NOT EXISTS (
	SELECT 1 FROM message_edits WHERE chat_jid = ? AND message_id = ? AND text = ?)`,
		chat, id, text, replacedAt, chat, id, text)
}

// EditMessage applies an edit to message m.ID and keeps the previous text as
// an earlier version. When the original has not been archived yet, m is
// stored as a new row so the edit is not lost.
func (t *Tx) EditMessage(m *Message) error {
	var prev, kind string
	var prevEdited sql.NullInt64
	err := t.tx.QueryRowContext(t.ctx, `SELECT text, kind, edited_at FROM messages WHERE chat_jid = ? AND id = ?`,
		m.ChatJID, m.ID).Scan(&prev, &kind, &prevEdited)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if found && prevEdited.Valid && m.EditedAt < prevEdited.Int64 {
		// An older edit delivered late: keep it as an earlier version, replaced
		// by the newer edit that is already applied, and keep it searchable.
		if err := t.addVersion(m.ChatJID, m.ID, m.Text, prevEdited.Int64); err != nil {
			return err
		}
		var pk int64
		var fileName string
		var revoked bool
		err := t.tx.QueryRowContext(t.ctx, `SELECT pk, file_name, revoked_at IS NOT NULL FROM messages WHERE chat_jid = ? AND id = ?`,
			m.ChatJID, m.ID).Scan(&pk, &fileName, &revoked)
		if err != nil {
			return err
		}
		m.PK = pk
		return t.index(pk, m.ChatJID, m.ID, prev, fileName, revoked && !t.keep, true)
	}
	if found && prev != "" && prev != m.Text && kind != "revoked" {
		if err := t.addVersion(m.ChatJID, m.ID, prev, m.EditedAt); err != nil {
			return err
		}
	}
	var pk int64
	var text, fileName string
	var revoked bool
	err = t.tx.QueryRowContext(t.ctx, `
INSERT INTO messages (chat_jid, id, sender_jid, from_me, timestamp, kind, text, source, inserted_at, edited_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (chat_jid, id) DO UPDATE SET
	text      = CASE WHEN messages.revoked_at IS NULL THEN excluded.text ELSE messages.text END,
	edited_at = excluded.edited_at
RETURNING pk, text, file_name, revoked_at IS NOT NULL`,
		m.ChatJID, m.ID, m.SenderJID, m.FromMe, m.Timestamp, m.Kind, m.Text, m.Source, t.now, m.EditedAt,
	).Scan(&pk, &text, &fileName, &revoked)
	if err != nil {
		return err
	}
	m.PK = pk
	return t.index(pk, m.ChatJID, m.ID, text, fileName, revoked && !t.keep, true)
}

// RevokeMessage records a "delete for everyone" by revokedBy. By default
// (keep mode) the content stays and the message is only marked as deleted;
// otherwise the content is wiped and the path of a downloaded media file is
// returned for the caller to remove. Unknown messages become tombstones.
func (t *Tx) RevokeMessage(chat, id, sender string, fromMe bool, at int64, source, revokedBy string) (string, error) {
	if t.keep {
		return "", t.exec(`
INSERT INTO messages (chat_jid, id, sender_jid, from_me, timestamp, kind, source, inserted_at, revoked_at, revoked_by)
VALUES (?, ?, ?, ?, ?, 'revoked', ?, ?, ?, ?)
ON CONFLICT (chat_jid, id) DO UPDATE SET
	revoked_at = COALESCE(messages.revoked_at, excluded.revoked_at),
	revoked_by = CASE WHEN excluded.revoked_by <> '' THEN excluded.revoked_by ELSE messages.revoked_by END`,
			chat, id, sender, fromMe, at, source, t.now, at, revokedBy)
	}
	var mediaPath string
	err := t.tx.QueryRowContext(t.ctx, `SELECT media_path FROM messages WHERE chat_jid = ? AND id = ?`, chat, id).
		Scan(&mediaPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var pk int64
	err = t.tx.QueryRowContext(t.ctx, `
INSERT INTO messages (chat_jid, id, sender_jid, from_me, timestamp, kind, source, inserted_at, revoked_at, revoked_by)
VALUES (?, ?, ?, ?, ?, 'revoked', ?, ?, ?, ?)
ON CONFLICT (chat_jid, id) DO UPDATE SET
	revoked_at  = excluded.revoked_at,
	revoked_by  = excluded.revoked_by,
	text        = '',
	quoted_text = '',
	file_name   = '',
	raw         = NULL,
	has_media   = 0,
	media_path  = ''
RETURNING pk`,
		chat, id, sender, fromMe, at, source, t.now, at, revokedBy,
	).Scan(&pk)
	if err != nil {
		return "", err
	}
	return mediaPath, t.exec(`DELETE FROM messages_fts WHERE rowid = ?`, pk)
}

// SetAck raises the delivery state of own messages (never lowers it).
func (t *Tx) SetAck(chat string, ids []string, ack int) error {
	list, _ := json.Marshal(ids)
	return t.exec(`UPDATE messages SET ack = MAX(ack, ?) WHERE chat_jid = ? AND from_me = 1
	AND id IN (SELECT value FROM json_each(?))`, ack, chat, string(list))
}

// BumpUnread updates the unread counter for a newly received live message:
// incoming messages increment it, own messages (sent from any device) reset it.
func (t *Tx) BumpUnread(jid, kind string, fromMe bool) error {
	if fromMe {
		return t.SetChatFlag(jid, kind, "unread_count", 0)
	}
	return t.exec(`UPDATE chats SET unread_count = COALESCE(unread_count, 0) + 1, updated_at = ? WHERE jid = ?`, t.now, jid)
}

// SetStarred marks a message as starred or not.
func (t *Tx) SetStarred(chat, id string, starred bool) error {
	return t.exec(`UPDATE messages SET starred = ? WHERE chat_jid = ? AND id = ?`, starred, chat, id)
}

// SetReaction records the reaction of sender to a message; an empty emoji removes it.
func (t *Tx) SetReaction(chat, messageID, sender, emoji string, ts int64) error {
	if emoji == "" {
		return t.exec(`DELETE FROM reactions WHERE chat_jid = ? AND message_id = ? AND sender_jid = ?`,
			chat, messageID, sender)
	}
	return t.exec(`
INSERT INTO reactions (chat_jid, message_id, sender_jid, emoji, timestamp) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (chat_jid, message_id, sender_jid) DO UPDATE SET
	emoji = excluded.emoji, timestamp = excluded.timestamp
WHERE excluded.timestamp >= reactions.timestamp`, chat, messageID, sender, emoji, ts)
}

// TouchChat makes sure a chat row exists and bumps its last activity.
func (t *Tx) TouchChat(jid, kind string, ts int64) error {
	return t.exec(`
INSERT INTO chats (jid, kind, last_message_at, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (jid) DO UPDATE SET
	last_message_at = MAX(COALESCE(chats.last_message_at, 0), excluded.last_message_at),
	updated_at = excluded.updated_at`, jid, kind, ts, t.now)
}

// ChatMeta is chat state reported by the phone during history sync.
type ChatMeta struct {
	JID              string
	Kind             string
	Name             string
	LastMessageAt    int64
	UnreadCount      int64
	Archived         bool
	Pinned           bool
	MarkedUnread     bool
	MutedUntil       int64
	EphemeralSeconds int64
}

// UpsertChatMeta stores chat state from a history sync conversation.
func (t *Tx) UpsertChatMeta(c ChatMeta) error {
	return t.exec(`
INSERT INTO chats (jid, kind, name, last_message_at, unread_count, archived, pinned, marked_unread,
	muted_until, ephemeral_seconds, updated_at, unread_known)
VALUES (?, ?, NULLIF(?, ''), NULLIF(?, 0), ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), ?, 1)
ON CONFLICT (jid) DO UPDATE SET
	name              = COALESCE(excluded.name, chats.name),
	last_message_at   = MAX(COALESCE(chats.last_message_at, 0), COALESCE(excluded.last_message_at, 0)),
	-- The first snapshot sets the count (keeping live increments that arrived
	-- earlier); once known, older snapshots never resurrect a read badge.
	unread_count      = CASE WHEN chats.unread_known = 1 THEN chats.unread_count
	                         ELSE MAX(COALESCE(chats.unread_count, 0), excluded.unread_count) END,
	unread_known      = 1,
	archived          = excluded.archived,
	pinned            = excluded.pinned,
	marked_unread     = excluded.marked_unread,
	muted_until       = excluded.muted_until,
	ephemeral_seconds = excluded.ephemeral_seconds,
	updated_at        = excluded.updated_at`,
		c.JID, c.Kind, c.Name, c.LastMessageAt, c.UnreadCount, c.Archived, c.Pinned, c.MarkedUnread,
		c.MutedUntil, c.EphemeralSeconds, t.now)
}

var chatFlagColumns = map[string]bool{
	"archived": true, "pinned": true, "marked_unread": true,
	"muted_until": true, "ephemeral_seconds": true, "unread_count": true,
}

// SetChatFlag updates one piece of chat state (archived, pinned, muted_until, ...).
func (t *Tx) SetChatFlag(jid, kind, column string, value int64) error {
	if !chatFlagColumns[column] {
		return fmt.Errorf("unknown chat column %q", column)
	}
	known := 0
	if column == "unread_count" {
		known = 1 // a live read/reset establishes the true count
	}
	return t.exec(fmt.Sprintf(`
INSERT INTO chats (jid, kind, %[1]s, updated_at, unread_known) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (jid) DO UPDATE SET %[1]s = excluded.%[1]s, updated_at = excluded.updated_at,
	unread_known = MAX(chats.unread_known, excluded.unread_known)`, column),
		jid, kind, value, t.now, known)
}

// Contact holds the names WhatsApp knows for a user.
type Contact struct {
	JID          string
	FullName     string // name in the phone's address book
	FirstName    string
	PushName     string // name the user chose for themselves
	BusinessName string
	UpdatedAt    int64
}

// UpsertContact merges non-empty fields into the stored contact.
func (t *Tx) UpsertContact(c Contact) error {
	if c.JID == "" {
		return nil
	}
	return t.exec(`
INSERT INTO contacts (jid, full_name, first_name, push_name, business_name, updated_at)
VALUES (?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)
ON CONFLICT (jid) DO UPDATE SET
	full_name     = COALESCE(excluded.full_name, contacts.full_name),
	first_name    = COALESCE(excluded.first_name, contacts.first_name),
	push_name     = COALESCE(excluded.push_name, contacts.push_name),
	business_name = COALESCE(excluded.business_name, contacts.business_name),
	updated_at    = excluded.updated_at`,
		c.JID, c.FullName, c.FirstName, c.PushName, c.BusinessName, t.now)
}

// PutLIDMapping records that a LID (hidden user id) belongs to a phone-number JID.
func (t *Tx) PutLIDMapping(lid, pn string) error {
	return t.exec(`INSERT INTO lid_map (lid, pn) VALUES (?, ?) ON CONFLICT (lid) DO UPDATE SET pn = excluded.pn`, lid, pn)
}

// Group is group metadata.
type Group struct {
	JID              string
	Name             string
	Topic            string
	OwnerJID         string
	CreatedAt        int64
	Announce         bool
	Locked           bool
	IsCommunity      bool
	ParentJID        string
	ParticipantCount int
	IsMember         bool
	UpdatedAt        int64
}

// Participant is a member of a group.
type Participant struct {
	GroupJID string
	JID      string
	Role     string // member, admin or superadmin
}

// UpsertGroup stores authoritative group metadata fetched from WhatsApp and
// replaces the participant list.
func (t *Tx) UpsertGroup(g Group, participants []Participant) error {
	err := t.exec(`
INSERT INTO groups (jid, name, topic, owner_jid, created_at, announce, locked, is_community, parent_jid,
	participant_count, is_member, updated_at)
VALUES (?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), ?, ?, ?, NULLIF(?, ''), ?, ?, ?)
ON CONFLICT (jid) DO UPDATE SET
	name = excluded.name, topic = excluded.topic,
	owner_jid = COALESCE(excluded.owner_jid, groups.owner_jid),
	created_at = COALESCE(excluded.created_at, groups.created_at),
	announce = excluded.announce, locked = excluded.locked, is_community = excluded.is_community,
	parent_jid = excluded.parent_jid, participant_count = excluded.participant_count,
	is_member = excluded.is_member, updated_at = excluded.updated_at`,
		g.JID, g.Name, g.Topic, g.OwnerJID, g.CreatedAt, g.Announce, g.Locked, g.IsCommunity, g.ParentJID,
		g.ParticipantCount, g.IsMember, t.now)
	if err != nil {
		return err
	}
	if err := t.exec(`DELETE FROM group_participants WHERE group_jid = ?`, g.JID); err != nil {
		return err
	}
	if err := t.insertParticipants(participants); err != nil {
		return err
	}
	return t.exec(`INSERT INTO chats (jid, kind, updated_at) VALUES (?, 'group', ?) ON CONFLICT (jid) DO NOTHING`, g.JID, t.now)
}

// MergeGroupFromHistory fills gaps in group metadata from a history sync
// without overriding data fetched live. Participants are only used when none
// are known yet (typically for groups the account has left).
func (t *Tx) MergeGroupFromHistory(g Group, participants []Participant) error {
	err := t.exec(`
INSERT INTO groups (jid, name, topic, owner_jid, created_at, is_community, parent_jid, participant_count, updated_at)
VALUES (?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), ?, NULLIF(?, ''), ?, ?)
ON CONFLICT (jid) DO UPDATE SET
	name = COALESCE(groups.name, excluded.name),
	topic = COALESCE(groups.topic, excluded.topic),
	owner_jid = COALESCE(groups.owner_jid, excluded.owner_jid),
	created_at = COALESCE(groups.created_at, excluded.created_at),
	is_community = MAX(groups.is_community, excluded.is_community),
	parent_jid = COALESCE(groups.parent_jid, excluded.parent_jid),
	participant_count = CASE WHEN groups.participant_count = 0 THEN excluded.participant_count
	                         ELSE groups.participant_count END`,
		g.JID, g.Name, g.Topic, g.OwnerJID, g.CreatedAt, g.IsCommunity, g.ParentJID, g.ParticipantCount, t.now)
	if err != nil || len(participants) == 0 {
		return err
	}
	var n int
	if err := t.tx.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM group_participants WHERE group_jid = ?`, g.JID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return t.insertParticipants(participants)
}

func (t *Tx) insertParticipants(ps []Participant) error {
	for _, p := range ps {
		if p.JID == "" {
			continue
		}
		err := t.exec(`INSERT INTO group_participants (group_jid, jid, role) VALUES (?, ?, ?)
ON CONFLICT (group_jid, jid) DO UPDATE SET role = excluded.role`, p.GroupJID, p.JID, p.Role)
		if err != nil {
			return err
		}
	}
	return nil
}

// SetGroupMembership marks exactly the given groups as ones the account belongs to.
func (t *Tx) SetGroupMembership(current []string) error {
	ids, _ := json.Marshal(current)
	return t.exec(`UPDATE groups SET is_member = (jid IN (SELECT value FROM json_each(?)))`, string(ids))
}

// Call is one entry of the call log.
type Call struct {
	ID           string
	ChatJID      string // peer for 1:1 calls, group JID for group calls
	CreatorJID   string
	FromMe       bool
	Video        bool
	IsGroup      bool
	StartedAt    int64
	AcceptedAt   int64
	Duration     int64 // seconds
	Result       string
	Participants []string
	Source       string
}

// UpsertCall stores or updates a call log entry.
func (t *Tx) UpsertCall(c Call) error {
	return t.exec(`
INSERT INTO calls (id, chat_jid, creator_jid, from_me, video, is_group, started_at, accepted_at, duration,
	result, participants, source)
VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
	chat_jid     = CASE WHEN excluded.chat_jid <> '' THEN excluded.chat_jid ELSE calls.chat_jid END,
	creator_jid  = CASE WHEN excluded.creator_jid <> '' THEN excluded.creator_jid ELSE calls.creator_jid END,
	video        = MAX(calls.video, excluded.video),
	is_group     = MAX(calls.is_group, excluded.is_group),
	accepted_at  = COALESCE(calls.accepted_at, excluded.accepted_at),
	duration     = MAX(calls.duration, excluded.duration),
	result       = CASE WHEN excluded.result = '' THEN calls.result
	                    WHEN excluded.source = 'live' OR calls.source <> 'live' OR calls.result = '' THEN excluded.result
	                    ELSE calls.result END,
	source       = CASE WHEN calls.source = 'live' THEN 'live' ELSE excluded.source END,
	participants = CASE WHEN excluded.participants <> '' THEN excluded.participants ELSE calls.participants END`,
		c.ID, c.ChatJID, c.CreatorJID, c.FromMe, c.Video, c.IsGroup, c.StartedAt, c.AcceptedAt, c.Duration,
		c.Result, strings.Join(c.Participants, ","), c.Source)
}

// CallByID returns a stored call, used to compute durations of live calls.
func (t *Tx) CallByID(id string) (*Call, error) {
	var c Call
	var accepted sql.NullInt64
	err := t.tx.QueryRowContext(t.ctx, `SELECT id, started_at, accepted_at, result FROM calls WHERE id = ?`, id).
		Scan(&c.ID, &c.StartedAt, &accepted, &c.Result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	c.AcceptedAt = accepted.Int64
	return &c, err
}

// SetMeta stores a key/value pair.
func (t *Tx) SetMeta(key, value string) error {
	return t.exec(`INSERT INTO meta (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, t.now)
}

// SetMeta stores a key/value pair in its own transaction.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.Write(ctx, func(t *Tx) error { return t.SetMeta(key, value) })
}

// SetMedia records where a message's media was saved, or why it failed.
func (s *Store) SetMedia(ctx context.Context, chat, id, path, errText string) error {
	return s.Write(ctx, func(t *Tx) error {
		return t.exec(`UPDATE messages SET media_path = ?, media_error = ? WHERE chat_jid = ? AND id = ?`,
			path, errText, chat, id)
	})
}

// MergeJID rewrites every reference to from (a LID) to to (its phone-number
// JID) once the mapping becomes known, merging duplicate rows.
func (t *Tx) MergeJID(from, to string) error {
	if from == "" || to == "" || from == to {
		return nil
	}
	// Messages stored under the LID chat move to the phone chat; rows that
	// already exist there are duplicates and are dropped.
	if err := t.exec(`UPDATE OR IGNORE messages SET chat_jid = ? WHERE chat_jid = ?`, to, from); err != nil {
		return err
	}
	if err := t.exec(`DELETE FROM messages_fts WHERE rowid IN (SELECT pk FROM messages WHERE chat_jid = ?)`, from); err != nil {
		return err
	}
	stmts := []string{
		`DELETE FROM messages WHERE chat_jid = ?2`,
		`UPDATE messages SET sender_jid = ?1 WHERE sender_jid = ?2`,
		`UPDATE messages SET quoted_sender = ?1 WHERE quoted_sender = ?2`,
		`UPDATE messages SET revoked_by = ?1 WHERE revoked_by = ?2`,
		`UPDATE message_edits SET chat_jid = ?1 WHERE chat_jid = ?2`,
		`UPDATE OR IGNORE reactions SET chat_jid = ?1 WHERE chat_jid = ?2`,
		`DELETE FROM reactions WHERE chat_jid = ?2`,
		`UPDATE OR IGNORE reactions SET sender_jid = ?1 WHERE sender_jid = ?2`,
		`DELETE FROM reactions WHERE sender_jid = ?2`,
		`UPDATE OR IGNORE group_participants SET jid = ?1 WHERE jid = ?2`,
		`DELETE FROM group_participants WHERE jid = ?2`,
		`INSERT INTO chats (jid, kind, name, last_message_at, unread_count, archived, pinned, marked_unread,
			muted_until, ephemeral_seconds, updated_at, unread_known)
		 SELECT ?1, kind, name, last_message_at, unread_count, archived, pinned, marked_unread,
			muted_until, ephemeral_seconds, updated_at, unread_known FROM chats WHERE jid = ?2
		 ON CONFLICT (jid) DO UPDATE SET
			name = COALESCE(chats.name, excluded.name),
			last_message_at = MAX(COALESCE(chats.last_message_at, 0), COALESCE(excluded.last_message_at, 0)),
			unread_count = MAX(COALESCE(chats.unread_count, 0), COALESCE(excluded.unread_count, 0)),
			unread_known = MAX(chats.unread_known, excluded.unread_known),
			archived = MAX(chats.archived, excluded.archived),
			pinned = MAX(chats.pinned, excluded.pinned),
			marked_unread = MAX(chats.marked_unread, excluded.marked_unread),
			muted_until = CASE WHEN chats.muted_until = -1 OR excluded.muted_until = -1 THEN -1
			                   ELSE MAX(COALESCE(chats.muted_until, 0), COALESCE(excluded.muted_until, 0)) END,
			ephemeral_seconds = COALESCE(chats.ephemeral_seconds, excluded.ephemeral_seconds)`,
		`DELETE FROM chats WHERE jid = ?2`,
		`INSERT INTO contacts (jid, full_name, first_name, push_name, business_name, updated_at)
		 SELECT ?1, full_name, first_name, push_name, business_name, updated_at FROM contacts WHERE jid = ?2
		 ON CONFLICT (jid) DO UPDATE SET
			full_name = COALESCE(contacts.full_name, excluded.full_name),
			first_name = COALESCE(contacts.first_name, excluded.first_name),
			push_name = COALESCE(contacts.push_name, excluded.push_name),
			business_name = COALESCE(contacts.business_name, excluded.business_name)`,
		`DELETE FROM contacts WHERE jid = ?2`,
		`UPDATE calls SET chat_jid = ?1 WHERE chat_jid = ?2`,
		`UPDATE calls SET creator_jid = ?1 WHERE creator_jid = ?2`,
		`UPDATE groups SET owner_jid = ?1 WHERE owner_jid = ?2`,
		`INSERT INTO contact_meta (jid, alias, description, priority, tags, fields, updated_at)
		 SELECT ?1, alias, description, priority, tags, fields, updated_at FROM contact_meta WHERE jid = ?2
		 ON CONFLICT (jid) DO NOTHING`,
		`DELETE FROM contact_meta WHERE jid = ?2`,
	}
	for _, q := range stmts {
		if err := t.exec(q, to, from); err != nil {
			return fmt.Errorf("merge %s into %s: %w", from, to, err)
		}
	}
	return nil
}
