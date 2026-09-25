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

// Exit codes (PROTOCOL.md §9).
const (
	ExitOK             = 0
	ExitCrash          = 1
	ExitNoInit         = 2
	ExitStoreKey       = 3
	ExitStoreLocked    = 4
	ExitClientOutdated = 5
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

	mu       sync.Mutex
	inited   bool
	init     protocol.Init
	key      []byte
	sess     *session
	state    string
	pair     *pairing
	stopped  bool
	outdated int // ClientOutdated events seen

	sending  atomic.Bool
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
	}
}

// Fatal delivers the exit code after a fatal error event has been written.
func (b *Bridge) Fatal() <-chan int { return b.fatal }

// Initialized reports whether init succeeded.
func (b *Bridge) Initialized() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inited
}

func (b *Bridge) emit(typ string, payload any) {
	if err := b.cfg.Out.Emit(typ, payload); err != nil {
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
	select {
	case b.fatal <- exit:
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

func (b *Bridge) setState(s string) {
	b.mu.Lock()
	changed := b.state != s
	b.state = s
	b.mu.Unlock()
	if changed {
		b.emit(protocol.EvStatus, protocol.Status{State: s})
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
		b.async(func() { b.sendText(id, c) })
	case *protocol.SendMedia:
		b.async(func() { b.sendMedia(id, c) })
	case *protocol.MarkRead:
		b.async(func() { b.markRead(id, c) })
	case *protocol.FetchMedia:
		b.async(func() { b.fetchMedia(id, c) })
	case *protocol.Empty:
		switch env.Type {
		case protocol.CmdPing:
			b.emit(protocol.EvPong, protocol.Reply{ReplyTo: id})
		case protocol.CmdShutdown:
			return true
		case protocol.CmdPairQR:
			b.startPairing(id, "")
		case protocol.CmdLogout:
			b.async(func() { b.logout(id) })
		}
	}
	return false
}

func (b *Bridge) async(f func()) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				b.cfg.Log.Error("panic_recovered")
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
	if err := os.MkdirAll(c.MediaDir, 0o700); err != nil {
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
	b.mu.Lock()
	b.init = *c
	b.key = key
	b.caps = &history.Caps{Days: c.Limits.HistoryDays, MaxPerChat: c.Limits.HistoryMaxPerChat, Now: b.cfg.Now}
	b.lim = media.Limits{ImageMaxBytes: c.Limits.ImageMaxBytes, VoiceMaxSeconds: c.Limits.VoiceMaxSeconds, VoiceMaxBytes: c.Limits.VoiceBytes(), FileMaxBytes: protocol.MaxFileBytes}
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
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.historyLoop(s)
	}()
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

// Stop disconnects, closes the store and reports "stopped". It is used for
// shutdown and stdin EOF, and returns within a few hundred milliseconds.
func (b *Bridge) Stop() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	s := b.sess
	p := b.pair
	b.pair = nil
	b.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	if s != nil {
		s.cancel()
		s.cli.Disconnect()
		if err := s.st.Close(); err != nil {
			b.cfg.Log.Warn("store_close_failed")
		}
	}
	if b.Initialized() {
		b.setState(protocol.StateStopped)
	}
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		b.cfg.Log.Warn("stop_timeout")
	}
}

// resetAfterLogout wipes the store, opens a fresh one with the same key and a
// new client, and reports the logout. Synced chats are the client's business.
func (b *Bridge) resetAfterLogout(old *session, reason string, code int) {
	b.mu.Lock()
	if b.sess != old || b.stopped {
		b.mu.Unlock()
		return
	}
	p := b.pair
	b.pair = nil
	init := b.init
	key := b.key
	b.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	old.cancel()
	old.cli.Disconnect()
	if err := old.st.Wipe(); err != nil {
		b.cfg.Log.Error("store_wipe_failed")
	}
	b.cfg.Log.Info("store_wiped")
	b.emit(protocol.EvLoggedOut, protocol.LoggedOut{Reason: reason, Code: code})
	b.setState(protocol.StateStopped)
	b.window.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	st, err := b.cfg.OpenStore(ctx, init.StoreDir, key)
	if err != nil {
		b.cfg.Log.Error("store_reopen_failed")
		b.fail("", protocol.ErrStoreIO)
		return
	}
	if _, err := b.install(st); err != nil {
		st.Close()
		b.fail("", protocol.ErrStoreIO)
		return
	}
	b.setState(protocol.StateUnpaired)
}
