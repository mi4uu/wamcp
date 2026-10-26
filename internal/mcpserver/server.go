// Package mcpserver exposes the WhatsApp archive as MCP tools. Tools only
// read WhatsApp data; the one write tool edits local contact annotations.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mi4uu/wamcp/internal/archive"
)

// Live is implemented by the running daemon. It is nil for the stdio server,
// which then reports the status persisted by the daemon.
type Live interface {
	Status() archive.DaemonStatus
	DownloadMedia(ctx context.Context, chat, id string) (path, mimeType string, size int64, err error)
	RequestOlderMessages(ctx context.Context, chat string, count int) error
}

type server struct {
	arc  *archive.Store
	live Live
	loc  *time.Location
	now  func() time.Time
}

const instructions = `Read-only access to the user's WhatsApp account, archived locally and kept in sync by the wamcp daemon:
all chats, messages (text, captions, media metadata, replies, reactions, edits, deletions), groups and their members,
contacts, call history, plus the user's own notes on contacts (alias, description, priority 1-5, tags, custom fields).
Typical flow: list_chats or search_contacts to find a chat, then get_messages; search_messages for full-text search
across everything; get_recent_messages for "what happened lately". Chats and people can be given as JID, phone number
or (part of) a name. Times are shown in the user's local time zone.`

// New builds the MCP server. live may be nil.
func New(arc *archive.Store, live Live, version string) *mcp.Server {
	s := &server{arc: arc, live: live, loc: time.Local, now: time.Now}
	srv := mcp.NewServer(&mcp.Implementation{Name: "wamcp", Title: "WhatsApp", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}

	mcp.AddTool(srv, &mcp.Tool{Name: "whatsapp_status", Annotations: ro,
		Description: "Connection and sync state of the WhatsApp daemon, plus archive totals. Use it to judge how complete and fresh the data is."},
		s.status)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_chats", Annotations: ro,
		Description: "List chats (direct, group, status, broadcast, channel) with last activity, unread counts, message counts and the user's priority/tags. Sorted by recent activity by default."},
		s.listChats)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_chat", Annotations: ro,
		Description: "Details of one chat: state, the user's notes, activity statistics (message counts, first/last message, most active senders) and, for groups, description and members."},
		s.getChat)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_messages", Annotations: ro,
		Description: "Read messages of one chat in chronological order. By default returns the newest messages; use before/after to page or pick a time range. Media messages show their id for download_media."},
		s.getMessages)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_recent_messages", Annotations: ro,
		Description: "Messages across all chats in a time window (default: last 24 hours), grouped by chat. Good for 'what did I miss' or daily summaries. Can be limited to direct chats, groups, or chats with a minimum priority."},
		s.recentMessages)
	mcp.AddTool(srv, &mcp.Tool{Name: "search_messages", Annotations: ro,
		Description: "Full-text search over all messages, captions and file names (case- and accent-insensitive, words match as prefixes, \"quoted phrases\" match exactly). Newest first."},
		s.searchMessages)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_message_context", Annotations: ro,
		Description: "Messages around one message (e.g. a search hit) to understand the conversation."},
		s.messageContext)
	mcp.AddTool(srv, &mcp.Tool{Name: "search_contacts", Annotations: ro,
		Description: "Find people by name, phone number, tag or note text. Returns names WhatsApp knows (address book, self-chosen push name, business name), phone numbers and the user's notes."},
		s.searchContacts)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_contact", Annotations: ro,
		Description: "Everything about one person: names, phone, the user's notes (alias, description, priority, tags, custom fields), direct chat statistics and shared groups."},
		s.getContact)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_groups", Annotations: ro,
		Description: "List groups and communities with member counts, the user's role, and whether the user is still a member."},
		s.listGroups)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_calls", Annotations: ro,
		Description: "Call history (voice/video, incoming/outgoing, missed/answered, duration), newest first."},
		s.listCalls)
	mcp.AddTool(srv, &mcp.Tool{Name: "update_contact_meta",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		Description: "Edit the user's private notes on a contact or chat (alias, description, priority 1-5, tags, custom fields). Stored only in the local archive; nothing is sent to WhatsApp. Only fields you pass are changed."},
		s.updateMeta)
	if live != nil {
		mcp.AddTool(srv, &mcp.Tool{Name: "download_media",
			Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)},
			Description: "Download the attachment of a message (image, video, voice note, document, sticker) to the local archive and return the file path. Expired media is re-requested from the phone."},
			s.downloadMedia)
		mcp.AddTool(srv, &mcp.Tool{Name: "request_older_messages",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false)},
			Description: "Ask the user's phone for messages older than the oldest archived one in a chat. They arrive asynchronously within a minute if the phone is online; read them afterwards with get_messages."},
			s.requestOlder)
	}
	return srv
}

func ptr[T any](v T) *T { return &v }

func textResult(s string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}, nil, nil
}

func (s *server) load(ctx context.Context) (*resolver, formatter, error) {
	dir, err := s.arc.Directory(ctx)
	if err != nil {
		return nil, formatter{}, err
	}
	chats, err := s.arc.Chats(ctx)
	if err != nil {
		return nil, formatter{}, err
	}
	return &resolver{dir: dir, chats: chats}, formatter{dir: dir, loc: s.loc}, nil
}

