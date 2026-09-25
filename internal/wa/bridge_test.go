// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/media"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

// ------------------------------------------------------------ handshake

func TestNothingButPingBeforeInit(t *testing.T) {
	h := newHarness(t, nil)
	id := h.cmd("ping", map[string]any{})
	if reply(h.expect(protocol.EvPong)) != id {
		t.Fatal("pong replyTo")
	}
	for _, c := range []struct {
		typ string
		p   any
	}{
		{"pair_qr", map[string]any{}},
		{"send_text", map[string]any{"chatJid": alice, "text": "hi", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX"}},
		{"logout", map[string]any{}},
		{"ack", map[string]any{"seq": 1}},
	} {
		id := h.cmd(c.typ, c.p)
		h.expectError(id, protocol.ErrNotInitialized)
	}
	id = h.cmd("fetch_history", map[string]any{})
	h.expectError(id, protocol.ErrUnknownCommand)
}

func TestInitReadyAndSecondInit(t *testing.T) {
	h := newHarness(t, nil)
	h.init(nil)
	h.expectState(protocol.StateUnpaired)
	id := h.cmd("init", map[string]any{"storeKey": testKey, "storeDir": h.storeDir, "mediaDir": h.mediaDir, "deviceName": "Voolo",
		"limits": map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 16 << 20, "voiceMaxSeconds": 3600}})
	h.expectError(id, protocol.ErrAlreadyInitialized)
	if !fileExists(filepath.Join(h.storeDir, store.DBFile)) || !fileExists(h.mediaDir) {
		t.Fatal("store or media folder missing")
	}
}

func TestInitWrongKeyIsFatalExit3(t *testing.T) {
	h := newHarness(t, nil)
	h.init(nil)
	h.b.Stop()
	h2 := newHarness(t, nil)
	h2.storeDir = h.storeDir
	id := h2.cmd("init", map[string]any{"storeKey": strings.Repeat("ab", 32), "storeDir": h2.storeDir, "mediaDir": h2.mediaDir, "deviceName": "Voolo",
		"limits": map[string]any{"historyDays": 1, "historyMaxPerChat": 1, "imageMaxBytes": 1, "voiceMaxSeconds": 1}})
	e := field[protocol.ErrorEvent](h2.expect(protocol.EvError))
	if e.ReplyTo != id || e.Code != protocol.ErrStoreKeyInvalid || !e.Fatal || e.Retryable {
		t.Fatalf("got %+v", e)
	}
	select {
	case code := <-h2.b.Fatal():
		if code != ExitStoreKey {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("no fatal exit")
	}
	if h2.b.Initialized() {
		t.Fatal("initialized with a wrong key")
	}
}

func TestInitLockedStoreIsFatalExit4(t *testing.T) {
	h := newHarness(t, nil)
	h.init(nil)
	h2 := newHarness(t, nil)
	h2.storeDir = h.storeDir
	h2.init2()
	e := field[protocol.ErrorEvent](h2.expect(protocol.EvError))
	if e.Code != protocol.ErrStoreLocked || !e.Fatal {
		t.Fatalf("got %+v", e)
	}
	if code := <-h2.b.Fatal(); code != ExitStoreLocked {
		t.Fatalf("exit %d", code)
	}
}

func (h *harness) init2() string {
	return h.cmd("init", map[string]any{"storeKey": testKey, "storeDir": h.storeDir, "mediaDir": h.mediaDir, "deviceName": "Voolo",
		"limits": map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 16 << 20, "voiceMaxSeconds": 3600}})
}

func TestStrictCommandsNeverExecute(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	id := h.cmd("send_text", map[string]any{"chatJid": alice, "text": "hi", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX", "recipients": []string{alice, bob}})
	h.expectError(id, protocol.ErrBadRequest)
	id = h.cmd("send_text", map[string]any{"chatJid": alice, "text": "hi", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX", "sendAt": 1790334000000})
	h.expectError(id, protocol.ErrBadRequest)
	id = h.cmd("shutdown", map[string]any{"now": true})
	h.expectError(id, protocol.ErrBadRequest)
	id = h.cmd("pair_phone", map[string]any{"phone": "01001234567"})
	h.expectError(id, protocol.ErrPhoneInvalid)
	time.Sleep(50 * time.Millisecond)
	if f.sendCalls != 0 {
		t.Fatal("a refused command reached WhatsApp")
	}
}

// ------------------------------------------------------------ pairing

func TestPairQRRotatesThenTimesOut(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := newFake()
		f.onConnect = func(f *fakeClient) {
			f.dispatch(&events.QR{Codes: []string{"2@ref0,noise,id,ADVOLD", "2@ref1,noise,id,ADVOLD", "2@ref2,noise,id,ADVOLD"}})
		}
		return f
	})
	h.init(nil)
	id := h.cmd("pair_qr", map[string]any{})
	q0 := field[protocol.QR](h.expect(protocol.EvQR))
	if q0.ReplyTo != id || q0.Index != 0 || q0.Code != "2@ref0,noise,id,ADVOLD" || q0.ExpiresAt != h.now.Add(150*time.Millisecond).UnixMilli() {
		t.Fatalf("first qr %+v", q0)
	}
	// A second pairing while one runs is refused.
	h.expectError(h.cmd("pair_qr", map[string]any{}), protocol.ErrBusy)
	h.fake().dispatch(&events.RotateADVSecret{OldSecret: "ADVOLD", NewSecret: "ADVNEW"})
	q := field[protocol.QR](h.expect(protocol.EvQR))
	if q.Code != "2@ref0,noise,id,ADVNEW" || q.Index != 1 {
		t.Fatalf("rotated qr %+v", q)
	}
	for _, want := range []string{"2@ref1,noise,id,ADVNEW", "2@ref2,noise,id,ADVNEW"} {
		if q := field[protocol.QR](h.expect(protocol.EvQR)); q.Code != want {
			t.Fatalf("qr %q, want %q", q.Code, want)
		}
	}
	pf := field[protocol.PairFailed](h.expect(protocol.EvPairFailed))
	if pf.ReplyTo != id || pf.Reason != protocol.PairTimeout {
		t.Fatalf("pair_failed %+v", pf)
	}
	if h.fake().disconnects == 0 {
		t.Fatal("pre-login socket not closed after timeout")
	}
}

func TestPairSuccessThenConnected(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := newFake()
		f.onConnect = func(f *fakeClient) { f.dispatch(&events.QR{Codes: []string{"2@a", "2@b"}}) }
		return f
	})
	h.init(nil)
	h.cmd("pair_qr", map[string]any{})
	h.expect(protocol.EvQR)
	f := h.fake()
	f.mu.Lock()
	f.account = AccountInfo{JID: mustParse(me)}
	f.mu.Unlock()
	f.dispatch(&events.PairSuccess{ID: mustParse("15550100001:12@s.whatsapp.net"), LID: mustParse("100000000000001:12@lid"), Platform: "android"})
	p := field[protocol.Paired](h.expect(protocol.EvPaired))
	if p.JID != me || p.LID != "100000000000001@lid" {
		t.Fatalf("paired %+v", p)
	}
	h.expectState(protocol.StateConnecting)
	h.none(protocol.EvPairFailed, 400*time.Millisecond) // the QR clock was stopped
	f.dispatch(&events.Connected{})
	h.expectState(protocol.StateConnected)
	h.expectError(h.cmd("pair_qr", map[string]any{}), protocol.ErrAlreadyPaired)
}

func TestPairPhoneGivesCodeAndNoQR(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := newFake()
		f.onConnect = func(f *fakeClient) { f.dispatch(&events.QR{Codes: []string{"2@a", "2@b"}}) }
		return f
	})
	h.init(nil)
	id := h.cmd("pair_phone", map[string]any{"phone": "15550100001"})
	pc := field[protocol.PairCode](h.expect(protocol.EvPairCode))
	if pc.ReplyTo != id || pc.Code != "ABCD2345" {
		t.Fatalf("pair_code %+v", pc)
	}
	f := h.fake()
	if len(f.pairPhones) != 1 || f.pairPhones[0] != "15550100001" || f.pairNames[0] != PhoneLinkDisplayName() {
		t.Fatalf("PairPhone called with %v %v", f.pairPhones, f.pairNames)
	}
	if !strings.HasPrefix(PhoneLinkDisplayName(), "Chrome (") {
		t.Fatal("display name must be Browser (OS)")
	}
	for {
		e := h.next()
		if e.Type == protocol.EvQR {
			t.Fatal("qr sent during phone-code pairing")
		}
		if e.Type == protocol.EvPairFailed {
			if field[protocol.PairFailed](e).Reason != protocol.PairTimeout {
				t.Fatal("reason")
			}
			return
		}
	}
}

