// Command wamcp is a WhatsApp linked-device client that archives everything
// locally, serves a web client, and exposes the archive to AI assistants as
// an MCP server.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/config"
	"github.com/mi4uu/wamcp/internal/mcpserver"
	"github.com/mi4uu/wamcp/internal/outbox"
	"github.com/mi4uu/wamcp/internal/service"
	"github.com/mi4uu/wamcp/internal/wa"
	"github.com/mi4uu/wamcp/internal/web"
)

var version = "dev"

const usage = `wamcp — WhatsApp archive, web client and MCP server

Usage:
  wamcp serve     Run the daemon: link/sync WhatsApp, web client and MCP over HTTP
  wamcp mcp       Serve MCP over stdio (reads the archive; for Claude Desktop & co.)
  wamcp open      Open the web client in the browser (signs you in)
  wamcp config    Print MCP client configuration (Claude Code, Claude Desktop)
  wamcp token     Print the access token
  wamcp status    Show daemon, sync and archive status
  wamcp audit     Show messages MCP agents sent or tried to send
  wamcp service   install | uninstall | restart | status — run the daemon in the background (macOS)
  wamcp logout    Unlink this device from WhatsApp (daemon must be stopped)
  wamcp version

Common flags: --data-dir (default ~/.wamcp, env WAMCP_DATA_DIR),
              --listen (default 127.0.0.1:8765, env WAMCP_LISTEN)
Run "wamcp <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "mcp":
		err = cmdMCP(args)
	case "open":
		err = cmdOpen(args)
	case "config":
		err = cmdConfig(args)
	case "token":
		err = cmdToken(args)
	case "status":
		err = cmdStatus(args)
	case "audit":
		err = cmdAudit(args)
	case "service":
		err = cmdService(args)
	case "logout":
		err = cmdLogout(args)
	case "version", "--version", "-v":
		fmt.Println("wamcp", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type common struct {
	dataDir string
	listen  string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dataDir, "data-dir", config.DefaultDataDir(), "directory for the archive, session and media")
	fs.StringVar(&c.listen, "listen", config.DefaultListenAddr(), "address for the web client and MCP endpoint")
}

func (c *common) paths() config.Paths { return config.Paths{Dir: c.dataDir} }

func (c *common) baseURL() string {
	host, port, err := net.SplitHostPort(c.listen)
	if err != nil {
		return "http://" + c.listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// liveAdapter exposes the manager to the MCP server.
type liveAdapter struct{ m *wa.Manager }

func (l liveAdapter) Status() archive.DaemonStatus { return l.m.Status() }
func (l liveAdapter) DownloadMedia(ctx context.Context, chat, id string) (string, string, int64, error) {
	f, err := l.m.DownloadMedia(ctx, chat, id)
	if err != nil {
		return "", "", 0, err
	}
	return f.Path, f.MimeType, f.Size, nil
}
func (l liveAdapter) RequestOlderMessages(ctx context.Context, chat string, count int) error {
	return l.m.RequestOlderMessages(ctx, chat, count)
}

// envBool reads a boolean environment variable (1, true, yes, on).
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sendFlags are the opt-in switches for MCP sending.
type sendFlags struct {
	allow   bool
	to      string
	perHour int
}

func (f *sendFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.allow, "mcp-allow-send", envBool("WAMCP_MCP_ALLOW_SEND"),
		"let MCP agents send WhatsApp text messages (send_message tool); off by default, env WAMCP_MCP_ALLOW_SEND")
	fs.StringVar(&f.to, "mcp-send-to", os.Getenv("WAMCP_MCP_SEND_TO"),
		"comma-separated recipients agents may send to: phone numbers with country code, JIDs, or me; empty allows any chat (loopback only), env WAMCP_MCP_SEND_TO")
	fs.IntVar(&f.perHour, "mcp-send-per-hour", outbox.DefaultPerHour, "maximum messages agents may send in any rolling hour")
}

func (f *sendFlags) args() []string {
	if !f.allow {
		return nil
	}
	out := []string{"--mcp-allow-send", "--mcp-send-per-hour", fmt.Sprint(f.perHour)}
	if f.to != "" {
		out = append(out, "--mcp-send-to", f.to)
	}
	return out
}

// outbox builds the send policy, or returns nil when sending is off.
func (f *sendFlags) outbox(exposed bool, arc *archive.Store, mgr *wa.Manager, log *slog.Logger) (*outbox.Outbox, error) {
	if !f.allow {
		return nil, nil
	}
	allow := strings.Split(f.to, ",")
	ob, err := outbox.New(arc, outbox.Config{Allow: allow, PerHour: f.perHour},
		func(ctx context.Context, chat, text, replyTo, id string) (string, error) {
			res, err := mgr.SendTextWithID(ctx, chat, text, replyTo, id)
			if err != nil {
				return "", err
			}
			return res.ID, nil
		}, mgr.NewMessageID, log)
	if err != nil {
		return nil, fmt.Errorf("--mcp-send-to: %w", err)
	}
	if ob.AllowsAnyone() && exposed {
		return nil, errors.New("--mcp-allow-send with a non-loopback --listen or a --public-host needs a recipient allowlist (--mcp-send-to)")
	}
	return ob, nil
}

// remoteLive lets the stdio MCP server use the running daemon for live
// operations (status, media download, older history, sending).
type remoteLive struct {
	base, token string
	arc         *archive.Store
	http        *http.Client
}

func (r *remoteLive) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("the wamcp daemon is not reachable at %s (install it with `wamcp service install`): %w", r.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *remoteLive) Status() archive.DaemonStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var st archive.DaemonStatus
	if err := r.call(ctx, http.MethodGet, "/api/live/status", nil, &st); err == nil {
		return st
	} else if saved, _ := r.arc.DaemonStatus(ctx); saved != nil {
		st = *saved
		st.State += " (daemon not reachable now)"
		st.LastError = err.Error()
		return st
	} else {
		return archive.DaemonStatus{State: "not running", LastError: err.Error()}
	}
}

func (r *remoteLive) DownloadMedia(ctx context.Context, chat, id string) (string, string, int64, error) {
	var out struct {
		Path, Mime string
		Size       int64
	}
	err := r.call(ctx, http.MethodPost, "/api/live/media", map[string]string{"chat": chat, "id": id}, &out)
	return out.Path, out.Mime, out.Size, err
}

func (r *remoteLive) RequestOlderMessages(ctx context.Context, chat string, count int) error {
	return r.call(ctx, http.MethodPost, "/api/live/older", map[string]any{"chat": chat, "count": count}, nil)
}

// remoteSender forwards send_message to the daemon, which enforces the policy.
type remoteSender struct {
	*remoteLive
	policy string
}

func (r *remoteSender) SendText(ctx context.Context, req outbox.Request) (outbox.Result, error) {
	var out outbox.Result
	err := r.call(ctx, http.MethodPost, "/api/live/send", req, &out)
	return out, err
}

func (r *remoteSender) Policy() string { return r.policy + " (enforced by the wamcp daemon)" }

// openLog returns the log destination: stderr, or a file rotated to
// <path>.1 whenever it grows past 20 MB.
func openLog(path string) (io.Writer, error) {
	if path == "" {
		return os.Stderr, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &rotatingLog{path: path, max: 20 << 20}
	return l, l.open()
}

type rotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil { // rotation failed earlier: keep logging to stderr
		return os.Stderr.Write(p)
	}
	if l.size+int64(len(p)) > l.max {
		l.f.Close()
		_ = os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			l.f = nil
			fmt.Fprintf(os.Stderr, "wamcp: cannot reopen log %s (%v); logging to stderr\n", l.path, err)
			return os.Stderr.Write(p)
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// crashLoop records a startup failure of the background service and reports
// whether to give up (5 failures within 10 minutes), so launchd stops
// restarting a daemon that cannot start instead of looping forever.
func crashLoop(paths config.Paths, cause error) bool {
	file := filepath.Join(paths.Dir, "crashloop")
	count, first := 0, time.Now()
	if raw, err := os.ReadFile(file); err == nil {
		var c int
		var f int64
		if _, err := fmt.Sscanf(string(raw), "%d %d", &c, &f); err == nil && time.Since(time.Unix(f, 0)) < 10*time.Minute {
			count, first = c, time.Unix(f, 0)
		}
	}
	count++
	_ = os.WriteFile(file, []byte(fmt.Sprintf("%d %d\n%v\n", count, first.Unix(), cause)), 0o600)
	return count >= 5
}

func cmdServe(args []string) (retErr error) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.register(fs)
	var send sendFlags
	send.register(fs)
	pairPhone := fs.String("pair-phone", "", "link with a pair code for this phone number instead of scanning the QR code")
	deviceName := fs.String("device-name", "wamcp", "name shown in WhatsApp → Linked devices")
	noFullHistory := fs.Bool("no-full-history", false, "do not ask the phone for the complete history when linking")
	autoMedia := fs.Bool("auto-media", false, "download media of new messages automatically")
	mediaMaxMB := fs.Int64("media-max-mb", 25, "size limit for --auto-media downloads")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	waLogLevel := fs.String("wa-log-level", "warn", "log level of the WhatsApp library")
	logFile := fs.String("log-file", "", "write logs to this file instead of stderr")
	deleted := fs.String("deleted", "keep", "messages deleted for everyone: keep (content stays, marked as deleted) or remove (wiped like in WhatsApp)")
	publicHost := fs.String("public-host", os.Getenv("WAMCP_PUBLIC_HOST"),
		"comma-separated host names of a reverse proxy or tunnel in front of wamcp (for example wa.example.com); turns on token sign-in for the web client, env WAMCP_PUBLIC_HOST")
	webAuth := fs.String("web-auth", "local", "web client sign-in: local (browsers on this machine are signed in automatically) or token")
	asService := fs.Bool("service", false, "running under launchd (stop restarting after repeated startup failures)")
	_ = fs.Parse(args)
	if *deleted != "keep" && *deleted != "remove" {
		return fmt.Errorf("--deleted must be keep or remove, got %q", *deleted)
	}
	if *webAuth != "local" && *webAuth != "token" {
		return fmt.Errorf("--web-auth must be local or token, got %q", *webAuth)
	}

	paths := c.paths()
	if err := paths.Ensure(); err != nil {
		return err
	}
	if *asService {
		started := time.Now()
		defer func() {
			if retErr != nil && time.Since(started) < time.Minute && crashLoop(paths, retErr) {
				fmt.Fprintf(os.Stderr, "wamcp: giving up after repeated startup failures: %v\nFix the cause, then run `wamcp service restart`.\n", retErr)
				retErr = nil // exit 0: launchd does not restart a successful exit
			}
		}()
		time.AfterFunc(time.Minute, func() { _ = os.Remove(filepath.Join(paths.Dir, "crashloop")) })
	}
	unlock, err := paths.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	token, err := paths.LoadOrCreateToken()
	if err != nil {
		return err
	}
	out, err := openLog(*logFile)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: wa.ParseLevel(*logLevel)}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	arc, err := archive.Open(ctx, paths.Archive())
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer arc.Close()
	arc.SetKeepDeleted(*deleted == "keep")
	interactive := *logFile == "" && term.IsTerminal(int(os.Stderr.Fd()))
	mgr, err := wa.New(ctx, wa.Config{
		DataDir: c.dataDir, DeviceName: *deviceName, PairPhone: *pairPhone, FullHistory: !*noFullHistory,
		AutoMedia: *autoMedia, MediaMaxBytes: *mediaMaxMB << 20, PrintQR: interactive, WALogLevel: *waLogLevel,
	}, arc, log)
	if err != nil {
		return err
	}

	var publicHosts []string
	for _, h := range strings.Split(*publicHost, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			publicHosts = append(publicHosts, h)
		}
	}
	exposed := !isLoopback(c.listen) || len(publicHosts) > 0
	ob, err := send.outbox(exposed, arc, mgr, log)
	if err != nil {
		return err
	}
	var mcpOpts []mcpserver.Option
	webOpts := web.Options{Token: token, Archive: arc, Daemon: mgr, Log: log,
		AllowAnyHost: !isLoopback(c.listen), PublicHosts: publicHosts, TrustLocal: *webAuth == "local" && !exposed}
	if ob != nil {
		mcpOpts = append(mcpOpts, mcpserver.WithSender(ob))
		webOpts.AgentSend = ob
		log.Warn("MCP agents can send WhatsApp messages (--mcp-allow-send)", "policy", ob.Policy())
	}
	mcpSrv := mcpserver.New(arc, liveAdapter{mgr}, version, mcpOpts...)
	webOpts.MCP = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	webSrv := web.Handler(webOpts)
	httpSrv := &http.Server{Addr: c.listen, Handler: webSrv, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", c.listen, err)
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()
	if exposed {
		log.Warn("reachable beyond this machine; anyone who can reach it and has the token can read your WhatsApp", "listen", c.listen, "public_host", publicHosts)
	}
	log.Info("wamcp started", "version", version, "web", c.baseURL(), "mcp", c.baseURL()+"/mcp", "data", c.dataDir)
	if interactive {
		link := c.baseURL()
		if *webAuth == "token" || exposed {
			link += "/login?code=" + webSrv.NewLoginCode()
		}
		fmt.Fprintf(os.Stderr, "\n  Web client:   %s\n  MCP endpoint: %s/mcp  (see `wamcp config`)\n\n", link, c.baseURL())
	}

	runErr := mgr.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Info("wamcp stopped")
	return runErr
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	var c common
	c.register(fs)
	allowSend := fs.Bool("allow-send", envBool("WAMCP_MCP_ALLOW_SEND"),
		"offer send_message; the daemon must also run with --mcp-allow-send, and it enforces the policy (env WAMCP_MCP_ALLOW_SEND)")
	_ = fs.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	arc, err := archive.OpenExisting(ctx, c.paths().Archive())
	if err != nil {
		return err
	}
	defer arc.Close()
	var live mcpserver.Live
	var opts []mcpserver.Option
	if tok, err := c.paths().ReadToken(); err == nil {
		rl := &remoteLive{base: c.baseURL(), token: tok, arc: arc, http: &http.Client{Timeout: 90 * time.Second}}
		live = rl
		if *allowSend {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			var p struct{ Policy string }
			err := rl.call(pctx, http.MethodGet, "/api/live/send", nil, &p)
			cancel()
			if err != nil {
				log.Warn("send_message is not offered", "reason", err)
			} else {
				opts = append(opts, mcpserver.WithSender(&remoteSender{remoteLive: rl, policy: p.Policy}))
			}
		}
	} else if *allowSend {
		log.Warn("send_message is not offered: no access token yet; start the daemon first")
	}
	srv := mcpserver.New(arc, live, version, opts...)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Error("mcp stdio", "err", err)
		return err
	}
	return nil
}

func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	paths := c.paths()
	if err := paths.Ensure(); err != nil {
		return err
	}
	tok, err := paths.LoadOrCreateToken()
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	paths := c.paths()
	if err := paths.Ensure(); err != nil {
		return err
	}
	tok, err := paths.LoadOrCreateToken()
	if err != nil {
		return err
	}
	exe, _ := os.Executable()
	desktop, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
		"whatsapp": map[string]any{"command": exe, "args": []string{"mcp", "--data-dir", c.dataDir}},
	}}, "", "  ")
	fmt.Printf(`MCP endpoint (daemon must be running): %[1]s/mcp
