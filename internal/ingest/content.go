package ingest

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Content is the archive-ready projection of a message payload.
type Content struct {
	Kind         string // empty when the payload has nothing user-visible
	Text         string // body, caption or a textual description
	MimeType     string
	FileName     string
	FileSize     int64
	Duration     int64
	HasMedia     bool // downloadable attachment present
	Forwarded    bool
	QuotedID     string
	QuotedSender string // raw JID string from the context info
	QuotedText   string
}

// KeepRaw reports whether the raw payload is worth storing (needed to download
// media later, or to re-parse rich types).
func (c Content) KeepRaw() bool {
	return c.Kind != "" && c.Kind != "text"
}

const quoteLimit = 200

// Extract converts an unwrapped message payload into Content.
func Extract(m *waE2E.Message) Content {
	var c Content
	if m == nil {
		return c
	}
	var ci *waE2E.ContextInfo
	switch {
	case m.GetConversation() != "":
		c.Kind, c.Text = "text", m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		x := m.GetExtendedTextMessage()
		c.Kind, c.Text = "text", x.GetText()
		ci = x.GetContextInfo()
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		c.Kind, c.Text = "image", x.GetCaption()
		c.MimeType, c.FileSize, c.HasMedia = x.GetMimetype(), int64(x.GetFileLength()), true
		ci = x.GetContextInfo()
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		c.Kind, c.Text = "video", x.GetCaption()
		if x.GetGifPlayback() {
			c.Kind = "gif"
		}
		c.MimeType, c.FileSize, c.Duration, c.HasMedia = x.GetMimetype(), int64(x.GetFileLength()), int64(x.GetSeconds()), true
		ci = x.GetContextInfo()
	case m.GetPtvMessage() != nil:
		x := m.GetPtvMessage()
		c.Kind = "video_note"
		c.MimeType, c.FileSize, c.Duration, c.HasMedia = x.GetMimetype(), int64(x.GetFileLength()), int64(x.GetSeconds()), true
		ci = x.GetContextInfo()
	case m.GetAudioMessage() != nil:
		x := m.GetAudioMessage()
		c.Kind = "audio"
		if x.GetPTT() {
			c.Kind = "voice"
		}
		c.MimeType, c.FileSize, c.Duration, c.HasMedia = x.GetMimetype(), int64(x.GetFileLength()), int64(x.GetSeconds()), true
		ci = x.GetContextInfo()
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		c.Kind, c.Text = "document", x.GetCaption()
		c.FileName = firstNonEmpty(x.GetFileName(), x.GetTitle())
		c.MimeType, c.FileSize, c.HasMedia = x.GetMimetype(), int64(x.GetFileLength()), true
		ci = x.GetContextInfo()
	case m.GetStickerMessage() != nil:
		x := m.GetStickerMessage()
		c.Kind = "sticker"
		c.MimeType, c.FileSize, c.HasMedia = x.GetMimetype(), int64(x.GetFileLength()), true
		ci = x.GetContextInfo()
	case m.GetLocationMessage() != nil:
		x := m.GetLocationMessage()
		c.Kind = "location"
		c.Text = describeLocation(x.GetName(), x.GetAddress(), x.GetDegreesLatitude(), x.GetDegreesLongitude())
		ci = x.GetContextInfo()
	case m.GetLiveLocationMessage() != nil:
		x := m.GetLiveLocationMessage()
		c.Kind = "live_location"
		c.Text = describeLocation(x.GetCaption(), "", x.GetDegreesLatitude(), x.GetDegreesLongitude())
		ci = x.GetContextInfo()
	case m.GetContactMessage() != nil:
		x := m.GetContactMessage()
		c.Kind, c.Text = "contact", describeVCard(x.GetDisplayName(), x.GetVcard())
		ci = x.GetContextInfo()
	case m.GetContactsArrayMessage() != nil:
		x := m.GetContactsArrayMessage()
		var parts []string
		for _, ct := range x.GetContacts() {
			parts = append(parts, describeVCard(ct.GetDisplayName(), ct.GetVcard()))
		}
		c.Kind, c.Text = "contact", strings.Join(parts, "; ")
		ci = x.GetContextInfo()
	case firstPoll(m) != nil:
		x := firstPoll(m)
		var opts []string
		for _, o := range x.GetOptions() {
			opts = append(opts, o.GetOptionName())
		}
		c.Kind = "poll"
		c.Text = x.GetName()
		if len(opts) > 0 {
			c.Text += "\noptions: " + strings.Join(opts, " | ")
		}
		ci = x.GetContextInfo()
	case m.GetEventMessage() != nil:
		x := m.GetEventMessage()
		c.Kind = "event"
		parts := []string{x.GetName()}
		if x.GetStartTime() > 0 {
			parts = append(parts, "starts "+time.Unix(x.GetStartTime(), 0).Format("2006-01-02 15:04"))
		}
		if loc := x.GetLocation(); loc != nil {
			parts = append(parts, "at "+describeLocation(loc.GetName(), loc.GetAddress(), loc.GetDegreesLatitude(), loc.GetDegreesLongitude()))
		}
		if x.GetIsCanceled() {
			parts = append(parts, "(canceled)")
		}
		c.Text = joinNonEmpty(parts, ", ")
		if d := x.GetDescription(); d != "" {
			c.Text += "\n" + d
		}
		ci = x.GetContextInfo()
	case m.GetGroupInviteMessage() != nil:
		x := m.GetGroupInviteMessage()
		c.Kind = "group_invite"
		c.Text = joinNonEmpty([]string{"invitation to group " + strconv.Quote(x.GetGroupName()), x.GetCaption()}, ": ")
		ci = x.GetContextInfo()
	case m.GetScheduledCallCreationMessage() != nil:
		x := m.GetScheduledCallCreationMessage()
		c.Kind = "event"
		c.Text = "scheduled call " + strconv.Quote(x.GetTitle())
		if ms := x.GetScheduledTimestampMS(); ms > 0 {
			c.Text += " at " + time.UnixMilli(ms).Format("2006-01-02 15:04")
		}
	case m.GetButtonsResponseMessage() != nil:
		x := m.GetButtonsResponseMessage()
		c.Kind, c.Text = "text", x.GetSelectedDisplayText()
		ci = x.GetContextInfo()
	case m.GetTemplateButtonReplyMessage() != nil:
		x := m.GetTemplateButtonReplyMessage()
		c.Kind, c.Text = "text", x.GetSelectedDisplayText()
		ci = x.GetContextInfo()
	case m.GetListResponseMessage() != nil:
		x := m.GetListResponseMessage()
		c.Kind, c.Text = "text", firstNonEmpty(x.GetTitle(), x.GetSingleSelectReply().GetSelectedRowID())
		ci = x.GetContextInfo()
	case m.GetInteractiveResponseMessage() != nil:
		x := m.GetInteractiveResponseMessage()
		c.Kind, c.Text = "text", x.GetBody().GetText()
		ci = x.GetContextInfo()
	case m.GetButtonsMessage() != nil:
		x := m.GetButtonsMessage()
		c.Kind, c.Text = "text", x.GetContentText()
		ci = x.GetContextInfo()
	case m.GetTemplateMessage() != nil:
		x := m.GetTemplateMessage()
		c.Kind, c.Text = "text", x.GetHydratedTemplate().GetHydratedContentText()
		ci = x.GetContextInfo()
	case m.GetListMessage() != nil:
		x := m.GetListMessage()
		c.Kind, c.Text = "text", joinNonEmpty([]string{x.GetTitle(), x.GetDescription()}, "\n")
		ci = x.GetContextInfo()
	case m.GetInteractiveMessage() != nil:
		x := m.GetInteractiveMessage()
		c.Kind, c.Text = "text", joinNonEmpty([]string{x.GetHeader().GetTitle(), x.GetBody().GetText()}, "\n")
		ci = x.GetContextInfo()
	case m.GetProductMessage() != nil:
		x := m.GetProductMessage()
		c.Kind = "product"
		c.Text = joinNonEmpty([]string{x.GetProduct().GetTitle(), x.GetBody()}, "\n")
		ci = x.GetContextInfo()
	case m.GetRequestPhoneNumberMessage() != nil:
		c.Kind, c.Text = "system", "requested your phone number"
	case m.GetAlbumMessage() != nil, m.GetKeepInChatMessage() != nil, m.GetPinInChatMessage() != nil,
		m.GetPollUpdateMessage() != nil, m.GetEncReactionMessage() != nil, m.GetEncCommentMessage() != nil,
		m.GetEncEventResponseMessage() != nil, m.GetMessageHistoryBundle() != nil, m.GetMessageHistoryNotice() != nil:
		// Bookkeeping payloads without standalone content.
		return c
	default:
		if inner := futureProofInner(m); inner != nil {
			return Extract(inner)
		}
		if name := contentField(m); name != "" {
			c.Kind, c.Text = "unsupported", "["+name+"]"
		}
		return c
	}
	if ci != nil {
		c.Forwarded = ci.GetIsForwarded()
		c.QuotedID = ci.GetStanzaID()
		c.QuotedSender = ci.GetParticipant()
		if q := ci.GetQuotedMessage(); q != nil && c.QuotedID != "" {
			c.QuotedText = truncate(Extract(q).Summary(), quoteLimit)
		}
	}
	return c
}

