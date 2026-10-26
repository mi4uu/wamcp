// Package wa runs the WhatsApp linked-device connection: pairing, durable
// event ingestion, catch-up after downtime, background refresh jobs, media
// and the actions offered by the web client.
package wa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/mi4uu/wamcp/internal/archive"
	"github.com/mi4uu/wamcp/internal/ingest"
)

// Connection states reported in DaemonStatus.State.
const (
	StateStarting     = "starting"
	StatePairing      = "pairing"    // waiting for the QR code / pair code
	StateQRExpired    = "qr_expired" // pairing paused until someone asks for a new code
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateDisconnected = "disconnected" // temporary, reconnects automatically
	StateLoggedOut    = "logged_out"
	StateReplaced     = "replaced" // another process uses the same session
	StateBanned       = "temporarily_banned"
)

// Config configures the WhatsApp connection.
type Config struct {
	DataDir       string
	DeviceName    string // shown in the phone's "Linked devices" list
	PairPhone     string // optional: pair with a code instead of scanning the QR
	FullHistory   bool   // ask the phone for the complete history when pairing
	AutoMedia     bool   // download media of new messages automatically
	MediaMaxBytes int64
	PrintQR       bool // render QR codes in the terminal
	WALogLevel    string
}

// Manager owns the whatsmeow client.
type Manager struct {
	cfg       Config
	log       *slog.Logger
	arc       *archive.Store
	ing       *ingest.Ingestor
	container *sqlstore.Container
	bus       *Bus
	jobs      *jobQueue

	ctx context.Context

	mu          sync.RWMutex
	client      *whatsmeow.Client
	status      archive.DaemonStatus
	qr          string
	pairPhone   string
	catchingUp  bool
	sessionOpen bool // catch-up tracking started for the current connection
	pairTrigger chan struct{}
	restart     chan string // "logout" (device unlinked) or "reconnect" (watchdog)
	mediaQueue  chan [2]string
	media       singleflight.Group // one download per message at a time
	workers     sync.WaitGroup

	mediaMu   sync.Mutex
	mediaWait map[string]chan *events.MediaRetry
	mediaFail map[string]time.Time // recent download failures, to avoid hammering the phone
	mediaSem  chan struct{}

	avatarSem chan struct{}
}

