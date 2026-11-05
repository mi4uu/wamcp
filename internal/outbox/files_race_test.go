package outbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mi4uu/wamcp/internal/demo"
)

// TestSendFilePathRaces covers what a check-then-open would miss: a parent
// directory that is a symlink, a FIFO, and the files directory given through
// a symlinked alias.
func TestSendFilePathRaces(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	allowed := filepath.Join(root, "outbox")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{allowed, outside, filepath.Join(allowed, "real")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(outside, "secret.jpg"), filepath.Join(allowed, "real", "ok.jpg")} {
		if err := os.WriteFile(p, jpeg, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(allowed, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(allowed, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(allowed, alias); err != nil {
		t.Fatal(err)
	}
	o, wa, _ := setupFiles(t, Config{FilesDir: alias})
	send := func(p string) error {
		_, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), Text: p, File: &File{Path: p}})
		return err
	}
	if err := send("sub/secret.jpg"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked parent: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- send("pipe") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("FIFO accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO hung")
	}
	for _, p := range []string{"real/ok.jpg", filepath.Join(alias, "real", "ok.jpg"), filepath.Join(allowed, "real", "ok.jpg")} {
		if err := send(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if wa.count() != 3 {
		t.Fatalf("sent %v", wa.sent)
	}
}

func TestVoiceNoteCheckedByContent(t *testing.T) {
	ctx := context.Background()
	o, wa, _ := setupFiles(t, Config{})
	if _, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &File{Data: []byte("ID3 not ogg"), MimeType: "audio/ogg", Voice: true}}); err == nil {
		t.Error("mp3 claiming audio/ogg accepted as a voice note")
	}
	if _, err := o.SendFile(ctx, Request{Chat: demo.Anna.String(), File: &File{Data: []byte("OggS real"), MimeType: "audio/mpeg", Voice: true}}); err != nil {
		t.Fatal(err)
	}
	if wa.count() != 1 || !strings.Contains(wa.sent[0], "|audio/ogg; codecs=opus|") {
		t.Fatalf("sent %v", wa.sent)
	}
}
