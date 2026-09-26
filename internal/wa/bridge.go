// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/history"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/media"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

// Exit codes (PROTOCOL.md §9). 2 is never used by the bridge: it is the Go
// runtime's code for an unrecovered panic or a fatal runtime error (review M1).
const (
	ExitOK             = 0
	ExitCrash          = 1 // a panic recovered on the main goroutine
	ExitStoreKey       = 3
	ExitStoreLocked    = 4
	ExitClientOutdated = 5
	ExitNoInit         = 6
	ExitLoggedOut      = 7 // after logout or a remote logout: the store is deleted
)

// Config wires the bridge to its outside world. Tests replace OpenStore,
// NewClient and RefreshVersion with fakes.
type Config struct {
	Out            *protocol.Writer
	Log            *logx.Logger
	Now            func() time.Time
	OpenStore      func(ctx context.Context, dir string, key []byte) (*store.Store, error)
	NewClient      func(st *store.Store) (Client, error)
	RefreshVersion func(ctx context.Context) error
	// ConfigureDevice applies the device name and history window (global whatsmeow state).
	ConfigureDevice func(deviceName string, historyDays int)

	QRFirst      time.Duration // lifetime of the first QR code (60 s)
	QRNext       time.Duration // lifetime of each later code (20 s)
	FirstQRWait  time.Duration // how long to wait for the pre-login socket (30 s)
	HistoryIdle  time.Duration // no new history for this long after some arrived → done (60 s)
	SendTimeout  time.Duration // whatsmeow send timeout (50 s, under the client's 60 s)
	FetchTimeout time.Duration
}

func (c *Config) defaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = logx.Discard()
	}
	if c.ConfigureDevice == nil {
		c.ConfigureDevice = ConfigureDevice
	}
	if c.QRFirst == 0 {
		c.QRFirst = 60 * time.Second
	}
	if c.QRNext == 0 {
		c.QRNext = 20 * time.Second
	}
	if c.FirstQRWait == 0 {
		c.FirstQRWait = 30 * time.Second
	}
	if c.HistoryIdle == 0 {
		c.HistoryIdle = 60 * time.Second
	}
	if c.SendTimeout == 0 {
		c.SendTimeout = 50 * time.Second
	}
	if c.FetchTimeout == 0 {
		c.FetchTimeout = 110 * time.Second
	}
}

// session is the runtime of one linked account (or of an unpaired store).
// It is replaced after a logout.
type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	cli    Client
	st     *store.Store

	hmu   sync.Mutex
	hq    []*waE2E.HistorySyncNotification
	hsig  chan struct{}
	hseen bool
	hdone bool

	offlineDone atomic.Bool
	groupsOnce  sync.Once
}

// Bridge is the protocol engine. Handle is called for every inbound command;
// whatsmeow events arrive through the handler it registers on each client.
type Bridge struct {
	cfg   Config
	fatal chan int

	mu        sync.Mutex
	inited    bool
	init      protocol.Init
	sess      *session
	state     string
	pair      *pairing
	stopped   bool
	outdated  int         // ClientOutdated events seen
	loggedOut *logoutInfo // set once the account is unlinked; Stop wipes the store
	sends     []time.Time // reservation times of recent sends, oldest first (send backstop)
	pins      []time.Time // times of recent set_pin writes (their own backstop, rev 2)
	lastPic   time.Time   // when the last fetch_avatar asked WhatsApp (≤ 1 per second, rev 2)

	sending  atomic.Bool
	picMu    sync.Mutex // one fetch_avatar at a time
	fetchSem chan struct{}
	window   *history.Window
	caps     *history.Caps
	lim      media.Limits

	// Reported-data bookkeeping (guarded by mu).
	chats        map[string]bool // reported chat JID → @lid waiting for its alias
	contacts     map[string]bool
	groupsSent   map[string]bool
	groupCache   map[string]*types.GroupInfo
	groupsLoaded bool
	senders      *lru // "chat|id" → sender JID, for quoted replies
	rawSenders   *lru // "chat|id" → the sender JID as WhatsApp addressed it (@lid in some groups)

	wg sync.WaitGroup
}

// New returns a bridge.
func New(cfg Config) *Bridge {
	cfg.defaults()
	return &Bridge{
		cfg:        cfg,
		fatal:      make(chan int, 1),
		fetchSem:   make(chan struct{}, 2),
		window:     history.NewWindow(history.WindowSize),
		chats:      map[string]bool{},
		contacts:   map[string]bool{},
		groupsSent: map[string]bool{},
		groupCache: map[string]*types.GroupInfo{},
		senders:    newLRU(10000),
		rawSenders: newLRU(10000),
	}
}