// ------------------------------------------------------------ live events

func textMsg(chat, sender types.JID, id, text string, fromMe bool) *events.Message {
	return &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: fromMe, IsGroup: chat.Server == types.GroupServer}, ID: id, Timestamp: time.UnixMilli(1790330340000), PushName: "Bob"},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func TestLiveDMMessage(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := pairedFake()
		f.contacts[alice] = types.ContactInfo{Found: true, FullName: "Alice Example", PushName: "Alice"}
		return f
	})
	f := h.initPairedConnected()
	f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0A1", "صباح الخير — good morning", false))
	ch := field[protocol.ChatEvent](h.expect(protocol.EvChat))
	if ch.Chat.JID != alice || ch.Chat.Kind != "dm" || ch.Chat.Name != "Alice Example" {
		t.Fatalf("chat %+v", ch)
	}
	c := field[protocol.Contact](h.expect(protocol.EvContact))
	if c.JID != alice || c.Name != "Alice Example" {
		t.Fatalf("contact %+v", c)
	}
	m := field[protocol.MessageEvent](h.expect(protocol.EvMessage)).Message
	if m.ChatJID != alice || m.SenderJID != alice || m.Text != "صباح الخير — good morning" || m.Kind != "text" || m.FromMe || m.TS != 1790330340000 {
		t.Fatalf("message %+v", m)
	}
	// A second message in the same chat does not repeat chat or contact.
	f.dispatch(textMsg(mustParse(alice), mustParse(me), "3EB0A2", "ok", true))
	e := h.next()
	for e.Type == protocol.EvStatus {
		e = h.next()
	}
	if e.Type != protocol.EvMessage || field[protocol.MessageEvent](e).Message.SenderJID != me {
		t.Fatalf("got %s %s", e.Type, e.Payload)
	}
}

