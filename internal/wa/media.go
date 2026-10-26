package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/mi4uu/wamcp/internal/archive"
)

// MediaFile is a downloaded attachment.
type MediaFile struct {
	Path     string
	MimeType string
	Size     int64
}

// DownloadMedia saves the attachment of a message locally (once) and returns
// its path. Expired media is re-requested from the phone. Concurrent calls for
// the same message share one download.
func (m *Manager) DownloadMedia(ctx context.Context, chat, id string) (*MediaFile, error) {
	v, err, _ := m.media.Do(chat+"/"+id, func() (any, error) { return m.downloadMedia(ctx, chat, id) })
	if err != nil {
		return nil, err
	}
	return v.(*MediaFile), nil
}

func (m *Manager) downloadMedia(ctx context.Context, chat, id string) (*MediaFile, error) {
	msg, err := m.arc.Message(ctx, chat, id)
	if err != nil {
		return nil, err
	}
	if msg.Deleted() {
		return nil, errors.New("the message was deleted")
	}
	if msg.MediaPath != "" {
		if fi, err := os.Stat(msg.MediaPath); err == nil {
			return &MediaFile{Path: msg.MediaPath, MimeType: msg.MimeType, Size: fi.Size()}, nil
		}
	}
	if !msg.HasMedia || len(msg.Raw) == 0 {
		return nil, errors.New("the message has no downloadable media")
	}
	key := chat + "/" + id
	m.mediaMu.Lock()
	failedAt, failed := m.mediaFail[key]
	m.mediaMu.Unlock()
	if failed && time.Since(failedAt) < time.Hour && msg.MediaError != "" {
		return nil, errors.New(msg.MediaError)
	}
	cli, err := m.connectedClient()
	if err != nil {
		return nil, err
	}
	select {
	case m.mediaSem <- struct{}{}:
		defer func() { <-m.mediaSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var payload waE2E.Message
	if err := proto.Unmarshal(msg.Raw, &payload); err != nil {
		return nil, fmt.Errorf("decode stored message: %w", err)
	}
	dl := downloadable(&payload)
	if dl == nil {
		return nil, whatsmeow.ErrNothingDownloadableFound
	}
	data, err := cli.Download(ctx, dl) // covers video notes (PTV), unlike DownloadAny
	expired := errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403)
	switch {
	case expired && strings.HasSuffix(chat, "@newsletter"):
		err = errors.New("channel media is no longer on WhatsApp's servers")
	case expired:
		m.log.Info("media expired on the server; asking the phone to re-upload it", "chat", chat, "id", id)
		data, err = m.retryMedia(ctx, cli, msg, &payload)
	}
	if err != nil {
		m.mediaMu.Lock()
		m.mediaFail[key] = time.Now()
		m.mediaMu.Unlock()
		_ = m.arc.SetMedia(ctx, chat, id, "", err.Error())
		return nil, err
	}
	path := m.mediaPath(msg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	if err := m.arc.SetMedia(ctx, chat, id, path, ""); err != nil {
		return nil, err
	}
	m.bus.Publish(Event{Type: "message", Chat: chat, ID: id})
	return &MediaFile{Path: path, MimeType: msg.MimeType, Size: int64(len(data))}, nil
}

func downloadable(p *waE2E.Message) whatsmeow.DownloadableMessage {
	switch {
	case p.GetImageMessage() != nil:
		return p.GetImageMessage()
	case p.GetVideoMessage() != nil:
		return p.GetVideoMessage()
	case p.GetPtvMessage() != nil:
		return p.GetPtvMessage()
	case p.GetAudioMessage() != nil:
		return p.GetAudioMessage()
	case p.GetDocumentMessage() != nil:
		return p.GetDocumentMessage()
	case p.GetStickerMessage() != nil:
		return p.GetStickerMessage()
	}
	return nil
}

func (m *Manager) retryMedia(ctx context.Context, cli *whatsmeow.Client, msg *archive.Message, payload *waE2E.Message) ([]byte, error) {
	dl := downloadable(payload)
	if dl == nil {
		return nil, errors.New("unsupported media type")
	}
	chatJID, err := types.ParseJID(msg.ChatJID)
	if err != nil {
		return nil, err
	}
	sender, _ := types.ParseJID(msg.SenderJID)
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chatJID, Sender: sender, IsFromMe: msg.FromMe, IsGroup: chatJID.Server == types.GroupServer},
		ID:            msg.ID,
	}
	ch := make(chan *events.MediaRetry, 1)
	m.mediaMu.Lock()
	m.mediaWait[msg.ID] = ch
	m.mediaMu.Unlock()
	defer func() {
		m.mediaMu.Lock()
		if m.mediaWait[msg.ID] == ch {
			delete(m.mediaWait, msg.ID)
		}
		m.mediaMu.Unlock()
	}()
	if err := cli.SendMediaRetryReceipt(ctx, info, dl.GetMediaKey()); err != nil {
		return nil, fmt.Errorf("ask phone to re-upload: %w", err)
	}
	select {
	case evt := <-ch:
		if evt.Error != nil {
			return nil, fmt.Errorf("the phone could not re-upload the media (code %d)", evt.Error.Code)
		}
		notif, err := whatsmeow.DecryptMediaRetryNotification(evt, dl.GetMediaKey())
		if err != nil {
			return nil, err
		}
		if notif.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS {
			return nil, fmt.Errorf("the phone no longer has this media (%s)", notif.GetResult())
		}
		return cli.DownloadMediaWithPath(ctx, notif.GetDirectPath(), dl.GetFileEncSHA256(), dl.GetFileSHA256(),
			dl.GetMediaKey(), whatsmeow.GetMediaType(dl), "", false)
	case <-time.After(45 * time.Second):
		return nil, errors.New("timed out waiting for the phone to re-upload the media (is the phone online?)")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) deliverMediaRetry(evt *events.MediaRetry) {
	m.mediaMu.Lock()
	ch := m.mediaWait[evt.MessageID]
	m.mediaMu.Unlock()
	if ch != nil {
		select {
		case ch <- evt:
		default:
		}
	}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

var extByMime = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
	"video/mp4": ".mp4", "video/3gpp": ".3gp", "audio/ogg": ".ogg", "audio/mpeg": ".mp3",
	"audio/mp4": ".m4a", "audio/aac": ".aac", "application/pdf": ".pdf",
}

func (m *Manager) mediaPath(msg *archive.Message) string {
	chatDir := unsafeChars.ReplaceAllString(strings.Split(msg.ChatJID, "@")[0], "_")
	month := time.Unix(msg.Timestamp, 0).Format("2006-01")
	name := unsafeChars.ReplaceAllString(msg.ID, "_")
	if msg.FileName != "" {
		name += "_" + unsafeChars.ReplaceAllString(msg.FileName, "_")
	} else {
		name += MediaExt(msg.MimeType)
	}
	return filepath.Join(m.cfg.DataDir, "media", chatDir, month, name)
}

// MediaExt returns a file extension for a MIME type.
func MediaExt(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	base = strings.TrimSpace(base)
	if ext, ok := extByMime[base]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(base); len(exts) > 0 {
		return exts[len(exts)-1]
	}
	return ".bin"
}

// Avatar returns the path of a cached profile picture thumbnail for jid, or
// "" when the user or group has none (or hides it).
func (m *Manager) Avatar(ctx context.Context, jid string) (string, error) {
	j, err := types.ParseJID(jid)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(m.cfg.DataDir, "avatars")
	base := filepath.Join(dir, unsafeChars.ReplaceAllString(j.User+"_"+j.Server, "_"))
	fresh := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && time.Since(fi.ModTime()) < 24*time.Hour
	}
	if fresh(base + ".jpg") {
		return base + ".jpg", nil
	}
	if fresh(base + ".none") {
		return "", nil
	}
	cli, err := m.connectedClient()
	if err != nil {
		if _, statErr := os.Stat(base + ".jpg"); statErr == nil {
			return base + ".jpg", nil // stale is better than nothing while offline
		}
		return "", nil
	}
	select {
	case m.avatarSem <- struct{}{}:
		defer func() { <-m.avatarSem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := cli.GetProfilePictureInfo(ctx, j, &whatsmeow.GetProfilePictureParams{Preview: true})
	if err != nil || info == nil || info.URL == "" {
		if err == nil || errors.Is(err, whatsmeow.ErrProfilePictureNotSet) || errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) {
			_ = os.WriteFile(base+".none", nil, 0o600)
			_ = os.Remove(base + ".jpg")
			return "", nil
		}
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("profile picture download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".jpg", data, 0o600); err != nil {
		return "", err
	}
	_ = os.Remove(base + ".none")
	return base + ".jpg", nil
}

// RequestOlderMessages asks the phone to send up to count messages older than
// the oldest archived message of chat. They arrive asynchronously as an
// on-demand history sync; the phone must be online.
func (m *Manager) RequestOlderMessages(ctx context.Context, chat string, count int) error {
	cli, err := m.connectedClient()
	if err != nil {
		return err
	}
	oldest, err := m.arc.OldestMessage(ctx, chat)
	if errors.Is(err, archive.ErrNotFound) {
		return errors.New("the chat has no archived messages to anchor the request")
	}
	if err != nil {
		return err
	}
	chatJID, err := types.ParseJID(chat)
	if err != nil {
		return err
	}
	sender, _ := types.ParseJID(oldest.SenderJID)
	count = min(max(count, 1), 500)
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chatJID, Sender: sender, IsFromMe: oldest.FromMe, IsGroup: chatJID.Server == types.GroupServer},
		ID:            oldest.ID,
		Timestamp:     time.Unix(oldest.Timestamp, 0),
	}
	_, err = cli.SendPeerMessage(ctx, cli.BuildHistorySyncRequest(info, count))
	return err
}
