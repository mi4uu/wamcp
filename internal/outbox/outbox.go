// Package outbox is the only path from an MCP agent to WhatsApp. It exists
// only when the daemon runs with --mcp-allow-send and enforces the recipient
// allowlist, the rate limit and retry de-duplication, and writes every attempt
// to the agent_sends audit table before anything is sent.
package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mi4uu/wamcp/internal/archive"
)

// Refusal errors. Callers can match them with errors.Is.
var (
	ErrNotAllowed     = errors.New("recipient is not on the send allowlist")
	ErrRateLimited    = errors.New("send rate limit reached")
	ErrUnknownOutcome = errors.New("an earlier attempt of this message did not finish")
	ErrKeyReused      = errors.New("idempotency_key was already used for a different message")
)

// Config is the send policy.
type Config struct {
	// Allow lists recipients as JIDs, phone numbers or "me". Empty allows any.
	Allow []string
	// PerHour caps sends in any rolling hour. Zero or less means the default.
	PerHour int
	// RetryWindow is how long an identical text to the same chat counts as a
	// retry when the caller gives no idempotency key.
	RetryWindow time.Duration
}

const (
	DefaultPerHour     = 20
	DefaultRetryWindow = 10 * time.Minute
	keyLookback        = 24 * time.Hour
	MaxTextLen         = 4096
)

// SendFunc delivers a text message with a caller-chosen message ID and
// returns the ID WhatsApp accepted.
type SendFunc func(ctx context.Context, chat, text, replyTo, messageID string) (string, error)

// Request is one send asked for by an agent. Chat must already be a JID.
type Request struct {
	Chat           string
	Text           string
	ReplyTo        string
	IdempotencyKey string
	Client         string
}

// Result describes a sent message.
type Result struct {
	Chat      string
	MessageID string
	SentAt    time.Time
	// Duplicate is set when the request repeated an earlier one and nothing
	// new was sent.
	Duplicate bool
}

// Outbox applies the policy. It is safe for concurrent use; sends are
// serialized so a retry racing the original sees its outcome.
type Outbox struct {
	arc   *archive.Store
	send  SendFunc
	newID func() string
	log   *slog.Logger
	now   func() time.Time

	allowAll  bool
	allow     map[string]bool
	allowSelf bool
	perHour   int
	window    time.Duration

	mu sync.Mutex
}

// New validates cfg and builds an Outbox.
func New(arc *archive.Store, cfg Config, send SendFunc, newID func() string, log *slog.Logger) (*Outbox, error) {
	o := &Outbox{arc: arc, send: send, newID: newID, log: log, now: time.Now,
		allow: map[string]bool{}, perHour: cfg.PerHour, window: cfg.RetryWindow}
	if o.perHour <= 0 {
		o.perHour = DefaultPerHour
	}
	if o.window <= 0 {
		o.window = DefaultRetryWindow
	}
	for _, raw := range cfg.Allow {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if strings.EqualFold(raw, "me") || strings.EqualFold(raw, "self") {
			o.allowSelf = true
			continue
		}
		jid, err := ParseRecipient(raw)
		if err != nil {
			return nil, err
		}
		o.allow[jid] = true
	}
	o.allowAll = len(o.allow) == 0 && !o.allowSelf
	return o, nil
}

// AllowsAnyone reports whether no allowlist is configured.
func (o *Outbox) AllowsAnyone() bool { return o.allowAll }

// PerHour returns the effective rate limit.
func (o *Outbox) PerHour() int { return o.perHour }

// Policy summarizes the policy for logs and tool descriptions.
func (o *Outbox) Policy() string {
	who := "any chat"
	if !o.allowAll {
		var list []string
		if o.allowSelf {
			list = append(list, "me")
		}
		for j := range o.allow {
			list = append(list, j)
		}
		who = strings.Join(sortedCopy(list), ", ")
	}
	return fmt.Sprintf("recipients: %s; at most %d messages per hour", who, o.perHour)
}

// ParseRecipient turns a JID or phone number into a JID. Names are not
// accepted: an allowlist must not depend on how contacts are named.
func ParseRecipient(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if user, server, ok := strings.Cut(raw, "@"); ok {
		if user == "" || server == "" {
			return "", fmt.Errorf("invalid recipient %q", raw)
		}
		if server == "c.us" {
			server = "s.whatsapp.net"
		}
		return user + "@" + server, nil
	}
	var digits strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsDigit(r):
			digits.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", fmt.Errorf("invalid recipient %q: use a phone number with country code, a JID, or me", raw)
		}
	}
	if digits.Len() < 7 {
		return "", fmt.Errorf("invalid recipient %q: phone number too short", raw)
	}
	return digits.String() + "@s.whatsapp.net", nil
}

func (o *Outbox) allowed(dir *archive.Directory, chat string) bool {
	if o.allowAll {
		return true
	}
	c := dir.Canonical(chat)
	if o.allowSelf && dir.IsSelf(c) {
		return true
	}
	return o.allow[c] || o.allow[chat]
}

func dedupeKey(req Request) string {
	if req.IdempotencyKey != "" {
		return "key:" + req.IdempotencyKey
	}
	sum := sha256.Sum256([]byte(req.Chat + "\x00" + req.ReplyTo + "\x00" + req.Text))
	return "text:" + hex.EncodeToString(sum[:])
}

