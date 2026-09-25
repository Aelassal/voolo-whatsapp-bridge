// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

// Regression tests for the P1-A security review (docs/reviews/P1-A-security-review.md).
// Each test names the finding it guards.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/media"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

// pushHistory queues one history notification of convs chats × per messages.
func (h *harness) pushHistory(f *fakeClient, convs, per int) {
	var cs []*waHistorySync.Conversation
	for c := 0; c < convs; c++ {
		chat := fmt.Sprintf("155503%05d@s.whatsapp.net", c)
		conv := &waHistorySync.Conversation{ID: proto.String(chat)}
		for i := 0; i < per; i++ {
			conv.Messages = append(conv.Messages, histMsg(chat, fmt.Sprintf("3EB0Y%dX%d", c, i), h.now.Add(-time.Duration(i)*time.Minute), "m"))
		}
		cs = append(cs, conv)
	}
	n := &waE2E.HistorySyncNotification{}
	f.mu.Lock()
	f.history[n] = &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(), Conversations: cs, Progress: proto.Uint32(100)}
	f.mu.Unlock()
	f.dispatch(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(me), Sender: mustParse(me), IsFromMe: true}, ID: "3EB0N"},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: n}}})
}

// holdHistory makes the history worker stop at its 50th parsed message until
// the fake has been disconnected, and then keep going 100 ms later: long
// enough for a Stop or logout that closes the store before waiting for the
// worker to do so under it (the review's H1 reproduction, made deterministic).
func holdHistory(f *fakeClient, atStop func()) <-chan struct{} {
	reached := make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	f.parseHook = func(n int) {
		if n != 50 {
			return
		}
		once.Do(func() { close(reached) })
		if atStop != nil {
			atStop()
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			f.mu.Lock()
			d := f.disconnects
			f.mu.Unlock()
			if d > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.mu.Unlock()
	return reached
}

func (h *harness) expectExit(want int) {
	h.t.Helper()
	select {
	case c := <-h.b.Fatal():
		if c != want {
			h.t.Fatalf("exit code %d, want %d", c, want)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("no exit request; output:\n%s", h.raw.String())
	}
}

func noPanicOnStderr(t *testing.T, h *harness) {
	t.Helper()
	if s := h.stderr.String(); strings.Contains(s, "panic") {
		t.Fatalf("a panic was recovered on stderr:\n%s", s)
	}
}

// H1: shutdown while history sync runs. Before the fix Stop closed the store
// (s.DB = nil) and then waited, so the history worker crashed the process in
// store.AddChats with a nil *sql.DB.
func TestH1StopDuringHistorySyncDoesNotCrash(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	st := h.b.current().st
	reached := holdHistory(f, nil)
	h.pushHistory(f, 3, 2000)
	<-reached
	h.b.Stop()
	h.expectState(protocol.StateStopped)
	if !st.Closed() {
		t.Fatal("store not closed by Stop")
	}
	noPanicOnStderr(t, h)
	// Store methods after Close return an error instead of panicking.
	if err := st.AddChats(context.Background(), []string{alice}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("AddChats after Close: %v", err)
	}
	if _, err := st.ReserveOutbox(context.Background(), "01M3C03V80N87VFZS5G0J0NFEX", h.now); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("ReserveOutbox after Close: %v", err)
	}
}

// H1 + M3: a remote logout while history sync runs. Before the fix the logout
// goroutine wiped the store (DB = nil) under the running history worker.
func TestH1RemoteLogoutDuringHistorySyncDoesNotCrash(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	reached := holdHistory(f, func() { f.dispatch(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}) })
	h.pushHistory(f, 3, 2000)
	<-reached
	h.expectExit(ExitLoggedOut)
	h.b.Stop()
	if lo := field[protocol.LoggedOut](h.expect(protocol.EvLoggedOut)); lo.Reason != protocol.LogoutDeviceRemoved {
		t.Fatalf("logged_out %+v", lo)
	}
	h.expectState(protocol.StateStopped)
	if fileExists(filepath.Join(h.storeDir, store.DBFile)) {
		t.Fatal("store file kept after logout")
	}
	noPanicOnStderr(t, h)
}

// H1/M1: a panic in the history worker is recovered with a constant code and
// never reaches the runtime (which would print it and exit 2).
func TestH1PanicInHistoryWorkerIsRecovered(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.mu.Lock()
	f.parseHook = func(n int) { panic("history panic with 15550100002@s.whatsapp.net") }
	f.mu.Unlock()
	h.pushHistory(f, 1, 5)
	waitFor(t, func() bool { return strings.Contains(h.stderr.String(), "panic_recovered") })
	if strings.Contains(h.stderr.String(), "15550100002") {
		t.Fatal("panic value reached stderr")
	}
	// Live events still flow.
	f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0AFTER", "still alive", false))
	h.expect(protocol.EvMessage)
}

// M1/H1: a panic in a command goroutine gets a final `internal` reply, frees
// the send slot and prints nothing but a constant code.
func TestH1PanicInSendRepliesInternalAndFreesSlot(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	f.mu.Lock()
	f.sendPanic = "send panic with " + alice
	f.mu.Unlock()
	id := h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFP1"})
	h.expectError(id, protocol.ErrInternal)
	f.mu.Lock()
	f.sendPanic = ""
	f.mu.Unlock()
	h.advance(2 * time.Second)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "y", "outboxId": "01M3C03V80N87VFZS5G0J0NFP2"})
	h.expect(protocol.EvSendResult)
	if s := h.stderr.String(); !strings.Contains(s, "panic_recovered") || strings.Contains(s, "15550100002") {
		t.Fatalf("stderr:\n%s", s)
	}
}