// New opens the whatsmeow device store and prepares the manager.
func New(ctx context.Context, cfg Config, arc *archive.Store, log *slog.Logger) (*Manager, error) {
	if cfg.DeviceName == "" {
		cfg.DeviceName = "wamcp"
	}
	store.SetOSInfo(cfg.DeviceName, [3]uint32{0, 1, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	store.DeviceProps.RequireFullSync = proto.Bool(cfg.FullHistory)

	dbPath := filepath.Join(cfg.DataDir, "whatsmeow.db")
	container, err := sqlstore.New(ctx, "sqlite",
		"file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate",
		NewWALogger(log, "whatsmeow/store", cfg.WALogLevel))
	if err != nil {
		return nil, fmt.Errorf("open device store: %w", err)
	}
	_ = os.Chmod(dbPath, 0o600)

	m := &Manager{
		cfg:         cfg,
		log:         log,
		arc:         arc,
		ing:         ingest.New(arc, log),
		container:   container,
		bus:         NewBus(),
		jobs:        newJobQueue(),
		pairPhone:   cfg.PairPhone,
		pairTrigger: make(chan struct{}, 1),
		restart:     make(chan string, 1),
		mediaQueue:  make(chan [2]string, 4096),
		mediaWait:   map[string]chan *events.MediaRetry{},
		mediaFail:   map[string]time.Time{},
		mediaSem:    make(chan struct{}, 4),
		avatarSem:   make(chan struct{}, 3),
		status:      archive.DaemonStatus{State: StateStarting, StartedAt: time.Now()},
	}
	if err := m.ing.LoadMappings(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// Bus returns the live event bus.
func (m *Manager) Bus() *Bus { return m.bus }

// Archive returns the archive the manager writes to.
func (m *Manager) Archive() *archive.Store { return m.arc }

// Run connects and keeps the connection alive until ctx is canceled. After
// the phone unlinks this device it starts a new pairing.
func (m *Manager) Run(ctx context.Context) error {
	m.ctx = ctx
	m.goWorker(ctx, func(ctx context.Context) { m.jobs.run(ctx, m.log) })
	m.goWorker(ctx, m.heartbeat)
	if m.cfg.AutoMedia {
		for range 2 {
			m.goWorker(ctx, m.mediaWorker)
		}
	}
	defer m.container.Close()
	defer m.workers.Wait() // background jobs finish before the store closes
	autoRounds := 3        // QR sessions started without anyone asking
	for ctx.Err() == nil {
		m.drainRestart() // signals from a previous connection are stale
		device, err := m.container.GetFirstDevice(ctx)
		if err != nil {
			return fmt.Errorf("load device: %w", err)
		}
		cli := whatsmeow.NewClient(device, NewWALogger(m.log, "whatsmeow", m.cfg.WALogLevel))
		cli.EmitAppStateEventsOnFullSync = true
		cli.AutomaticMessageRerequestFromPhone = true
		cli.InitialAutoReconnect = true
		// Acknowledge messages only after the archive committed them, and keep
		// decrypted payloads until then: a crash never loses a message, the
		// server simply delivers it again.
		cli.SynchronousAck = true
		cli.EnableDecryptedEventBuffer = true
		cli.BackgroundEventCtx = ctx
		cli.AutoReconnectHook = func(err error) bool {
			m.update(func(s *archive.DaemonStatus) { s.LastError = "reconnecting: " + err.Error() })
			return true // keep retrying; the watchdog rebuilds the client if this never succeeds
		}
		handler := cli.AddEventHandlerWithSuccessStatus(m.handleEvent)
		m.mu.Lock()
		m.client = cli
		m.mu.Unlock()
		m.ing.SetResolver(device.LIDs)
		// Disconnect before removing the handler: a message arriving without a
		// handler would be acknowledged and never redelivered.
		teardown := func() {
			cli.Disconnect()
			cli.RemoveEventHandler(handler)
		}

		if device.ID == nil {
			if err := m.pair(ctx, cli, &autoRounds); err != nil {
				teardown()
				if ctx.Err() != nil {
					break
				}
				m.setError(err)
				sleep(ctx, 5*time.Second)
				continue
			}
		} else {
			m.setState(StateConnecting)
			if err := cli.Connect(); err != nil {
				m.setError(fmt.Errorf("connect: %w", err))
				teardown()
				sleep(ctx, 15*time.Second)
				continue
			}
		}
		select {
		case <-ctx.Done():
		case reason := <-m.restart:
			if reason == "logout" {
				m.log.Warn("this device was unlinked from the phone; waiting for a new pairing")
				autoRounds = 1
			} else {
				m.log.Warn("the connection stayed down; reconnecting")
			}
		}
		teardown()
	}
	m.markOnline()
	m.persistStatus()
	return nil
}

func (m *Manager) goWorker(ctx context.Context, fn func(context.Context)) {
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		fn(ctx)
	}()
}

// signalRestart asks Run to rebuild the connection. A logout replaces a
// pending reconnect; a reconnect never replaces a pending logout.
func (m *Manager) signalRestart(reason string) {
	if reason == "logout" {
		m.drainRestart()
	}
	select {
	case m.restart <- reason:
	default:
	}
}

func (m *Manager) drainRestart() {
	for {
		select {
		case <-m.restart:
		default:
			return
		}
	}
}

// pair runs QR (or pair code) sessions until the phone links this device.
func (m *Manager) pair(ctx context.Context, cli *whatsmeow.Client, autoRounds *int) error {
	for {
		qrCtx, cancel := context.WithCancel(ctx)
		ch, err := cli.GetQRChannel(qrCtx)
		if err != nil {
			cancel()
			return err
		}
		if err := cli.Connect(); err != nil {
			cancel()
			return fmt.Errorf("connect for pairing: %w", err)
		}
		m.setState(StatePairing)
		ok, err := m.consumeQR(ctx, cli, ch)
		cancel()
		m.setQR("")
		if ok {
			m.setState(StateConnecting)
			return nil
		}
		if err != nil {
			m.setError(err)
		}
		cli.Disconnect()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if *autoRounds > 0 {
			*autoRounds--
			continue
		}
		m.setState(StateQRExpired)
		m.log.Info("pairing paused; open the web interface to get a new QR code")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.pairTrigger:
		}
	}
}

func (m *Manager) consumeQR(ctx context.Context, cli *whatsmeow.Client, ch <-chan whatsmeow.QRChannelItem) (bool, error) {
	codeRequested := false
	for item := range ch {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			m.setQR(item.Code)
			if m.cfg.PrintQR {
				fmt.Fprintln(os.Stderr, "\nScan with WhatsApp → Settings → Linked devices → Link a device:")
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stderr)
			}
			m.mu.RLock()
			phone := m.pairPhone
			m.mu.RUnlock()
			if phone != "" && !codeRequested {
				codeRequested = true
				m.requestPairCode(ctx, cli, phone)
			}
		case whatsmeow.QRChannelSuccess.Event:
			m.log.Info("pairing successful")
			return true, nil
		case whatsmeow.QRChannelEventPasskeyResponse:
			code := item.PasskeyConfirmation.Code
			m.setPairCode(code)
			m.log.Info("confirm the pairing on your phone", "code", code)
			if err := cli.SendPasskeyConfirmation(ctx); err != nil {
				return false, fmt.Errorf("passkey confirmation: %w", err)
			}
		case whatsmeow.QRChannelEventPasskeyRequest:
			return false, errors.New("this account asks for passkey pairing, which wamcp does not support; use the QR code or a pair code")
		case whatsmeow.QRChannelEventError:
			return false, item.Error
		case whatsmeow.QRChannelTimeout.Event:
			return false, nil
		default:
			return false, fmt.Errorf("pairing failed: %s", item.Event)
		}
	}
	return false, nil
}