func (s *server) times(after, before string) (int64, int64, error) {
	a, err := parseTime(after, s.now(), s.loc)
	if err != nil {
		return 0, 0, err
	}
	b, err := parseTime(before, s.now(), s.loc)
	return a, b, err
}

// --- whatsapp_status

type statusIn struct{}

func (s *server) status(ctx context.Context, _ *mcp.CallToolRequest, _ statusIn) (*mcp.CallToolResult, any, error) {
	var st *archive.DaemonStatus
	if s.live != nil {
		v := s.live.Status()
		st = &v
	} else {
		var err error
		if st, err = s.arc.DaemonStatus(ctx); err != nil {
			return nil, nil, err
		}
	}
	stats, err := s.arc.Stats(ctx)
	if err != nil {
		return nil, nil, err
	}
	f := formatter{loc: s.loc}
	var b strings.Builder
	if st == nil {
		b.WriteString("Daemon: never ran (start it with `wamcp serve`)\n")
	} else {
		state := st.State
		if s.live == nil && time.Since(st.UpdatedAt) > 3*time.Minute {
			state += fmt.Sprintf(" (last report %s ago — the daemon is probably not running)", time.Since(st.UpdatedAt).Round(time.Minute))
		}
		fmt.Fprintf(&b, "Daemon: %s\n", state)
		if st.Account != "" {
			fmt.Fprintf(&b, "Account: %s %s\n", archive.Phone(st.Account), st.PushName)
		}
		if !st.ConnectedAt.IsZero() {
			fmt.Fprintf(&b, "Connected since: %s\n", f.time(st.ConnectedAt.Unix()))
		}
		if !st.LastEventAt.IsZero() {
			fmt.Fprintf(&b, "Last event: %s\n", f.time(st.LastEventAt.Unix()))
		}
		if cu := st.CatchUp; cu.OfflineSeconds > 0 || cu.Received > 0 {
			state := "in progress"
			if cu.Done {
				state = "done " + f.time(cu.CompletedAt.Unix())
			}
			fmt.Fprintf(&b, "Catch-up after downtime: offline %s, %d messages recovered (%s)\n",
				(time.Duration(cu.OfflineSeconds) * time.Second).String(), cu.Received, state)
		}
		if h := st.History; h.Batches > 0 {
			fmt.Fprintf(&b, "History sync: %d batches, %d messages, last %s (%d%%) at %s\n",
				h.Batches, h.Messages, h.LastType, h.LastProgress, f.time(h.LastAt.Unix()))
		}
		if st.LastError != "" {
			fmt.Fprintf(&b, "Last error: %s\n", st.LastError)
		}
	}
	fmt.Fprintf(&b, "Archive: %d messages in %d chats (%s → %s), %d groups, %d contacts, %d calls, %d media (%d downloaded), %s on disk\n",
		stats.Messages, stats.Chats, f.time(stats.Oldest), f.time(stats.Newest), stats.Groups, stats.Contacts, stats.Calls,
		stats.Media, stats.Downloaded, humanSize(stats.DBBytes))
	fmt.Fprintf(&b, "Time zone: %s\n", f.zone())
	return textResult(b.String())
}

// --- list_chats

type listChatsIn struct {
	Query           string `json:"query,omitempty" jsonschema:"filter by chat name, phone number or JID fragment (case and accent insensitive)"`
	Kind            string `json:"kind,omitempty" jsonschema:"only this kind: direct, group, status, broadcast or channel"`
	IncludeArchived *bool  `json:"include_archived,omitempty" jsonschema:"include archived chats (default true)"`
	UnreadOnly      bool   `json:"unread_only,omitempty" jsonschema:"only chats with unread messages"`
	MinPriority     int    `json:"min_priority,omitempty" jsonschema:"only chats whose contact/group priority (1-5) is at least this"`
	Tag             string `json:"tag,omitempty" jsonschema:"only chats tagged with this tag"`
	Sort            string `json:"sort,omitempty" jsonschema:"recent (default), priority, messages or name"`
	Limit           int    `json:"limit,omitempty" jsonschema:"max chats (default 30, max 500)"`
	Offset          int    `json:"offset,omitempty" jsonschema:"skip this many chats (paging)"`
}

