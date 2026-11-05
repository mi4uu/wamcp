package archive

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Statuses of an AgentSend.
const (
	SendPending = "pending"
	SendSent    = "sent"
	SendFailed  = "failed"
	SendRefused = "refused"
)

// AgentSend is one audit record of a message an agent asked to send.
type AgentSend struct {
	PK        int64
	CreatedAt time.Time
	UpdatedAt time.Time
	DedupeKey string
	ChatJID   string
	Text      string
	ReplyTo   string
	Client    string
	Status    string
	MessageID string
	Error     string
}

const agentSendCols = `pk, created_at, updated_at, dedupe_key, chat_jid, text, reply_to, client, status, message_id, error`

func scanAgentSend(row interface{ Scan(...any) error }) (AgentSend, error) {
	var a AgentSend
	var created, updated int64
	err := row.Scan(&a.PK, &created, &updated, &a.DedupeKey, &a.ChatJID, &a.Text, &a.ReplyTo, &a.Client,
		&a.Status, &a.MessageID, &a.Error)
	a.CreatedAt, a.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return a, err
}

// AddAgentSend inserts an audit record and returns its primary key.
func (s *Store) AddAgentSend(ctx context.Context, a AgentSend) (int64, error) {
	if s.write == nil {
		return 0, errors.New("archive opened read-only")
	}
	res, err := s.write.ExecContext(ctx, `
INSERT INTO agent_sends (created_at, updated_at, dedupe_key, chat_jid, text, reply_to, client, status, message_id, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.CreatedAt.Unix(), a.CreatedAt.Unix(), a.DedupeKey, a.ChatJID, a.Text, a.ReplyTo, a.Client, a.Status, a.MessageID, a.Error)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetAgentSendStatus updates the outcome of an audit record.
func (s *Store) SetAgentSendStatus(ctx context.Context, pk int64, status, messageID, errText string, at time.Time) error {
	if s.write == nil {
		return errors.New("archive opened read-only")
	}
	_, err := s.write.ExecContext(ctx,
		`UPDATE agent_sends SET status = ?, message_id = ?, error = ?, updated_at = ? WHERE pk = ?`,
		status, messageID, errText, at.Unix(), pk)
	return err
}

// LastAgentSend returns the newest record with dedupeKey created at or after
// since, ignoring refusals. It returns ErrNotFound when there is none.
func (s *Store) LastAgentSend(ctx context.Context, dedupeKey string, since time.Time) (AgentSend, error) {
	a, err := scanAgentSend(s.write.QueryRowContext(ctx, `SELECT `+agentSendCols+` FROM agent_sends
WHERE dedupe_key = ? AND created_at >= ? AND status != ? ORDER BY pk DESC LIMIT 1`,
		dedupeKey, since.Unix(), SendRefused))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentSend{}, ErrNotFound
	}
	return a, err
}

// CountAgentSends counts records created at or after since that sent or may
// have sent a message (pending or sent).
func (s *Store) CountAgentSends(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.write.QueryRowContext(ctx, `SELECT count(*) FROM agent_sends WHERE created_at >= ? AND status IN (?, ?)`,
		since.Unix(), SendPending, SendSent).Scan(&n)
	return n, err
}

// AgentSends returns the newest audit records, newest first.
func (s *Store) AgentSends(ctx context.Context, limit int) ([]AgentSend, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+agentSendCols+` FROM agent_sends ORDER BY pk DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentSend
	for rows.Next() {
		a, err := scanAgentSend(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