func (m *Manager) requestPairCode(ctx context.Context, cli *whatsmeow.Client, phone string) {
	code, err := cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		m.setError(fmt.Errorf("pair code: %w", err))
		return
	}
	m.setPairCode(code)
	m.log.Info("enter this code on your phone (Linked devices → Link with phone number)", "code", code)
	if m.cfg.PrintQR {
		fmt.Fprintf(os.Stderr, "\nPair code: %s\n", code)
	}
}

// RequestPairing starts a new QR session when pairing was paused.
// It returns false when the device is already paired.
func (m *Manager) RequestPairing() bool {
	st := m.Status()
	switch st.State {
	case StateQRExpired, StateLoggedOut:
		select {
		case m.pairTrigger <- struct{}{}:
		default:
		}
		return true
	case StatePairing, StateStarting:
		return true
	}
	return false
}

// RequestPairCode switches pairing to the phone-number flow. The code shows
// up in the status (PairCode) within a few seconds.
func (m *Manager) RequestPairCode(ctx context.Context, phone string) error {
	digits := onlyDigits(phone)
	if len(digits) < 7 {
		return errors.New("enter the full phone number with country code, e.g. +48 600 000 000")
	}
	m.mu.Lock()
	m.pairPhone = digits
	cli, state := m.client, m.status.State
	m.mu.Unlock()
	if state == StatePairing && cli != nil && cli.IsConnected() && m.QR() != "" {
		m.requestPairCode(ctx, cli, digits)
		return nil
	}
	if !m.RequestPairing() {
		return errors.New("already paired")
	}
	return nil
}