func (s *server) listChats(ctx context.Context, _ *mcp.CallToolRequest, in listChatsIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	tag := strings.ToLower(strings.TrimSpace(in.Tag))
	var out []archive.Chat
	for _, c := range r.chats {
		meta, _ := r.dir.Meta(c.JID)
		switch {
		case in.Kind != "" && c.Kind != in.Kind:
		case in.IncludeArchived != nil && !*in.IncludeArchived && c.Archived:
		case in.UnreadOnly && c.UnreadCount == 0 && !c.MarkedUnread:
		case in.MinPriority > 0 && meta.Priority < in.MinPriority:
		case tag != "" && !contains(meta.Tags, tag):
		case in.Query != "" && !archive.ContainsFold(r.dir.Name(c.JID)+" "+r.dir.WhatsAppName(c.JID)+" "+c.JID, in.Query):
		default:
			out = append(out, c)
		}
	}
	prio := func(c archive.Chat) int { m, _ := r.dir.Meta(c.JID); return m.Priority }
	switch in.Sort {
	case "name":
		sort.SliceStable(out, func(i, j int) bool {
			return archive.SearchText(r.dir.Name(out[i].JID)) < archive.SearchText(r.dir.Name(out[j].JID))
		})
	case "messages":
		sort.SliceStable(out, func(i, j int) bool { return out[i].MessageCount > out[j].MessageCount })
	case "priority":
		sort.SliceStable(out, func(i, j int) bool { return prio(out[i]) > prio(out[j]) })
	}
	total := len(out)
	limit := clampInt(in.Limit, 30, 500)
	offset := max(in.Offset, 0)
	out = out[min(offset, total):min(offset+limit, total)]
	var b strings.Builder
	fmt.Fprintf(&b, "%d chats match; showing %d from #%d.\n", total, len(out), offset+1)
	for _, c := range out {
		fmt.Fprintf(&b, "- %s (%s) · %s\n  last: %s", r.dir.Name(c.JID), c.Kind, c.JID, f.time(c.LastMessageAt))
		var flags []string
		if c.UnreadCount > 0 {
			flags = append(flags, fmt.Sprintf("%d unread", c.UnreadCount))
		} else if c.MarkedUnread {
			flags = append(flags, "marked unread")
		}
		if c.Pinned {
			flags = append(flags, "pinned")
		}
		if c.Archived {
			flags = append(flags, "archived")
		}
		if mt := muteText(c.MutedUntil); mt != "" {
			flags = append(flags, mt)
		}
		flags = append(flags, fmt.Sprintf("%d messages", c.MessageCount))
		b.WriteString(" · " + strings.Join(flags, " · "))
		if meta, ok := r.dir.Meta(c.JID); ok {
			if ml := metaLine(meta); ml != "" {
				b.WriteString("\n  " + ml)
			}
		}
		if c.LastKind != "" {
			preview := archive.Message{Kind: c.LastKind, Text: c.LastText, SenderJID: c.LastSender, FromMe: c.LastFromMe}
			fmt.Fprintf(&b, "\n  ↳ %s: %s", f.sender(preview), truncate(f.body(preview), 100))
		}
		b.WriteString("\n")
	}
	if offset+len(out) < total {
		fmt.Fprintf(&b, "More: call again with offset=%d.\n", offset+len(out))
	}
	return textResult(b.String())
}

// --- get_chat

type getChatIn struct {
	Chat                string `json:"chat" jsonschema:"chat JID, phone number, or (part of) the chat or contact name"`
	IncludeParticipants *bool  `json:"include_participants,omitempty" jsonschema:"list group members (default true, max 300)"`
}

func (s *server) getChat(ctx context.Context, _ *mcp.CallToolRequest, in getChatIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	jid, err := r.chat(in.Chat)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chat: %s · %s\n", r.dir.Name(jid), jid)
	c, err := s.arc.Chat(ctx, jid)
	if err == nil {
		fmt.Fprintf(&b, "Kind: %s · unread: %d · pinned: %v · archived: %v", c.Kind, c.UnreadCount, c.Pinned, c.Archived)
		if mt := muteText(c.MutedUntil); mt != "" {
			b.WriteString(" · " + mt)
		}
		if c.EphemeralSeconds > 0 {
			fmt.Fprintf(&b, " · disappearing messages: %s", time.Duration(c.EphemeralSeconds)*time.Second)
		}
		b.WriteString("\n")
	} else if !errors.Is(err, archive.ErrNotFound) {
		return nil, nil, err
	}
	meta, _ := r.dir.Meta(jid)
	b.WriteString(metaBlock(meta))
	act, err := s.arc.ChatActivity(ctx, jid, 0, 0, 10)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(&b, "Messages: %d (first %s, last %s), with media: %d\n", act.Messages, f.time(act.First), f.time(act.Last), act.Media)
	if len(act.Senders) > 0 {
		var parts []string
		for _, sc := range act.Senders {
			name := r.dir.Name(sc.JID)
			if r.dir.IsSelf(sc.JID) {
				name = "me"
			}
			parts = append(parts, fmt.Sprintf("%s %d", name, sc.Count))
		}
		b.WriteString("Most active: " + strings.Join(parts, ", ") + "\n")
	}
	if g, ok := r.dir.Group(jid); ok {
		b.WriteString("Group: ")
		var parts []string
		if g.IsCommunity {
			parts = append(parts, "community")
		}
		if g.ParentJID != "" {
			parts = append(parts, "part of community "+r.dir.Name(g.ParentJID))
		}
		if g.OwnerJID != "" {
			parts = append(parts, "created by "+r.dir.Label(g.OwnerJID))
		}
		if g.CreatedAt != 0 {
			parts = append(parts, "created "+f.time(g.CreatedAt))
		}
		if g.Announce {
			parts = append(parts, "only admins can send")
		}
		if !g.IsMember {
			parts = append(parts, "you are no longer a member")
		}
		fmt.Fprintf(&b, "%s · %d members\n", strings.Join(parts, " · "), g.ParticipantCount)
		if g.Topic != "" {
			b.WriteString("Description: " + strings.ReplaceAll(g.Topic, "\n", "\n  ") + "\n")
		}
		if in.IncludeParticipants == nil || *in.IncludeParticipants {
			ps, err := s.arc.Participants(ctx, jid)
			if err != nil {
				return nil, nil, err
			}
			b.WriteString("Members:\n")
			for i, p := range ps {
				if i == 300 {
					fmt.Fprintf(&b, "  … and %d more\n", len(ps)-300)
					break
				}
				line := "  - " + r.dir.Label(p.JID)
				if r.dir.IsSelf(p.JID) {
					line = "  - me"
				}
				if p.Role != "member" {
					line += " [" + p.Role + "]"
				}
				if m, ok := r.dir.Meta(p.JID); ok {
					if ml := metaLine(m); ml != "" {
						line += " · " + ml
					}
				}
				b.WriteString(line + "\n")
			}
		}
	}
	return textResult(b.String())
}