// Fatal delivers an exit code: after a fatal error event has been written, or
// after a logout (ExitLoggedOut). The caller then calls Stop and exits.
func (b *Bridge) Fatal() <-chan int { return b.fatal }

// Initialized reports whether init succeeded.
func (b *Bridge) Initialized() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inited
}

func (b *Bridge) emit(typ string, payload any) {
	err := b.cfg.Out.Emit(typ, payload)
	switch {
	case errors.Is(err, protocol.ErrQuiesced):
		// Stop has begun: only its own final lines are written (review R-M1).
		b.cfg.Log.Warn("emit_dropped", logx.Code(typ), logx.N(b.cfg.Out.Dropped()))
	case err != nil:
		b.cfg.Log.Error("emit_failed", logx.Code(typ))
	}
}

// emitFinal writes one of Stop's own last lines, which pass the quiesced writer.
func (b *Bridge) emitFinal(typ string, payload any) {
	if err := b.cfg.Out.EmitFinal(typ, payload); err != nil {
		b.cfg.Log.Error("emit_failed", logx.Code(typ))
	}
}

func (b *Bridge) fail(replyTo, code string) {
	b.emit(protocol.EvError, protocol.NewError(replyTo, code))
}

func (b *Bridge) failFatal(replyTo, code string, exit int) {
	e := protocol.NewError(replyTo, code)
	e.Fatal = true
	b.emit(protocol.EvError, e)
	b.cfg.Log.Error("fatal", logx.Code(code))
	b.exit(exit)
}

// exit asks the caller to Stop the bridge and exit with code (the first request wins).
func (b *Bridge) exit(code int) {
	select {
	case b.fatal <- code:
	default:
	}
}

// HostBlocked reports a connection refused by the host allowlist (§10.2).
func (b *Bridge) HostBlocked() {
	b.fail("", protocol.ErrHostBlocked)
	b.cfg.Log.Warn("host_blocked")
}

// Reject answers a line that is not processed (for example a wrong version).
func (b *Bridge) Reject(replyTo, code string) { b.fail(replyTo, code) }

func (b *Bridge) setState(s string) { b.changeState(s, b.emit) }

func (b *Bridge) changeState(s string, emit func(string, any)) {
	b.mu.Lock()
	changed := b.state != s
	b.state = s
	b.mu.Unlock()
	if changed {
		emit(protocol.EvStatus, protocol.Status{State: s})
		b.cfg.Log.Info("status", logx.Code(s))
	}
}

func (b *Bridge) current() *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sess
}

// Handle executes one decoded command envelope. It returns true when the
// command was a valid shutdown: the caller then calls Stop and exits 0.
func (b *Bridge) Handle(env protocol.Envelope) (shutdown bool) {
	id := env.ID
	if !protocol.IsCommand(env.Type) {
		b.fail(id, protocol.ErrUnknownCommand)
		return false
	}
	switch env.Type {
	case protocol.CmdPing, protocol.CmdShutdown, protocol.CmdInit:
	default:
		if !b.Initialized() {
			b.fail(id, protocol.ErrNotInitialized)
			return false
		}
	}
	v, err := protocol.DecodeCommand(env.Type, env.Payload)
	if err != nil {
		if errors.Is(err, protocol.PhoneInvalid) {
			b.fail(id, protocol.ErrPhoneInvalid)
		} else {
			b.fail(id, protocol.ErrBadRequest)
		}
		b.cfg.Log.Warn("command_refused", logx.Code(env.Type))
		return false
	}
	switch c := v.(type) {
	case *protocol.Init:
		b.doInit(id, c)
	case *protocol.Ack:
		b.window.Ack(c.Seq)
	case *protocol.PairPhone:
		b.startPairing(id, c.Phone)
	case *protocol.SendText:
		b.asyncCmd(id, func() { b.sendText(id, c) })
	case *protocol.SendMedia:
		b.asyncCmd(id, func() { b.sendMedia(id, c) })
	case *protocol.MarkRead:
		b.asyncCmd(id, func() { b.markRead(id, c) })
	case *protocol.FetchMedia:
		b.asyncCmd(id, func() { b.fetchMedia(id, c) })
	case *protocol.SetPin:
		b.asyncCmd(id, func() { b.setPin(id, c) })
	case *protocol.FetchAvatar:
		b.asyncCmd(id, func() { b.fetchAvatar(id, c) })
	case *protocol.Empty:
		switch env.Type {
		case protocol.CmdPing:
			b.emit(protocol.EvPong, protocol.Reply{ReplyTo: id})
		case protocol.CmdShutdown:
			return true
		case protocol.CmdPairQR:
			b.startPairing(id, "")
		case protocol.CmdLogout:
			b.asyncCmd(id, func() { b.logout(id) })
		}
	}
	return false
}