// Logout unlinks this device from the phone. Pairing starts again afterwards.
func (m *Manager) Logout(ctx context.Context) error {
	cli := m.Client()
	if cli == nil || !cli.IsLoggedIn() {
		return errors.New("not logged in")
	}
	if err := cli.Logout(ctx); err != nil {
		return err
	}
	m.setState(StateLoggedOut)
	m.signalRestart("logout")
	return nil
}

// Client returns the current whatsmeow client (nil before Run).
func (m *Manager) Client() *whatsmeow.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client
}

func (m *Manager) connectedClient() (*whatsmeow.Client, error) {
	cli := m.Client()
	if cli == nil || !cli.IsLoggedIn() {
		return nil, errors.New("WhatsApp is not connected (check the status page)")
	}
	return cli, nil
}

// Status returns a snapshot of the live status.
func (m *Manager) Status() archive.DaemonStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.status
	st.UpdatedAt = time.Now()
	return st
}

// QR returns the current QR code payload ("" when not pairing).
func (m *Manager) QR() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.qr
}

// QRPNG renders the current QR code as a PNG image.
func (m *Manager) QRPNG(size int) ([]byte, bool) {
	code := m.QR()
	if code == "" {
		return nil, false
	}
	png, err := qrcode.Encode(code, qrcode.Medium, size)
	return png, err == nil
}

func (m *Manager) update(fn func(*archive.DaemonStatus)) {
	m.mu.Lock()
	fn(&m.status)
	st := m.status
	m.mu.Unlock()
	m.bus.Publish(Event{Type: "status", Data: st})
}

func (m *Manager) setState(state string) {
	m.update(func(s *archive.DaemonStatus) {
		s.State = state
		if state == StateConnected {
			s.LastError, s.PairCode = "", ""
		}
	})
	m.persistStatus()
}

func (m *Manager) setError(err error) {
	m.log.Error("whatsapp", "err", err)
	m.update(func(s *archive.DaemonStatus) { s.LastError = err.Error() })
}

func (m *Manager) setPairCode(code string) {
	m.update(func(s *archive.DaemonStatus) { s.PairCode = code })
}

func (m *Manager) setQR(code string) {
	m.mu.Lock()
	m.qr = code
	m.mu.Unlock()
	m.bus.Publish(Event{Type: "qr", Data: code != ""})
}

func (m *Manager) persistStatus() {
	raw, err := json.Marshal(m.Status())
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.arc.SetMeta(ctx, archive.MetaDaemonStatus, string(raw)); err != nil {
		m.log.Warn("persist status", "err", err)
	}
}

// markOnline records that the daemon was connected right now; after a
// restart this tells how long the gap was.
func (m *Manager) markOnline() {
	if cli := m.Client(); cli == nil || !cli.IsLoggedIn() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.arc.SetMeta(ctx, archive.MetaLastOnline, strconv.FormatInt(time.Now().Unix(), 10))
}

