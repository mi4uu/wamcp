package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register decoders for image dimensions
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/ingest"
)

// Actions in this file are only reachable from the authenticated web client;
// the MCP server never sends anything to WhatsApp.

func (m *Manager) target(chat string) (*whatsmeow.Client, types.JID, error) {
	cli, err := m.connectedClient()
	if err != nil {
		return nil, types.EmptyJID, err
	}
	j, err := types.ParseJID(chat)
	if err != nil {
		return nil, types.EmptyJID, fmt.Errorf("invalid chat %q: %w", chat, err)
	}
	return cli, j, nil
}

// record stores a message this client sent, since WhatsApp does not echo it back.
func (m *Manager) record(ctx context.Context, cli *whatsmeow.Client, chat types.JID, resp whatsmeow.SendResponse, msg *waE2E.Message) (*ingest.MessageResult, error) {
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: cli.Store.ID.ToNonAD(), IsFromMe: true,
				IsGroup: chat.Server == types.GroupServer},
			ID: resp.ID, Timestamp: resp.Timestamp, PushName: cli.Store.PushName,
		},
		RawMessage: msg,
	}
	evt.UnwrapRaw()
	res, err := m.ing.Message(ctx, evt, "sent")
	if err == nil {
		m.bus.Publish(Event{Type: "message", Chat: res.ChatJID, ID: res.ID})
	}
	return res, err
}

// quote builds reply context for replyTo, or nil.
func (m *Manager) quote(ctx context.Context, chat, replyTo string) *waE2E.ContextInfo {
	if replyTo == "" {
		return nil
	}
	orig, err := m.arc.Message(ctx, chat, replyTo)
	if err != nil {
		return &waE2E.ContextInfo{StanzaID: proto.String(replyTo)}
	}
	quoted := &waE2E.Message{Conversation: proto.String(orig.Text)}
	if len(orig.Raw) > 0 {
		var raw waE2E.Message
		if proto.Unmarshal(orig.Raw, &raw) == nil {
			quoted = &raw
		}
	}
	ci := &waE2E.ContextInfo{StanzaID: proto.String(replyTo), QuotedMessage: quoted}
	if orig.SenderJID != "" {
		ci.Participant = proto.String(orig.SenderJID)
	}
	return ci
}

// SendText sends a text message, optionally as a reply to replyTo.
func (m *Manager) SendText(ctx context.Context, chat, text, replyTo string) (*ingest.MessageResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty message")
	}
	cli, to, err := m.target(chat)
	if err != nil {
		return nil, err
	}
	msg := &waE2E.Message{Conversation: proto.String(text)}
	if ci := m.quote(ctx, chat, replyTo); ci != nil {
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ci}}
	}
	resp, err := cli.SendMessage(ctx, to, msg)
	if err != nil {
		return nil, err
	}
	return m.record(ctx, cli, to, resp, msg)
}

// MediaUpload is a file to send.
type MediaUpload struct {
	Data     []byte
	FileName string
	MimeType string
	Caption  string
	ReplyTo  string
	Voice    bool // send audio as a voice note
	AsFile   bool // send images/videos as documents (original quality)
}