func TestBroadcastStatusAndNewsletterNeverReported(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	for _, chat := range []string{"status@broadcast", "1234@broadcast", "120363000000000009@newsletter"} {
		f.dispatch(textMsg(mustParse(chat), mustParse(alice), "3EB0B1", "x", false))
	}
	h.none(protocol.EvMessage, 200*time.Millisecond)
}

func TestGroupMessageReportsGroupAndSenderContact(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := pairedFake()
		f.contacts[bob] = types.ContactInfo{Found: true, PushName: "Bob"}
		f.groups = []*types.GroupInfo{{JID: mustParse(group), GroupName: types.GroupName{Name: "فريق المشروع"}, Participants: []types.GroupParticipant{
			{JID: mustParse(me), IsAdmin: true}, {JID: mustParse("100000000000003@lid"), PhoneNumber: mustParse(bob)},
		}}}
		return f
	})
	f := h.initPairedConnected()
	time.Sleep(50 * time.Millisecond) // joined groups load
	msg := textMsg(mustParse(group), mustParse(bob), "3EB0C4", "Meeting بكرة", false)
	msg.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("Meeting بكرة"),
		ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{me}, StanzaID: proto.String("3EB0Q"), Participant: proto.String(alice)}}}
	f.dispatch(msg)
	ch := field[protocol.ChatEvent](h.expect(protocol.EvChat))
	if ch.Chat.Kind != "group" || ch.Chat.Name != "فريق المشروع" {
		t.Fatalf("chat %+v", ch)
	}
	g := field[protocol.Group](h.expect(protocol.EvGroup))
	if g.Name != "فريق المشروع" || len(g.Participants) != 2 || g.Participants[1].JID != bob || !g.Participants[0].IsAdmin {
		t.Fatalf("group %+v", g)
	}
	m := field[protocol.MessageEvent](h.expect(protocol.EvMessage)).Message
	if m.SenderJID != bob || len(m.Mentions) != 1 || m.Mentions[0] != me || m.Quoted == nil || m.Quoted.ID != "3EB0Q" || m.Quoted.SenderJID != alice || m.PushName != "Bob" {
		t.Fatalf("message %+v", m)
	}
	if c := field[protocol.Contact](h.expect(protocol.EvContact)); c.JID != bob {
		t.Fatalf("contact %+v", c)
	}
}

func TestLIDChatAliasedOnce(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.dispatch(textMsg(mustParse(aliceLID), mustParse(aliceLID), "3EB0L1", "hi", false))
	if ch := field[protocol.ChatEvent](h.expect(protocol.EvChat)); ch.Chat.JID != aliceLID {
		t.Fatalf("chat %+v", ch)
	}
	h.expect(protocol.EvMessage)
	f.mu.Lock()
	f.pnForLID[aliceLID] = mustParse(alice)
	f.mu.Unlock()
	f.dispatch(textMsg(mustParse(aliceLID), mustParse(aliceLID), "3EB0L2", "again", false))
	u := field[protocol.ChatUpdate](h.expect(protocol.EvChatUpdate))
	if u.JID != alice || u.AliasOf != aliceLID {
		t.Fatalf("chat_update %+v", u)
	}
	m := field[protocol.MessageEvent](h.expect(protocol.EvMessage)).Message
	if m.ChatJID != alice || m.SenderJID != alice {
		t.Fatalf("message after alias %+v", m)
	}
	f.dispatch(textMsg(mustParse(aliceLID), mustParse(aliceLID), "3EB0L3", "third", false))
	h.none(protocol.EvChatUpdate, 150*time.Millisecond)
}