// handleEvent processes one whatsmeow event. Returning false for a message
// leaves it unacknowledged so the server delivers it again.
func (m *Manager) handleEvent(raw any) bool {
	ctx := m.ctx
	m.mu.Lock()
	m.status.LastEventAt = time.Now()
	m.mu.Unlock()
	switch evt := raw.(type) {
	case *events.Message:
		return m.onMessage(ctx, evt)
	case *events.HistorySync:
		m.onHistorySync(ctx, evt)
	case *events.Receipt:
		if chat, err := m.ing.Receipt(ctx, evt); err != nil {
			m.log.Warn("store receipt", "err", err)
		} else {
			m.bus.Publish(Event{Type: "receipt", Chat: chat})
		}
	case *events.Connected:
		m.onConnected(ctx)
	case *events.OfflineSyncPreview:
		m.beginSession(ctx)
		m.log.Info("catching up with messages queued while offline", "messages", evt.Messages, "total", evt.Total)
		m.update(func(s *archive.DaemonStatus) { s.CatchUp.Expected = evt.Messages })
	case *events.OfflineSyncCompleted:
		m.beginSession(ctx)
		m.mu.Lock()
		m.catchingUp = false
		m.mu.Unlock()
		m.update(func(s *archive.DaemonStatus) {
			s.CatchUp.Done, s.CatchUp.CompletedAt = true, time.Now()
		})
		st := m.Status().CatchUp
		m.log.Info("caught up", "offline_for", (time.Duration(st.OfflineSeconds) * time.Second).String(),
			"messages", st.Received, "items", evt.Count)
		m.persistStatus()
		m.jobs.add("reconcile", m.reconcile)
	case *events.PairSuccess:
		m.log.Info("paired", "account", evt.ID.ToNonAD(), "platform", evt.Platform)
		m.setAccount(ctx, evt.ID, evt.LID)
	case *events.Disconnected:
		m.endSession()
		m.setState(StateDisconnected)
	case *events.KeepAliveTimeout:
		m.log.Warn("keepalive timeout", "errors", evt.ErrorCount)
	case *events.LoggedOut:
		m.endSession()
		m.setState(StateLoggedOut)
		m.setError(fmt.Errorf("device unlinked from the phone (%s)", evt.Reason.String()))
		m.signalRestart("logout")
	case *events.StreamReplaced:
		m.endSession()
		m.setState(StateReplaced)
		m.setError(errors.New("another program connected with this session; only one may run at a time"))
	case *events.TemporaryBan:
		m.setState(StateBanned)
		m.setError(errors.New(evt.String()))
	case *events.ConnectFailure:
		m.setError(fmt.Errorf("connect failure: %s %s", evt.Reason, evt.Message))
	case *events.ClientOutdated:
		m.setError(errors.New("WhatsApp rejected this client version; update go.mau.fi/whatsmeow and rebuild"))
	case *events.PushName:
		m.logErr("push name", m.ing.PushName(ctx, evt.JID, evt.JIDAlt, evt.NewPushName))
	case *events.BusinessName:
		m.logErr("business name", m.ing.BusinessName(ctx, evt.JID, evt.NewBusinessName))
	case *events.Contact:
		m.logErr("contact", m.ing.ContactAction(ctx, evt.JID, evt.Action))
	case *events.Archive:
		m.chatFlag(ctx, evt.JID, "archived", boolInt(evt.Action.GetArchived()))
	case *events.Pin:
		m.chatFlag(ctx, evt.JID, "pinned", boolInt(evt.Action.GetPinned()))
	case *events.Mute:
		until := int64(0)
		if evt.Action.GetMuted() {
			until = ingest.MuteUntil(evt.Action.GetMuteEndTimestamp())
		}
		m.chatFlag(ctx, evt.JID, "muted_until", until)
	case *events.MarkChatAsRead:
		m.logErr("mark read", m.ing.ChatRead(ctx, evt.JID, evt.Action.GetRead()))
		m.bus.Publish(Event{Type: "chat", Chat: evt.JID.String()})
	case *events.Star:
		m.logErr("star", m.ing.Star(ctx, evt.ChatJID, evt.MessageID, evt.Action.GetStarred()))
		m.bus.Publish(Event{Type: "message", Chat: evt.ChatJID.String(), ID: evt.MessageID})
	case *events.JoinedGroup:
		m.logErr("joined group", m.ing.Groups(ctx, []*types.GroupInfo{&evt.GroupInfo}, false))
		m.bus.Publish(Event{Type: "chat", Chat: evt.JID.String()})
	case *events.GroupInfo:
		m.jobs.add("groups", m.refreshGroups)
	case *events.AppStateSyncComplete:
		m.jobs.add("contacts", m.refreshContacts)
	case *events.UndecryptableMessage:
		m.log.Debug("undecryptable message; asked the sender/phone to resend", "chat", evt.Info.Chat, "id", evt.Info.ID)
	case *events.MediaRetry:
		m.deliverMediaRetry(evt)
	case *events.ChatPresence:
		m.bus.Publish(Event{Type: "typing", Chat: evt.Chat.String(), Data: map[string]any{
			"sender": evt.Sender.String(), "state": string(evt.State), "media": string(evt.Media)}})
	case *events.Presence:
		m.bus.Publish(Event{Type: "presence", Chat: evt.From.String(), Data: map[string]any{
			"online": !evt.Unavailable, "last_seen": evt.LastSeen.Unix()}})
	case *events.CallOffer:
		m.call(ctx, ingest.CallEvent{Meta: evt.BasicCallMeta, Kind: "offer", Video: hasChild(evt.Data, "video")})
	case *events.CallOfferNotice:
		m.call(ctx, ingest.CallEvent{Meta: evt.BasicCallMeta, Kind: "offer", Video: evt.Media == "video", Group: evt.Type == "group"})
	case *events.CallAccept:
		m.call(ctx, ingest.CallEvent{Meta: evt.BasicCallMeta, Kind: "accept"})
	case *events.CallReject:
		m.call(ctx, ingest.CallEvent{Meta: evt.BasicCallMeta, Kind: "reject"})
	case *events.CallTerminate:
		m.call(ctx, ingest.CallEvent{Meta: evt.BasicCallMeta, Kind: "terminate", Reason: evt.Reason})
	}
	return true
}