Access token: %[2]s

Claude Code (HTTP, all tools incl. media download):
  claude mcp add --transport http --scope user whatsapp %[1]s/mcp --header "Authorization: Bearer %[2]s"

Claude Desktop / other stdio clients (reads the archive directly), add to claude_desktop_config.json:
%[3]s

Any MCP client over HTTP: URL %[1]s/mcp, header "Authorization: Bearer <token>".
`, c.baseURL(), tok, desktop)
	return nil
}

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	tok, err := c.paths().ReadToken()
	if err != nil {
		return errors.New("no token yet; start the daemon first with `wamcp serve`")
	}
	req, _ := http.NewRequest(http.MethodPost, c.baseURL()+"/api/login-code", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("the daemon is not reachable at %s (start it with `wamcp serve` or `wamcp service install`): %w", c.baseURL(), err)
	}
	defer resp.Body.Close()
	var out struct {
		Code string `json:"code"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return fmt.Errorf("sign-in failed: HTTP %d", resp.StatusCode)
	}
	url := c.baseURL() + "/login?code=" + out.Code
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, url).Start(); err != nil {
		fmt.Println("Open this link in your browser (valid 5 minutes):", url)
		return nil
	}
	fmt.Println("Opened", c.baseURL())
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	fmt.Println("Background service:", service.Status())
	ctx := context.Background()
	arc, err := archive.OpenExisting(ctx, c.paths().Archive())
	if err != nil {
		return err
	}
	defer arc.Close()
	st, err := arc.DaemonStatus(ctx)
	if err != nil {
		return err
	}
	if st != nil {
		age := time.Since(st.UpdatedAt).Round(time.Second)
		fmt.Printf("Daemon: %s (reported %s ago)\n", st.State, age)
		if age > 3*time.Minute {
			fmt.Println("  the daemon does not seem to be running")
		}
		if st.Account != "" {
			fmt.Println("Account:", archive.Phone(st.Account), st.PushName)
		}
		if st.History.Batches > 0 {
			fmt.Printf("History sync: %d batches, %d messages, last %s %d%%\n", st.History.Batches, st.History.Messages,
				st.History.LastType, st.History.LastProgress)
		}
		if cu := st.CatchUp; cu.OfflineSeconds > 0 {
			fmt.Printf("Last catch-up: offline %s, %d messages recovered, done=%v\n",
				time.Duration(cu.OfflineSeconds)*time.Second, cu.Received, cu.Done)
		}
		if st.LastError != "" {
			fmt.Println("Last error:", st.LastError)
		}
	}
	stats, err := arc.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Archive: %d messages, %d chats, %d groups, %d contacts, %d calls, %.1f MB\n",
		stats.Messages, stats.Chats, stats.Groups, stats.Contacts, stats.Calls, float64(stats.DBBytes)/(1<<20))
	return nil
}