// M3: the logout command wipes the store and exits; the old key never opens a
// new store (the client restarts the bridge with a fresh key).
func TestM3LogoutCommandWipesStoreAndExits(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	old := h.b.current().st
	id := h.cmd("logout", map[string]any{})
	h.expectExit(ExitLoggedOut)
	h.b.Stop()
	if lo := field[protocol.LoggedOut](h.expect(protocol.EvLoggedOut)); lo.Reason != protocol.LogoutUser {
		t.Fatalf("logged_out %+v", lo)
	}
	h.expectState(protocol.StateStopped)
	if reply(h.expect(protocol.EvOK)) != id {
		t.Fatal("ok replyTo")
	}
	if f.logouts != 1 || !old.Closed() {
		t.Fatalf("logouts=%d closed=%v", f.logouts, old.Closed())
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if fileExists(filepath.Join(h.storeDir, store.DBFile+suffix)) {
			t.Fatalf("%s%s kept", store.DBFile, suffix)
		}
	}
	h.mu.Lock()
	opens := h.opens
	h.mu.Unlock()
	if opens != 1 {
		t.Fatalf("store opened %d times: a store was reopened under the old key", opens)
	}
	h.none(protocol.EvStatus, 100*time.Millisecond)
}

// M4: no failure of a send is ever retried by the bridge, whatever it maps to.
func TestM4SendNeverRetried(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"timeout", whatsmeow.ErrMessageTimedOut, protocol.ErrTimeout},
		{"deadline", context.DeadlineExceeded, protocol.ErrTimeout},
		{"cancel", context.Canceled, protocol.ErrTimeout},
		{"send_failed", errors.New("refused"), protocol.ErrSendFailed},
		{"not_connected", whatsmeow.ErrNotConnected, protocol.ErrNotConnected},
		{"rate_limited", serverErr{}, protocol.ErrRateLimited},
	}
	for _, c := range cases {
		for _, kind := range []string{"text", "media"} {
			t.Run(c.name+"/"+kind, func(t *testing.T) {
				h := newHarness(t, pairedFake)
				f := h.initPairedConnected()
				h.knownChat(f)
				f.mu.Lock()
				f.sendErr = c.err
				f.mu.Unlock()
				outbox := "01M3C03V80N87VFZS5G0J0NFQ1"
				id := h.sendKind(kind, outbox)
				h.expectError(id, c.code)
				time.Sleep(100 * time.Millisecond)
				f.mu.Lock()
				calls, ups := f.sendCalls, f.uploads
				f.mu.Unlock()
				if calls != 1 {
					t.Fatalf("SendMessage called %d times", calls)
				}
				if kind == "media" && ups != 1 {
					t.Fatalf("Upload called %d times", ups)
				}
			})
		}
	}
	// Upload failures: nothing reaches SendMessage, and the upload is not repeated.
	for _, c := range []struct {
		err  error
		code string
	}{{errors.New("upload refused"), protocol.ErrTimeout}, {whatsmeow.ErrNotConnected, protocol.ErrNotConnected}, {context.Canceled, protocol.ErrTimeout}} {
		h := newHarness(t, pairedFake)
		f := h.initPairedConnected()
		h.knownChat(f)
		f.mu.Lock()
		f.uploadErr = c.err
		f.mu.Unlock()
		h.expectError(h.sendKind("media", "01M3C03V80N87VFZS5G0J0NFQ2"), c.code)
		time.Sleep(100 * time.Millisecond)
		f.mu.Lock()
		calls, ups := f.sendCalls, f.uploads
		f.mu.Unlock()
		if calls != 0 || ups != 1 {
			t.Fatalf("%v: send %d upload %d", c.err, calls, ups)
		}
	}
	// A send cut short by shutdown is not retried either.
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	f.mu.Lock()
	f.sendGate = make(chan struct{})
	f.mu.Unlock()
	h.sendKind("text", "01M3C03V80N87VFZS5G0J0NFQ3")
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.sendCalls == 1 })
	h.b.Stop()
	time.Sleep(50 * time.Millisecond)
	if f.sendCalls != 1 {
		t.Fatalf("SendMessage called %d times", f.sendCalls)
	}
}

