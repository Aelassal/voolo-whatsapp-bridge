// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

// Regression tests for the P1-A security re-review
// (docs/reviews/P1-A-security-rereview.md). Each test names the finding it guards.

import (
	"errors"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

// quiet asserts that no line at all reaches stdout within d.
func (h *harness) quiet(d time.Duration, after string) {
	h.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-h.lines:
			h.t.Fatalf("line after %s: %s %s", after, e.Type, e.Payload)
		case <-deadline:
			return
		}
	}
}

// R-M1: a history worker that outlives Stop's 1.5 s wait (one parse takes 3 s)
// writes nothing after status {stopped}: not a history_batch, not a reaction,
// nothing. The reviewer's TestRR_LogoutTimeoutLateOutput (remote logout) and
// TestRR_StopTimeoutThenLateOutput (plain shutdown).
func TestRM1NothingAfterStoppedWhenHistoryOutlivesStop(t *testing.T) {
	for _, remote := range []bool{true, false} {
		name := "shutdown"
		if remote {
			name = "remote_logout"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, pairedFake)
			f := h.initPairedConnected()
			reached, hookDone := make(chan struct{}), make(chan struct{})
			f.mu.Lock()
			f.parseHook = func(n int) {
				if n != 50 {
					return
				}
				close(reached)
				if remote {
					f.dispatch(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
				}
				time.Sleep(3 * time.Second) // past Stop's 1.5 s wait
				close(hookDone)
			}
			f.mu.Unlock()
			h.pushHistory(f, 3, 2000)
			<-reached
			if remote {
				h.expectExit(ExitLoggedOut)
			}
			h.b.Stop()
			if remote {
				h.expect(protocol.EvLoggedOut)
			}
			h.expectState(protocol.StateStopped)
			if !strings.Contains(h.stderr.String(), `"stop_timeout"`) {
				t.Fatal("Stop did not time out: the test did not reproduce the case")
			}
			<-hookDone
			h.quiet(700*time.Millisecond, "status {stopped}")
			f.mu.Lock()
			parses := f.parses
			f.mu.Unlock()
			// The worker stops parsing once the session is cancelled.
			if parses > 50+256 {
				t.Fatalf("history worker parsed %d messages after the session was cancelled", parses)
			}
			if n := h.b.window.Outstanding(); n != 0 {
				t.Fatalf("history window handed out %d seqs after Stop", n)
			}
		})
	}
}

// R-M1 (defence in depth): once Stop has begun, any other line that would
// reach stdout is dropped and counted on stderr, content-free.
func TestRM1EmitAfterStopIsDropped(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.initPairedConnected()
	h.b.Stop()
	h.expectState(protocol.StateStopped)
	h.b.HostBlocked() // a transport callback that fires late
	h.b.Reject("01M3C03V80N87VFZS5G0J0NFT1", protocol.ErrUnsupportedVersion)
	h.quiet(200*time.Millisecond, "Stop")
	if !strings.Contains(h.stderr.String(), `"emit_dropped"`) {
		t.Fatalf("dropped lines not counted:\n%s", h.stderr.String())
	}
}

// R-L6 (R-mut-H1a): Stop closes the store only after the bridge's goroutines
// have finished; a held history worker still sees an open store after Stop
// has cancelled and disconnected.
func TestRL6StoreStaysOpenWhileWorkerRuns(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	st := h.b.current().st
	reached := make(chan struct{})
	var closedInHook atomic.Bool
	var checked atomic.Bool
	f.mu.Lock()
	f.parseHook = func(n int) {
		if n != 50 {
			return
		}
		close(reached)
		waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.disconnects > 0 })
		time.Sleep(100 * time.Millisecond)
		closedInHook.Store(st.Closed())
		checked.Store(true)
	}
	f.mu.Unlock()
	h.pushHistory(f, 1, 100)
	<-reached
	h.b.Stop()
	if !checked.Load() {
		t.Fatal("Stop returned before the held worker finished")
	}
	if closedInHook.Load() {
		t.Fatal("the store was closed while a bridge goroutine was still running")
	}
	if !st.Closed() {
		t.Fatal("store not closed by Stop")
	}
}

// R-L6 (R-mut-H1c): a panic in one history notification drops only that
// notification; the worker goes on with the next one.
func TestRL6HistoryWorkerSurvivesAPanic(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.mu.Lock()
	f.parseHook = func(n int) {
		if n == 1 {
			panic("history panic")
		}
	}
	f.mu.Unlock()
	h.pushHistory(f, 1, 3)
	waitFor(t, func() bool { return strings.Contains(h.stderr.String(), "panic_recovered") })
	h.pushHistory(f, 1, 3)
	if hb := field[protocol.HistoryBatch](h.expect(protocol.EvHistoryBatch)); len(hb.Messages) != 3 {
		t.Fatalf("second notification: %d messages", len(hb.Messages))
	}
}