func TestEditRevokeReactionReceipts(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	chat := mustParse(alice)
	base := types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "3EB0P", Timestamp: time.UnixMilli(1790330350000)}
	f.dispatch(&events.Message{Info: base, Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &msgKey{ID: proto.String("3EB0ORIG")},
		EditedMessage: &waE2E.Message{Conversation: proto.String("edited نص")}, TimestampMS: proto.Int64(1790330351000)}}})
	u := field[protocol.MessageUpdate](h.expect(protocol.EvMsgUpdate))
	if u.Kind != "edit" || u.MessageID != "3EB0ORIG" || u.Text != "edited نص" || u.At != 1790330351000 || u.By != alice {
		t.Fatalf("edit %+v", u)
	}
	f.dispatch(&events.Message{Info: base, Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &msgKey{ID: proto.String("3EB0ORIG")}}}})
	if u := field[protocol.MessageUpdate](h.expect(protocol.EvMsgUpdate)); u.Kind != "revoke" || u.By != alice {
		t.Fatalf("revoke %+v", u)
	}
	f.dispatch(&events.Message{Info: base, Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &msgKey{ID: proto.String("3EB0ORIG")}, Text: proto.String("👍"), SenderTimestampMS: proto.Int64(1790330360000)}}})
	if r := field[protocol.Reaction](h.expect(protocol.EvReaction)); r.Emoji != "👍" || r.SenderJID != alice || r.MessageID != "3EB0ORIG" {
		t.Fatalf("reaction %+v", r)
	}
	for _, c := range []struct {
		t      types.ReceiptType
		fromMe bool
		want   string
	}{{types.ReceiptTypeDelivered, false, "delivered"}, {types.ReceiptTypeRead, false, "read"}, {types.ReceiptTypePlayed, false, "played"}, {types.ReceiptTypeReadSelf, true, "read_self"}} {
		f.dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat, IsFromMe: c.fromMe}, MessageIDs: []string{"3EB0X"}, Type: c.t, Timestamp: time.UnixMilli(1790330375000)})
		r := field[protocol.Receipt](h.expect(protocol.EvReceipt))
		if r.Kind != c.want || r.ChatJID != alice || r.MessageIDs[0] != "3EB0X" {
			t.Fatalf("receipt %+v", r)
		}
	}
	f.dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, MessageIDs: []string{"3EB0X"}, Type: types.ReceiptTypeRetry})
	h.none(protocol.EvReceipt, 150*time.Millisecond)
}

func TestChatStateUpdates(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.dispatch(&events.Pin{JID: mustParse(alice), Action: &waSyncAction.PinAction{Pinned: proto.Bool(true)}})
	if u := field[protocol.ChatUpdate](h.expect(protocol.EvChatUpdate)); u.Pinned == nil || !*u.Pinned {
		t.Fatalf("pin %+v", u)
	}
	f.dispatch(&events.MarkChatAsRead{JID: mustParse(alice), Action: &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(true)}})
	if u := field[protocol.ChatUpdate](h.expect(protocol.EvChatUpdate)); u.MarkedRead == nil || !*u.MarkedRead || u.UnreadCount == nil || *u.UnreadCount != 0 {
		t.Fatalf("read %+v", u)
	}
	f.dispatch(&events.Mute{JID: mustParse(alice), Action: &waSyncAction.MuteAction{Muted: proto.Bool(true), MuteEndTimestamp: proto.Int64(-1)}})
	if u := field[protocol.ChatUpdate](h.expect(protocol.EvChatUpdate)); u.MutedUntil == nil || *u.MutedUntil != -1 {
		t.Fatalf("mute %+v", u)
	}
	f.dispatch(&events.Mute{JID: mustParse(alice), Action: &waSyncAction.MuteAction{Muted: proto.Bool(false)}})
	if u := field[protocol.ChatUpdate](h.expect(protocol.EvChatUpdate)); u.MutedUntil == nil || *u.MutedUntil != 0 {
		t.Fatalf("unmute %+v", u)
	}
}

// ------------------------------------------------------------ signals (S3)

func TestSignalsAndLogout(t *testing.T) {
	cases := []struct {
		evt  any
		kind string
		code any
	}{
		{&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour}, "temp_banned", float64(101)},
		{&events.StreamReplaced{}, "stream_replaced", nil},
		{&events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable}, "connect_failure", float64(503)},
		{&events.ConnectFailure{Reason: events.ConnectFailureInternalServerError}, "connect_failure", float64(500)},
		{&events.StreamError{Code: "999"}, "stream_error", "999"},
		{&events.KeepAliveTimeout{ErrorCount: 3}, "keepalive_timeout", nil},
	}
	for _, c := range cases {
		h := newHarness(t, pairedFake)
		f := h.initPairedConnected()
		f.dispatch(c.evt)
		s := field[map[string]any](h.expect(protocol.EvSignal))
		if s["kind"] != c.kind || s["code"] != c.code {
			t.Fatalf("%T: %v", c.evt, s)
		}
		if c.kind == "temp_banned" && s["expiresInMs"] != float64(3600000) {
			t.Fatalf("expires %v", s)
		}
		h.b.Stop()
	}
}

