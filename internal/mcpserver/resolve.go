package mcpserver

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/mi4uu/wamcp/internal/archive"
)

// resolver maps user input (JID, phone number or name) to JIDs.
type resolver struct {
	dir   *archive.Directory
	chats []archive.Chat
}

func normalizeJID(s string) string {
	s = strings.TrimSpace(s)
	user, server, ok := strings.Cut(s, "@")
	if !ok {
		return s
	}
	if server == "c.us" {
		server = "s.whatsapp.net"
	}
	return user + "@" + server
}

// phoneJID returns user@s.whatsapp.net when s looks like a phone number.
func phoneJID(s string) (string, bool) {
	digits := 0
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			digits++
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", false
		}
	}
	if digits < 7 {
		return "", false
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String() + "@s.whatsapp.net", true
}

type candidate struct {
	jid   string
	label string
	exact bool
	last  int64
}

func pick(input string, cands []candidate, what string) (string, error) {
	if len(cands) == 0 {
		return "", fmt.Errorf("no %s matches %q; list them with list_chats or search_contacts", what, input)
	}
	var exact []candidate
	for _, c := range cands {
		if c.exact {
			exact = append(exact, c)
		}
	}
	if len(exact) == 1 {
		return exact[0].jid, nil
	}
	if len(cands) == 1 {
		return cands[0].jid, nil
	}
	if len(exact) > 1 {
		cands = exact
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].last > cands[j].last })
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d %ss; call again with one of these JIDs:\n", input, len(cands), what)
	for i, c := range cands {
		if i == 10 {
			fmt.Fprintf(&b, "… and %d more\n", len(cands)-10)
			break
		}
		fmt.Fprintf(&b, "- %s · %s\n", c.label, c.jid)
	}
	return "", errors.New(b.String())
}

// chat resolves input to a chat JID.
func (r *resolver) chat(input string) (string, error) {
	input = strings.TrimSpace(input)
	switch {
	case input == "":
		return "", errors.New("chat is required")
	case strings.EqualFold(input, "me") || strings.EqualFold(input, "self"):
		if r.dir.Self == "" {
			return "", errors.New("the account is not known yet")
		}
		return r.dir.Self, nil
	case strings.Contains(input, "@"):
		return r.dir.Canonical(normalizeJID(input)), nil
	}
	if j, ok := phoneJID(input); ok {
		return j, nil
	}
	needle := archive.SearchText(input)
	var cands []candidate
	for _, c := range r.chats {
		names := []string{r.dir.Name(c.JID), r.dir.WhatsAppName(c.JID)}
		match, exact := false, false
		for _, n := range names {
			f := archive.SearchText(n)
			if strings.Contains(f, needle) {
				match = true
			}
			if f == needle {
				exact = true
			}
		}
		if match {
			cands = append(cands, candidate{jid: c.JID, label: fmt.Sprintf("%s (%s)", r.dir.Name(c.JID), c.Kind),
				exact: exact, last: c.LastMessageAt})
		}
	}
	return pick(input, cands, "chat")
}

// user resolves input to a user JID (contacts and chat partners).
func (r *resolver) user(input string) (string, error) {
	input = strings.TrimSpace(input)
	switch {
	case input == "":
		return "", errors.New("contact is required")
	case strings.EqualFold(input, "me") || strings.EqualFold(input, "self"):
		if r.dir.Self == "" {
			return "", errors.New("the account is not known yet")
		}
		return r.dir.Self, nil
	case strings.Contains(input, "@"):
		return r.dir.Canonical(normalizeJID(input)), nil
	}
	if j, ok := phoneJID(input); ok {
		return j, nil
	}
	needle := archive.SearchText(input)
	seen := map[string]bool{}
	var cands []candidate
	add := func(jid string, last int64) {
		if seen[jid] || strings.HasSuffix(jid, "@g.us") || strings.HasSuffix(jid, "@broadcast") {
			return
		}
		match, exact := false, false
		names := []string{r.dir.Name(jid), r.dir.WhatsAppName(jid)}
		if c, ok := r.dir.Contact(jid); ok {
			names = append(names, c.FullName, c.FirstName, c.PushName, c.BusinessName)
		}
		for _, n := range names {
			if n == "" {
				continue
			}
			f := archive.SearchText(n)
			match = match || strings.Contains(f, needle)
			exact = exact || f == needle
		}
		if match {
			seen[jid] = true
			cands = append(cands, candidate{jid: jid, label: r.dir.Label(jid), exact: exact, last: last})
		}
	}
	for _, c := range r.chats {
		if c.Kind == "direct" {
			add(c.JID, c.LastMessageAt)
		}
	}
	for _, c := range r.dir.Contacts() {
		add(c.JID, 0)
	}
	return pick(input, cands, "contact")
}

// parseTime accepts absolute dates/times and relative expressions like
// "3d", "12h", "30m", "today", "yesterday". Returns unix seconds (0 for "").
func parseTime(raw string, now time.Time, loc *time.Location) (int64, error) {
	raw = strings.TrimSpace(raw)
	s := strings.ToLower(raw)
	if s == "" {
		return 0, nil
	}
	midnight := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	switch s {
	case "now":
		return now.Unix(), nil
	case "today":
		return midnight.Unix(), nil
	case "yesterday":
		return midnight.AddDate(0, 0, -1).Unix(), nil
	}
	if n := len(s); n >= 2 && s[0] >= '0' && s[0] <= '9' {
		unit := s[n-1]
		var v int
		if _, err := fmt.Sscanf(s[:n-1], "%d", &v); err == nil && fmt.Sprint(v) == s[:n-1] {
			switch unit {
			case 'm':
				return now.Add(-time.Duration(v) * time.Minute).Unix(), nil
			case 'h':
				return now.Add(-time.Duration(v) * time.Hour).Unix(), nil
			case 'd':
				return now.AddDate(0, 0, -v).Unix(), nil
			case 'w':
				return now.AddDate(0, 0, -7*v).Unix(), nil
			}
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04",
		"2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, strings.ToUpper(raw), loc); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("cannot parse time %q; use YYYY-MM-DD, YYYY-MM-DD HH:MM, RFC3339, or relative like 2h, 3d, today", raw)
}
