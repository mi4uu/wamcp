// Package web serves the MCP endpoint and the browser client: pairing,
// chats, messages, media, sending, contact notes and live updates.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/wa"
)

//go:embed static
var staticFS embed.FS

// Daemon is the part of wa.Manager the web client uses.
type Daemon interface {
	Status() archive.DaemonStatus
	QRPNG(size int) ([]byte, bool)
	RequestPairing() bool
	RequestPairCode(ctx context.Context, phone string) error
	Logout(ctx context.Context) error
	Bus() *wa.Bus
	DownloadMedia(ctx context.Context, chat, id string) (*wa.MediaFile, error)
	Avatar(ctx context.Context, jid string) (string, error)
	RequestOlderMessages(ctx context.Context, chat string, count int) error
	SendText(ctx context.Context, chat, text, replyTo string) (*ingestResult, error)
	SendMedia(ctx context.Context, chat string, up wa.MediaUpload) (*ingestResult, error)
	React(ctx context.Context, chat, id, emoji string) error
	Edit(ctx context.Context, chat, id, text string) error
	Delete(ctx context.Context, chat, id string) error
	SetStarred(ctx context.Context, chat, id string, starred bool) error
	MarkRead(ctx context.Context, chat string) error
	SetArchived(ctx context.Context, chat string, archived bool) error
	SetPinned(ctx context.Context, chat string, pinned bool) error
	SetMuted(ctx context.Context, chat string, d time.Duration) error
	SetTyping(ctx context.Context, chat string, typing bool) error
	SetAvailable(ctx context.Context, available bool) error
	SubscribePresence(ctx context.Context, jid string) error
}

// Options configures the HTTP handler.
type Options struct {
	Token        string       // bearer token for /mcp and the web login
	MCP          http.Handler // streamable HTTP MCP handler
	Archive      *archive.Store
	Daemon       Daemon
	Log          *slog.Logger
	AllowAnyHost bool // set when listening on a non-loopback address
	// TrustLocal signs in every browser connecting from this machine, so the
	// web client needs no token. /mcp always requires the bearer token.
	TrustLocal bool
}

const cookieName = "wamcp_session"

type server struct {
	opt     Options
	session string // cookie value derived from the token

	codesMu sync.Mutex
	codes   map[string]time.Time

	presenceMu sync.Mutex
	tabs       map[string]bool // web client tab -> visible
	streams    map[string]int  // open live streams per tab
	goneTimers map[string]*time.Timer
	wantOnline bool

	applyMu   sync.Mutex
	available *bool // presence last sent to WhatsApp (guarded by applyMu)
}

// tabConnected cancels a pending "tab gone" after a reconnect.
func (s *server) tabConnected(tab string) {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	s.streams[tab]++
	if t := s.goneTimers[tab]; t != nil {
		t.Stop()
		delete(s.goneTimers, tab)
	}
}

// tabDisconnected forgets a tab only if its live stream stays closed for a
// while: network blips and reconnects must not flip the account offline.
func (s *server) tabDisconnected(tab string) {
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	s.streams[tab]--
	if s.streams[tab] > 0 {
		return
	}
	delete(s.streams, tab)
	s.goneTimers[tab] = time.AfterFunc(20*time.Second, func() {
		s.presenceMu.Lock()
		gone := s.streams[tab] == 0
		if gone {
			delete(s.goneTimers, tab)
		}
		s.presenceMu.Unlock()
		if gone {
			s.setTabPresence(tab, false, true)
		}
	})
}

// setTabPresence records whether a tab is visible (or gone) and keeps the
// account online while at least one tab is visible. Presence is one flag for
// the whole account, so it is derived from all tabs, not the last request.
func (s *server) setTabPresence(tab string, visible, gone bool) {
	s.presenceMu.Lock()
	if gone {
		delete(s.tabs, tab)
	} else {
		s.tabs[tab] = visible
	}
	want := false
	for _, v := range s.tabs {
		want = want || v
	}
	s.wantOnline = want
	s.presenceMu.Unlock()
	go s.applyPresence()
}

