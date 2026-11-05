package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

// File is an attachment for SendFile. Exactly one of Data, Path or
// ForwardChat with ForwardID names the content.
type File struct {
	// Data is the file itself.
	Data []byte `json:",omitempty"`
	// Path is a file under Config.FilesDir, absolute or relative to it.
	Path string `json:",omitempty"`
	// ForwardChat and ForwardID name an archived message whose downloaded
	// media is sent again.
	ForwardChat string `json:",omitempty"`
	ForwardID   string `json:",omitempty"`
	// Name and MimeType are guessed when empty.
	Name     string `json:",omitempty"`
	MimeType string `json:",omitempty"`
	// Voice sends audio/ogg (Opus) as a voice note.
	Voice bool `json:",omitempty"`
	// AsDocument sends images and videos as files, in original quality.
	AsDocument bool `json:",omitempty"`
}

func resolveDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("files directory: %w", err)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("files directory %s is not a directory", dir)
	}
	return real, nil
}

// resolve loads the content of f and fills in its name and type.
func (o *Outbox) resolve(ctx context.Context, f File) (File, error) {
	sources := 0
	for _, set := range []bool{len(f.Data) > 0, f.Path != "", f.ForwardChat != "" || f.ForwardID != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return File{}, errors.New("give exactly one of: file data, a path, or a message to forward")
	}
	switch {
	case f.Path != "":
		data, name, err := o.readPath(f.Path)
		if err != nil {
			return File{}, err
		}
		f.Data = data
		if f.Name == "" {
			f.Name = name
		}
	case f.ForwardChat != "" || f.ForwardID != "":
		if f.ForwardChat == "" || f.ForwardID == "" {
			return File{}, errors.New("forwarding needs both the chat and the message id")
		}
		m, err := o.arc.Message(ctx, f.ForwardChat, f.ForwardID)
		if err != nil {
			return File{}, fmt.Errorf("message %s in %s: %w", f.ForwardID, f.ForwardChat, err)
		}
		if !m.HasMedia {
			return File{}, fmt.Errorf("message %s has no attachment", f.ForwardID)
		}
		if m.MediaPath == "" {
			return File{}, fmt.Errorf("the attachment of message %s is not downloaded yet; call download_media first", f.ForwardID)
		}
		data, err := readLimited(m.MediaPath, o.maxFile, 0)
		if err != nil {
			return File{}, err
		}
		f.Data = data
		if f.Name == "" {
			f.Name = m.FileName
		}
		if f.MimeType == "" {
			f.MimeType = m.MimeType
		}
		if m.Kind == "voice" {
			f.Voice = true
		}
	}
	if int64(len(f.Data)) > o.maxFile {
		return File{}, fmt.Errorf("file is %d bytes; the limit is %d", len(f.Data), o.maxFile)
	}
	f.Path, f.ForwardChat, f.ForwardID = "", "", ""
	f.Name = cleanName(f.Name)
	if f.MimeType == "" && f.Name != "" {
		f.MimeType = mime.TypeByExtension(filepath.Ext(f.Name))
	}
	if f.MimeType == "" {
		f.MimeType = http.DetectContentType(f.Data)
	}
	base, _, _ := strings.Cut(f.MimeType, ";")
	if f.Voice && strings.TrimSpace(base) != "audio/ogg" {
		return File{}, fmt.Errorf("voice notes must be audio/ogg with Opus, got %s; send it without voice to deliver it as an audio file", f.MimeType)
	}
	return f, nil
}

func (o *Outbox) readPath(p string) ([]byte, string, error) {
	if o.filesDir == "" {
		return nil, "", errors.New("sending files by path is off; pass the file as data, or start wamcp with --mcp-send-files-from DIR")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(o.filesDir, p)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return nil, "", fmt.Errorf("path: %w", err)
	}
	rel, err := filepath.Rel(o.filesDir, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, "", fmt.Errorf("path %s is outside %s, the only directory files may be sent from", p, o.filesDir)
	}
	// O_NOFOLLOW: a symlink swapped in after the check is refused, not followed.
	data, err := readLimited(real, o.maxFile, syscall.O_NOFOLLOW)
	return data, filepath.Base(real), err
}

func readLimited(path string, max int64, flags int) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|flags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("file is %d bytes; the limit is %d", fi.Size(), max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("file is over the %d byte limit", max)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return data, nil
}

// cleanName keeps a file name WhatsApp shows to the recipient free of paths
// and control characters.
func cleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "/" {
		return ""
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return strings.TrimSpace(name)
}

const captionSep = "\n"

// describe is the audit text of a file send: what was sent, named by hash,
// then the caption.
func describe(f File, caption string) string {
	sum := sha256.Sum256(f.Data)
	kind := "file"
	if f.Voice {
		kind = "voice note"
	}
	d := fmt.Sprintf("[%s %q, %s, %d bytes, sha256 %s]", kind, f.Name, f.MimeType, len(f.Data), hex.EncodeToString(sum[:]))
	if caption != "" {
		d += captionSep + caption
	}
	return d
}

func captionOf(described string) string {
	_, caption, _ := strings.Cut(described, captionSep)
	return caption
}