// R-L6 (R-mut-H1d): a panic inside beginSend, after the send slot was taken,
// frees the slot: the next send is not refused as busy.
func TestRL6PanicInBeginSendFreesSlot(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	var armed atomic.Bool
	hook := func() {
		// A panic inside beginSend, after the slot was taken (standing in
		// for a panic in OutboxKnown or ReserveOutbox).
		if armed.Load() && strings.Contains(string(debug.Stack()), "beginSend") && armed.CompareAndSwap(true, false) {
			panic("store hook panic")
		}
	}
	h.nowHook.Store(&hook)
	armed.Store(true)
	h.expectError(h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFX1"}), protocol.ErrInternal)
	h.advance(2 * time.Second)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "y", "outboxId": "01M3C03V80N87VFZS5G0J0NFX2"})
	if e := h.next(); e.Type != protocol.EvSendResult {
		t.Fatalf("second send: %s %s", e.Type, e.Payload)
	}
}

// R-L6 (R-mut-H1f): after Stop has begun, async starts nothing.
func TestRL6AsyncAfterStopRunsNothing(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.initPairedConnected()
	h.b.Stop()
	var ran atomic.Bool
	h.b.async(func() { ran.Store(true) })
	h.b.asyncCmd("01M3C03V80N87VFZS5G0J0NFV1", func() { ran.Store(true) })
	time.Sleep(100 * time.Millisecond)
	if ran.Load() {
		t.Fatal("a goroutine started after Stop")
	}
}

// R-L6 (R-mut-L10b): a voice note must be declared as Ogg; an Ogg body with
// another mime type is refused.
func TestRL6VoiceMimeChecked(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	s := sealForTest(t, h.mediaDir, oggStream(5))
	h.expectError(h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFW1", "kind": "voice", "path": s.path, "key": s.key, "sha256": s.sha, "mime": "audio/wav"}), protocol.ErrMediaInvalid)
	if f.sendCalls != 0 {
		t.Fatal("sent")
	}
}

// R-L4: stamps from a clock that has since gone back never block sends for
// longer than the window: neither after a restart with the clock set back
// (the reviewer's TestRR_BackstopClockBack) nor within one run.
func TestRL4BackstopClockGoesBack(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	ids := protocol.NewIDSource(nil)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "1", "outboxId": ids.New()})
	h.expect(protocol.EvSendResult)
	h.b.Stop()

	h2 := newHarness(t, pairedFake)
	h2.storeDir, h2.mediaDir = h.storeDir, h.mediaDir
	h2.offset.Store(int64(-time.Hour)) // the clock was set back one hour
	f2 := h2.initPairedConnected()
	h2.knownChat(f2)
	h2.advance(30 * time.Minute)
	h2.cmd("send_text", map[string]any{"chatJid": alice, "text": "2", "outboxId": ids.New()})
	if e := h2.next(); e.Type != protocol.EvSendResult {
		t.Fatalf("after restart with the clock back: %s %s", e.Type, e.Payload)
	}
	// Within one run: the clock goes back an hour right after a send.
	h2.advance(-time.Hour)
	h2.advance(2 * time.Second)
	h2.cmd("send_text", map[string]any{"chatJid": alice, "text": "3", "outboxId": ids.New()})
	if e := h2.next(); e.Type != protocol.EvSendResult {
		t.Fatalf("after the clock went back: %s %s", e.Type, e.Payload)
	}
	// The floor still holds right after that send.
	h2.expectError(h2.cmd("send_text", map[string]any{"chatJid": alice, "text": "4", "outboxId": ids.New()}), protocol.ErrRateLimitedLocal)
}

// R-L7: a logout that WhatsApp confirms while a shutdown is starting still
// deletes the store and reports logged_out; the old key is never left on a
// store whose device is unlinked.
func TestRL7LogoutRacingShutdownStillWipes(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	gate := make(chan struct{})
	f.mu.Lock()
	f.logoutGate = gate
	f.mu.Unlock()
	id := h.cmd("logout", map[string]any{})
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.logouts == 1 })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); h.b.Stop() }()
	waitFor(t, func() bool { h.b.mu.Lock(); defer h.b.mu.Unlock(); return h.b.stopped })
	close(gate)
	wg.Wait()
	if lo := field[protocol.LoggedOut](h.expect(protocol.EvLoggedOut)); lo.Reason != protocol.LogoutUser {
		t.Fatalf("logged_out %+v", lo)
	}
	h.expectState(protocol.StateStopped)
	if reply(h.expect(protocol.EvOK)) != id {
		t.Fatal("ok replyTo")
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if fileExists(filepath.Join(h.storeDir, store.DBFile+suffix)) {
			t.Fatalf("%s%s kept after a confirmed logout", store.DBFile, suffix)
		}
	}
	h.expectExit(ExitLoggedOut)
	if !h.b.LoggedOut() {
		t.Fatal("LoggedOut() false: the caller would exit 0 instead of 7")
	}
}