// --- get_messages

type getMessagesIn struct {
	Chat       string `json:"chat" jsonschema:"chat JID, phone number, or (part of) the chat or contact name; 'me' is the chat with yourself"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max messages (default 50, max 500)"`
	Before     string `json:"before,omitempty" jsonschema:"only messages before this time: YYYY-MM-DD, YYYY-MM-DD HH:MM[:SS], RFC3339, or relative like 2h, 3d, 1w, today, yesterday"`
	After      string `json:"after,omitempty" jsonschema:"only messages at or after this time (same formats); with only 'after' set, the oldest matching messages come first"`
	Sender     string `json:"sender,omitempty" jsonschema:"only messages from this person (JID, phone or name; 'me' for your own)"`
	MediaOnly  bool   `json:"media_only,omitempty" jsonschema:"only messages with attachments"`
	Starred    bool   `json:"starred,omitempty" jsonschema:"only starred messages"`
	IncludeIDs bool   `json:"include_ids,omitempty" jsonschema:"show every message ID (needed for get_message_context)"`
	BeforeID   string `json:"before_message_id,omitempty" jsonschema:"page backwards: only messages older than this message (use the value from the 'Older' hint)"`
	AfterID    string `json:"after_message_id,omitempty" jsonschema:"page forwards: only messages newer than this message (use the value from the 'More' hint)"`
}

func (s *server) getMessages(ctx context.Context, _ *mcp.CallToolRequest, in getMessagesIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	jid, err := r.chat(in.Chat)
	if err != nil {
		return nil, nil, err
	}
	after, before, err := s.times(in.After, in.Before)
	if err != nil {
		return nil, nil, err
	}
	q := archive.MessageQuery{ChatJID: jid, After: after, Before: before, Limit: clampInt(in.Limit, 50, 500),
		MediaOnly: in.MediaOnly, Starred: in.Starred}
	if in.BeforeID != "" {
		m, err := s.arc.Message(ctx, jid, in.BeforeID)
		if err != nil {
			return nil, nil, fmt.Errorf("before_message_id %s not found in this chat", in.BeforeID)
		}
		q.Before, q.BeforePK = m.Timestamp, m.PK
	}
	if in.AfterID != "" {
		m, err := s.arc.Message(ctx, jid, in.AfterID)
		if err != nil {
			return nil, nil, fmt.Errorf("after_message_id %s not found in this chat", in.AfterID)
		}
		q.After, q.AfterPK = m.Timestamp, m.PK
	}
	q.OldestFirst = q.After != 0 && q.Before == 0
	if in.Sender != "" {
		if q.SenderJID, err = r.user(in.Sender); err != nil {
			return nil, nil, err
		}
	}
	msgs, err := s.arc.Messages(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chat: %s · %s\n", r.dir.Name(jid), jid)
	if meta, ok := r.dir.Meta(jid); ok {
		if ml := metaLine(meta); ml != "" {
			b.WriteString("Your notes: " + ml + "\n")
		}
	}
	if len(msgs) == 0 {
		b.WriteString("No messages in this range.\n")
		return textResult(b.String())
	}
	fmt.Fprintf(&b, "%d messages, %s → %s, times in %s\n\n", len(msgs), f.time(msgs[0].Timestamp), f.time(msgs[len(msgs)-1].Timestamp), f.zone())
	if err := s.writeMessages(ctx, &b, f, jid, msgs, lineOpts{ids: in.IncludeIDs}); err != nil {
		return nil, nil, err
	}
	if len(msgs) == q.Limit {
		if q.OldestFirst {
			fmt.Fprintf(&b, "\nMore: call again with after_message_id=%q.\n", msgs[len(msgs)-1].ID)
		} else {
			fmt.Fprintf(&b, "\nOlder: call again with before_message_id=%q.\n", msgs[0].ID)
		}
	}
	return textResult(b.String())
}

func (s *server) writeMessages(ctx context.Context, b *strings.Builder, f formatter, chat string, msgs []archive.Message, o lineOpts) error {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	rs, err := s.arc.Reactions(ctx, chat, ids)
	if err != nil {
		return err
	}
	o.reactions = rs
	if o.versions, err = s.arc.EditHistory(ctx, chat, editedIDs(msgs)); err != nil {
		return err
	}
	for _, m := range msgs {
		b.WriteString(f.line(m, o) + "\n")
	}
	return nil
}

// --- get_recent_messages

type recentIn struct {
	After       string `json:"after,omitempty" jsonschema:"start of the window (default 24h ago); same formats as get_messages"`
	Before      string `json:"before,omitempty" jsonschema:"end of the window (default now)"`
	Kind        string `json:"kind,omitempty" jsonschema:"only direct or group chats"`
	MinPriority int    `json:"min_priority,omitempty" jsonschema:"only chats whose priority (1-5) is at least this"`
	IncludeOwn  *bool  `json:"include_own,omitempty" jsonschema:"include your own messages (default true)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max messages in total (default 300, max 2000)"`
}

func (s *server) recentMessages(ctx context.Context, _ *mcp.CallToolRequest, in recentIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	if in.After == "" {
		in.After = "24h"
	}
	after, before, err := s.times(in.After, in.Before)
	if err != nil {
		return nil, nil, err
	}
	limit := clampInt(in.Limit, 300, 2000)
	q := archive.MessageQuery{After: after, Before: before, ChatKind: in.Kind, Limit: limit,
		ExcludeFromMe: in.IncludeOwn != nil && !*in.IncludeOwn}
	if in.MinPriority > 0 {
		for _, c := range r.chats {
			if meta, _ := r.dir.Meta(c.JID); meta.Priority >= in.MinPriority {
				q.ChatJIDs = append(q.ChatJIDs, c.JID)
			}
		}
		if len(q.ChatJIDs) == 0 {
			return textResult(fmt.Sprintf("No chats have priority %d or higher; set priorities with update_contact_meta.\n", in.MinPriority))
		}
	}
	msgs, err := s.arc.Messages(ctx, q) // newest `limit` messages, chronological
	if err != nil {
		return nil, nil, err
	}
	byChat := map[string][]archive.Message{}
	var order []string // chats by most recent activity
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if _, ok := byChat[m.ChatJID]; !ok {
			order = append(order, m.ChatJID)
		}
		byChat[m.ChatJID] = append(byChat[m.ChatJID], m)
	}
	var b strings.Builder
	if before == 0 {
		fmt.Fprintf(&b, "%d messages in %d chats since %s (%s)\n", len(msgs), len(order), f.time(after), f.zone())
	} else {
		fmt.Fprintf(&b, "%d messages in %d chats between %s and %s (%s)\n", len(msgs), len(order), f.time(after), f.time(before), f.zone())
	}
	for _, chat := range order {
		cm := byChat[chat]
		slices.Reverse(cm)
		fmt.Fprintf(&b, "\n## %s · %s", r.dir.Name(chat), chat)
		if meta, ok := r.dir.Meta(chat); ok {
			if ml := metaLine(meta); ml != "" {
				b.WriteString(" · " + ml)
			}
		}
		b.WriteString("\n")
		if err := s.writeMessages(ctx, &b, f, chat, cm, lineOpts{}); err != nil {
			return nil, nil, err
		}
	}
	if len(msgs) == limit {
		fmt.Fprintf(&b, "\n(limit of %d reached: older messages in this window are not shown; narrow the window or raise limit)\n", limit)
	}
	return textResult(b.String())
}

