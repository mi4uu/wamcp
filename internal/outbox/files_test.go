package outbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/demo"
)

var jpeg = append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte("x"), 100)...)

func (f *fakeWA) sendFile(_ context.Context, chat string, file File, caption, _ string, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.sent = append(f.sent, fmt.Sprintf("%s|%s|%s|%s|%s|%d|voice=%v", chat, id, file.Name, file.MimeType, caption, len(file.Data), file.Voice))
	return id, nil
}

func setupFiles(t *testing.T, cfg Config) (*Outbox, *fakeWA, *archive.Store) {
	t.Helper()
	o, wa, store := setup(t, cfg)
	o.WithFiles(wa.sendFile)
	return o, wa, store
}

func TestSendFileData(t *testing.T) {
	o, wa, store := setupFiles(t, Config{})
	ctx := context.Background()
	req := Request{Chat: demo.Anna.String(), Text: "the view", File: &File{Data: jpeg, Name: "dir/../view.jpg"}}
	res, err := o.SendFile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if wa.count() != 1 || !strings.Contains(wa.sent[0], "|view.jpg|image/jpeg|the view|104|voice=false") {
		t.Fatalf("delivered %v", wa.sent)
	}
	recs := audit(t, store)
	if len(recs) != 1 || recs[0].Status != archive.SendSent ||
		!strings.HasPrefix(recs[0].Text, `[file "view.jpg", image/jpeg, 104 bytes, sha256 `) || !strings.HasSuffix(recs[0].Text, "\nthe view") {
		t.Fatalf("audit %+v", recs)
	}
	again, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), Text: "the view", File: &File{Data: jpeg, Name: "view.jpg"}})
	if err != nil || !again.Duplicate || again.MessageID != res.MessageID || wa.count() != 1 {
		t.Fatalf("retry: %+v %v, deliveries %d", again, err, wa.count())
	}
	other, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), Text: "the view", File: &File{Data: append(jpeg, 'y'), Name: "view.jpg"}})
	if err != nil || other.Duplicate || wa.count() != 2 {
		t.Fatalf("different content must be a new send: %+v %v", other, err)
	}
}

func TestSendFilePolicy(t *testing.T) {
	ctx := context.Background()
	o, wa, _ := setupFiles(t, Config{Allow: []string{"me"}, PerHour: 1})
	_, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &File{Data: jpeg, Name: "a.jpg"}})
	if !errors.Is(err, ErrNotAllowed) || wa.count() != 0 {
		t.Fatalf("allowlist: %v, deliveries %d", err, wa.count())
	}
	if _, err := o.SendText(ctx, Request{Chat: demo.Me.String(), Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	_, err = o.SendFile(ctx, Request{Chat: demo.Me.String(), File: &File{Data: jpeg, Name: "a.jpg"}})
	if !errors.Is(err, ErrRateLimited) || wa.count() != 1 {
		t.Fatalf("files share the hourly limit: %v, deliveries %d", err, wa.count())
	}

	plain, _, _ := setup(t, Config{})
	if _, err := plain.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &File{Data: jpeg}}); err == nil {
		t.Fatal("SendFile without WithFiles must refuse")
	}
}

func TestSendFileValidation(t *testing.T) {
	ctx := context.Background()
	o, wa, store := setupFiles(t, Config{MaxFileBytes: 50})
	cases := map[string]File{
		"no source":      {},
		"two sources":    {Data: jpeg[:10], Path: "a.jpg"},
		"path off":       {Path: "a.jpg"},
		"too big":        {Data: jpeg},
		"voice not ogg":  {Data: []byte("ID3abc"), Name: "a.mp3", Voice: true},
		"forward no id":  {ForwardChat: demo.Anna.String()},
		"forward absent": {ForwardChat: demo.Anna.String(), ForwardID: "NOPE"},
	}
	for name, f := range cases {
		f := f
		if _, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &f}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if wa.count() != 0 {
		t.Fatalf("sent %v", wa.sent)
	}
	if _, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &File{Path: "a.jpg"}}); err == nil || !strings.Contains(err.Error(), "--mcp-send-files-from") {
		t.Errorf("path off: %v", err)
	}
	for _, r := range audit(t, store) {
		if r.Status != archive.SendRefused {
			t.Errorf("audit %+v", r)
		}
	}
	ogg, _, _ := setupFiles(t, Config{})
	if _, err := ogg.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &File{Data: []byte("OggS..."), Name: "n.ogg", MimeType: "audio/ogg; codecs=opus", Voice: true}}); err != nil {
		t.Errorf("ogg voice note: %v", err)
	}
}

func TestSendFilePathConfined(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	allowed := filepath.Join(root, "outbox")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(p string) {
		if err := os.WriteFile(p, jpeg, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(allowed, "ok.jpg"))
	write(filepath.Join(root, "secret.jpg"))
	if err := os.Symlink(filepath.Join(root, "secret.jpg"), filepath.Join(allowed, "link.jpg")); err != nil {
		t.Fatal(err)
	}
	o, wa, _ := setupFiles(t, Config{FilesDir: allowed})
	for _, p := range []string{"ok.jpg", filepath.Join(allowed, "ok.jpg")} {
		if _, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), Text: p, File: &File{Path: p}}); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"../secret.jpg", filepath.Join(root, "secret.jpg"), "link.jpg", ".", "missing.jpg"} {
		if _, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &File{Path: p}}); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
	if wa.count() != 2 {
		t.Fatalf("sent %v", wa.sent)
	}
	if _, err := New(nil, Config{FilesDir: filepath.Join(root, "nope")}, nil, nil, nil); err == nil {
		t.Error("missing files directory accepted")
	}
}

func TestSendFileForward(t *testing.T) {
	ctx := context.Background()
	o, wa, store := setupFiles(t, Config{})
	var photo *archive.Message
	for n := 0; n < 26*3 && photo == nil; n++ {
		id := "DEMO" + string(rune('A'+n/26)) + string(rune('A'+n%26))
		if m, err := store.Message(ctx, demo.Anna.String(), id); err == nil && m.Kind == "image" {
			photo = m
		}
	}
	if photo == nil {
		t.Fatal("demo photo not found")
	}
	fwd := File{ForwardChat: demo.Anna.String(), ForwardID: photo.ID}
	if _, err := o.SendFile(ctx, Request{Chat: demo.Me.String(), File: &fwd}); err == nil || !strings.Contains(err.Error(), "download_media") {
		t.Fatalf("not downloaded: %v", err)
	}
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, jpeg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMedia(ctx, demo.Anna.String(), photo.ID, path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SendFile(ctx, Request{Chat: demo.Me.String(), Text: "from Anna", File: &fwd}); err != nil {
		t.Fatal(err)
	}
	if wa.count() != 1 || !strings.Contains(wa.sent[0], "|image/jpeg|from Anna|104|") {
		t.Fatalf("delivered %v", wa.sent)
	}
}