func (m *Manager) onMessage(ctx context.Context, evt *events.Message) bool {
	var res *ingest.MessageResult
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if res, err = m.ing.Message(ctx, evt, "live"); err == nil {
			break
		}
		sleep(ctx, time.Duration(attempt+1)*200*time.Millisecond)
	}
	if err != nil {
		m.log.Error("store message; leaving it unacknowledged for redelivery", "chat", evt.Info.Chat, "id", evt.Info.ID, "err", err)
		return false
	}
	m.mu.Lock()
	if m.catchingUp && res.Stored {
		m.status.CatchUp.Received++
	}
	m.mu.Unlock()
	m.bus.Publish(Event{Type: "message", Chat: res.ChatJID, ID: res.ID})
	if res.Stored && res.HasMedia && m.cfg.AutoMedia && res.FileSize <= m.cfg.MediaMaxBytes {
		select {
		case m.mediaQueue <- [2]string{res.ChatJID, res.ID}:
		default: // full; the periodic sweep picks it up later
		}
	}
	return true
}

func (m *Manager) onHistorySync(ctx context.Context, evt *events.HistorySync) {
	cli := m.Client()
	if cli == nil {
		return
	}
	var st ingest.HistoryStats
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if st, err = m.ing.HistorySync(ctx, evt.Data, cli.ParseWebMessage); err == nil {
			break
		}
		sleep(ctx, time.Second)
	}
	if err != nil {
		m.log.Error("store history sync batch", "type", evt.Data.GetSyncType(), "err", err)
		return
	}
	m.log.Info("history sync batch stored", "type", st.SyncType, "chunk", st.Chunk, "progress", st.Progress,
		"conversations", st.Conversations, "messages", st.Messages, "calls", st.Calls, "skipped", st.Skipped)
	m.update(func(s *archive.DaemonStatus) {
		s.History.Batches++
		s.History.Messages += st.Messages
		s.History.LastType = st.SyncType
		s.History.LastProgress = int(st.Progress)
		s.History.LastAt = time.Now()
	})
	m.persistStatus()
	m.bus.Publish(Event{Type: "sync"})
	m.jobs.add("reconcile", m.reconcile)
	m.jobs.add("contacts", m.refreshContacts)
}

func (m *Manager) onConnected(ctx context.Context) {
	cli := m.Client()
	if cli == nil || cli.Store.ID == nil {
		return
	}
	m.beginSession(ctx)
	m.update(func(s *archive.DaemonStatus) {
		s.ConnectedAt = time.Now()
		s.PushName = cli.Store.PushName
	})
	m.setState(StateConnected)
	m.setAccount(ctx, *cli.Store.ID, cli.Store.LID)
	m.log.Info("connected", "account", cli.Store.ID.ToNonAD(),
		"offline_for", (time.Duration(m.Status().CatchUp.OfflineSeconds) * time.Second).String())
	m.markOnline()
	m.jobs.add("groups", m.refreshGroups)
	m.jobs.add("contacts", m.refreshContacts)
	m.jobs.add("reconcile", m.reconcile)
}