// R-L2: for a received voice note, a file that reads much shorter than the
// declared length does not lower it: the longer of the two is reported and
// capped. Within a small margin the file's own duration wins.
func TestRL2ReceivedVoiceKeepsTheLongerDuration(t *testing.T) {
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
	voice("3EB0SHORT", "/v/short", 12, oggStream(3)) // declares 12 s, the file reads 3 s
	h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0SHORT"})
	if mr := field[protocol.MediaReady](h.expect(protocol.EvMediaReady)); mr.DurationS != 12 {
		t.Fatalf("durationS %d, want the declared 12", mr.DurationS)
	}
	voice("3EB0NEAR", "/v/near", 4, oggStream(3))
	h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0NEAR"})
	if mr := field[protocol.MediaReady](h.expect(protocol.EvMediaReady)); mr.DurationS != 3 {
		t.Fatalf("durationS %d, want the file's 3", mr.DurationS)
	}
}

// R-L4: a full window of stamps from a clock that has since gone back blocks
// sends for at most one window from the restart, not from the first send.
func TestRL4FullWindowClockBackExpiresFromRestart(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	ids := protocol.NewIDSource(nil)
	for i := 0; i < MaxSendsInWindow; i++ {
		h.advance(time.Second)
		h.cmd("send_text", map[string]any{"chatJid": alice, "text": "n", "outboxId": ids.New()})
		h.expect(protocol.EvSendResult)
	}
	h.b.Stop()
	h2 := newHarness(t, pairedFake)
	h2.storeDir, h2.mediaDir = h.storeDir, h.mediaDir
	h2.offset.Store(h.offset.Load() - int64(time.Hour))
	f2 := h2.initPairedConnected()
	h2.knownChat(f2)
	h2.advance(SendWindow + time.Second)
	h2.cmd("send_text", map[string]any{"chatJid": alice, "text": "late", "outboxId": ids.New()})
	if e := h2.next(); e.Type != protocol.EvSendResult {
		t.Fatalf("one window after the restart: %s %s", e.Type, e.Payload)
	}
}

// Lead decision (2026-09-25): a send refused before it is handed to WhatsApp
// (here media_invalid after the outbox id was reserved, and a failed upload)
// does not count toward the backstop, so the next send need not wait.
func TestBackstopCountsOnlyDispatchedSends(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	ids := protocol.NewIDSource(nil)
	s := sealForTest(t, h.mediaDir, []byte("%PDF"))
	h.expectError(h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": ids.New(), "kind": "file", "path": s.path, "key": s.key, "sha256": strings.Repeat("0", 64), "mime": "application/pdf"}), protocol.ErrMediaInvalid)
	f.mu.Lock()
	f.uploadErr = errors.New("upload refused")
	f.mu.Unlock()
	h.expectError(h.sendKind("media", ids.New()), protocol.ErrTimeout)
	f.mu.Lock()
	f.uploadErr = nil
	f.mu.Unlock()
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "now", "outboxId": ids.New()})
	if e := h.next(); e.Type != protocol.EvSendResult {
		t.Fatalf("send after two undispatched refusals: %s %s", e.Type, e.Payload)
	}
	// A dispatched send does count: the floor applies right after it.
	h.expectError(h.cmd("send_text", map[string]any{"chatJid": alice, "text": "again", "outboxId": ids.New()}), protocol.ErrRateLimitedLocal)
}

// Lead decision (2026-09-25), PROTOCOL.md §1: the bridge never writes invalid
// UTF-8 or a lone surrogate, whatever WhatsApp delivers; invalid bytes become
// U+FFFD.
func TestEventsAreValidUTF8(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0UTF8", "ok \xff\xfe \xed\xa0\x80 \xed\xb0\x80 end", false))
	e := h.expect(protocol.EvMessage)
	raw := h.raw.String()
	if !utf8.ValidString(raw) {
		t.Fatal("stdout carried invalid UTF-8")
	}
	for _, esc := range []string{`\ud8`, `\ud9`, `\uda`, `\udb`, `\udc`, `\udd`, `\ude`, `\udf`} {
		if strings.Contains(strings.ToLower(raw), esc) {
			t.Fatalf("stdout carried a surrogate escape %s", esc)
		}
	}
	if m := field[protocol.MessageEvent](e).Message; !strings.Contains(m.Text, "�") || !strings.HasPrefix(m.Text, "ok ") {
		t.Fatalf("text %q", m.Text)
	}
}