// sendKind sends a text, or a small document through send_media.
func (h *harness) sendKind(kind, outbox string) string {
	if kind == "text" {
		return h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": outbox})
	}
	s := sealForTest(h.t, h.mediaDir, []byte("%PDF-1.7 document"))
	return h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": outbox, "kind": "file", "path": s.path, "key": s.key, "sha256": s.sha, "mime": "application/pdf"})
}

// M6: a compiled-in backstop: at least 1 s between sends and at most 30 sends
// per 10 minutes, refused with rate_limited_local without using the outbox id.
func TestM6SendBackstop(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	ids := protocol.NewIDSource(nil)
	first := ids.New()
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "1", "outboxId": first})
	h.expect(protocol.EvSendResult)
	// Right away: refused, retryable, and the outbox id stays usable.
	next := ids.New()
	id := h.cmd("send_text", map[string]any{"chatJid": alice, "text": "2", "outboxId": next})
	e := field[protocol.ErrorEvent](h.expect(protocol.EvError))
	if e.ReplyTo != id || e.Code != protocol.ErrRateLimitedLocal || !e.Retryable {
		t.Fatalf("error %+v", e)
	}
	// A repeated outbox id is still reported as such, not as rate limited.
	h.expectError(h.cmd("send_text", map[string]any{"chatJid": alice, "text": "1", "outboxId": first}), protocol.ErrDuplicateOutboxID)
	h.advance(time.Second)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "2", "outboxId": next})
	h.expect(protocol.EvSendResult)
	// 28 more, one per second: 30 in the window.
	for i := 0; i < 28; i++ {
		h.advance(time.Second)
		h.cmd("send_text", map[string]any{"chatJid": alice, "text": "n", "outboxId": ids.New()})
		h.expect(protocol.EvSendResult)
	}
	h.advance(time.Second)
	h.expectError(h.cmd("send_text", map[string]any{"chatJid": alice, "text": "31", "outboxId": ids.New()}), protocol.ErrRateLimitedLocal)
	// The window also holds across a restart of the bridge.
	h.b.Stop()
	h2 := newHarness(t, pairedFake)
	h2.storeDir, h2.mediaDir = h.storeDir, h.mediaDir
	h2.offset.Store(h.offset.Load())
	f2 := h2.initPairedConnected()
	h2.knownChat(f2)
	h2.expectError(h2.cmd("send_text", map[string]any{"chatJid": alice, "text": "31", "outboxId": ids.New()}), protocol.ErrRateLimitedLocal)
	// Ten minutes after the first send, it has left the window.
	h2.advance(10*time.Minute - 29*time.Second)
	h2.cmd("send_text", map[string]any{"chatJid": alice, "text": "31", "outboxId": ids.New()})
	h2.expect(protocol.EvSendResult)
	f.mu.Lock()
	calls := f.sendCalls
	f.mu.Unlock()
	if calls != 30 || f2.sendCalls != 1 {
		t.Fatalf("SendMessage calls %d + %d, want 30 + 1", calls, f2.sendCalls)
	}
}

