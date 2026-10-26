package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/ingest"
	"github.com/mi4uu/wamcp/internal/wa"
)

type ingestResult = ingest.MessageResult

type metaJSON struct {
	Alias       string            `json:"alias"`
	Description string            `json:"description"`
	Priority    int               `json:"priority"`
	Tags        []string          `json:"tags"`
	Fields      map[string]string `json:"fields"`
}

func toMetaJSON(m archive.ContactMeta) metaJSON {
	out := metaJSON{Alias: m.Alias, Description: m.Description, Priority: m.Priority, Tags: m.Tags, Fields: m.Fields}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.Fields == nil {
		out.Fields = map[string]string{}
	}
	return out
}

type previewJSON struct {
	Sender  string `json:"sender"`
	FromMe  bool   `json:"from_me"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Deleted bool   `json:"deleted,omitempty"`
}

type chatJSON struct {
	JID          string       `json:"jid"`
	Kind         string       `json:"kind"`
	Name         string       `json:"name"`
	WAName       string       `json:"wa_name"`
	Phone        string       `json:"phone,omitempty"`
	LastTS       int64        `json:"last_ts"`
	Unread       int64        `json:"unread"`
	MarkedUnread bool         `json:"marked_unread"`
	Pinned       bool         `json:"pinned"`
	Archived     bool         `json:"archived"`
	MutedUntil   int64        `json:"muted_until"`
	Ephemeral    int64        `json:"ephemeral"`
	Count        int64        `json:"count"`
	Preview      *previewJSON `json:"preview,omitempty"`
	Meta         metaJSON     `json:"meta"`
	Member       bool         `json:"member"`
}

func (s *server) chatView(dir *archive.Directory, c archive.Chat) chatJSON {
	meta, _ := dir.Meta(c.JID)
	out := chatJSON{JID: c.JID, Kind: c.Kind, Name: dir.Name(c.JID), WAName: dir.WhatsAppName(c.JID),
		LastTS: c.LastMessageAt, Unread: c.UnreadCount, MarkedUnread: c.MarkedUnread, Pinned: c.Pinned,
		Archived: c.Archived, MutedUntil: c.MutedUntil, Ephemeral: c.EphemeralSeconds, Count: c.MessageCount,
		Meta: toMetaJSON(meta), Member: true}
	if c.Kind == "direct" {
		out.Phone = archive.Phone(c.JID)
	}
	if g, ok := dir.Group(c.JID); ok {
		out.Member = g.IsMember
	}
	if c.LastKind != "" {
		p := &previewJSON{FromMe: c.LastFromMe, Kind: c.LastKind, Text: c.LastText, Deleted: c.LastDeleted}
		if c.Kind == "group" && !c.LastFromMe && c.LastSender != "" {
			p.Sender = dir.Name(c.LastSender)
		}
		out.Preview = p
	}
	return out
}

func (s *server) apiChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir, err := s.opt.Archive.Directory(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	chats, err := s.opt.Archive.Chats(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	out := make([]chatJSON, 0, len(chats))
	for _, c := range chats {
		out = append(out, s.chatView(dir, c))
	}
	writeJSON(w, out)
}

type personJSON struct {
	JID      string   `json:"jid"`
	Name     string   `json:"name"`
	Phone    string   `json:"phone,omitempty"`
	Role     string   `json:"role,omitempty"`
	Me       bool     `json:"me,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

func person(dir *archive.Directory, jid string) personJSON {
	p := personJSON{JID: jid, Name: dir.Name(jid), Me: dir.IsSelf(jid)}
	if ph := archive.Phone(dir.Canonical(jid)); strings.HasPrefix(ph, "+") {
		p.Phone = ph
	}
	if m, ok := dir.Meta(jid); ok {
		p.Priority, p.Tags = m.Priority, m.Tags
	}
	return p
}

func (s *server) apiChatInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jid := r.PathValue("jid")
	dir, err := s.opt.Archive.Directory(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	out := map[string]any{"jid": jid, "name": dir.Name(jid), "wa_name": dir.WhatsAppName(jid)}
	if c, err := s.opt.Archive.Chat(ctx, jid); err == nil {
		out["chat"] = s.chatView(dir, *c)
	}
	meta, _ := dir.Meta(jid)
	out["meta"] = toMetaJSON(meta)
	if act, err := s.opt.Archive.ChatActivity(ctx, jid, 0, 0, 5); err == nil {
		senders := []map[string]any{}
		for _, sc := range act.Senders {
			senders = append(senders, map[string]any{"name": person(dir, sc.JID).Name, "me": dir.IsSelf(sc.JID), "count": sc.Count})
		}
		out["activity"] = map[string]any{"messages": act.Messages, "first": act.First, "last": act.Last,
			"media": act.Media, "senders": senders}
	}
	if g, ok := dir.Group(jid); ok {
		ps, err := s.opt.Archive.Participants(ctx, jid)
		if err != nil {
			writeError(w, 500, err)
			return
		}
		members := make([]personJSON, 0, len(ps))
		for _, p := range ps {
			pj := person(dir, p.JID)
			pj.Role = p.Role
			members = append(members, pj)
		}
		out["group"] = map[string]any{"topic": g.Topic, "owner": dir.Name(g.OwnerJID), "created": g.CreatedAt,
			"announce": g.Announce, "locked": g.Locked, "community": g.IsCommunity, "parent": g.ParentJID,
			"parent_name": dir.Name(g.ParentJID), "member": g.IsMember, "count": g.ParticipantCount, "members": members}
	} else if strings.HasSuffix(jid, "@s.whatsapp.net") || strings.HasSuffix(jid, "@lid") {
		c, _ := dir.Contact(jid)
		groups, _ := s.opt.Archive.GroupsOf(ctx, jid)
		shared := []map[string]string{}
		for _, g := range groups {
			shared = append(shared, map[string]string{"jid": g, "name": dir.Name(g)})
		}
		sort.Slice(shared, func(i, j int) bool { return shared[i]["name"] < shared[j]["name"] })
		out["contact"] = map[string]any{"phone": archive.Phone(dir.Canonical(jid)), "full_name": c.FullName,
			"first_name": c.FirstName, "push_name": c.PushName, "business_name": c.BusinessName, "groups": shared}
	}
	writeJSON(w, out)
}

type reactionJSON struct {
	Emoji string `json:"emoji"`
	Name  string `json:"name"`
	Me    bool   `json:"me"`
}

type quoteJSON struct {
	ID     string `json:"id"`
	Sender string `json:"sender"`
	Me     bool   `json:"me"`
	Text   string `json:"text"`
}

type versionJSON struct {
	Text       string `json:"text"`
	ReplacedAt int64  `json:"replaced_at"`
}

type messageJSON struct {
	PK         int64          `json:"pk"`
	ID         string         `json:"id"`
	Chat       string         `json:"chat"`
	ChatName   string         `json:"chat_name,omitempty"`
	Sender     string         `json:"sender"`
	SenderName string         `json:"sender_name"`
	FromMe     bool           `json:"from_me"`
	TS         int64          `json:"ts"`
	Kind       string         `json:"kind"`
	Text       string         `json:"text"`
	Quote      *quoteJSON     `json:"quote,omitempty"`
	Forwarded  bool           `json:"forwarded,omitempty"`
	ViewOnce   bool           `json:"view_once,omitempty"`
	Starred    bool           `json:"starred,omitempty"`
	Edited     bool           `json:"edited,omitempty"`
	Deleted    bool           `json:"deleted,omitempty"`
	DeletedAt  int64          `json:"deleted_at,omitempty"`
	DeletedBy  string         `json:"deleted_by,omitempty"`
	Mime       string         `json:"mime,omitempty"`
	FileName   string         `json:"file_name,omitempty"`
	FileSize   int64          `json:"file_size,omitempty"`
	Duration   int64          `json:"duration,omitempty"`
	HasMedia   bool           `json:"has_media,omitempty"`
	Downloaded bool           `json:"downloaded,omitempty"`
	MediaError string         `json:"media_error,omitempty"`
	Ack        int            `json:"ack,omitempty"`
	Reactions  []reactionJSON `json:"reactions,omitempty"`
	Versions   []versionJSON  `json:"versions,omitempty"` // earlier texts of an edited message
}

func (s *server) messageViews(r *http.Request, dir *archive.Directory, msgs []archive.Message, withChat bool) ([]messageJSON, error) {
	byChat := map[string][]string{}
	edited := map[string][]string{}
	for _, m := range msgs {
		byChat[m.ChatJID] = append(byChat[m.ChatJID], m.ID)
		if m.EditedAt != 0 {
			edited[m.ChatJID] = append(edited[m.ChatJID], m.ID)
		}
	}
	reactions := map[string]map[string][]archive.Reaction{}
	for chat, ids := range byChat {
		rs, err := s.opt.Archive.Reactions(r.Context(), chat, ids)
		if err != nil {
			return nil, err
		}
		reactions[chat] = rs
	}
	versions := map[string]map[string][]archive.Version{}
	for chat, ids := range edited {
		vs, err := s.opt.Archive.EditHistory(r.Context(), chat, ids)
		if err != nil {
			return nil, err
		}
		versions[chat] = vs
	}
	out := make([]messageJSON, 0, len(msgs))
	for _, m := range msgs {
		v := messageJSON{PK: m.PK, ID: m.ID, Chat: m.ChatJID, Sender: m.SenderJID, FromMe: m.FromMe || dir.IsSelf(m.SenderJID),
			TS: m.Timestamp, Kind: m.Kind, Text: m.Text, Forwarded: m.Forwarded, ViewOnce: m.ViewOnce, Starred: m.Starred,
			Edited: m.EditedAt != 0, Deleted: m.Deleted(), Mime: m.MimeType, FileName: m.FileName, FileSize: m.FileSize,
			Duration: m.Duration, HasMedia: m.HasMedia, Downloaded: m.MediaPath != "", MediaError: m.MediaError, Ack: m.Ack}
		switch {
		case v.FromMe:
			v.SenderName = "You"
		case m.SenderJID != "":
			v.SenderName = dir.Name(m.SenderJID)
			if strings.HasPrefix(v.SenderName, "hidden number") && m.PushName != "" {
				v.SenderName = "~" + m.PushName
			}
		default:
			v.SenderName = m.PushName
		}
		if withChat {
			v.ChatName = dir.Name(m.ChatJID)
		}
		if m.Deleted() {
			v.DeletedAt = m.RevokedAt
			switch {
			case m.RevokedBy == "" || m.RevokedBy == m.SenderJID:
				v.DeletedBy = v.SenderName
			case dir.IsSelf(m.RevokedBy):
				v.DeletedBy = "You"
			default:
				v.DeletedBy = dir.Name(m.RevokedBy)
			}
		}
		if m.Kind == "system" {
			v.Text = jidText(dir, m.Text)
		}
		if m.QuotedID != "" {
			q := &quoteJSON{ID: m.QuotedID, Text: m.QuotedText, Me: dir.IsSelf(m.QuotedSender)}
			if q.Me {
				q.Sender = "You"
			} else if m.QuotedSender != "" {
				q.Sender = dir.Name(m.QuotedSender)
			}
			v.Quote = q
		}
		for _, ver := range versions[m.ChatJID][m.ID] {
			v.Versions = append(v.Versions, versionJSON{Text: ver.Text, ReplacedAt: ver.ReplacedAt})
		}
		for _, rc := range reactions[m.ChatJID][m.ID] {
			me := dir.IsSelf(rc.SenderJID)
			name := dir.Name(rc.SenderJID)
			if me {
				name = "You"
			}
			v.Reactions = append(v.Reactions, reactionJSON{Emoji: rc.Emoji, Name: name, Me: me})
		}
		out = append(out, v)
	}
	return out, nil
}

func jidText(dir *archive.Directory, text string) string {
	words := strings.Fields(text)
	for i, w := range words {
		trimmed := strings.TrimRight(w, ",")
		if strings.Contains(trimmed, "@s.whatsapp.net") || strings.Contains(trimmed, "@lid") {
			words[i] = dir.Name(trimmed) + w[len(trimmed):]
		}
	}
	return strings.Join(words, " ")
}

func queryInt(r *http.Request, key string, def int64) int64 {
	v, err := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if err != nil {
		return def
	}
	return v
}

func (s *server) apiMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chat := r.PathValue("jid")
	dir, err := s.opt.Archive.Directory(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	limit := int(min(max(queryInt(r, "limit", 60), 1), 500))
	var msgs []archive.Message
	q := r.URL.Query()
	switch {
	case q.Get("around") != "":
		msgs, err = s.opt.Archive.MessageContext(ctx, chat, q.Get("around"), limit/2, limit/2)
	case q.Get("after_ts") != "":
		msgs, err = s.opt.Archive.Messages(ctx, archive.MessageQuery{ChatJID: chat, After: queryInt(r, "after_ts", 0),
			Limit: limit, OldestFirst: true})
	default:
		msgs, err = s.opt.Archive.Messages(ctx, archive.MessageQuery{ChatJID: chat, Before: queryInt(r, "before_ts", 0),
			BeforePK: queryInt(r, "before_pk", 0), Limit: limit, MediaOnly: q.Get("media") == "1", Starred: q.Get("starred") == "1"})
	}
	if errors.Is(err, archive.ErrNotFound) {
		writeError(w, 404, err)
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	views, err := s.messageViews(r, dir, msgs, false)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"messages": views, "has_more": len(msgs) >= limit})
}