// --- search_messages

type searchIn struct {
	Query  string `json:"query" jsonschema:"words to find; every word must appear (prefix match, accent-insensitive); use \"double quotes\" for exact phrases"`
	Chat   string `json:"chat,omitempty" jsonschema:"limit to one chat (JID, phone or name)"`
	Sender string `json:"sender,omitempty" jsonschema:"limit to one sender (JID, phone or name; 'me' for yourself)"`
	After  string `json:"after,omitempty" jsonschema:"only messages at or after this time"`
	Before string `json:"before,omitempty" jsonschema:"only messages before this time"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results (default 30, max 200)"`
	Offset int    `json:"offset,omitempty" jsonschema:"skip this many results (paging)"`
}

func (s *server) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	after, before, err := s.times(in.After, in.Before)
	if err != nil {
		return nil, nil, err
	}
	q := archive.SearchQuery{Text: in.Query, After: after, Before: before, Limit: clampInt(in.Limit, 30, 200), Offset: max(in.Offset, 0)}
	if in.Chat != "" {
		if q.ChatJID, err = r.chat(in.Chat); err != nil {
			return nil, nil, err
		}
	}
	if in.Sender != "" {
		if q.SenderJID, err = r.user(in.Sender); err != nil {
			return nil, nil, err
		}
	}
	msgs, err := s.arc.Search(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	if len(msgs) == 0 {
		fmt.Fprintf(&b, "No messages match %q.\n", in.Query)
		return textResult(b.String())
	}
	fmt.Fprintf(&b, "%d results for %q (newest first, %s). Use get_message_context with chat and id to see the conversation.\n\n", len(msgs), in.Query, f.zone())
	versions := map[string]map[string][]archive.Version{}
	for _, m := range msgs {
		if m.EditedAt == 0 || versions[m.ChatJID] != nil {
			continue
		}
		var chatMsgs []archive.Message
		for _, x := range msgs {
			if x.ChatJID == m.ChatJID {
				chatMsgs = append(chatMsgs, x)
			}
		}
		if versions[m.ChatJID], err = s.arc.EditHistory(ctx, m.ChatJID, editedIDs(chatMsgs)); err != nil {
			return nil, nil, err
		}
	}
	for _, m := range msgs {
		b.WriteString(f.line(m, lineOpts{ids: true, chatName: true, versions: versions[m.ChatJID]}) + "\n")
		b.WriteString("    chat: " + m.ChatJID + "\n")
	}
	if len(msgs) == q.Limit {
		fmt.Fprintf(&b, "\nMore: call again with offset=%d.\n", q.Offset+len(msgs))
	}
	return textResult(b.String())
}

