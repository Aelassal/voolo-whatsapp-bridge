// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/transport"
)

// fakeClient stands in for whatsmeow. It never touches the network.
type fakeClient struct {
	mu        sync.Mutex
	handlers  []whatsmeow.EventHandler
	connected bool
	account   AccountInfo

	connects, disconnects int
	onConnect             func(*fakeClient)

	sent      []sentMsg
	sendErr   error
	sendGate  chan struct{} // when set, SendMessage waits for it
	sendCalls int
	sendPanic string // when set, SendMessage panics with it

	marked  []markCall
	markErr error

	downloads map[string][]byte // direct path → plaintext
	dlErr     error
	dlCalls   int
	dlLimits  []int64 // transport body limit carried by each download context (-1: none)

	uploads   int
	uploadErr error

	parses    int
	parseHook func(n int) // called for every ParseWebMessage, with its 1-based count

	pnForLID map[string]types.JID
	contacts map[string]types.ContactInfo
	groups   []*types.GroupInfo

	history map[*waE2E.HistorySyncNotification]*waHistorySync.HistorySync
	dlHist  int

	patches  []appstate.PatchInfo
	patchErr error

	pairPhones []string
	pairNames  []string
	logoutErr  error
	logouts    int
	logoutGate chan struct{} // when set, Logout waits for it (ignoring ctx) and then succeeds
}

type sentMsg struct {
	to  types.JID
	msg *waE2E.Message
}

type markCall struct {
	ids          []string
	chat, sender types.JID
}

func newFake() *fakeClient {
	return &fakeClient{downloads: map[string][]byte{}, pnForLID: map[string]types.JID{}, contacts: map[string]types.ContactInfo{},
		history: map[*waE2E.HistorySyncNotification]*waHistorySync.HistorySync{}}
}

func (f *fakeClient) AddEventHandler(h whatsmeow.EventHandler) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
	return uint32(len(f.handlers))
}

func (f *fakeClient) dispatch(evt any) {
	f.mu.Lock()
	hs := append([]whatsmeow.EventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range hs {
		h(evt)
	}
}

func (f *fakeClient) Connect() error {
	f.mu.Lock()
	f.connects++
	f.connected = true
	cb := f.onConnect
	f.mu.Unlock()
	if cb != nil {
		go cb(f)
	}
	return nil
}

func (f *fakeClient) Disconnect() {
	f.mu.Lock()
	f.disconnects++
	f.connected = false
	f.mu.Unlock()
}

func (f *fakeClient) IsConnected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connected }
func (f *fakeClient) IsLoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected && !f.account.JID.IsEmpty()
}

func (f *fakeClient) PairPhone(ctx context.Context, phone string, _ bool, _ whatsmeow.PairClientType, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairPhones = append(f.pairPhones, phone)
	f.pairNames = append(f.pairNames, name)
	return "ABCD-2345", nil
}

func (f *fakeClient) SendMessage(ctx context.Context, to types.JID, m *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	f.sendCalls++
	gate, err, pv := f.sendGate, f.sendErr, f.sendPanic
	f.mu.Unlock()
	if pv != "" {
		panic(pv)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return whatsmeow.SendResponse{}, ctx.Err()
		}
	}
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}
	f.mu.Lock()
	f.sent = append(f.sent, sentMsg{to, m})
	f.mu.Unlock()
	return whatsmeow.SendResponse{ID: "3EB0SENT00000001", Timestamp: time.UnixMilli(1790330400900)}, nil
}

func (f *fakeClient) MarkRead(ctx context.Context, ids []types.MessageID, _ time.Time, chat, sender types.JID, _ ...types.ReceiptType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, markCall{ids: ids, chat: chat, sender: sender})
	return f.markErr
}

func (f *fakeClient) DownloadMediaWithPath(ctx context.Context, path string, _, _, _ []byte, _ whatsmeow.MediaType, _ string, _ bool) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dlCalls++
	lim, ok := transport.BodyLimit(ctx)
	if !ok {
		lim = -1
	}
	f.dlLimits = append(f.dlLimits, lim)
	if f.dlErr != nil {
		return nil, f.dlErr
	}
	return append([]byte(nil), f.downloads[path]...), nil
}

func (f *fakeClient) Upload(ctx context.Context, data []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	f.uploads++
	err := f.uploadErr
	f.mu.Unlock()
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}
	return whatsmeow.UploadResponse{URL: "https://mmg.whatsapp.net/x", DirectPath: "/v/x", MediaKey: []byte{1}, FileLength: uint64(len(data))}, nil
}