// Summary is a one-line description, used for quotes and previews.
func (c Content) Summary() string {
	text := strings.Join(strings.Fields(c.Text), " ")
	switch {
	case c.Kind == "" || c.Kind == "text":
		return text
	case text != "":
		return "[" + c.Kind + "] " + text
	case c.FileName != "":
		return "[" + c.Kind + "] " + c.FileName
	}
	return "[" + c.Kind + "]"
}

func firstPoll(m *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessage(), m.GetPollCreationMessageV2(),
		m.GetPollCreationMessageV3(), m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6()} {
		if p != nil {
			return p
		}
	}
	return nil
}

var futureProofName = (&waE2E.FutureProofMessage{}).ProtoReflect().Descriptor().FullName()

// futureProofInner digs into wrapper payloads (spoilers, questions, mentions of
// groups, ...) that UnwrapRaw does not know about.
func futureProofInner(m *waE2E.Message) *waE2E.Message {
	var inner *waE2E.Message
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() && fd.Message().FullName() == futureProofName {
			if fp, ok := v.Message().Interface().(*waE2E.FutureProofMessage); ok && fp.GetMessage() != nil {
				inner = fp.GetMessage()
				return false
			}
		}
		return true
	})
	return inner
}

// contentField returns the name of the first populated field that carries
// content (ignoring encryption bookkeeping), or "" when there is none.
func contentField(m *waE2E.Message) string {
	var name string
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		switch fd.Name() {
		case "messageContextInfo", "senderKeyDistributionMessage", "fastRatchetKeySenderKeyDistributionMessage",
			"protocolMessage", "reactionMessage":
			return true
		}
		name = string(fd.Name())
		return false
	})
	return name
}