func (s *server) apiSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir, err := s.opt.Archive.Directory(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	msgs, err := s.opt.Archive.Search(ctx, archive.SearchQuery{Text: r.URL.Query().Get("q"), ChatJID: r.URL.Query().Get("chat"),
		Limit: int(min(max(queryInt(r, "limit", 50), 1), 200)), Offset: int(max(queryInt(r, "offset", 0), 0))})
	if err != nil {
		writeError(w, 400, err)
		return
	}
	views, err := s.messageViews(r, dir, msgs, true)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, views)
}

func (s *server) apiStarred(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir, err := s.opt.Archive.Directory(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	msgs, err := s.opt.Archive.Messages(ctx, archive.MessageQuery{Starred: true, Limit: 500})
	if err != nil {
		writeError(w, 500, err)
		return
	}
	views, err := s.messageViews(r, dir, msgs, true)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, views)
}

func (s *server) apiCalls(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir, err := s.opt.Archive.Directory(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	calls, err := s.opt.Archive.Calls(ctx, archive.CallQuery{ChatJID: r.URL.Query().Get("chat"), Limit: 300})
	if err != nil {
		writeError(w, 500, err)
		return
	}
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{"id": c.ID, "chat": c.ChatJID, "name": dir.Name(c.ChatJID), "from_me": c.FromMe,
			"video": c.Video, "group": c.IsGroup, "ts": c.StartedAt, "duration": c.Duration, "result": c.Result})
	}
	writeJSON(w, out)
}