func (f *fakeClient) Logout(ctx context.Context) error {
	f.mu.Lock()
	f.logouts++
	gate := f.logoutGate
	f.mu.Unlock()
	if gate != nil {
		<-gate // WhatsApp confirms the unlink whatever happens meanwhile
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logoutErr != nil {
		return f.logoutErr
	}
	f.connected = false
	f.account = AccountInfo{}
	return nil
}

func (f *fakeClient) ParseWebMessage(chat types.JID, w *waWeb.WebMessageInfo) (*events.Message, error) {
	f.mu.Lock()
	f.parses++
	n, hook := f.parses, f.parseHook
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: w.GetKey().GetFromMe(), IsGroup: chat.Server == types.GroupServer},
		ID:            w.GetKey().GetID(), PushName: w.GetPushName(), Timestamp: time.Unix(int64(w.GetMessageTimestamp()), 0),
	}
	switch {
	case info.IsFromMe:
		info.Sender = f.Account().JID
	case !info.IsGroup:
		info.Sender = chat
	default:
		info.Sender, _ = types.ParseJID(w.GetKey().GetParticipant())
	}
	evt := &events.Message{Info: info, RawMessage: w.GetMessage(), SourceWebMsg: w}
	return evt.UnwrapRaw(), nil
}

func (f *fakeClient) DownloadHistorySync(ctx context.Context, n *waE2E.HistorySyncNotification, _ bool) (*waHistorySync.HistorySync, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dlHist++
	return f.history[n], nil
}

func (f *fakeClient) DeleteMedia(context.Context, whatsmeow.MediaType, string, []byte, string) error {
	return nil
}

func (f *fakeClient) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups, nil
}

func (f *fakeClient) GetGroupInfo(ctx context.Context, j types.JID) (*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.groups {
		if g.JID == j {
			return g, nil
		}
	}
	return nil, whatsmeow.ErrGroupNotFound
}

func (f *fakeClient) SendAppState(_ context.Context, p appstate.PatchInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patchErr != nil {
		return f.patchErr
	}
	f.patches = append(f.patches, p)
	return nil
}

func (f *fakeClient) Account() AccountInfo { f.mu.Lock(); defer f.mu.Unlock(); return f.account }

func (f *fakeClient) PNForLID(_ context.Context, lid types.JID) types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pnForLID[lid.String()]
}

func (f *fakeClient) Contact(_ context.Context, j types.JID) types.ContactInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contacts[j.String()]
}

// ---------------------------------------------------------------- harness

const (
	me       = "15550100001@s.whatsapp.net"
	alice    = "15550100002@s.whatsapp.net"
	bob      = "15550100003@s.whatsapp.net"
	group    = "120363000000000001@g.us"
	aliceLID = "100000000000002@lid"
	// A key with letters and digits, so a leak through the digit rule of the
	// stderr scrubber alone would not hide it (review L1).
	testKey = "9f3ab7c2e81d4f06a5b9c3d7e2f14a8b6c0d9e3f7a2b5c8d1e4f7a0b3c6d9e2f"
)

type harness struct {
	t        *testing.T
	b        *Bridge
	stdin    chan []byte
	lines    chan protocol.Envelope
	raw      *lockedBuf
	stderr   *lockedBuf
	storeDir string
	mediaDir string
	fakes    []*fakeClient
	mkFake   func() *fakeClient
	now      time.Time // base time; Now() returns now plus the advanced offset
	offset   atomic.Int64
	nowHook  atomic.Pointer[func()] // when set, runs on every call of the bridge's clock
	ids      *protocol.IDSource
	refresh  int
	opens    int // OpenStore calls
	mu       sync.Mutex
}

// advance moves the bridge's clock forward (the send backstop uses it).
func (h *harness) advance(d time.Duration) { h.offset.Add(int64(d)) }

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func mustParse(s string) types.JID { j, _ := types.ParseJID(s); return j }

func newHarness(t *testing.T, mk func() *fakeClient) *harness {
	t.Helper()
	store.RestrictUmask()
	dir := t.TempDir()
	h := &harness{t: t, lines: make(chan protocol.Envelope, 10000), raw: &lockedBuf{}, stderr: &lockedBuf{},
		storeDir: filepath.Join(dir, "store"), mediaDir: filepath.Join(dir, "media"), mkFake: mk,
		now: time.UnixMilli(1790330400000), ids: protocol.NewIDSource(nil)}
	if h.mkFake == nil {
		h.mkFake = newFake
	}
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 2<<20), 2<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			_, _ = h.raw.Write(append(line, '\n'))
			env, err := protocol.ParseEnvelope(line)
			if err != nil {
				t.Errorf("bridge wrote an invalid line: %s", line)
				continue
			}
			h.lines <- env
		}
	}()
	cfg := Config{
		Out: protocol.NewWriter(pw, nil),
		Log: logx.New(h.stderr, nil),
		Now: func() time.Time {
			if f := h.nowHook.Load(); f != nil {
				(*f)()
			}
			return h.now.Add(time.Duration(h.offset.Load()))
		},
		OpenStore: func(ctx context.Context, d string, key []byte) (*store.Store, error) {
			h.mu.Lock()
			h.opens++
			h.mu.Unlock()
			return store.Open(ctx, d, key, logx.NewWA(logx.New(h.stderr, nil), "Database"))
		},
		NewClient: func(st *store.Store) (Client, error) {
			// After a logout the store is new, so later clients are unpaired.
			h.mu.Lock()
			first := len(h.fakes) == 0
			h.mu.Unlock()
			f := newFake()
			if first {
				f = h.mkFake()
			}
			h.mu.Lock()
			h.fakes = append(h.fakes, f)
			h.mu.Unlock()
			return f, nil
		},
		RefreshVersion:  func(context.Context) error { h.mu.Lock(); h.refresh++; h.mu.Unlock(); return nil },
		ConfigureDevice: func(string, int) {},
		QRFirst:         150 * time.Millisecond,
		QRNext:          50 * time.Millisecond,
		FirstQRWait:     2 * time.Second,
		HistoryIdle:     300 * time.Millisecond,
		SendTimeout:     2 * time.Second,
		FetchTimeout:    2 * time.Second,
	}
	h.b = New(cfg)
	t.Cleanup(func() { h.b.Stop(); pw.Close() })
	return h
}