// --- get_message_context

type contextIn struct {
	Chat      string `json:"chat" jsonschema:"chat JID (as printed by search_messages) or name"`
	MessageID string `json:"message_id" jsonschema:"message id (the ⟨id:…⟩ value)"`
	Before    int    `json:"before,omitempty" jsonschema:"messages before it (default 10, max 200)"`
	After     int    `json:"after,omitempty" jsonschema:"messages after it (default 10, max 200)"`
}

func (s *server) messageContext(ctx context.Context, _ *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	jid, err := r.chat(in.Chat)
	if err != nil {
		return nil, nil, err
	}
	msgs, err := s.arc.MessageContext(ctx, jid, in.MessageID, in.Before, in.After)
	if errors.Is(err, archive.ErrNotFound) {
		return nil, nil, fmt.Errorf("message %s not found in %s", in.MessageID, jid)
	}
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chat: %s · %s (the requested message is marked with »)\n\n", r.dir.Name(jid), jid)
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	rs, err := s.arc.Reactions(ctx, jid, ids)
	if err != nil {
		return nil, nil, err
	}
	vs, err := s.arc.EditHistory(ctx, jid, editedIDs(msgs))
	if err != nil {
		return nil, nil, err
	}
	for _, m := range msgs {
		prefix := "  "
		if m.ID == in.MessageID {
			prefix = "» "
		}
		b.WriteString(prefix + f.line(m, lineOpts{ids: true, reactions: rs, versions: vs}) + "\n")
	}
	return textResult(b.String())
}

// --- search_contacts

type searchContactsIn struct {
	Query       string `json:"query,omitempty" jsonschema:"name, phone number or note text (case and accent insensitive); empty lists everyone"`
	Tag         string `json:"tag,omitempty" jsonschema:"only people with this tag"`
	MinPriority int    `json:"min_priority,omitempty" jsonschema:"only people whose priority (1-5) is at least this"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max results (default 30, max 500)"`
}

func (s *server) searchContacts(ctx context.Context, _ *mcp.CallToolRequest, in searchContactsIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	lastByJID := map[string]int64{}
	for _, c := range r.chats {
		if c.Kind == "direct" {
			lastByJID[c.JID] = c.LastMessageAt
		}
	}
	people := map[string]bool{}
	for _, c := range r.dir.Contacts() {
		people[c.JID] = true
	}
	for j := range lastByJID {
		people[j] = true
	}
	tag := strings.ToLower(strings.TrimSpace(in.Tag))
	var matches []string
	for j := range people {
		if strings.HasSuffix(j, "@lid") && r.dir.Canonical(j) != j {
			continue
		}
		meta, _ := r.dir.Meta(j)
		if tag != "" && !contains(meta.Tags, tag) {
			continue
		}
		if in.MinPriority > 0 && meta.Priority < in.MinPriority {
			continue
		}
		if in.Query != "" {
			c, _ := r.dir.Contact(j)
			hay := strings.Join([]string{r.dir.Name(j), c.FullName, c.FirstName, c.PushName, c.BusinessName, j,
				archive.Phone(j), meta.Description, strings.Join(meta.Tags, " ")}, " ")
			if !archive.ContainsFold(hay, in.Query) && !archive.ContainsFold(strings.ReplaceAll(hay, " ", ""), strings.ReplaceAll(in.Query, " ", "")) {
				continue
			}
		}
		matches = append(matches, j)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		pi, _ := r.dir.Meta(matches[i])
		pj, _ := r.dir.Meta(matches[j])
		if pi.Priority != pj.Priority {
			return pi.Priority > pj.Priority
		}
		if lastByJID[matches[i]] != lastByJID[matches[j]] {
			return lastByJID[matches[i]] > lastByJID[matches[j]]
		}
		return matches[i] < matches[j]
	})
	limit := clampInt(in.Limit, 30, 500)
	var b strings.Builder
	fmt.Fprintf(&b, "%d people match; showing %d.\n", len(matches), min(limit, len(matches)))
	for i, j := range matches {
		if i == limit {
			break
		}
		fmt.Fprintf(&b, "- %s · %s", r.dir.Label(j), j)
		c, _ := r.dir.Contact(j)
		if c.PushName != "" && c.PushName != r.dir.Name(j) {
			fmt.Fprintf(&b, " · calls themselves %q", c.PushName)
		}
		if c.BusinessName != "" {
			fmt.Fprintf(&b, " · business %q", c.BusinessName)
		}
		if last := lastByJID[j]; last != 0 {
			fmt.Fprintf(&b, " · last chat %s", f.time(last))
		}
		if meta, ok := r.dir.Meta(j); ok {
			if ml := metaLine(meta); ml != "" {
				b.WriteString("\n  " + ml)
			}
		}
		b.WriteString("\n")
	}
	return textResult(b.String())
}

// --- get_contact

type getContactIn struct {
	Contact string `json:"contact" jsonschema:"JID, phone number or (part of) the name"`
}