// M2: the size cap holds against the real download, not only the declared
// size, and the download context carries the transport's body limit.
func TestM2FetchMediaBoundedByRealSize(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.init(map[string]any{"historyDays": 90, "historyMaxPerChat": 100, "imageMaxBytes": 1000, "voiceMaxSeconds": 60, "voiceMaxBytes": 2000})
	f := h.fake()
	h.expectState(protocol.StateConnecting)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connects > 0 })
	f.dispatch(&events.Connected{})
	f.downloads["/v/big"] = make([]byte, 1001) // declared 20 bytes, really 1,001
	f.dispatch(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(alice), Sender: mustParse(alice)}, ID: "3EB0BIG", Timestamp: h.now},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{FileLength: proto.Uint64(20), Mimetype: proto.String("image/jpeg"), DirectPath: proto.String("/v/big"), MediaKey: []byte{1}}}})
	h.expect(protocol.EvMessage)
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0BIG"}), protocol.ErrMediaTooLarge)
	f.mu.Lock()
	lims := append([]int64(nil), f.dlLimits...)
	f.mu.Unlock()
	if len(lims) != 1 || lims[0] != 1000+media.DownloadOverhead {
		t.Fatalf("download body limits %v, want [%d]", lims, 1000+media.DownloadOverhead)
	}
	if entries, _ := os.ReadDir(h.mediaDir); len(entries) != 0 {
		t.Fatal("a hand-off file was written for an oversized download")
	}
}

func oggStream(seconds int) []byte {
	page := func(granule uint64, payload []byte) []byte {
		b := make([]byte, 27)
		copy(b, "OggS")
		binary.LittleEndian.PutUint64(b[6:], granule)
		return append(b, payload...)
	}
	head := append([]byte("OpusHead"), 1, 1, 0, 0)
	return append(page(0, head), page(uint64(seconds)*48000, []byte("opus data"))...)
}

// L10: voice duration comes from the file where it can be read.
func TestL10VoiceDurationFromFile(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.init(map[string]any{"historyDays": 90, "historyMaxPerChat": 100, "imageMaxBytes": 1000, "voiceMaxSeconds": 15})
	f := h.fake()
	h.expectState(protocol.StateConnecting)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connects > 0 })
	f.dispatch(&events.Connected{})
	voice := func(id, path string, declared uint32, data []byte) {
		f.mu.Lock()
		f.downloads[path] = data
		f.mu.Unlock()
		f.dispatch(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(alice), Sender: mustParse(alice)}, ID: id, Timestamp: h.now},
			Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(declared), FileLength: proto.Uint64(uint64(len(data))),
				Mimetype: proto.String("audio/ogg; codecs=opus"), DirectPath: proto.String(path), MediaKey: []byte{1}}}})
		h.expect(protocol.EvMessage)
	}
	voice("3EB0LONG", "/v/long", 10, oggStream(20)) // declares 10 s, really 20 s
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0LONG"}), protocol.ErrMediaTooLarge)
	voice("3EB0OK", "/v/ok", 5, oggStream(12))
	h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0OK"})
	if mr := field[protocol.MediaReady](h.expect(protocol.EvMediaReady)); mr.DurationS != 12 {
		t.Fatalf("durationS %d, want 12 (from the file)", mr.DurationS)
	}
	// send_media voice: Ogg Opus only, with a readable duration within the cap.
	h.knownChat(f)
	ids := protocol.NewIDSource(nil)
	sendVoice := func(data []byte, mime string) string {
		s := sealForTest(t, h.mediaDir, data)
		h.advance(2 * time.Second)
		return h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": ids.New(), "kind": "voice", "path": s.path, "key": s.key, "sha256": s.sha, "mime": mime})
	}
	h.expectError(sendVoice([]byte("RIFF....WAVEfmt not ogg at all, no"), "audio/wav"), protocol.ErrMediaInvalid)
	h.expectError(sendVoice([]byte("OggS but too short"), "audio/ogg; codecs=opus"), protocol.ErrMediaInvalid)
	h.expectError(sendVoice(oggStream(16), "audio/ogg; codecs=opus"), protocol.ErrMediaTooLarge)
	sendVoice(oggStream(9), "audio/ogg; codecs=opus")
	h.expect(protocol.EvSendResult)
	if s := f.sent[len(f.sent)-1].msg.GetAudioMessage().GetSeconds(); s != 9 {
		t.Fatalf("seconds %d", s)
	}
}