func TestLoggedOutReasonsWipeStore(t *testing.T) {
	for reason, want := range map[events.ConnectFailureReason]string{
		events.ConnectFailureLoggedOut: "device_removed", events.ConnectFailureMainDeviceGone: "primary_gone",
		events.ConnectFailureUnknownLogout: "banned", events.ConnectFailureReason(400): "unknown",
	} {
		h := newHarness(t, pairedFake)
		f := h.initPairedConnected()
		f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0Z", "x", false))
		h.expect(protocol.EvMessage)
		f.dispatch(&events.LoggedOut{Reason: reason})
		lo := field[protocol.LoggedOut](h.expect(protocol.EvLoggedOut))
		if lo.Reason != want || lo.Code != int(reason) {
			t.Fatalf("%d: %+v", reason, lo)
		}
		h.expectState(protocol.StateStopped)
		h.expectState(protocol.StateUnpaired)
		// The store was wiped and a fresh one opened: the reported chat is gone.
		chats, err := h.b.current().st.Chats(context.Background())
		if err != nil || len(chats) != 0 {
			t.Fatalf("store not wiped: %v %v", chats, err)
		}
		h.b.Stop()
	}
}

func TestLogoutCommandUnlinksAndDeletesStore(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0Z", "x", false))
	h.expect(protocol.EvMessage)
	old := h.b.current().st
	id := h.cmd("logout", map[string]any{})
	lo := field[protocol.LoggedOut](h.expect(protocol.EvLoggedOut))
	if lo.Reason != "user" {
		t.Fatalf("%+v", lo)
	}
	if reply(h.expect(protocol.EvOK)) != id {
		t.Fatal("ok replyTo")
	}
	if f.logouts != 1 {
		t.Fatal("whatsmeow Logout not called")
	}
	if old.DB != nil {
		t.Fatal("old store still open")
	}
	chats, _ := h.b.current().st.Chats(context.Background())
	if len(chats) != 0 {
		t.Fatal("store not deleted")
	}
	h.expectError(h.cmd("logout", map[string]any{}), protocol.ErrNotPaired)
}

func TestClientOutdatedRefreshOnceThenFatal(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.dispatch(&events.ClientOutdated{})
	if s := field[protocol.Signal](h.expect(protocol.EvSignal)); s.Kind != "client_outdated" {
		t.Fatal(s)
	}
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	refreshed := h.refresh
	h.mu.Unlock()
	f.mu.Lock()
	connects := f.connects
	f.mu.Unlock()
	if refreshed != 1 || connects < 2 {
		t.Fatalf("refresh %d connects %d", refreshed, connects)
	}
	f.dispatch(&events.ClientOutdated{})
	h.expect(protocol.EvSignal)
	e := field[protocol.ErrorEvent](h.expect(protocol.EvError))
	if e.Code != "client_outdated" || !e.Fatal {
		t.Fatalf("%+v", e)
	}
	if code := <-h.b.Fatal(); code != ExitClientOutdated {
		t.Fatalf("exit %d", code)
	}
}

// ------------------------------------------------------------ sending (S2)

func (h *harness) knownChat(f *fakeClient) {
	f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0K", "hello", false))
	h.expect(protocol.EvMessage)
}