func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	var c common
	c.register(fs)
	limit := fs.Int("limit", 50, "number of records, newest first")
	_ = fs.Parse(args)
	ctx := context.Background()
	arc, err := archive.OpenExisting(ctx, c.paths().Archive())
	if err != nil {
		return err
	}
	defer arc.Close()
	recs, err := arc.AgentSends(ctx, max(*limit, 1))
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Println("No messages were sent by MCP agents.")
		return nil
	}
	for _, a := range recs {
		fmt.Printf("%s  %-8s  %s  id=%s", a.CreatedAt.Format("2006-01-02 15:04:05"), a.Status, a.ChatJID, a.MessageID)
		if a.Client != "" {
			fmt.Printf("  client=%q", a.Client)
		}
		if a.Error != "" {
			fmt.Printf("  error=%q", a.Error)
		}
		fmt.Printf("\n    %s\n", strings.ReplaceAll(a.Text, "\n", "\n    "))
	}
	return nil
}

func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: wamcp service install|uninstall|restart|status [serve flags]")
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ExitOnError)
		var c common
		c.register(fs)
		autoMedia := fs.Bool("auto-media", false, "download media of new messages automatically")
		var send sendFlags
		send.register(fs)
		publicHost := fs.String("public-host", os.Getenv("WAMCP_PUBLIC_HOST"), "host names of a reverse proxy or tunnel in front of wamcp")
		_ = fs.Parse(args[1:])
		if _, err := send.outbox(!isLoopback(c.listen) || strings.TrimSpace(*publicHost) != "", nil, nil, nil); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		if strings.Contains(exe, "go-build") {
			return errors.New("install a built binary first (make install), not `go run`")
		}
		paths := c.paths()
		if err := paths.Ensure(); err != nil {
			return err
		}
		service.Stop()
		var unlock func()
		for range 15 { // the old daemon needs a few seconds to shut down cleanly
			if unlock, err = paths.Lock(); !errors.Is(err, config.ErrLocked) {
				break
			}
			time.Sleep(time.Second)
		}
		if errors.Is(err, config.ErrLocked) {
			pid, _ := os.ReadFile(paths.LockFile())
			return fmt.Errorf("another wamcp daemon is running (pid %s); stop it first", strings.TrimSpace(string(pid)))
		}
		if err != nil {
			return err
		}
		unlock()
		logFile := filepath.Join(paths.Logs(), "wamcp.log")
		serveArgs := []string{exe, "serve", "--service", "--data-dir", c.dataDir, "--listen", c.listen, "--log-file", logFile}
		if *autoMedia {
			serveArgs = append(serveArgs, "--auto-media")
		}
		serveArgs = append(serveArgs, send.args()...)
		if *publicHost != "" {
			serveArgs = append(serveArgs, "--public-host", *publicHost)
		}
		path, err := service.Install(serveArgs, filepath.Join(paths.Logs(), "launchd.log"))
		if err != nil {
			return err
		}
		fmt.Printf("Installed %s\nThe daemon now runs in the background and starts at login.\nLogs: %s\nNext: wamcp open\n", path, logFile)
		return nil
	case "uninstall":
		if err := service.Uninstall(); err != nil {
			return err
		}
		fmt.Println("Removed the background service. The archive in the data directory is kept.")
		return nil
	case "restart":
		if err := service.Restart(); err != nil {
			return err
		}
		fmt.Println("Restarted the background service.")
		return nil
	case "status":
		fmt.Println(service.Status())
		return nil
	}
	return fmt.Errorf("unknown service command %q", args[0])
}

func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	paths := c.paths()
	unlock, err := paths.Lock()
	if errors.Is(err, config.ErrLocked) {
		return errors.New("the daemon is running; unlink from the web client (Settings → Unlink device) or stop it first")
	}
	if err != nil {
		return err
	}
	defer unlock()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := wa.LogoutOffline(ctx, c.dataDir, log); err != nil {
		return err
	}
	fmt.Println("Unlinked. The archive is kept; run `wamcp serve` to link again.")
	return nil
}
