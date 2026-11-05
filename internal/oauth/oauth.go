// Package oauth is a small OAuth 2.1 authorization server for the MCP
// endpoint, enough for clients that only connect with OAuth (claude.ai
// connectors and others): protected resource and authorization server
// metadata, dynamic client registration, authorization code with PKCE
// (S256 only) and rotating refresh tokens.
//
// The owner approves a client by entering the main access token on the
// consent page. Issued tokens open /mcp only, like the MCP token.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	CodeTTL    = 10 * time.Minute
	AccessTTL  = time.Hour
	RefreshTTL = 30 * 24 * time.Hour
	// MaxClients caps dynamic registrations, which anyone can call.
	MaxClients = 50
	// unusedClientTTL is how long a registered client that never got a
	// token is kept.
	unusedClientTTL = 24 * time.Hour
)

// Client is a registered OAuth client.
type Client struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
	// ApprovedAt is set once the owner allowed the client.
	ApprovedAt time.Time `json:"approved_at,omitzero"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

type grant struct {
	ClientID  string    `json:"client_id"`
	Kind      string    `json:"kind"` // code, access, refresh
	ExpiresAt time.Time `json:"expires_at"`
	// For codes only.
	RedirectURI   string `json:"redirect_uri,omitempty"`
	CodeChallenge string `json:"code_challenge,omitempty"`
}

type state struct {
	Clients map[string]*Client `json:"clients"`
	// Grants are keyed by the SHA-256 of the secret; secrets are never stored.
	Grants map[string]*grant `json:"grants"`
}

// Server keeps clients and grants in a JSON file next to the archive.
type Server struct {
	path string
	now  func() time.Time

	mu sync.Mutex
	st state
}

// Open loads (or starts) the store at path.
func Open(path string) (*Server, error) {
	s := &Server{path: path, now: time.Now, st: state{Clients: map[string]*Client{}, Grants: map[string]*grant{}}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if s.st.Clients == nil {
			s.st.Clients = map[string]*Client{}
		}
		if s.st.Grants == nil {
			s.st.Grants = map[string]*grant{}
		}
	}
	return s, nil
}

func (s *Server) save() error {
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// prune drops expired grants and stale unapproved clients. Callers hold mu.
func (s *Server) prune() {
	now := s.now()
	for k, g := range s.st.Grants {
		if now.After(g.ExpiresAt) || s.st.Clients[g.ClientID] == nil {
			delete(s.st.Grants, k)
		}
	}
	for id, c := range s.st.Clients {
		if c.ApprovedAt.IsZero() && now.Sub(c.CreatedAt) > unusedClientTTL {
			delete(s.st.Clients, id)
		}
	}
}

// evictUnapproved drops the oldest client the owner never approved, so
// anonymous registrations cannot lock out a real client. Callers hold mu.
func (s *Server) evictUnapproved() bool {
	var oldest *Client
	for _, c := range s.st.Clients {
		if c.ApprovedAt.IsZero() && (oldest == nil || c.CreatedAt.Before(oldest.CreatedAt)) {
			oldest = c
		}
	}
	if oldest == nil {
		return false
	}
	delete(s.st.Clients, oldest.ID)
	return true
}

// Errors returned to clients, named as in RFC 6749 and RFC 7591.
var (
	ErrInvalidRedirect = errors.New("invalid_redirect_uri")
	ErrInvalidClient   = errors.New("invalid_client")
	ErrInvalidGrant    = errors.New("invalid_grant")
	ErrInvalidRequest  = errors.New("invalid_request")
	ErrTooManyClients  = errors.New("too many registered clients; remove some with `wamcp oauth revoke`")
)

// ValidRedirect accepts https URLs and http on loopback (native apps).
func ValidRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

// Register adds a client (RFC 7591).
func (s *Server) Register(name string, redirects []string) (*Client, error) {
	if len(redirects) == 0 {
		return nil, ErrInvalidRedirect
	}
	for _, r := range redirects {
		if !ValidRedirect(r) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidRedirect, r)
		}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "MCP client"
	}
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if len(s.st.Clients) >= MaxClients && !s.evictUnapproved() {
		return nil, ErrTooManyClients
	}
	c := &Client{ID: "wamcp-" + secret()[:22], Name: name, RedirectURIs: redirects, CreatedAt: s.now()}
	s.st.Clients[c.ID] = c
	cp := *c
	return &cp, s.save()
}

// AuthRequest is a validated authorization request.
type AuthRequest struct {
	Client        Client
	RedirectURI   string
	State         string
	CodeChallenge string
}

// CheckAuthorize validates the parameters of /oauth/authorize. Errors before
// the redirect URI is known must not redirect.
func (s *Server) CheckAuthorize(q url.Values) (*AuthRequest, error) {
	s.mu.Lock()
	c := s.st.Clients[q.Get("client_id")]
	var cp Client
	if c != nil {
		cp = *c
	}
	s.mu.Unlock()
	if c == nil {
		return nil, ErrInvalidClient
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(cp.RedirectURIs) == 1 {
		redirect = cp.RedirectURIs[0]
	}
	ok := false
	for _, r := range cp.RedirectURIs {
		ok = ok || r == redirect
	}
	if !ok {
		return nil, ErrInvalidRedirect
	}
	req := &AuthRequest{Client: cp, RedirectURI: redirect, State: q.Get("state"), CodeChallenge: q.Get("code_challenge")}
	switch {
	case q.Get("response_type") != "code":
		return req, fmt.Errorf("%w: response_type must be code", ErrInvalidRequest)
	case q.Get("code_challenge_method") != "S256" || len(req.CodeChallenge) < 43 || len(req.CodeChallenge) > 128:
		return req, fmt.Errorf("%w: PKCE with code_challenge_method S256 is required", ErrInvalidRequest)
	}
	return req, nil
}

// Approve issues an authorization code for an approved request.
func (s *Server) Approve(req *AuthRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.st.Clients[req.Client.ID]
	if c == nil {
		return "", ErrInvalidClient
	}
	if c.ApprovedAt.IsZero() {
		c.ApprovedAt = s.now()
	}
	code := secret()
	s.st.Grants[hash(code)] = &grant{ClientID: c.ID, Kind: "code", ExpiresAt: s.now().Add(CodeTTL),
		RedirectURI: req.RedirectURI, CodeChallenge: req.CodeChallenge}
	return code, s.save()
}

// Tokens is a token endpoint response.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Server) issue(clientID string) Tokens {
	access, refresh := secret(), secret()
	now := s.now()
	s.st.Grants[hash(access)] = &grant{ClientID: clientID, Kind: "access", ExpiresAt: now.Add(AccessTTL)}
	s.st.Grants[hash(refresh)] = &grant{ClientID: clientID, Kind: "refresh", ExpiresAt: now.Add(RefreshTTL)}
	if c := s.st.Clients[clientID]; c != nil {
		c.LastUsedAt = now
	}
	return Tokens{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(AccessTTL.Seconds()), RefreshToken: refresh, Scope: "mcp"}
}

// ExchangeCode redeems an authorization code (single use).
func (s *Server) ExchangeCode(clientID, code, redirectURI, verifier string) (Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := hash(code)
	g := s.st.Grants[k]
	if g == nil || g.Kind != "code" {
		return Tokens{}, ErrInvalidGrant
	}
	delete(s.st.Grants, k)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	switch {
	case s.now().After(g.ExpiresAt), g.ClientID != clientID, redirectURI != "" && redirectURI != g.RedirectURI,
		subtle.ConstantTimeCompare([]byte(challenge), []byte(g.CodeChallenge)) != 1:
		_ = s.save()
		return Tokens{}, ErrInvalidGrant
	}
	t := s.issue(clientID)
	return t, s.save()
}

// Refresh rotates a refresh token.
func (s *Server) Refresh(clientID, refresh string) (Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := hash(refresh)
	g := s.st.Grants[k]
	if g == nil || g.Kind != "refresh" || g.ClientID != clientID || s.now().After(g.ExpiresAt) {
		return Tokens{}, ErrInvalidGrant
	}
	delete(s.st.Grants, k)
	t := s.issue(clientID)
	return t, s.save()
}

// Valid reports whether token is a live access token.
func (s *Server) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.st.Grants[hash(token)]
	return g != nil && g.Kind == "access" && s.now().Before(g.ExpiresAt) && s.st.Clients[g.ClientID] != nil
}

// Clients lists registered clients, oldest first.
func (s *Server) Clients() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Client, 0, len(s.st.Clients))
	for _, c := range s.st.Clients {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Revoke removes a client and every token issued to it.
func (s *Server) Revoke(clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Clients[clientID] == nil {
		return ErrInvalidClient
	}
	delete(s.st.Clients, clientID)
	s.prune()
	return s.save()
}