func (s *server) getContact(ctx context.Context, _ *mcp.CallToolRequest, in getContactIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	jid, err := r.user(in.Contact)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Contact: %s · %s\n", r.dir.Label(jid), jid)
	if c, ok := r.dir.Contact(jid); ok {
		for _, kv := range [][2]string{{"address book name", c.FullName}, {"first name", c.FirstName},
			{"push name (self-chosen)", c.PushName}, {"business name", c.BusinessName}} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "%s: %s\n", kv[0], kv[1])
			}
		}
	}
	meta, _ := r.dir.Meta(jid)
	b.WriteString(metaBlock(meta))
	act, err := s.arc.ChatActivity(ctx, jid, 0, 0, 10)
	if err != nil {
		return nil, nil, err
	}
	if act.Messages > 0 {
		var mine int64
		for _, sc := range act.Senders {
			if r.dir.IsSelf(sc.JID) {
				mine = sc.Count
			}
		}
		fmt.Fprintf(&b, "Direct chat: %d messages (%d from you), first %s, last %s\n", act.Messages, mine, f.time(act.First), f.time(act.Last))
	} else {
		b.WriteString("Direct chat: none archived\n")
	}
	groups, err := s.arc.GroupsOf(ctx, jid)
	if err != nil {
		return nil, nil, err
	}
	if len(groups) > 0 {
		names := make([]string, 0, len(groups))
		for _, g := range groups {
			names = append(names, r.dir.Name(g))
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "Shared groups (%d): %s\n", len(names), strings.Join(names, "; "))
	}
	calls, err := s.arc.Calls(ctx, archive.CallQuery{ChatJID: jid, Limit: 1})
	if err == nil && len(calls) > 0 {
		fmt.Fprintf(&b, "Last call: %s (%s)\n", f.time(calls[0].StartedAt), calls[0].Result)
	}
	return textResult(b.String())
}

// --- list_groups

type listGroupsIn struct {
	Query       string `json:"query,omitempty" jsonschema:"filter by group name or description"`
	IncludeLeft bool   `json:"include_left,omitempty" jsonschema:"include groups you are no longer a member of"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max groups (default 50, max 500)"`
	Offset      int    `json:"offset,omitempty" jsonschema:"skip this many (paging)"`
}

func (s *server) listGroups(ctx context.Context, _ *mcp.CallToolRequest, in listGroupsIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	groups, err := s.arc.Groups(ctx)
	if err != nil {
		return nil, nil, err
	}
	last := map[string]int64{}
	for _, c := range r.chats {
		last[c.JID] = c.LastMessageAt
	}
	var out []archive.Group
	for _, g := range groups {
		if !in.IncludeLeft && !g.IsMember {
			continue
		}
		if in.Query != "" && !archive.ContainsFold(r.dir.Name(g.JID)+" "+g.Topic, in.Query) {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return last[out[i].JID] > last[out[j].JID] })
	total := len(out)
	limit, offset := clampInt(in.Limit, 50, 500), max(in.Offset, 0)
	out = out[min(offset, total):min(offset+limit, total)]
	var b strings.Builder
	fmt.Fprintf(&b, "%d groups match; showing %d.\n", total, len(out))
	for _, g := range out {
		fmt.Fprintf(&b, "- %s · %s · %d members · last activity %s", r.dir.Name(g.JID), g.JID, g.ParticipantCount, f.time(last[g.JID]))
		if g.IsCommunity {
			b.WriteString(" · community")
		}
		if g.ParentJID != "" {
			b.WriteString(" · in " + r.dir.Name(g.ParentJID))
		}
		if !g.IsMember {
			b.WriteString(" · left")
		}
		if meta, ok := r.dir.Meta(g.JID); ok {
			if ml := metaLine(meta); ml != "" {
				b.WriteString("\n  " + ml)
			}
		}
		b.WriteString("\n")
	}
	if offset+len(out) < total {
		fmt.Fprintf(&b, "More: call again with offset=%d.\n", offset+len(out))
	}
	return textResult(b.String())
}

// --- list_calls

type listCallsIn struct {
	Chat  string `json:"chat,omitempty" jsonschema:"only calls with this person or group"`
	After string `json:"after,omitempty" jsonschema:"only calls at or after this time"`
	Limit int    `json:"limit,omitempty" jsonschema:"max calls (default 30, max 500)"`
}