func (s *server) apiContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dir, err := s.opt.Archive.Directory(ctx)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	q := r.URL.Query().Get("q")
	seen := map[string]bool{}
	var out []personJSON
	add := func(jid string) {
		jid = dir.Canonical(jid)
		if seen[jid] || !(strings.HasSuffix(jid, "@s.whatsapp.net") || strings.HasSuffix(jid, "@lid")) {
			return
		}
		seen[jid] = true
		p := person(dir, jid)
		c, _ := dir.Contact(jid)
		if q != "" && !archive.ContainsFold(p.Name+" "+c.PushName+" "+c.FullName+" "+p.Phone+" "+jid, q) {
			return
		}
		out = append(out, p)
	}
	for _, c := range dir.Contacts() {
		add(c.JID)
	}
	chats, err := s.opt.Archive.Chats(ctx)
	if err == nil {
		for _, c := range chats {
			if c.Kind == "direct" {
				add(c.JID)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return archive.SearchText(out[i].Name) < archive.SearchText(out[j].Name)
	})
	if len(out) > 500 {
		out = out[:500]
	}
	writeJSON(w, out)
}

func (s *server) apiMedia(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chat, id := r.PathValue("chat"), r.PathValue("id")
	msg, err := s.opt.Archive.Message(ctx, chat, id)
	if err != nil {
		writeError(w, 404, err)
		return
	}
	path := msg.MediaPath
	if _, statErr := os.Stat(path); path == "" || statErr != nil {
		f, err := s.opt.Daemon.DownloadMedia(ctx, chat, id)
		if err != nil {
			writeError(w, 502, err)
			return
		}
		path = f.Path
	}
	if msg.MimeType != "" {
		w.Header().Set("Content-Type", strings.TrimSpace(strings.Split(msg.MimeType, ";")[0]))
	}
	name := msg.FileName
	if name == "" {
		name = msg.ID + wa.MediaExt(msg.MimeType)
	}
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, name))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	http.ServeFile(w, r, path)
}