// applyPresence sends the latest wanted presence to WhatsApp; calls are
// serialized so an older value never overwrites a newer one.
func (s *server) applyPresence() {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.presenceMu.Lock()
	want := s.wantOnline
	s.presenceMu.Unlock()
	if s.available != nil && *s.available == want {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.opt.Daemon.SetAvailable(ctx, want); err != nil {
		if s.opt.Log != nil {
			s.opt.Log.Debug("set presence", "err", err)
		}
		return
	}
	s.available = &want
}

// Server is the HTTP handler plus access to one-time sign-in links.
type Server struct {
	http.Handler
	s *server
}

// NewLoginCode returns a one-time code for GET /login?code=…, valid 5 minutes.
func (x *Server) NewLoginCode() string { return x.s.newLoginCode() }

// Handler builds the HTTP handler.
func Handler(opt Options) *Server {
	mac := hmac.New(sha256.New, []byte(opt.Token))
	mac.Write([]byte("wamcp-web-session"))
	s := &server{opt: opt, session: hex.EncodeToString(mac.Sum(nil)), codes: map[string]time.Time{}, tabs: map[string]bool{},
		streams: map[string]int{}, goneTimers: map[string]*time.Timer{}}

	mux := http.NewServeMux()
	if opt.MCP != nil {
		mux.Handle("/mcp", s.bearer(opt.MCP))
	}
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(files)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// Browsers of the local user are signed in when they open the page;
		// API calls always need the session cookie.
		if s.opt.TrustLocal && !s.signedIn(r) && sameUserPeer(r) {
			s.setSession(w)
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, static, "index.html")
	})

	// Public: needed before signing in (pairing and login).
	mux.HandleFunc("GET /api/session", s.apiSession)
	mux.HandleFunc("GET /qr.png", s.qrPNG)
	mux.HandleFunc("POST /api/pairing/restart", s.apiPairRestart)
	mux.HandleFunc("POST /api/pairing/phone", s.apiPairPhone)
	mux.HandleFunc("POST /api/login", s.apiLogin)
	mux.HandleFunc("GET /login", s.loginWithCode)
	mux.Handle("POST /api/login-code", s.bearer(http.HandlerFunc(s.apiLoginCode)))
	// Live operations for the stdio MCP server (bearer token).
	mux.Handle("GET /api/live/status", s.bearer(http.HandlerFunc(s.apiLiveStatus)))
	mux.Handle("POST /api/live/media", s.bearer(http.HandlerFunc(s.apiLiveMedia)))
	mux.Handle("POST /api/live/older", s.bearer(http.HandlerFunc(s.apiLiveOlder)))

	// Signed in.
	auth := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireSession(h)) }
	auth("POST /api/logout", s.apiLogout)
	auth("GET /api/status", s.apiStatus)
	auth("GET /api/events", s.apiEvents)
	auth("GET /api/chats", s.apiChats)
	auth("GET /api/chats/{jid}", s.apiChatInfo)
	auth("GET /api/chats/{jid}/messages", s.apiMessages)
	auth("GET /api/search", s.apiSearch)
	auth("GET /api/starred", s.apiStarred)
	auth("GET /api/calls", s.apiCalls)
	auth("GET /api/contacts", s.apiContacts)
	auth("GET /api/media/{chat}/{id}", s.apiMedia)
	auth("GET /api/avatar/{jid}", s.apiAvatar)
	auth("GET /api/meta/{jid}", s.apiGetMeta)
	auth("PUT /api/meta/{jid}", s.apiPutMeta)
	auth("POST /api/chats/{jid}/messages", s.apiSend)
	auth("POST /api/chats/{jid}/media", s.apiSendMedia)
	auth("POST /api/chats/{jid}/messages/{id}/react", s.apiReact)
	auth("POST /api/chats/{jid}/messages/{id}/edit", s.apiEdit)
	auth("POST /api/chats/{jid}/messages/{id}/delete", s.apiDelete)
	auth("POST /api/chats/{jid}/messages/{id}/star", s.apiStar)
	auth("POST /api/chats/{jid}/read", s.apiRead)
	auth("POST /api/chats/{jid}/archive", s.apiArchive)
	auth("POST /api/chats/{jid}/pin", s.apiPin)
	auth("POST /api/chats/{jid}/mute", s.apiMute)
	auth("POST /api/chats/{jid}/typing", s.apiTyping)
	auth("POST /api/chats/{jid}/older", s.apiOlder)
	auth("POST /api/chats/{jid}/presence", s.apiSubscribePresence)
	auth("POST /api/presence", s.apiPresence)
	auth("POST /api/device/logout", s.apiDeviceLogout)

	cop := http.NewCrossOriginProtection()
	return &Server{Handler: s.localOnly(cop.Handler(mux)), s: s}
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// localOnly rejects requests whose Host is not a loopback name, which blocks
// DNS-rebinding attacks from web pages.
func (s *server) localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.opt.AllowAnyHost {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			host = strings.Trim(host, "[]")
			if host != "localhost" && host != "127.0.0.1" && host != "::1" {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (s *server) bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !equal(strings.TrimSpace(got), s.opt.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wamcp"`)
			http.Error(w, "missing or invalid bearer token (see `wamcp config`)", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) signedIn(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && equal(c.Value, s.session)
}

func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// lsofPath finds lsof once; launchd starts services with a minimal PATH.
var lsofPath = sync.OnceValue(func() string {
	for _, p := range []string{"/usr/sbin/lsof", "/usr/bin/lsof"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath("lsof")
	return p
})

// sameUserPeer reports whether the client end of a loopback TCP connection
// belongs to a process of the OS user running wamcp. Loopback alone is not
// proof: other users on the machine can connect too. Checked with lsof, only
// when a browser without a session opens the page.
func sameUserPeer(r *http.Request) bool {
	if !fromLoopback(r) {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	_, port, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	if lsofPath() == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, lsofPath(), "-nP", "-iTCP:"+port, "-sTCP:ESTABLISHED", "-F", "pun").Output()
	want := r.RemoteAddr + "->" + local.String()
	uid := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			uid = ""
		case strings.HasPrefix(line, "u"):
			uid = line[1:]
		case strings.HasPrefix(line, "n") && line[1:] == want:
			return uid == strconv.Itoa(os.Getuid())
		}
	}
	return false
}

func (s *server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.signedIn(r) {
			writeError(w, http.StatusUnauthorized, errors.New("sign in first (run `wamcp open`)"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) setSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.session, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 90 * 24 * 3600})
}

func (s *server) newLoginCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	code := hex.EncodeToString(b)
	s.codesMu.Lock()
	defer s.codesMu.Unlock()
	for c, exp := range s.codes {
		if time.Now().After(exp) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = time.Now().Add(5 * time.Minute)
	return code
}

func (s *server) useLoginCode(code string) bool {
	s.codesMu.Lock()
	defer s.codesMu.Unlock()
	exp, ok := s.codes[code]
	delete(s.codes, code)
	return ok && time.Now().Before(exp)
}

func (s *server) apiLiveStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.opt.Daemon.Status())
}

