package oauth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Handler serves the OAuth endpoints.
type Handler struct {
	S *Server
	// OwnerToken is the main access token; entering it approves a client.
	OwnerToken string
	// Base returns the public origin (scheme and host) of a request.
	Base func(*http.Request) string
	// Access describes what an approved client can do, for the consent page.
	Access string

	failMu   sync.Mutex
	failures []time.Time
}

const (
	maxFailures   = 10
	failureWindow = time.Minute
)

// locked reports whether too many wrong owner tokens were entered lately,
// across all visitors.
func (h *Handler) locked() bool {
	h.failMu.Lock()
	defer h.failMu.Unlock()
	cut := time.Now().Add(-failureWindow)
	kept := h.failures[:0]
	for _, t := range h.failures {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	h.failures = kept
	return len(h.failures) >= maxFailures
}

func (h *Handler) fail() {
	h.failMu.Lock()
	h.failures = append(h.failures, time.Now())
	h.failMu.Unlock()
}

// Register adds the OAuth routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", h.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", h.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.serverMetadata)
	mux.HandleFunc("POST /oauth/register", h.register)
	mux.HandleFunc("GET /oauth/authorize", h.authorize)
	mux.HandleFunc("POST /oauth/authorize", h.authorize)
	mux.HandleFunc("POST /oauth/token", h.token)
}

// ResourceMetadataURL is advertised in WWW-Authenticate on 401 from /mcp.
func (h *Handler) ResourceMetadataURL(r *http.Request) string {
	return h.Base(r) + "/.well-known/oauth-protected-resource"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code int, err error) {
	name, desc, _ := strings.Cut(err.Error(), ": ")
	switch {
	case errors.Is(err, ErrTooManyClients):
		name, desc = "invalid_client_metadata", err.Error()
	case !strings.HasPrefix(name, "invalid_") && name != "unsupported_grant_type":
		name, desc = "server_error", err.Error()
	}
	writeJSON(w, code, map[string]string{"error": name, "error_description": desc})
}

func (h *Handler) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := h.Base(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"mcp"},
		"resource_name":            "whatsupd",
	})
}