// L3: mark_read only for reported chats, and in groups only for messages of
// the named sender when the bridge knows them.
func TestL3MarkReadOnlyReportedChats(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.expectError(h.cmd("mark_read", map[string]any{"chatJid": alice, "messageIds": []string{"3EB0K"}}), protocol.ErrUnknownChat)
	h.expectError(h.cmd("mark_read", map[string]any{"chatJid": group, "messageIds": []string{"3EB0C4"}, "senderJid": bob}), protocol.ErrUnknownChat)
	if len(f.marked) != 0 {
		t.Fatal("read receipts sent for an unreported chat")
	}
	f.dispatch(textMsg(mustParse(group), mustParse(bob), "3EB0C4", "hi", false))
	h.expect(protocol.EvMessage)
	h.expectError(h.cmd("mark_read", map[string]any{"chatJid": group, "messageIds": []string{"3EB0C4"}, "senderJid": alice}), protocol.ErrBadRequest)
	id := h.cmd("mark_read", map[string]any{"chatJid": group, "messageIds": []string{"3EB0C4"}, "senderJid": bob})
	if reply(h.expect(protocol.EvOK)) != id {
		t.Fatal("ok")
	}
}

// L4: the hand-off file of a send_media refused before it was read is deleted too.
func TestL4SendMediaFileDeletedOnEarlyRefusal(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.initPairedConnected()
	s := sealForTest(t, h.mediaDir, []byte("%PDF"))
	h.expectError(h.cmd("send_media", map[string]any{"chatJid": bob, "outboxId": "01M3C03V80N87VFZS5G0J0NFR1", "kind": "file", "path": s.path, "key": s.key, "sha256": s.sha, "mime": "application/pdf"}), protocol.ErrUnknownChat)
	waitFor(t, func() bool { return !fileExists(s.path) })
}

// L5: an existing media folder is tightened to owner-only.
func TestL5MediaDirTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the profile ACL (PROTOCOL.md known limits)")
	}
	h := newHarness(t, nil)
	if err := os.MkdirAll(h.mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(h.mediaDir, 0o755)
	h.init(nil)
	fi, _ := os.Stat(h.mediaDir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("media dir mode %o", fi.Mode().Perm())
	}
}

// L1: no 16-character piece of the store key ever reaches stderr, whatever
// path the key takes (a random key with letters, so the digit rule alone
// cannot hide a leak).
func TestL1NoKeyFragmentOnStderr(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFS1"})
	h.expect(protocol.EvSendResult)
	h.b.Stop()
	out := h.stderr.String()
	for i := 0; i+16 <= len(testKey); i++ {
		if strings.Contains(out, testKey[i:i+16]) {
			t.Fatalf("stderr contains key fragment %q", testKey[i:i+16])
		}
	}
}

// Review M1: exit codes are distinct and never 2, the Go runtime's code for
// an unrecovered panic or fatal error.
func TestM1ExitCodes(t *testing.T) {
	seen := map[int]bool{}
	for _, c := range []int{ExitOK, ExitCrash, ExitStoreKey, ExitStoreLocked, ExitClientOutdated, ExitNoInit, ExitLoggedOut} {
		if c == 2 || seen[c] {
			t.Fatalf("exit code %d reused", c)
		}
		seen[c] = true
	}
	if ExitNoInit != 6 || ExitLoggedOut != 7 {
		t.Fatal("PROTOCOL.md §9 codes changed")
	}
}