func (s *server) listCalls(ctx context.Context, _ *mcp.CallToolRequest, in listCallsIn) (*mcp.CallToolResult, any, error) {
	r, f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	after, _, err := s.times(in.After, "")
	if err != nil {
		return nil, nil, err
	}
	q := archive.CallQuery{After: after, Limit: clampInt(in.Limit, 30, 500)}
	if in.Chat != "" {
		if q.ChatJID, err = r.chat(in.Chat); err != nil {
			return nil, nil, err
		}
	}
	calls, err := s.arc.Calls(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	if len(calls) == 0 {
		return textResult("No calls archived for this filter.\n")
	}
	for _, c := range calls {
		dir := "incoming"
		if c.FromMe {
			dir = "outgoing"
		}
		kind := "voice"
		if c.Video {
			kind = "video"
		}
		if c.IsGroup {
			kind = "group " + kind
		}
		fmt.Fprintf(&b, "[%s] %s %s call with %s · %s", f.time(c.StartedAt), dir, kind, r.dir.Label(c.ChatJID), c.Result)
		if c.Duration > 0 {
			fmt.Fprintf(&b, " · %s", clock(c.Duration))
		}
		b.WriteString("\n")
	}
	return textResult(b.String())
}

// --- update_contact_meta

type updateMetaIn struct {
	Contact     string            `json:"contact" jsonschema:"person or chat: JID, phone number or name"`
	Alias       *string           `json:"alias,omitempty" jsonschema:"custom display name; empty string removes it"`
	Description *string           `json:"description,omitempty" jsonschema:"free-form notes; empty string removes them"`
	Priority    *string           `json:"priority,omitempty" jsonschema:"1-5 or lowest/low/normal/high/top; 0 or empty removes it"`
	Tags        []string          `json:"tags,omitempty" jsonschema:"replace tags with this list (empty list removes all)"`
	AddTags     []string          `json:"add_tags,omitempty" jsonschema:"tags to add"`
	RemoveTags  []string          `json:"remove_tags,omitempty" jsonschema:"tags to remove"`
	Fields      map[string]string `json:"fields,omitempty" jsonschema:"custom fields to set; an empty value removes that field"`
}

func (s *server) updateMeta(ctx context.Context, req *mcp.CallToolRequest, in updateMetaIn) (*mcp.CallToolResult, any, error) {
	r, _, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	jid, err := r.chat(in.Contact)
	if err != nil {
		if jid, err = r.user(in.Contact); err != nil {
			return nil, nil, err
		}
	}
	meta, err := s.arc.ContactMeta(ctx, jid)
	if err != nil {
		return nil, nil, err
	}
	if in.Alias != nil {
		meta.Alias = *in.Alias
	}
	if in.Description != nil {
		meta.Description = *in.Description
	}
	if in.Priority != nil {
		if meta.Priority, err = archive.ParsePriority(*in.Priority); err != nil {
			return nil, nil, err
		}
	}
	if in.Tags != nil || tagsPresent(req) {
		meta.Tags = archive.NormalizeTags(in.Tags)
	}
	meta.Tags = archive.NormalizeTags(append(meta.Tags, in.AddTags...))
	for _, t := range archive.NormalizeTags(in.RemoveTags) {
		meta.Tags = removeString(meta.Tags, t)
	}
	if len(in.Fields) > 0 && meta.Fields == nil {
		meta.Fields = map[string]string{}
	}
	for k, v := range in.Fields {
		if strings.TrimSpace(v) == "" {
			delete(meta.Fields, k)
		} else {
			meta.Fields[k] = v
		}
	}
	if err := s.arc.SetContactMeta(ctx, meta); err != nil {
		return nil, nil, err
	}
	saved, err := s.arc.ContactMeta(ctx, jid)
	if err != nil {
		return nil, nil, err
	}
	return textResult(fmt.Sprintf("Saved notes for %s · %s\n%s", r.dir.WhatsAppName(jid), jid, metaBlock(saved)))
}

// tagsPresent reports whether the arguments contain a top-level "tags" key
// (so an explicit empty list clears the tags).
func tagsPresent(req *mcp.CallToolRequest) bool {
	if req == nil || req.Params == nil {
		return false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(req.Params.Arguments, &raw) != nil {
		return false
	}
	_, ok := raw["tags"]
	return ok
}

// --- live tools

type mediaIn struct {
	Chat      string `json:"chat" jsonschema:"chat JID or name"`
	MessageID string `json:"message_id" jsonschema:"id of the message with the attachment"`
}

func (s *server) downloadMedia(ctx context.Context, _ *mcp.CallToolRequest, in mediaIn) (*mcp.CallToolResult, any, error) {
	r, _, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	jid, err := r.chat(in.Chat)
	if err != nil {
		return nil, nil, err
	}
	path, mimeType, size, err := s.live.DownloadMedia(ctx, jid, in.MessageID)
	if err != nil {
		return nil, nil, err
	}
	return textResult(fmt.Sprintf("Saved %s (%s, %s)\n", path, mimeType, humanSize(size)))
}

type olderIn struct {
	Chat  string `json:"chat" jsonschema:"chat JID, phone or name"`
	Count int    `json:"count,omitempty" jsonschema:"how many older messages to ask for (default 100, max 500)"`
}

func (s *server) requestOlder(ctx context.Context, _ *mcp.CallToolRequest, in olderIn) (*mcp.CallToolResult, any, error) {
	r, _, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	jid, err := r.chat(in.Chat)
	if err != nil {
		return nil, nil, err
	}
	if err := s.live.RequestOlderMessages(ctx, jid, clampInt(in.Count, 100, 500)); err != nil {
		return nil, nil, err
	}
	return textResult(fmt.Sprintf("Asked the phone for older messages of %s. They arrive within a minute if the phone is online; then use get_messages.\n", r.dir.Name(jid)))
}

func clampInt(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(v, maxV)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func editedIDs(msgs []archive.Message) []string {
	var ids []string
	for _, m := range msgs {
		if m.EditedAt != 0 {
			ids = append(ids, m.ID)
		}
	}
	return ids
}