func (h *Handler) serverMetadata(w http.ResponseWriter, r *http.Request) {
	base := h.Base(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"mcp"},
	})
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		oauthError(w, http.StatusBadRequest, errors.New("invalid_client_metadata: invalid JSON"))
		return
	}
	c, err := h.S.Register(in.ClientName, in.RedirectURIs)
	if err != nil {
		oauthError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect to whatsupd</title>
<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--fg:#15171a;--muted:#5b6270;--card:#fff;--line:#d9dde3;--accent:#1f7a4d;--err:#b3261e}
@media (prefers-color-scheme:dark){:root{--bg:#111315;--fg:#e8eaed;--muted:#9aa1ab;--card:#1b1e21;--line:#2c3136;--accent:#3fb37a;--err:#f2b8b5}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:24px;max-width:420px;width:100%;box-sizing:border-box}
h1{font-size:20px;margin:0 0 12px}p{margin:0 0 12px;color:var(--muted)}b{color:var(--fg)}code{font-size:14px}
input{width:100%;box-sizing:border-box;padding:10px;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--fg);font:inherit;margin:4px 0 16px}
.row{display:flex;gap:8px}button{flex:1;padding:10px;border-radius:8px;border:1px solid var(--line);font:inherit;cursor:pointer;background:var(--bg);color:var(--fg)}
button.allow{background:var(--accent);border-color:var(--accent);color:#fff}.err{color:var(--err)}
</style></head><body><main>
<h1>Allow {{.Client}} to use your WhatsApp archive?</h1>
<p>It will connect to <b>{{.Redirect}}</b> and can {{.Access}}.</p>
<p>Enter your whatsupd access token to confirm. Get it on the machine running whatsupd with <code>whatsupd token</code>.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/oauth/authorize">
{{range $k, $v := .Params}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<label for="t">Access token</label>
<input id="t" name="owner_token" type="password" autocomplete="off" autofocus>
<div class="row"><button name="decision" value="deny">Deny</button><button class="allow" name="decision" value="allow">Allow</button></div>
</form></main></body></html>`))

var authorizeParams = []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method", "scope", "resource"}

func redirectWith(w http.ResponseWriter, r *http.Request, to string, params url.Values) {
	u, err := url.Parse(to)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			if v != "" {
				q.Set(k, v)
			}
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	q := url.Values{}
	for _, k := range authorizeParams {
		q.Set(k, r.Form.Get(k))
	}
	req, err := h.S.CheckAuthorize(q)
	if req == nil {
		// Unknown client or redirect URI: never redirect to it.
		http.Error(w, "unknown client or redirect_uri ("+err.Error()+"); register the client again", http.StatusBadRequest)
		return
	}
	iss := h.Base(r)
	if err == nil && !h.ownResource(r, q.Get("resource")) {
		err = errors.New("invalid_target: resource must be " + iss + "/mcp")
	}
	if err != nil {
		name, desc, _ := strings.Cut(err.Error(), ": ")
		redirectWith(w, r, req.RedirectURI, url.Values{"error": {name}, "error_description": {desc}, "state": {req.State}, "iss": {iss}})
		return
	}
	page := map[string]any{"Client": req.Client.Name, "Redirect": hostOf(req.RedirectURI), "Access": h.Access, "Params": paramsMap(q)}
	if r.Method == http.MethodPost {
		if r.Form.Get("decision") == "deny" {
			redirectWith(w, r, req.RedirectURI, url.Values{"error": {"access_denied"}, "state": {req.State}, "iss": {iss}})
			return
		}
		if h.locked() {
			page["Error"] = "Too many wrong tokens in the last minute. Wait a minute and try again."
			w.WriteHeader(http.StatusTooManyRequests)
			h.renderConsent(w, page)
			return
		}
		given := strings.TrimSpace(r.Form.Get("owner_token"))
		if subtle.ConstantTimeCompare([]byte(given), []byte(h.OwnerToken)) != 1 {
			h.fail()
			page["Error"] = "That is not the access token. Copy it from `whatsupd token`."
			w.WriteHeader(http.StatusUnauthorized)
			h.renderConsent(w, page)
			return
		}
		code, err := h.S.Approve(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		redirectWith(w, r, req.RedirectURI, url.Values{"code": {code}, "state": {req.State}, "iss": {iss}})
		return
	}
	h.renderConsent(w, page)
}

// ownResource accepts an absent resource indicator (RFC 8707) or this
// server's MCP endpoint, the only resource its tokens open.
func (h *Handler) ownResource(r *http.Request, resource string) bool {
	return resource == "" || strings.TrimSuffix(resource, "/") == h.Base(r)+"/mcp"
}

func (h *Handler) renderConsent(w http.ResponseWriter, page map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// No form-action: browsers apply it to the redirect back to the client.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	_ = consentPage.Execute(w, page)
}

func paramsMap(q url.Values) map[string]string {
	m := map[string]string{}
	for k := range q {
		if v := q.Get(k); v != "" {
			m[k] = v
		}
	}
	return m
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, errors.New("invalid_request: form body expected"))
		return
	}
	var (
		t   Tokens
		err error
	)
	clientID := r.PostForm.Get("client_id")
	if !h.ownResource(r, r.PostForm.Get("resource")) {
		oauthError(w, http.StatusBadRequest, errors.New("invalid_target: resource must be "+h.Base(r)+"/mcp"))
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		t, err = h.S.ExchangeCode(clientID, r.PostForm.Get("code"), r.PostForm.Get("redirect_uri"), r.PostForm.Get("code_verifier"))
	case "refresh_token":
		t, err = h.S.Refresh(clientID, r.PostForm.Get("refresh_token"))
	default:
		oauthError(w, http.StatusBadRequest, errors.New("unsupported_grant_type: use authorization_code or refresh_token"))
		return
	}
	if err != nil {
		oauthError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