// SendText applies the policy and sends req.Text to req.Chat at most once.
func (o *Outbox) SendText(ctx context.Context, req Request) (Result, error) {
	req.Text = strings.TrimSpace(req.Text)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.Chat == "" {
		return Result{}, errors.New("chat is required")
	}
	if req.Text == "" {
		return Result{}, errors.New("text is required")
	}
	if n := len([]rune(req.Text)); n > MaxTextLen {
		return Result{}, fmt.Errorf("text is %d characters; the limit is %d", n, MaxTextLen)
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	dir, err := o.arc.Directory(ctx)
	if err != nil {
		return Result{}, err
	}
	req.Chat = dir.Canonical(req.Chat)
	now := o.now()
	key := dedupeKey(req)
	lookback := o.window
	if req.IdempotencyKey != "" {
		lookback = keyLookback
	}

	retryID := ""
	prev, err := o.arc.LastAgentSend(ctx, key, now.Add(-lookback))
	switch {
	case errors.Is(err, archive.ErrNotFound):
	case err != nil:
		return Result{}, err
	case prev.ChatJID != req.Chat || prev.Text != req.Text || prev.ReplyTo != req.ReplyTo:
		return Result{}, ErrKeyReused
	case prev.Status == archive.SendSent:
		o.log.Info("mcp send: duplicate request, not sent again", "chat", req.Chat, "id", prev.MessageID, "client", req.Client)
		return Result{Chat: req.Chat, MessageID: prev.MessageID, SentAt: prev.UpdatedAt, Duplicate: true}, nil
	case prev.Status == archive.SendPending:
		// The process stopped between writing the audit record and learning
		// the outcome. The message is archived if WhatsApp accepted it.
		if _, err := o.arc.Message(ctx, prev.ChatJID, prev.MessageID); err == nil {
			_ = o.arc.SetAgentSendStatus(ctx, prev.PK, archive.SendSent, prev.MessageID, "", now)
			return Result{Chat: req.Chat, MessageID: prev.MessageID, SentAt: prev.CreatedAt, Duplicate: true}, nil
		}
		return Result{}, fmt.Errorf("%w (message id %s, started %s): check the chat with get_messages; if it is not there, send again with a new idempotency_key",
			ErrUnknownOutcome, prev.MessageID, prev.CreatedAt.Format(time.RFC3339))
	case prev.Status == archive.SendFailed:
		// Reuse the ID so a failure that did reach WhatsApp is not delivered twice.
		retryID = prev.MessageID
	}

	audit := archive.AgentSend{CreatedAt: now, DedupeKey: key, ChatJID: req.Chat, Text: req.Text, ReplyTo: req.ReplyTo, Client: req.Client}
	refuse := func(cause error) (Result, error) {
		audit.Status, audit.Error = archive.SendRefused, cause.Error()
		if _, err := o.arc.AddAgentSend(ctx, audit); err != nil {
			return Result{}, errors.Join(cause, err)
		}
		o.log.Warn("mcp send refused", "chat", req.Chat, "reason", cause, "client", req.Client)
		return Result{}, cause
	}
	if !o.allowed(dir, req.Chat) {
		return refuse(fmt.Errorf("%w: %s (%s)", ErrNotAllowed, dir.Name(req.Chat), req.Chat))
	}
	n, err := o.arc.CountAgentSends(ctx, now.Add(-time.Hour))
	if err != nil {
		return Result{}, err
	}
	if n >= o.perHour {
		return refuse(fmt.Errorf("%w: %d messages in the last hour, limit %d", ErrRateLimited, n, o.perHour))
	}

	audit.Status, audit.MessageID = archive.SendPending, retryID
	if audit.MessageID == "" {
		audit.MessageID = o.newID()
	}
	if audit.MessageID == "" {
		return Result{}, errors.New("WhatsApp is not connected; nothing was sent")
	}
	pk, err := o.arc.AddAgentSend(ctx, audit)
	if err != nil {
		return Result{}, fmt.Errorf("audit log: %w; nothing was sent", err)
	}
	id, sendErr := o.send(ctx, req.Chat, req.Text, req.ReplyTo, audit.MessageID)
	done := o.now()
	if sendErr != nil {
		_ = o.arc.SetAgentSendStatus(context.WithoutCancel(ctx), pk, archive.SendFailed, audit.MessageID, sendErr.Error(), done)
		o.log.Warn("mcp send failed", "chat", req.Chat, "id", audit.MessageID, "err", sendErr, "client", req.Client)
		return Result{}, sendErr
	}
	if err := o.arc.SetAgentSendStatus(context.WithoutCancel(ctx), pk, archive.SendSent, id, "", done); err != nil {
		o.log.Error("mcp send: audit update failed", "pk", pk, "err", err)
	}
	o.log.Info("mcp send", "chat", req.Chat, "id", id, "chars", len([]rune(req.Text)), "client", req.Client)
	return Result{Chat: req.Chat, MessageID: id, SentAt: done}, nil
}

func sortedCopy(list []string) []string {
	out := append([]string(nil), list...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