// heavyFields are dropped from stored payloads: thumbnails and quoted copies
// are large and not needed to download media later.
var heavyFields = map[protoreflect.Name]bool{
	"JPEGThumbnail": true, "jpegThumbnail": true, "pngThumbnail": true, "thumbnail": true,
	"quotedMessage": true, "streamingSidecar": true, "waveform": true,
}

// RawForArchive serializes the payload without heavy fields.
func RawForArchive(m *waE2E.Message) []byte {
	c := proto.Clone(m).(*waE2E.Message)
	stripHeavy(c.ProtoReflect())
	b, err := proto.Marshal(c)
	if err != nil {
		return nil
	}
	return b
}

func stripHeavy(m protoreflect.Message) {
	var clear []protoreflect.FieldDescriptor
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case heavyFields[fd.Name()]:
			clear = append(clear, fd)
		case fd.Kind() == protoreflect.MessageKind && fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				stripHeavy(l.Get(i).Message())
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
			stripHeavy(v.Message())
		}
		return true
	})
	for _, fd := range clear {
		m.Clear(fd)
	}
}

func describeLocation(name, address string, lat, lon float64) string {
	parts := []string{name, address}
	if lat != 0 || lon != 0 {
		parts = append(parts, fmt.Sprintf("(%.5f, %.5f)", lat, lon))
	}
	return joinNonEmpty(parts, " ")
}

// describeVCard renders "Name (+48 600 000 000)" using TEL lines of the vCard.
func describeVCard(name, vcard string) string {
	var phones []string
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "TEL") {
			continue
		}
		if _, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(v) != "" {
			phones = append(phones, strings.TrimSpace(v))
		}
	}
	if len(phones) == 0 {
		return name
	}
	return name + " (" + strings.Join(phones, ", ") + ")"
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func joinNonEmpty(parts []string, sep string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