// SendMedia uploads and sends an image, video, audio or document.
func (m *Manager) SendMedia(ctx context.Context, chat string, up MediaUpload) (*ingest.MessageResult, error) {
	if len(up.Data) == 0 {
		return nil, errors.New("empty file")
	}
	cli, to, err := m.target(chat)
	if err != nil {
		return nil, err
	}
	mimeType := up.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	base, _, _ := strings.Cut(mimeType, ";")
	kind := "document"
	switch {
	case up.AsFile:
	case strings.HasPrefix(base, "image/") && base != "image/svg+xml":
		kind = "image"
	case strings.HasPrefix(base, "video/"):
		kind = "video"
	case strings.HasPrefix(base, "audio/"):
		kind = "audio"
	}
	mediaType := map[string]whatsmeow.MediaType{"image": whatsmeow.MediaImage, "video": whatsmeow.MediaVideo,
		"audio": whatsmeow.MediaAudio, "document": whatsmeow.MediaDocument}[kind]
	u, err := cli.Upload(ctx, up.Data, mediaType)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	ci := m.quote(ctx, chat, up.ReplyTo)
	var msg *waE2E.Message
	switch kind {
	case "image":
		im := &waE2E.ImageMessage{URL: proto.String(u.URL), DirectPath: proto.String(u.DirectPath), MediaKey: u.MediaKey,
			FileEncSHA256: u.FileEncSHA256, FileSHA256: u.FileSHA256, FileLength: proto.Uint64(u.FileLength),
			Mimetype: proto.String(mimeType), Caption: optional(up.Caption), ContextInfo: ci}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(up.Data)); err == nil {
			im.Width, im.Height = proto.Uint32(uint32(cfg.Width)), proto.Uint32(uint32(cfg.Height))
		}
		msg = &waE2E.Message{ImageMessage: im}
	case "video":
		msg = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{URL: proto.String(u.URL), DirectPath: proto.String(u.DirectPath),
			MediaKey: u.MediaKey, FileEncSHA256: u.FileEncSHA256, FileSHA256: u.FileSHA256, FileLength: proto.Uint64(u.FileLength),
			Mimetype: proto.String(mimeType), Caption: optional(up.Caption), ContextInfo: ci}}
	case "audio":
		msg = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{URL: proto.String(u.URL), DirectPath: proto.String(u.DirectPath),
			MediaKey: u.MediaKey, FileEncSHA256: u.FileEncSHA256, FileSHA256: u.FileSHA256, FileLength: proto.Uint64(u.FileLength),
			Mimetype: proto.String(mimeType), PTT: proto.Bool(up.Voice), ContextInfo: ci}}
	default:
		name := up.FileName
		if name == "" {
			name = "file" + MediaExt(mimeType)
		}
		msg = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{URL: proto.String(u.URL), DirectPath: proto.String(u.DirectPath),
			MediaKey: u.MediaKey, FileEncSHA256: u.FileEncSHA256, FileSHA256: u.FileSHA256, FileLength: proto.Uint64(u.FileLength),
			Mimetype: proto.String(mimeType), FileName: proto.String(name), Title: proto.String(name),
			Caption: optional(up.Caption), ContextInfo: ci}}
	}
	resp, err := cli.SendMessage(ctx, to, msg)
	if err != nil {
		return nil, err
	}
	res, err := m.record(ctx, cli, to, resp, msg)
	if err != nil || res == nil || !res.Stored {
		return res, err
	}
	// Keep the local copy so the web client shows it without downloading.
	if stored, err := m.arc.Message(ctx, res.ChatJID, res.ID); err == nil {
		path := m.mediaPath(stored)
		if os.MkdirAll(filepath.Dir(path), 0o700) == nil && os.WriteFile(path, up.Data, 0o600) == nil {
			_ = m.arc.SetMedia(ctx, res.ChatJID, res.ID, path, "")
		}
	}
	return res, nil
}

func optional(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return proto.String(s)
}

func (m *Manager) stored(ctx context.Context, chat, id string) (*archive.Message, types.JID, error) {
	msg, err := m.arc.Message(ctx, chat, id)
	if err != nil {
		return nil, types.EmptyJID, err
	}
	sender, _ := types.ParseJID(msg.SenderJID)
	return msg, sender, nil
}

// React sets (or with an empty emoji removes) this account's reaction.
func (m *Manager) React(ctx context.Context, chat, id, emoji string) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	if to.Server == types.NewsletterServer {
		// Channel reactions need the post's server ID, which is not archived.
		return errors.New("reacting to channel posts is not supported yet")
	}
	_, sender, err := m.stored(ctx, chat, id)
	if err != nil {
		return err
	}
	msg := cli.BuildReaction(to, sender, id, emoji)
	resp, err := cli.SendMessage(ctx, to, msg)
	if err != nil {
		return err
	}
	_, err = m.record(ctx, cli, to, resp, msg)
	return err
}

// Edit changes the text of one of this account's messages.
func (m *Manager) Edit(ctx context.Context, chat, id, text string) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	orig, _, err := m.stored(ctx, chat, id)
	if err != nil {
		return err
	}
	if !orig.FromMe {
		return errors.New("only your own messages can be edited")
	}
	content := &waE2E.Message{Conversation: proto.String(text)}
	if orig.Kind != "text" && len(orig.Raw) > 0 {
		return errors.New("only text messages can be edited here")
	}
	msg := cli.BuildEdit(to, id, content)
	resp, err := cli.SendMessage(ctx, to, msg)
	if err != nil {
		return err
	}
	_, err = m.record(ctx, cli, to, resp, msg)
	return err
}

// Delete deletes a message for everyone (own messages, or any message when
// this account is a group admin).
func (m *Manager) Delete(ctx context.Context, chat, id string) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	orig, sender, err := m.stored(ctx, chat, id)
	if err != nil {
		return err
	}
	if orig.FromMe {
		sender = types.EmptyJID
	}
	msg := cli.BuildRevoke(to, sender, id)
	resp, err := cli.SendMessage(ctx, to, msg)
	if err != nil {
		return err
	}
	_, err = m.record(ctx, cli, to, resp, msg)
	return err
}

func (m *Manager) lastMessageKey(ctx context.Context, chat string) (time.Time, *waCommon.MessageKey) {
	msgs, err := m.arc.Messages(ctx, archive.MessageQuery{ChatJID: chat, Limit: 1})
	if err != nil || len(msgs) == 0 {
		return time.Now(), nil
	}
	last := msgs[0]
	key := &waCommon.MessageKey{RemoteJID: proto.String(chat), FromMe: proto.Bool(last.FromMe), ID: proto.String(last.ID)}
	if !last.FromMe && strings.HasSuffix(chat, "@g.us") {
		key.Participant = proto.String(last.SenderJID)
	}
	return time.Unix(last.Timestamp, 0), key
}