// async runs f on a goroutine that Stop waits for. After Stop has begun it
// runs nothing. A panic in f is recovered and reported on stderr with a
// constant code only: the panic value, which could hold content, is dropped.
func (b *Bridge) async(f func()) { b.goSafe("async", nil, f) }

// asyncCmd is async for a command: a panic also gets the command its final
// reply, error{internal}.
func (b *Bridge) asyncCmd(id string, f func()) {
	b.goSafe("command", func() { b.fail(id, protocol.ErrInternal) }, f)
}

func (b *Bridge) goSafe(where string, onPanic func(), f func()) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1) // under mu, so never concurrent with Stop's Wait
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				b.cfg.Log.Error("panic_recovered", logx.Code(where))
				if onPanic != nil {
					onPanic()
				}
			}
		}()
		f()
	}()
}

func (b *Bridge) doInit(id string, c *protocol.Init) {
	b.mu.Lock()
	if b.inited {
		b.mu.Unlock()
		b.fail(id, protocol.ErrAlreadyInitialized)
		return
	}
	b.mu.Unlock()
	key, _ := hex.DecodeString(c.StoreKey) // validated: 64 lowercase hex
	defer clear(key)
	if err := os.MkdirAll(c.MediaDir, 0o700); err != nil {
		b.fail(id, protocol.ErrStoreIO)
		return
	}
	// An existing folder is tightened too (review L5).
	if err := store.RestrictDir(c.MediaDir); err != nil {
		b.fail(id, protocol.ErrStoreIO)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	st, err := b.cfg.OpenStore(ctx, c.StoreDir, key)
	switch {
	case errors.Is(err, store.ErrKeyInvalid):
		b.failFatal(id, protocol.ErrStoreKeyInvalid, ExitStoreKey)
		return
	case errors.Is(err, store.ErrLocked):
		b.failFatal(id, protocol.ErrStoreLocked, ExitStoreLocked)
		return
	case err != nil:
		b.cfg.Log.Error("store_open_failed")
		b.fail(id, protocol.ErrStoreIO)
		return
	}
	b.cfg.ConfigureDevice(c.DeviceName, c.Limits.HistoryDays)
	recent, err := st.RecentSends(ctx, b.cfg.Now().Add(-SendWindow))
	if err != nil {
		b.cfg.Log.Warn("store_write_failed", logx.Code("outbox"))
	}
	b.mu.Lock()
	b.init = *c
	b.init.StoreKey = "" // the key stays only in the open store's connection hook
	b.sends = b.clampSends(recent, b.cfg.Now())
	b.caps = &history.Caps{Days: c.Limits.HistoryDays, MaxPerChat: c.Limits.HistoryMaxPerChat, Now: b.cfg.Now}
	b.lim = media.Limits{ImageMaxBytes: c.Limits.ImageMaxBytes, VoiceMaxSeconds: c.Limits.VoiceMaxSeconds, VoiceMaxBytes: c.Limits.VoiceBytes(),
		FileMaxBytes: c.Limits.FileBytes(), VideoMaxBytes: c.Limits.VideoBytes()}
	b.mu.Unlock()
	if n := media.CleanStale(c.MediaDir, b.cfg.Now()); n > 0 {
		b.cfg.Log.Info("media_stale_deleted", logx.N(int64(n)))
	}
	if _, err := st.PruneMedia(ctx, b.cfg.Now()); err != nil {
		b.cfg.Log.Warn("media_prune_failed")
	}
	s, err := b.install(st)
	if err != nil {
		st.Close()
		b.fail(id, protocol.ErrStoreIO)
		return
	}
	b.mu.Lock()
	b.inited = true
	b.mu.Unlock()
	acct := s.cli.Account()
	ready := protocol.Ready{ReplyTo: id, Paired: !acct.JID.IsEmpty()}
	if ready.Paired {
		ready.Account = &protocol.Account{JID: acct.JID.String(), PushName: truncate(acct.PushName, protocol.MaxNameChars)}
		if acct.LID.Server == types.HiddenUserServer && acct.LID.User != "" {
			ready.Account.LID = acct.LID.String()
		}
	}
	b.emit(protocol.EvReady, ready)
	b.cfg.Log.Info("ready")
	if ready.Paired {
		b.connect(s)
	} else {
		b.setState(protocol.StateUnpaired)
	}
}

// install creates the client for an open store and makes it current.
func (b *Bridge) install(st *store.Store) (*session, error) {
	cli, err := b.cfg.NewClient(st)
	if err != nil {
		return nil, err
	}
	chats, err := st.Chats(context.Background())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ctx: ctx, cancel: cancel, cli: cli, st: st, hsig: make(chan struct{}, 1)}
	cli.AddEventHandler(func(evt any) { b.onEvent(s, evt) })
	b.mu.Lock()
	b.sess = s
	b.chats = chats
	b.contacts = map[string]bool{}
	b.groupsSent = map[string]bool{}
	b.groupCache = map[string]*types.GroupInfo{}
	b.groupsLoaded = false
	b.mu.Unlock()
	b.goSafe("history", nil, func() { b.historyLoop(s) })
	return s, nil
}