func (s *server) apiAvatar(w http.ResponseWriter, r *http.Request) {
	path, err := s.opt.Daemon.Avatar(r.Context(), r.PathValue("jid"))
	if err != nil || path == "" {
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

func (s *server) apiGetMeta(w http.ResponseWriter, r *http.Request) {
	m, err := s.opt.Archive.ContactMeta(r.Context(), r.PathValue("jid"))
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, toMetaJSON(m))
}

func (s *server) apiPutMeta(w http.ResponseWriter, r *http.Request) {
	var in metaJSON
	if err := readJSON(r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	m := archive.ContactMeta{JID: r.PathValue("jid"), Alias: in.Alias, Description: in.Description,
		Priority: in.Priority, Tags: in.Tags, Fields: in.Fields}
	if err := s.opt.Archive.SetContactMeta(r.Context(), m); err != nil {
		writeError(w, 400, err)
		return
	}
	s.opt.Daemon.Bus().Publish(wa.Event{Type: "chat", Chat: m.JID})
	saved, _ := s.opt.Archive.ContactMeta(r.Context(), m.JID)
	writeJSON(w, toMetaJSON(saved))
}

func (s *server) apiSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text    string `json:"text"`
		ReplyTo string `json:"reply_to"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	res, err := s.opt.Daemon.SendText(r.Context(), r.PathValue("jid"), in.Text, in.ReplyTo)
	if err != nil {
		writeError(w, 502, err)
		return
	}
	writeJSON(w, res)
}

func (s *server) apiSendMedia(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, 400, fmt.Errorf("upload too large or invalid (max 100 MB): %w", err))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	up := wa.MediaUpload{Data: data, FileName: hdr.Filename, MimeType: hdr.Header.Get("Content-Type"),
		Caption: r.FormValue("caption"), ReplyTo: r.FormValue("reply_to"),
		AsFile: r.FormValue("as_file") == "1", Voice: r.FormValue("voice") == "1"}
	if up.MimeType == "" {
		up.MimeType = http.DetectContentType(data)
	}
	res, err := s.opt.Daemon.SendMedia(r.Context(), r.PathValue("jid"), up)
	if err != nil {
		writeError(w, 502, err)
		return
	}
	writeJSON(w, res)
}

type messageAction func(r *http.Request, chat, id string) error

func (s *server) action(w http.ResponseWriter, r *http.Request, fn messageAction) {
	if err := fn(r, r.PathValue("jid"), r.PathValue("id")); err != nil {
		writeError(w, 502, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) apiReact(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, func(r *http.Request, chat, id string) error {
		var in struct {
			Emoji string `json:"emoji"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		return s.opt.Daemon.React(r.Context(), chat, id, in.Emoji)
	})
}