func TestSendTextHappyPath(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	id := h.cmd("send_text", map[string]any{"chatJid": alice, "text": "تمام — ok", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX", "quotedMessageId": "3EB0K"})
	r := field[protocol.SendResult](h.expect(protocol.EvSendResult))
	if r.ReplyTo != id || r.OutboxID != "01M3C03V80N87VFZS5G0J0NFEX" || r.MessageID != "3EB0SENT00000001" || r.At != 1790330400900 {
		t.Fatalf("%+v", r)
	}
	if len(f.sent) != 1 || f.sent[0].to.String() != alice {
		t.Fatalf("sent %+v", f.sent)
	}
	et := f.sent[0].msg.GetExtendedTextMessage()
	if et.GetText() != "تمام — ok" || et.GetContextInfo().GetStanzaID() != "3EB0K" || et.GetContextInfo().GetParticipant() != alice {
		t.Fatalf("message %v", f.sent[0].msg)
	}
}

func TestSendRefusals(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.init(nil)
	f := h.fake()
	h.expectState(protocol.StateConnecting)
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		n := f.connects
		f.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
	st := func(outbox string) string {
		return h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": outbox})
	}
	h.expectError(st("01M3C03V80N87VFZS5G0J0NFE1"), protocol.ErrNotConnected)
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	h.expectError(st("01M3C03V80N87VFZS5G0J0NFE2"), protocol.ErrUnknownChat)
	h.knownChat(f)
	st("01M3C03V80N87VFZS5G0J0NFE3")
	h.expect(protocol.EvSendResult)
	h.expectError(st("01M3C03V80N87VFZS5G0J0NFE3"), protocol.ErrDuplicateOutboxID)
	if f.sendCalls != 1 {
		t.Fatalf("SendMessage called %d times", f.sendCalls)
	}
}

func TestSendSingleFlight(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	gate := make(chan struct{})
	f.mu.Lock()
	f.sendGate = gate
	f.mu.Unlock()
	first := h.cmd("send_text", map[string]any{"chatJid": alice, "text": "one", "outboxId": "01M3C03V80N87VFZS5G0J0NFE4"})
	time.Sleep(50 * time.Millisecond)
	second := h.cmd("send_text", map[string]any{"chatJid": alice, "text": "two", "outboxId": "01M3C03V80N87VFZS5G0J0NFE5"})
	h.expectError(second, protocol.ErrBusy)
	close(gate)
	if r := field[protocol.SendResult](h.expect(protocol.EvSendResult)); r.ReplyTo != first {
		t.Fatal("first send result")
	}
	// The refused one was not reserved: it can be sent now.
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "two", "outboxId": "01M3C03V80N87VFZS5G0J0NFE5"})
	h.expect(protocol.EvSendResult)
}

func TestSendNeverRetriedAndRateLimitSignalled(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	f.mu.Lock()
	f.sendErr = serverErr{}
	f.mu.Unlock()
	id := h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFE6"})
	if s := field[protocol.Signal](h.expect(protocol.EvSignal)); s.Kind != "rate_limited" {
		t.Fatal(s)
	}
	h.expectError(id, protocol.ErrRateLimited)
	time.Sleep(100 * time.Millisecond)
	if f.sendCalls != 1 {
		t.Fatalf("send retried: %d calls", f.sendCalls)
	}
	// The same outbox id stays refused: a retry is a new user action with a new id.
	h.expectError(h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFE6"}), protocol.ErrDuplicateOutboxID)
}

type serverErr struct{}

func (serverErr) Error() string        { return whatsmeow.ErrServerReturnedError.Error() + " 429" }
func (serverErr) Is(target error) bool { return target == whatsmeow.ErrServerReturnedError }

type msgKey = waCommon.MessageKey

type sealed struct{ path, key, sha string }

func sealForTest(t *testing.T, dir string, data []byte) sealed {
	t.Helper()
	s, err := media.Seal(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	return sealed{s.Path, s.Key, s.SHA256}
}

func TestDuplicateOutboxRefusedAfterRestart(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFE7"})
	h.expect(protocol.EvSendResult)
	h.b.Stop()
	h2 := newHarness(t, pairedFake)
	h2.storeDir, h2.mediaDir = h.storeDir, h.mediaDir
	f2 := h2.initPairedConnected()
	h2.expectError(h2.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFE7"}), protocol.ErrDuplicateOutboxID)
	if f2.sendCalls != 0 {
		t.Fatal("repeat was sent after restart")
	}
}

func TestSendMedia(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	sealed := sealForTest(t, h.mediaDir, []byte("OggS fake voice"))
	id := h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFE8", "kind": "voice", "path": sealed.path,
		"key": sealed.key, "sha256": sealed.sha, "mime": "audio/ogg; codecs=opus"})
	if r := field[protocol.SendResult](h.expect(protocol.EvSendResult)); r.ReplyTo != id {
		t.Fatal("send_result")
	}
	if !f.sent[0].msg.GetAudioMessage().GetPTT() || fileExists(sealed.path) {
		t.Fatal("voice note not sent as PTT, or hand-off file kept")
	}
	bad := sealForTest(t, h.mediaDir, []byte("x"))
	id = h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFE9", "kind": "image", "path": bad.path,
		"key": strings.Repeat("00", 32), "sha256": bad.sha, "mime": "image/jpeg"})
	h.expectError(id, protocol.ErrMediaInvalid)
}

// ------------------------------------------------------------ media (S5)