func (b *Bridge) connect(s *session) {
	b.setState(protocol.StateConnecting)
	b.async(func() {
		if err := s.cli.Connect(); err != nil {
			b.cfg.Log.Warn("connect_failed")
			if s.ctx.Err() == nil {
				b.setState(protocol.StateReconnecting)
			}
		}
	})
}

// Stop ends the bridge: it closes stdout to everything but its own final
// lines, cancels every running operation, disconnects, waits for the bridge's
// goroutines, and only then closes the store (or, after a logout, deletes it).
// It is used for shutdown, stdin EOF, fatal errors and logouts, and returns
// within about two seconds. A goroutine that outlives the wait can no longer
// write to stdout (review R-M1).
func (b *Bridge) Stop() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true // from now on goSafe starts nothing
	s := b.sess
	p := b.pair
	b.pair = nil
	b.mu.Unlock()
	b.cfg.Out.Quiesce()
	if p != nil {
		p.cancel()
	}
	if s != nil {
		s.cancel()
		s.cli.Disconnect()
	}
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		// A goroutine still runs; the store refuses its later calls (ErrClosed).
		b.cfg.Log.Warn("stop_timeout")
	}
	// Read after the wait: a logout that WhatsApp confirmed while Stop was
	// starting still deletes the store (review R-L7).
	b.mu.Lock()
	lo := b.loggedOut
	b.mu.Unlock()
	if s != nil {
		if lo != nil {
			if err := s.st.Wipe(); err != nil {
				b.cfg.Log.Error("store_wipe_failed")
				b.emitFinal(protocol.EvError, protocol.NewError("", protocol.ErrStoreIO))
			} else {
				b.cfg.Log.Info("store_wiped")
			}
			b.emitFinal(protocol.EvLoggedOut, protocol.LoggedOut{Reason: lo.reason, Code: lo.code})
		} else if err := s.st.Close(); err != nil {
			b.cfg.Log.Warn("store_close_failed")
		}
	}
	if b.Initialized() {
		b.changeState(protocol.StateStopped, b.emitFinal)
	}
	if lo != nil && lo.replyTo != "" {
		b.emitFinal(protocol.EvOK, protocol.Reply{ReplyTo: lo.replyTo})
	}
}

// LoggedOut reports whether the account was unlinked (after Stop: whether the
// store was deleted). The caller then exits with ExitLoggedOut.
func (b *Bridge) LoggedOut() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loggedOut != nil
}

type logoutInfo struct {
	reason  string
	code    int
	replyTo string // the logout command, if the client asked
}

// loggedOutBy records that the account behind old is no longer linked and
// asks the caller to exit with ExitLoggedOut. The session stops at once; Stop
// deletes the store after every goroutine has finished, then reports
// logged_out, status stopped and, for the logout command, ok. The bridge never
// opens a new store with the same key: the client restarts it with a new key
// (PROTOCOL.md §6.4, review M3). It is recorded also when Stop has already
// begun: Stop reads it after its wait (review R-L7).
func (b *Bridge) loggedOutBy(old *session, reason string, code int, replyTo string) {
	b.mu.Lock()
	if b.sess != old || b.loggedOut != nil {
		b.mu.Unlock()
		return
	}
	b.loggedOut = &logoutInfo{reason: reason, code: code, replyTo: replyTo}
	p := b.pair
	b.pair = nil
	b.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	old.cancel()
	old.cli.Disconnect()
	b.cfg.Log.Info("exit_logged_out")
	b.exit(ExitLoggedOut)
}