func (s *server) apiEdit(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, func(r *http.Request, chat, id string) error {
		var in struct {
			Text string `json:"text"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if strings.TrimSpace(in.Text) == "" {
			return errors.New("empty text")
		}
		return s.opt.Daemon.Edit(r.Context(), chat, id, in.Text)
	})
}

func (s *server) apiDelete(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, func(r *http.Request, chat, id string) error { return s.opt.Daemon.Delete(r.Context(), chat, id) })
}

func (s *server) apiStar(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, func(r *http.Request, chat, id string) error {
		var in struct {
			Starred bool `json:"starred"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		return s.opt.Daemon.SetStarred(r.Context(), chat, id, in.Starred)
	})
}

func (s *server) chatAction(w http.ResponseWriter, r *http.Request, fn func(r *http.Request, chat string) error) {
	if err := fn(r, r.PathValue("jid")); err != nil {
		writeError(w, 502, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func boolBody(r *http.Request, key string) (bool, error) {
	var in map[string]bool
	if err := readJSON(r, &in); err != nil {
		return false, err
	}
	return in[key], nil
}

func (s *server) apiRead(w http.ResponseWriter, r *http.Request) {
	s.chatAction(w, r, func(r *http.Request, chat string) error { return s.opt.Daemon.MarkRead(r.Context(), chat) })
}

func (s *server) apiArchive(w http.ResponseWriter, r *http.Request) {
	s.chatAction(w, r, func(r *http.Request, chat string) error {
		v, err := boolBody(r, "archived")
		if err != nil {
			return err
		}
		return s.opt.Daemon.SetArchived(r.Context(), chat, v)
	})
}

func (s *server) apiPin(w http.ResponseWriter, r *http.Request) {
	s.chatAction(w, r, func(r *http.Request, chat string) error {
		v, err := boolBody(r, "pinned")
		if err != nil {
			return err
		}
		return s.opt.Daemon.SetPinned(r.Context(), chat, v)
	})
}

func (s *server) apiMute(w http.ResponseWriter, r *http.Request) {
	s.chatAction(w, r, func(r *http.Request, chat string) error {
		var in struct {
			Duration string `json:"duration"` // 8h, 1w, always, off
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		var d time.Duration
		switch in.Duration {
		case "off", "":
		case "always":
			d = -1
		case "8h":
			d = 8 * time.Hour
		case "1w":
			d = 7 * 24 * time.Hour
		default:
			return fmt.Errorf("unknown mute duration %q", in.Duration)
		}
		return s.opt.Daemon.SetMuted(r.Context(), chat, d)
	})
}

func (s *server) apiTyping(w http.ResponseWriter, r *http.Request) {
	s.chatAction(w, r, func(r *http.Request, chat string) error {
		v, err := boolBody(r, "typing")
		if err != nil {
			return err
		}
		return s.opt.Daemon.SetTyping(r.Context(), chat, v)
	})
}

func (s *server) apiOlder(w http.ResponseWriter, r *http.Request) {
	s.chatAction(w, r, func(r *http.Request, chat string) error {
		return s.opt.Daemon.RequestOlderMessages(r.Context(), chat, 100)
	})
}

func (s *server) apiSubscribePresence(w http.ResponseWriter, r *http.Request) {
	s.chatAction(w, r, func(r *http.Request, chat string) error { return s.opt.Daemon.SubscribePresence(r.Context(), chat) })
}

func (s *server) apiPresence(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Available bool   `json:"available"`
		Tab       string `json:"tab"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	if in.Tab == "" {
		in.Tab = "default"
	}
	s.setTabPresence(in.Tab, in.Available, false)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) apiDeviceLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.opt.Daemon.Logout(r.Context()); err != nil {
		writeError(w, 502, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) apiStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.opt.Archive.Stats(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"daemon": s.opt.Daemon.Status(), "stats": stats})
}