// beginSession starts catch-up tracking for a new connection. WhatsApp may
// report the offline queue before or after the Connected event, so whichever
// comes first opens the session; later calls are no-ops.
func (m *Manager) beginSession(ctx context.Context) {
	m.mu.Lock()
	if m.sessionOpen {
		m.mu.Unlock()
		return
	}
	m.sessionOpen, m.catchingUp = true, true
	m.mu.Unlock()
	var offline int64
	if raw, _ := m.arc.Meta(ctx, archive.MetaLastOnline); raw != "" {
		if last, err := strconv.ParseInt(raw, 10, 64); err == nil {
			offline = max(time.Now().Unix()-last, 0)
		}
	}
	m.update(func(s *archive.DaemonStatus) { s.CatchUp = archive.CatchUpStatus{OfflineSeconds: offline} })
}

// endSession marks the connection as gone; the next one tracks a new catch-up.
func (m *Manager) endSession() {
	m.mu.Lock()
	m.sessionOpen, m.catchingUp = false, false
	m.mu.Unlock()
}

func (m *Manager) setAccount(ctx context.Context, pn, lid types.JID) {
	pn = pn.ToNonAD()
	if prev, _ := m.arc.Meta(ctx, archive.MetaAccount); prev != "" && prev != pn.String() {
		m.log.Warn("the archive already holds data of a different WhatsApp account", "archive", prev, "now", pn)
	}
	m.logErr("set account", m.ing.SetSelf(ctx, pn, lid))
	m.update(func(s *archive.DaemonStatus) { s.Account = pn.String() })
}

func (m *Manager) chatFlag(ctx context.Context, jid types.JID, column string, value int64) {
	m.logErr("chat "+column, m.ing.ChatFlag(ctx, jid, column, value))
	m.bus.Publish(Event{Type: "chat", Chat: jid.String()})
}

func (m *Manager) call(ctx context.Context, e ingest.CallEvent) {
	m.logErr("call log", m.ing.Call(ctx, e))
	m.bus.Publish(Event{Type: "call"})
}

func (m *Manager) logErr(what string, err error) {
	if err != nil {
		m.log.Warn(what, "err", err)
	}
}

// ParseWebMessage exposes the client's history message parser.
func (m *Manager) ParseWebMessage(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error) {
	cli := m.Client()
	if cli == nil {
		return nil, errors.New("no client")
	}
	return cli.ParseWebMessage(chat, msg)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func onlyDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// LogoutOffline unlinks the stored device without running the daemon.
func LogoutOffline(ctx context.Context, dataDir string, log *slog.Logger) error {
	dbPath := filepath.Join(dataDir, "whatsmeow.db")
	if _, err := os.Stat(dbPath); err != nil {
		return errors.New("no linked device found")
	}
	container, err := sqlstore.New(ctx, "sqlite",
		"file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate",
		NewWALogger(log, "whatsmeow/store", "warn"))
	if err != nil {
		return err
	}
	defer container.Close()
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return err
	}
	if device.ID == nil {
		return errors.New("no linked device found")
	}
	cli := whatsmeow.NewClient(device, NewWALogger(log, "whatsmeow", "warn"))
	connected := make(chan struct{}, 1)
	cli.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	if err := cli.Connect(); err != nil {
		return err
	}
	defer cli.Disconnect()
	select {
	case <-connected:
	case <-time.After(30 * time.Second):
		return errors.New("timed out connecting to WhatsApp")
	case <-ctx.Done():
		return ctx.Err()
	}
	return cli.Logout(ctx)
}