func (h *harness) fake() *fakeClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fakes[len(h.fakes)-1]
}

// cmd sends one command line through the real envelope parser and returns its id.
func (h *harness) cmd(typ string, payload any) string {
	h.t.Helper()
	id := h.ids.New()
	p, _ := json.Marshal(payload)
	line, _ := json.Marshal(map[string]any{"v": 1, "id": id, "type": typ, "ts": 1790330400000, "payload": json.RawMessage(p)})
	env, err := protocol.ParseEnvelope(line)
	if err != nil {
		h.t.Fatalf("test built a bad line: %v", err)
	}
	h.b.Handle(env)
	return id
}

func (h *harness) init(limits map[string]any) {
	h.t.Helper()
	if limits == nil {
		limits = map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 16 << 20, "voiceMaxSeconds": 3600}
	}
	id := h.cmd("init", map[string]any{"storeKey": testKey, "storeDir": h.storeDir, "mediaDir": h.mediaDir, "deviceName": "Voolo", "limits": limits})
	r := h.expect(protocol.EvReady)
	if reply(r) != id {
		h.t.Fatalf("ready replyTo %s, want %s", reply(r), id)
	}
}

func reply(e protocol.Envelope) string {
	var p struct {
		ReplyTo string `json:"replyTo"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	return p.ReplyTo
}

func field[T any](e protocol.Envelope) T {
	var v T
	_ = json.Unmarshal(e.Payload, &v)
	return v
}

// expect returns the next line of type typ, failing on timeout. Lines of
// other types before it are skipped unless strict is used.
func (h *harness) expect(typ string) protocol.Envelope {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-h.lines:
			if e.Type == typ {
				return e
			}
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s; output so far:\n%s", typ, h.raw.String())
		}
	}
}

// next returns the next line whatever its type.
func (h *harness) next() protocol.Envelope {
	h.t.Helper()
	select {
	case e := <-h.lines:
		return e
	case <-time.After(5 * time.Second):
		h.t.Fatalf("timed out; output so far:\n%s", h.raw.String())
	}
	return protocol.Envelope{}
}

// none asserts that no line of type typ arrives within d.
func (h *harness) none(typ string, d time.Duration) {
	h.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-h.lines:
			if e.Type == typ {
				h.t.Fatalf("unexpected %s: %s", typ, e.Payload)
			}
		case <-deadline:
			return
		}
	}
}

func (h *harness) expectError(replyTo, code string) {
	h.t.Helper()
	e := h.expect(protocol.EvError)
	p := field[protocol.ErrorEvent](e)
	if p.ReplyTo != replyTo || p.Code != code {
		h.t.Fatalf("error = %+v, want replyTo %s code %s", p, replyTo, code)
	}
}

// paired makes the fake a linked, connected account and runs init.
func pairedFake() *fakeClient {
	f := newFake()
	f.account = AccountInfo{JID: mustParse(me), PushName: "Me"}
	return f
}

func (h *harness) initPairedConnected() *fakeClient {
	h.t.Helper()
	h.init(nil)
	f := h.fake()
	h.expectState(protocol.StateConnecting)
	// Wait for the bridge's own Connect before reporting "connected": a
	// Connected event that overtakes it would leave the fake disconnected
	// (review L13, the TestMarkRead flake).
	waitFor(h.t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connects > 0 })
	f.dispatch(&events.Connected{})
	h.expectState(protocol.StateConnected)
	return f
}

func (h *harness) expectState(s string) {
	h.t.Helper()
	for {
		e := h.expect(protocol.EvStatus)
		if field[protocol.Status](e).State == s {
			return
		}
	}
}

// waitFor polls cond until it holds, failing after 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5 s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
