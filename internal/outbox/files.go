package outbox

import (
	"bytes"
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

	"golang.org/x/sys/unix"
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

// openDir opens the files directory once. Paths are then opened relative to
// this handle, one component at a time, so moving or replacing anything
// above or inside it cannot redirect a read outside.
func openDir(dir string) (abs, real string, fd *os.File, err error) {
	if abs, err = filepath.Abs(dir); err != nil {
		return "", "", nil, err
	}
	if real, err = filepath.EvalSymlinks(abs); err != nil {
		return "", "", nil, fmt.Errorf("files directory: %w", err)
	}
	if fd, err = os.Open(real); err != nil {
		return "", "", nil, fmt.Errorf("files directory: %w", err)
	}
	if fi, err := fd.Stat(); err != nil || !fi.IsDir() {
		fd.Close()
		return "", "", nil, fmt.Errorf("files directory %s is not a directory", dir)
	}
	return abs, real, fd, nil
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
		mf, err := os.OpenFile(m.MediaPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return File{}, err
		}
		data, err := readAll(mf, o.maxFile)
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
	if f.Voice {
		// Checked on the content: a wrong type from the caller would make
		// WhatsApp show a voice note that does not play.
		if !bytes.HasPrefix(f.Data, []byte("OggS")) {
			return File{}, errors.New("voice notes must be Ogg Opus audio; send it without voice_note to deliver it as an audio file")
		}
		f.MimeType = "audio/ogg; codecs=opus"
	}
	return f, nil
}

func (o *Outbox) readPath(p string) ([]byte, string, error) {
	if o.filesFD == nil {
		return nil, "", errors.New("sending files by path is off; pass the file as data, or start whatsupd with --mcp-send-files-from DIR")
	}
	outside := fmt.Errorf("path %s is outside %s, the only directory files may be sent from", p, o.filesDir)
	rel := filepath.Clean(p)
	if filepath.IsAbs(rel) {
		// Only to find the path relative to the directory; the open below
		// still refuses every symlink. The parent is resolved for aliases
		// such as /var and /private/var on macOS.
		candidates := []string{filepath.Clean(p)}
		if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			candidates = append(candidates, filepath.Join(parent, filepath.Base(p)))
		}
		rel = ""
	find:
		for _, c := range candidates {
			for _, base := range []string{o.filesDir, o.filesAbs} {
				if r, err := filepath.Rel(base, c); err == nil && filepath.IsLocal(r) {
					rel = r
					break find
				}
			}
		}
	}
	if rel == "" || !filepath.IsLocal(rel) {
		return nil, "", outside
	}
	parts := strings.Split(rel, string(filepath.Separator))
	dir := int(o.filesFD.Fd())
	for i, part := range parts {
		// O_NOFOLLOW on every component: no symlink is followed, at any
		// depth. O_NONBLOCK: opening a FIFO must not hang the daemon.
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(dir, part, flags, 0)
		var st unix.Stat_t
		symlink := err != nil && unix.Fstatat(dir, part, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
		if dir != int(o.filesFD.Fd()) {
			unix.Close(dir)
		}
		if symlink {
			return nil, "", fmt.Errorf("path %s goes through a symlink; symlinks are not followed", p)
		}
		if err != nil {
			return nil, "", fmt.Errorf("path %s: %w", p, err)
		}
		dir = fd
	}
	data, err := readAll(os.NewFile(uintptr(dir), p), o.maxFile)
	return data, parts[len(parts)-1], err
}

// readAll reads a regular file of at most max bytes and closes it.
func readAll(f *os.File, max int64) ([]byte, error) {
	path := f.Name()
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
