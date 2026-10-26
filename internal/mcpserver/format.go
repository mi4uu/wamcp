package mcpserver

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mi4uu/wamcp/internal/archive"
)

type formatter struct {
	dir *archive.Directory
	loc *time.Location
}

func (f formatter) time(ts int64) string {
	if ts == 0 {
		return "—"
	}
	return time.Unix(ts, 0).In(f.loc).Format("2006-01-02 15:04")
}

func (f formatter) zone() string {
	name, offset := time.Now().In(f.loc).Zone()
	return fmt.Sprintf("%s (UTC%+d)", name, offset/3600)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func clock(secs int64) string {
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

var jidPattern = regexp.MustCompile(`\b\d{5,}@(s\.whatsapp\.net|lid|g\.us)\b`)

// body renders the content part of a message line.
func (f formatter) body(m archive.Message) string {
	if m.Deleted() && m.Text == "" && !m.HasMedia && m.FileName == "" {
		return "[deleted]"
	}
	var deleted string
	if m.Deleted() {
		who := f.sender(m)
		if m.RevokedBy != "" && m.RevokedBy != m.SenderJID {
			who = f.dir.Name(m.RevokedBy)
			if f.dir.IsSelf(m.RevokedBy) {
				who = "me"
			}
		}
		deleted = fmt.Sprintf("[deleted by %s at %s] ", who, f.time(m.RevokedAt))
	}
	var tag string
	switch m.Kind {
	case "text":
	case "system":
		return deleted + "— " + jidPattern.ReplaceAllStringFunc(m.Text, f.dir.Name)
	case "voice", "audio", "video", "video_note", "gif":
		label := strings.ReplaceAll(m.Kind, "_", " ")
		if m.Kind == "voice" {
			label = "voice note"
		}
		if m.Duration > 0 {
			label += " " + clock(m.Duration)
		}
		tag = label
	case "image", "sticker":
		tag = m.Kind
	case "document":
		tag = "document"
		if m.FileName != "" {
			tag += " " + fmt.Sprintf("%q", m.FileName)
		}
	default:
		tag = strings.ReplaceAll(m.Kind, "_", " ")
	}
	if m.HasMedia && m.FileSize > 0 {
		tag += ", " + humanSize(m.FileSize)
	}
	var parts []string
	if m.Forwarded {
		parts = append(parts, "[forwarded]")
	}
	if m.ViewOnce {
		parts = append(parts, "[view once]")
	}
	if tag != "" {
		parts = append(parts, "["+tag+"]")
	}
	if t := strings.TrimSpace(m.Text); t != "" {
		parts = append(parts, strings.ReplaceAll(t, "\n", "\n    "))
	}
	if m.EditedAt != 0 {
		parts = append(parts, "(edited)")
	}
	return deleted + strings.Join(parts, " ")
}

type lineOpts struct {
	ids       bool
	chatName  bool // prefix with the chat name (cross-chat listings)
	reactions map[string][]archive.Reaction
	versions  map[string][]archive.Version
}

func (f formatter) sender(m archive.Message) string {
	if m.FromMe || f.dir.IsSelf(m.SenderJID) {
		return "me"
	}
	if m.SenderJID == "" {
		if m.PushName != "" {
			return m.PushName
		}
		return "unknown"
	}
	name := f.dir.Name(m.SenderJID)
	if strings.HasPrefix(name, "hidden number") && m.PushName != "" {
		return "~" + m.PushName
	}
	return name
}

func (f formatter) line(m archive.Message, o lineOpts) string {
	var b strings.Builder
	b.WriteString("[" + f.time(m.Timestamp) + "] ")
	if o.chatName {
		b.WriteString(f.dir.Name(m.ChatJID) + " | ")
	}
	if m.Kind != "system" {
		b.WriteString(f.sender(m) + ": ")
	}
	b.WriteString(f.body(m))
	if m.Starred {
		b.WriteString(" ★")
	}
	if o.ids || m.HasMedia {
		b.WriteString(" ⟨id:" + m.ID + "⟩")
	}
	if m.QuotedID != "" {
		who := "message"
		if m.QuotedSender != "" {
			who = f.dir.Name(m.QuotedSender)
			if f.dir.IsSelf(m.QuotedSender) {
				who = "me"
			}
		}
		q := m.QuotedText
		if q == "" {
			q = "…"
		}
		b.WriteString(fmt.Sprintf("\n    ↪ reply to %s: %q", who, q))
	}
	if vs := o.versions[m.ID]; len(vs) > 0 {
		parts := make([]string, 0, len(vs))
		for _, v := range vs {
			parts = append(parts, fmt.Sprintf("%q", truncateRunes(strings.Join(strings.Fields(v.Text), " "), 200)))
		}
		b.WriteString("\n    earlier versions (oldest first): " + strings.Join(parts, " → "))
	}
	if rs := o.reactions[m.ID]; len(rs) > 0 {
		var parts []string
		for _, r := range rs {
			who := f.dir.Name(r.SenderJID)
			if f.dir.IsSelf(r.SenderJID) {
				who = "me"
			}
			parts = append(parts, r.Emoji+" "+who)
		}
		b.WriteString("\n    reactions: " + strings.Join(parts, ", "))
	}
	return b.String()
}

// metaLine renders user annotations in one line, or "".
func metaLine(m archive.ContactMeta) string {
	var parts []string
	if p := archive.PriorityLabel(m.Priority); p != "" {
		parts = append(parts, "priority "+p)
	}
	if len(m.Tags) > 0 {
		parts = append(parts, "tags: "+strings.Join(m.Tags, ", "))
	}
	if m.Description != "" {
		parts = append(parts, "note: "+strings.Join(strings.Fields(m.Description), " "))
	}
	return strings.Join(parts, " · ")
}

// metaBlock renders user annotations over several lines.
func metaBlock(m archive.ContactMeta) string {
	if m.Empty() {
		return "Your notes: none (add them with update_contact_meta or in the web client)\n"
	}
	var b strings.Builder
	b.WriteString("Your notes:\n")
	if m.Alias != "" {
		b.WriteString("  alias: " + m.Alias + "\n")
	}
	if p := archive.PriorityLabel(m.Priority); p != "" {
		b.WriteString("  priority: " + p + "\n")
	}
	if len(m.Tags) > 0 {
		b.WriteString("  tags: " + strings.Join(m.Tags, ", ") + "\n")
	}
	if m.Description != "" {
		b.WriteString("  description: " + strings.ReplaceAll(m.Description, "\n", "\n    ") + "\n")
	}
	keys := make([]string, 0, len(m.Fields))
	for k := range m.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("  " + k + ": " + m.Fields[k] + "\n")
	}
	return b.String()
}

func muteText(until int64) string {
	switch {
	case until == -1:
		return "muted"
	case until > time.Now().Unix():
		return "muted until " + time.Unix(until, 0).Format("2006-01-02 15:04")
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
