package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ContactMeta holds user-maintained annotations for a contact or chat. They
// live only in the local archive and are never sent to WhatsApp.
type ContactMeta struct {
	JID         string            `json:"jid"`
	Alias       string            `json:"alias,omitempty"`       // display name override
	Description string            `json:"description,omitempty"` // free-form notes
	Priority    int               `json:"priority,omitempty"`    // 0 = unset, 1 (lowest) .. 5 (top)
	Tags        []string          `json:"tags,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"` // custom key/value pairs
	UpdatedAt   int64             `json:"updated_at,omitempty"`
}

// Empty reports whether the annotation carries no data.
func (m ContactMeta) Empty() bool {
	return m.Alias == "" && m.Description == "" && m.Priority == 0 && len(m.Tags) == 0 && len(m.Fields) == 0
}

// PriorityLabels names the priority levels 1..5.
var PriorityLabels = []string{"", "lowest", "low", "normal", "high", "top"}

// PriorityLabel returns e.g. "high (4/5)", or "" when unset.
func PriorityLabel(p int) string {
	if p < 1 || p > 5 {
		return ""
	}
	return fmt.Sprintf("%s (%d/5)", PriorityLabels[p], p)
}

// ParsePriority accepts 0-5 or a level name ("high", "top", ...).
func ParsePriority(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "none", "unset", "0":
		return 0, nil
	}
	if len(s) == 1 && s[0] >= '1' && s[0] <= '5' {
		return int(s[0] - '0'), nil
	}
	if i := slices.Index(PriorityLabels, s); i > 0 {
		return i, nil
	}
	return 0, fmt.Errorf("invalid priority %q: use 1-5 or lowest/low/normal/high/top", s)
}

// NormalizeTags trims, lowercases and de-duplicates tags.
func NormalizeTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		for _, part := range strings.Split(t, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part != "" && !slices.Contains(out, part) {
				out = append(out, part)
			}
		}
	}
	return out
}

func scanMeta(sc interface{ Scan(...any) error }) (ContactMeta, error) {
	var m ContactMeta
	var tags, fields string
	if err := sc.Scan(&m.JID, &m.Alias, &m.Description, &m.Priority, &tags, &fields, &m.UpdatedAt); err != nil {
		return m, err
	}
	m.Tags = NormalizeTags([]string{tags})
	if fields != "" && fields != "{}" {
		_ = json.Unmarshal([]byte(fields), &m.Fields)
	}
	return m, nil
}

// ContactMetas returns all annotations keyed by JID.
func (s *Store) ContactMetas(ctx context.Context) (map[string]ContactMeta, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT jid, alias, description, priority, tags, fields, updated_at FROM contact_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ContactMeta{}
	for rows.Next() {
		m, err := scanMeta(rows)
		if err != nil {
			return nil, err
		}
		out[m.JID] = m
	}
	return out, rows.Err()
}

// ContactMeta returns the annotation for jid (zero value with JID set when none).
func (s *Store) ContactMeta(ctx context.Context, jid string) (ContactMeta, error) {
	m, err := scanMeta(s.read.QueryRowContext(ctx,
		`SELECT jid, alias, description, priority, tags, fields, updated_at FROM contact_meta WHERE jid = ?`, jid))
	if errors.Is(err, sql.ErrNoRows) {
		return ContactMeta{JID: jid}, nil
	}
	return m, err
}

// SetContactMeta replaces the annotation for m.JID; an empty annotation deletes it.
func (s *Store) SetContactMeta(ctx context.Context, m ContactMeta) error {
	if m.JID == "" {
		return errors.New("annotation without jid")
	}
	if m.Priority < 0 || m.Priority > 5 {
		return fmt.Errorf("priority must be between 0 and 5, got %d", m.Priority)
	}
	m.Alias = strings.TrimSpace(m.Alias)
	m.Description = strings.TrimSpace(m.Description)
	m.Tags = NormalizeTags(m.Tags)
	fields := map[string]string{}
	for k, v := range m.Fields {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			fields[k] = v
		}
	}
	m.Fields = fields
	return s.Write(ctx, func(t *Tx) error {
		if m.Empty() {
			return t.exec(`DELETE FROM contact_meta WHERE jid = ?`, m.JID)
		}
		raw, _ := json.Marshal(fields)
		return t.exec(`
INSERT INTO contact_meta (jid, alias, description, priority, tags, fields, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (jid) DO UPDATE SET alias = excluded.alias, description = excluded.description,
	priority = excluded.priority, tags = excluded.tags, fields = excluded.fields, updated_at = excluded.updated_at`,
			m.JID, m.Alias, m.Description, m.Priority, strings.Join(m.Tags, ","), string(raw), t.now)
	})
}