func TestFetchMedia(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	voice := []byte("VOICE BYTES بصوت")
	f.downloads["/v/voice"] = voice
	mk := func(id string, secs uint32, size uint64, ptt bool) *events.Message {
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(alice), Sender: mustParse(alice)}, ID: id, Timestamp: h.now},
			Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(ptt), Seconds: proto.Uint32(secs), FileLength: proto.Uint64(size),
				Mimetype: proto.String("audio/ogg; codecs=opus"), DirectPath: proto.String("/v/voice"), MediaKey: []byte{1, 2}}}}
	}
	f.dispatch(mk("3EB0V1", 14, uint64(len(voice)), true))
	m := field[protocol.MessageEvent](h.expect(protocol.EvMessage)).Message
	if m.Kind != "voice" || m.Media == nil || m.Media.DurationS != 14 {
		t.Fatalf("voice message %+v", m)
	}
	if f.dlCalls != 0 {
		t.Fatal("media downloaded without fetch_media")
	}
	id := h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0V1"})
	mr := field[protocol.MediaReady](h.expect(protocol.EvMediaReady))
	if mr.ReplyTo != id || mr.SizeBytes != int64(len(voice)) || mr.DurationS != 14 || filepath.Dir(mr.Path) != h.mediaDir {
		t.Fatalf("media_ready %+v", mr)
	}
	raw, _ := os.ReadFile(mr.Path)
	if strings.Contains(string(raw), "VOICE") {
		t.Fatal("plaintext on disk")
	}
	k, _ := hex.DecodeString(mr.Key)
	block, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCM(block)
	if got, err := gcm.Open(nil, raw[:12], raw[12:], nil); err != nil || string(got) != string(voice) {
		t.Fatal("decrypt")
	}
	// Caps are checked before any download.
	f.dispatch(mk("3EB0V2", 3601, 100, true))
	h.expect(protocol.EvMessage)
	f.dispatch(mk("3EB0V3", 60, 32<<20+1, true))
	h.expect(protocol.EvMessage)
	f.dispatch(mk("3EB0A4", 60, 100, false)) // plain audio: not fetchable in v1
	h.expect(protocol.EvMessage)
	calls := f.dlCalls
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0V2"}), protocol.ErrMediaTooLarge)
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0V3"}), protocol.ErrMediaTooLarge)
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0A4"}), protocol.ErrUnknownMessage)
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0NOPE"}), protocol.ErrUnknownMessage)
	if f.dlCalls != calls {
		t.Fatal("download attempted for a refused fetch")
	}
	f.mu.Lock()
	f.dlErr = whatsmeow.ErrMediaDownloadFailedWith410
	f.mu.Unlock()
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0V1"}), protocol.ErrMediaExpired)
}

func TestMarkRead(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	id := h.cmd("mark_read", map[string]any{"chatJid": group, "messageIds": []string{"3EB0C4"}, "senderJid": bob})
	if reply(h.expect(protocol.EvOK)) != id {
		t.Fatal("ok")
	}
	if len(f.marked) != 1 || f.marked[0].chat.String() != group || f.marked[0].sender.String() != bob || f.marked[0].ids[0] != "3EB0C4" {
		t.Fatalf("%+v", f.marked)
	}
}

// ------------------------------------------------------------ history (S5, §7)

func histMsg(chat string, id string, ts time.Time, text string) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:              &msgKey{RemoteJID: proto.String(chat), ID: proto.String(id), FromMe: proto.Bool(false)},
		MessageTimestamp: proto.Uint64(uint64(ts.Unix())), Message: &waE2E.Message{Conversation: proto.String(text)},
	}}
}