func (s *server) apiLiveMedia(w http.ResponseWriter, r *http.Request) {
	var in struct{ Chat, ID string }
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f, err := s.opt.Daemon.DownloadMedia(r.Context(), in.Chat, in.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]any{"path": f.Path, "mime": f.MimeType, "size": f.Size})
}

func (s *server) apiLiveOlder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Chat  string
		Count int
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.opt.Daemon.RequestOlderMessages(r.Context(), in.Chat, in.Count); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) apiLoginCode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"code": s.newLoginCode()})
}

func (s *server) loginWithCode(w http.ResponseWriter, r *http.Request) {
	if !s.useLoginCode(r.URL.Query().Get("code")) {
		http.Redirect(w, r, "/?login=expired", http.StatusSeeOther)
		return
	}
	s.setSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) apiLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !equal(strings.TrimSpace(in.Token), s.opt.Token) {
		writeError(w, http.StatusUnauthorized, errors.New("wrong token"))
		return
	}
	s.setSession(w)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) apiLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

// apiSession tells the client what to show: pairing, login or the app.
func (s *server) apiSession(w http.ResponseWriter, r *http.Request) {
	st := s.opt.Daemon.Status()
	_, hasQR := s.opt.Daemon.QRPNG(64)
	out := map[string]any{
		"authenticated": s.signedIn(r),
		"state":         st.State,
		"paired":        st.Account != "" && st.State != wa.StateLoggedOut && st.State != wa.StatePairing && st.State != wa.StateQRExpired,
		"has_qr":        hasQR,
	}
	// Pairing details are public only while pairing; afterwards only for signed-in users.
	if s.signedIn(r) || out["paired"] == false {
		out["pair_code"] = st.PairCode
		out["last_error"] = st.LastError
	}
	if s.signedIn(r) {
		out["account"] = st.Account
		out["push_name"] = st.PushName
	}
	writeJSON(w, out)
}

func (s *server) qrPNG(w http.ResponseWriter, _ *http.Request) {
	png, ok := s.opt.Daemon.QRPNG(320)
	if !ok {
		http.Error(w, "no QR code right now", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *server) apiPairRestart(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]bool{"ok": s.opt.Daemon.RequestPairing()})
}

func (s *server) apiPairPhone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.opt.Daemon.RequestPairCode(r.Context(), in.Phone); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	if tab := r.URL.Query().Get("tab"); tab != "" {
		s.tabConnected(tab)
		defer s.tabDisconnected(tab)
	}
	ch, unsubscribe := s.opt.Daemon.Bus().Subscribe()
	defer unsubscribe()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	_, _ = w.Write([]byte("retry: 3000\n\n"))
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(raw)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
