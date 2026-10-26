package archive

// migrations are applied in order; PRAGMA user_version records how many ran.
// Append new entries, never edit existing ones.
var migrations = []string{
	`
CREATE TABLE meta (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE chats (
	jid               TEXT PRIMARY KEY,
	kind              TEXT NOT NULL,
	name              TEXT,
	last_message_at   INTEGER,
	unread_count      INTEGER,
	archived          INTEGER NOT NULL DEFAULT 0,
	pinned            INTEGER NOT NULL DEFAULT 0,
	marked_unread     INTEGER NOT NULL DEFAULT 0,
	muted_until       INTEGER,
	ephemeral_seconds INTEGER,
	updated_at        INTEGER NOT NULL
);

CREATE TABLE contacts (
	jid           TEXT PRIMARY KEY,
	full_name     TEXT,
	first_name    TEXT,
	push_name     TEXT,
	business_name TEXT,
	updated_at    INTEGER NOT NULL
);

CREATE TABLE lid_map (
	lid TEXT PRIMARY KEY,
	pn  TEXT NOT NULL
);
CREATE INDEX lid_map_pn ON lid_map (pn);

CREATE TABLE groups (
	jid               TEXT PRIMARY KEY,
	name              TEXT,
	topic             TEXT,
	owner_jid         TEXT,
	created_at        INTEGER,
	announce          INTEGER NOT NULL DEFAULT 0,
	locked            INTEGER NOT NULL DEFAULT 0,
	is_community      INTEGER NOT NULL DEFAULT 0,
	parent_jid        TEXT,
	participant_count INTEGER NOT NULL DEFAULT 0,
	is_member         INTEGER NOT NULL DEFAULT 0,
	updated_at        INTEGER NOT NULL
);

CREATE TABLE group_participants (
	group_jid TEXT NOT NULL,
	jid       TEXT NOT NULL,
	role      TEXT NOT NULL DEFAULT 'member',
	PRIMARY KEY (group_jid, jid)
);
CREATE INDEX group_participants_jid ON group_participants (jid);

CREATE TABLE messages (
	pk            INTEGER PRIMARY KEY,
	chat_jid      TEXT NOT NULL,
	id            TEXT NOT NULL,
	sender_jid    TEXT NOT NULL DEFAULT '',
	from_me       INTEGER NOT NULL DEFAULT 0,
	timestamp     INTEGER NOT NULL,
	kind          TEXT NOT NULL,
	text          TEXT NOT NULL DEFAULT '',
	push_name     TEXT NOT NULL DEFAULT '',
	quoted_id     TEXT NOT NULL DEFAULT '',
	quoted_sender TEXT NOT NULL DEFAULT '',
	quoted_text   TEXT NOT NULL DEFAULT '',
	forwarded     INTEGER NOT NULL DEFAULT 0,
	view_once     INTEGER NOT NULL DEFAULT 0,
	starred       INTEGER NOT NULL DEFAULT 0,
	edited_at     INTEGER,
	revoked_at    INTEGER,
	mime_type     TEXT NOT NULL DEFAULT '',
	file_name     TEXT NOT NULL DEFAULT '',
	file_size     INTEGER NOT NULL DEFAULT 0,
	duration      INTEGER NOT NULL DEFAULT 0,
	has_media     INTEGER NOT NULL DEFAULT 0,
	ack           INTEGER NOT NULL DEFAULT 0, -- own messages: 1 sent, 2 delivered, 3 read, 4 played
	media_path    TEXT NOT NULL DEFAULT '',
	media_error   TEXT NOT NULL DEFAULT '',
	raw           BLOB,
	source        TEXT NOT NULL,
	inserted_at   INTEGER NOT NULL,
	UNIQUE (chat_jid, id)
);
CREATE INDEX messages_chat_time ON messages (chat_jid, timestamp);
CREATE INDEX messages_time ON messages (timestamp);
CREATE INDEX messages_sender_time ON messages (sender_jid, timestamp);

-- rowid = messages.pk; body is folded by SearchText (lowercase, no diacritics).
CREATE VIRTUAL TABLE messages_fts USING fts5 (body, tokenize = 'unicode61 remove_diacritics 2');

CREATE TABLE reactions (
	chat_jid   TEXT NOT NULL,
	message_id TEXT NOT NULL,
	sender_jid TEXT NOT NULL,
	emoji      TEXT NOT NULL,
	timestamp  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chat_jid, message_id, sender_jid)
);

CREATE TABLE calls (
	id           TEXT PRIMARY KEY,
	chat_jid     TEXT NOT NULL DEFAULT '',
	creator_jid  TEXT NOT NULL DEFAULT '',
	from_me      INTEGER NOT NULL DEFAULT 0,
	video        INTEGER NOT NULL DEFAULT 0,
	is_group     INTEGER NOT NULL DEFAULT 0,
	started_at   INTEGER NOT NULL,
	accepted_at  INTEGER,
	duration     INTEGER NOT NULL DEFAULT 0,
	result       TEXT NOT NULL DEFAULT '',
	participants TEXT NOT NULL DEFAULT '',
	source       TEXT NOT NULL
);
CREATE INDEX calls_time ON calls (started_at);
CREATE INDEX calls_chat ON calls (chat_jid, started_at);

-- User-maintained annotations for contacts and chats (not synced to WhatsApp).
CREATE TABLE contact_meta (
	jid         TEXT PRIMARY KEY,
	alias       TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	priority    INTEGER NOT NULL DEFAULT 0, -- 0 = unset, 1 (lowest) .. 5 (top)
	tags        TEXT NOT NULL DEFAULT '',   -- comma separated
	fields      TEXT NOT NULL DEFAULT '{}', -- JSON object with custom fields
	updated_at  INTEGER NOT NULL
);
`, // v2: who deleted a message for everyone (deleted messages are kept by default).
	`ALTER TABLE messages ADD COLUMN revoked_by TEXT NOT NULL DEFAULT '';`,
	// v3: earlier versions of edited messages.
	`CREATE TABLE message_edits (
	pk          INTEGER PRIMARY KEY,
	chat_jid    TEXT NOT NULL,
	message_id  TEXT NOT NULL,
	text        TEXT NOT NULL,
	replaced_at INTEGER NOT NULL -- when this version was replaced by an edit
);
CREATE INDEX message_edits_msg ON message_edits (chat_jid, message_id, replaced_at);`,
	// v4: whether unread_count is established (by a history snapshot or a live
	// read) rather than only counted from live messages.
	`ALTER TABLE chats ADD COLUMN unread_known INTEGER NOT NULL DEFAULT 0;
UPDATE chats SET unread_known = 1 WHERE unread_count IS NOT NULL;`,
}
