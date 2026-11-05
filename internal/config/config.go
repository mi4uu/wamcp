// Package config resolves the data directory, the access token and the
// single-instance lock.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DefaultListen is where the daemon serves MCP and the web client.
const DefaultListen = "127.0.0.1:8765"

// DefaultDataDir returns $WAMCP_DATA_DIR or ~/.wamcp.
func DefaultDataDir() string {
	if v := os.Getenv("WAMCP_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".wamcp"
	}
	return filepath.Join(home, ".wamcp")
}

// DefaultListenAddr returns $WAMCP_LISTEN or DefaultListen.
func DefaultListenAddr() string {
	if v := os.Getenv("WAMCP_LISTEN"); v != "" {
		return v
	}
	return DefaultListen
}

// Paths lists the files inside the data directory.
type Paths struct{ Dir string }

func (p Paths) Archive() string  { return filepath.Join(p.Dir, "archive.db") }
func (p Paths) Token() string    { return filepath.Join(p.Dir, "token") }
func (p Paths) MCPToken() string { return filepath.Join(p.Dir, "mcp-token") }
func (p Paths) LockFile() string { return filepath.Join(p.Dir, "wamcp.lock") }
func (p Paths) Logs() string     { return filepath.Join(p.Dir, "logs") }

// Ensure creates the data directory with owner-only permissions.
func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(p.Dir, 0o700)
}

// LoadOrCreateToken returns the access token, generating it on first use.
func (p Paths) LoadOrCreateToken() (string, error) { return loadOrCreate(p.Token()) }

// ReadToken returns the stored access token.
func (p Paths) ReadToken() (string, error) { return readToken(p.Token()) }

// LoadOrCreateMCPToken returns the token that only grants MCP access,
// generating it on first use.
func (p Paths) LoadOrCreateMCPToken() (string, error) { return loadOrCreate(p.MCPToken()) }

// ReadMCPToken returns the stored MCP-only token.
func (p Paths) ReadMCPToken() (string, error) { return readToken(p.MCPToken()) }

// RotateMCPToken replaces the MCP-only token with a new one.
func (p Paths) RotateMCPToken() (string, error) { return writeNewToken(p.MCPToken()) }

func loadOrCreate(path string) (string, error) {
	if tok, err := readToken(path); err == nil {
		return tok, nil
	}
	return writeNewToken(path)
}

func writeNewToken(path string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return tok, nil
}

func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errors.New("empty token file")
	}
	return tok, nil
}

// ErrLocked means another wamcp daemon already uses the data directory.
var ErrLocked = errors.New("another wamcp daemon is already running with this data directory")

// Lock takes an exclusive lock on the data directory. WhatsApp allows only one
// connection per session; a second process would kick the first one out.
func (p Paths) Lock() (func(), error) {
	f, err := os.OpenFile(p.LockFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", p.LockFile(), err)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