// MarkRead sends read receipts for the unread messages of a chat and syncs
// the read state to the other devices.
func (m *Manager) MarkRead(ctx context.Context, chat string) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	c, err := m.arc.Chat(ctx, chat)
	if err != nil {
		return err
	}
	n := int(min(max(c.UnreadCount, 1), 200))
	msgs, err := m.arc.Messages(ctx, archive.MessageQuery{ChatJID: chat, Limit: n})
	if err != nil {
		return err
	}
	bySender := map[string][]types.MessageID{}
	var lastTS time.Time
	for _, msg := range msgs {
		if msg.FromMe || msg.Kind == "system" || msg.Deleted() {
			continue
		}
		bySender[msg.SenderJID] = append(bySender[msg.SenderJID], msg.ID)
		lastTS = time.Unix(msg.Timestamp, 0)
	}
	for s, ids := range bySender {
		sender := types.EmptyJID
		if to.Server == types.GroupServer {
			sender, _ = types.ParseJID(s)
		}
		if err := cli.MarkRead(ctx, ids, lastTS, to, sender); err != nil {
			return err
		}
	}
	ts, key := m.lastMessageKey(ctx, chat)
	if err := cli.SendAppState(ctx, appstate.BuildMarkChatAsRead(to, true, ts, key)); err != nil {
		m.log.Warn("sync read state", "err", err)
	}
	m.chatFlag(ctx, to, "unread_count", 0)
	return m.ing.ChatRead(ctx, to, true)
}

// SetArchived archives or unarchives a chat on all devices.
func (m *Manager) SetArchived(ctx context.Context, chat string, archived bool) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	ts, key := m.lastMessageKey(ctx, chat)
	if err := cli.SendAppState(ctx, appstate.BuildArchive(to, archived, ts, key)); err != nil {
		return err
	}
	m.chatFlag(ctx, to, "archived", boolInt(archived))
	return nil
}

// SetPinned pins or unpins a chat on all devices.
func (m *Manager) SetPinned(ctx context.Context, chat string, pinned bool) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	if err := cli.SendAppState(ctx, appstate.BuildPin(to, pinned)); err != nil {
		return err
	}
	m.chatFlag(ctx, to, "pinned", boolInt(pinned))
	return nil
}

// SetMuted mutes a chat for d (d < 0 mutes forever, d == 0 unmutes).
func (m *Manager) SetMuted(ctx context.Context, chat string, d time.Duration) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	var patch appstate.PatchInfo
	var until int64
	switch {
	case d == 0:
		patch = appstate.BuildMute(to, false, 0)
	case d < 0:
		forever := int64(-1)
		patch, until = appstate.BuildMuteAbs(to, true, &forever), -1
	default:
		patch, until = appstate.BuildMute(to, true, d), time.Now().Add(d).Unix()
	}
	if err := cli.SendAppState(ctx, patch); err != nil {
		return err
	}
	m.chatFlag(ctx, to, "muted_until", until)
	return nil
}

// SetStarred stars or unstars a message on all devices.
func (m *Manager) SetStarred(ctx context.Context, chat, id string, starred bool) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	orig, sender, err := m.stored(ctx, chat, id)
	if err != nil {
		return err
	}
	if orig.FromMe {
		sender = cli.Store.ID.ToNonAD()
	}
	if err := cli.SendAppState(ctx, appstate.BuildStar(to, sender, id, orig.FromMe, starred)); err != nil {
		return err
	}
	if err := m.ing.Star(ctx, to, id, starred); err != nil {
		return err
	}
	m.bus.Publish(Event{Type: "message", Chat: chat, ID: id})
	return nil
}

// SetTyping shows or hides the "typing…" indicator in a chat.
func (m *Manager) SetTyping(ctx context.Context, chat string, typing bool) error {
	cli, to, err := m.target(chat)
	if err != nil {
		return err
	}
	state := types.ChatPresencePaused
	if typing {
		state = types.ChatPresenceComposing
	}
	return cli.SendChatPresence(ctx, to, state, types.ChatPresenceMediaText)
}

// SetAvailable marks the account online while the web client is in use. The
// daemon stays unavailable otherwise so the phone keeps its notifications.
func (m *Manager) SetAvailable(ctx context.Context, available bool) error {
	cli, err := m.connectedClient()
	if err != nil {
		return err
	}
	p := types.PresenceUnavailable
	if available {
		p = types.PresenceAvailable
	}
	return cli.SendPresence(ctx, p)
}

// SubscribePresence asks for online/last-seen updates of a contact.
func (m *Manager) SubscribePresence(ctx context.Context, jid string) error {
	cli, to, err := m.target(jid)
	if err != nil {
		return err
	}
	return cli.SubscribePresence(ctx, to)
}