func TestHistoryWindowCapsAndLiveEventsNotBlocked(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	now := h.now
	var convs []*waHistorySync.Conversation
	for c := 0; c < 6; c++ {
		chat := "1555020000" + string(rune('0'+c)) + "@s.whatsapp.net"
		conv := &waHistorySync.Conversation{ID: proto.String(chat), UnreadCount: proto.Uint32(1)}
		for i := 0; i < 250; i++ {
			conv.Messages = append(conv.Messages, histMsg(chat, "3EB0H"+string(rune('A'+c))+strings.Repeat("0", 3)+itoa(i), now.Add(-time.Duration(i)*time.Hour), "رسالة "+itoa(i)))
		}
		// Too old: dropped (S5 90 days).
		conv.Messages = append(conv.Messages, histMsg(chat, "3EB0OLD"+itoa(c), now.Add(-91*24*time.Hour), "old"))
		convs = append(convs, conv)
	}
	notif := &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_INITIAL_BOOTSTRAP.Enum()}
	f.history[notif] = &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(), Conversations: convs, Progress: proto.Uint32(100)}
	f.dispatch(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(me), Sender: mustParse(me), IsFromMe: true}, ID: "3EB0N"},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(), HistorySyncNotification: notif}}})
	var seqs []int64
	total := 0
	for len(seqs) < 4 {
		b := field[protocol.HistoryBatch](h.expect(protocol.EvHistoryBatch))
		seqs = append(seqs, b.Seq)
		total += len(b.Messages)
		if len(b.Messages) > 200 || b.SyncType != "initial" || b.Chat.UnreadCount == nil {
			t.Fatalf("batch %d: %d messages %+v", b.Seq, len(b.Messages), b.Chat)
		}
		for _, m := range b.Messages {
			if m.Text == "old" {
				t.Fatal("message older than 90 days in history")
			}
		}
	}
	if !reflect.DeepEqual(seqs, []int64{1, 2, 3, 4}) {
		t.Fatalf("seqs %v", seqs)
	}
	// Window full: no fifth batch, but a live message still goes through.
	f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0LIVE", "live while syncing", false))
	for {
		e := h.next()
		if e.Type == protocol.EvHistoryBatch {
			t.Fatal("fifth batch without an ack")
		}
		if e.Type == protocol.EvMessage {
			break
		}
	}
	h.none(protocol.EvHistoryBatch, 200*time.Millisecond)
	h.cmd("ack", map[string]any{"seq": 4})
	for {
		e := h.next()
		if e.Type == protocol.EvHistoryBatch {
			b := field[protocol.HistoryBatch](e)
			total += len(b.Messages)
			h.cmd("ack", map[string]any{"seq": b.Seq})
			continue
		}
		if e.Type == protocol.EvSyncProgress && field[protocol.SyncProgress](e).Done {
			break
		}
	}
	if total != 6*250 {
		t.Fatalf("delivered %d history messages, want %d", total, 6*250)
	}
	if f.dlHist != 1 {
		t.Fatalf("history downloaded %d times", f.dlHist)
	}
}

func TestHistoryPerChatCap(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.init(map[string]any{"historyDays": 90, "historyMaxPerChat": 150, "imageMaxBytes": 1 << 20, "voiceMaxSeconds": 300})
	f := h.fake()
	f.dispatch(&events.Connected{})
	conv := &waHistorySync.Conversation{ID: proto.String(alice)}
	for i := 0; i < 180; i++ {
		conv.Messages = append(conv.Messages, histMsg(alice, "3EB0M"+itoa(i), h.now.Add(-time.Duration(i)*time.Minute), "m"+itoa(i)))
	}
	notif := &waE2E.HistorySyncNotification{}
	f.history[notif] = &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{conv}}
	f.dispatch(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(me), Sender: mustParse(me), IsFromMe: true}, ID: "3EB0N"},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: notif}}})
	b := field[protocol.HistoryBatch](h.expect(protocol.EvHistoryBatch))
	if len(b.Messages) != 150 || b.SyncType != "recent" {
		t.Fatalf("%d messages", len(b.Messages))
	}
	if b.Messages[149].Text != "m0" || b.Messages[0].Text != "m149" {
		t.Fatal("the newest messages should be kept, oldest first")
	}
}

// ------------------------------------------------------------ guarantees

// No presence is ever sent: the client interface the bridge uses has no
// method that could send presence, subscribe to it, or read the address book.
func TestClientInterfaceHasNoPresenceOrBulk(t *testing.T) {
	it := reflect.TypeOf((*Client)(nil)).Elem()
	for i := 0; i < it.NumMethod(); i++ {
		n := strings.ToLower(it.Method(i).Name)
		for _, banned := range []string{"presence", "chatpresence", "subscribe", "status", "broadcast", "newsletter", "getallcontacts", "requesthistory", "buildhistorysyncrequest"} {
			if strings.Contains(n, banned) {
				t.Errorf("Client exposes %s", it.Method(i).Name)
			}
		}
	}
}

func TestStderrIsContentFree(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := pairedFake()
		f.contacts[alice] = types.ContactInfo{Found: true, FullName: "Alice Example"}
		return f
	})
	f := h.initPairedConnected()
	f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0S1", "secret words نص سري", false))
	h.expect(protocol.EvMessage)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "my reply سري", "outboxId": "01M3C03V80N87VFZS5G0J0NFEA"})
	h.expect(protocol.EvSendResult)
	h.cmd("send_text", map[string]any{"chatJid": bob, "text": "unknown chat", "outboxId": "01M3C03V80N87VFZS5G0J0NFEB"})
	h.expect(protocol.EvError)
	h.b.Stop()
	out := h.stderr.String()
	if out == "" {
		t.Fatal("expected some diagnostics")
	}
	for _, s := range []string{"15550100", "Alice", "secret", "سري", "reply", testKey, h.storeDir, h.mediaDir, "3EB0", "01M3C03V"} {
		if strings.Contains(out, s) {
			t.Fatalf("stderr contains %q:\n%s", s, out)
		}
	}
}

func itoa(i int) string {
	const d = "0123456789"
	if i < 10 {
		return string(d[i])
	}
	return itoa(i/10) + string(d[i%10])
}
