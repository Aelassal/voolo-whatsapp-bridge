// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	wstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/transport"
)

func TestContentOfKinds(t *testing.T) {
	media := func(m *waE2E.Message) *waE2E.Message { return m }
	cases := []struct {
		name string
		msg  *waE2E.Message
		kind string
		text string
		ok   bool
	}{
		{"conversation", &waE2E.Message{Conversation: proto.String("hi")}, "text", "hi", true},
		{"extended", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("مرحبا")}}, "text", "مرحبا", true},
		{"image", media(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("cap"), Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(10)}}), "image", "cap", true},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Seconds: proto.Uint32(3)}}, "video", "", true},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, "sticker", "", true},
		{"voice", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}, "voice", "", true},
		{"audio", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, "audio", "", true},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("a.pdf")}}, "document", "", true},
		{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}}, "location", "", true},
		{"contact", &waE2E.Message{ContactMessage: &waE2E.ContactMessage{Vcard: proto.String("BEGIN:VCARD TEL:+15550100009")}}, "contact", "", true},
		{"poll", &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("when?")}}, "poll", "when?", true},
		{"unknown type", &waE2E.Message{ButtonsMessage: &waE2E.ButtonsMessage{}}, "unsupported", "", true},
		{"reaction", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{}}, "", "", false},
		{"protocol", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}}, "", "", false},
		{"poll vote", &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}}, "", "", false},
		{"key distribution only", &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}}, "", "", false},
		{"key distribution with text", &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}, Conversation: proto.String("x")}, "text", "x", true},
		{"nil", nil, "", "", false},
	}
	for _, c := range cases {
		got, ok := ContentOf(c.msg, false)
		if ok != c.ok || got.Kind != c.kind || got.Text != c.text {
			t.Errorf("%s: got %q %q ok=%v", c.name, got.Kind, got.Text, ok)
		}
	}
	// A contact card's vCard (third-party numbers) is never reported.
	if c, _ := ContentOf(&waE2E.Message{ContactMessage: &waE2E.ContactMessage{Vcard: proto.String("TEL:+15550100009")}}, false); c.Text != "" {
		t.Fatal("vcard leaked")
	}
	// View-once media is neither described nor downloadable.
	c, ok := ContentOf(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{DirectPath: proto.String("/x"), MediaKey: []byte{1}}}, true)
	if !ok || c.Kind != "unsupported" || c.Media != nil || c.Keys != nil {
		t.Fatalf("view once: %+v", c)
	}
	// Text is capped at 65,536 characters.
	c, _ = ContentOf(&waE2E.Message{Conversation: proto.String(strings.Repeat("ع", protocol.MaxTextChars+5))}, false)
	if len([]rune(c.Text)) != protocol.MaxTextChars {
		t.Fatal("text not truncated")
	}
}

func TestSignalMapping(t *testing.T) {
	cases := []struct {
		evt  any
		kind string
		code any
	}{
		{&events.TemporaryBan{Code: 104, Expire: 2 * time.Hour}, "temp_banned", 104},
		{&events.StreamReplaced{}, "stream_replaced", nil},
		{&events.ConnectFailure{Reason: 500}, "connect_failure", 500},
		{&events.ConnectFailure{Reason: 503}, "connect_failure", 503},
		{&events.ConnectFailure{Reason: 429}, "rate_limited", 429},
		{&events.StreamError{Code: "516"}, "stream_error", "516"},
		{&events.KeepAliveTimeout{}, "keepalive_timeout", nil},
	}
	for _, c := range cases {
		s, ok := SignalFor(c.evt)
		if !ok || s.Kind != c.kind || s.Code != c.code {
			t.Errorf("%T: %+v", c.evt, s)
		}
	}
	// ClientOutdated is signalled only after the bridge's own retry failed
	// (TestClientOutdatedRefreshOnceThenFatal), never straight from the event.
	for _, e := range []any{&events.Connected{}, &events.Message{}, &events.LoggedOut{}, &events.ClientOutdated{}} {
		if _, ok := SignalFor(e); ok {
			t.Errorf("%T is not a signal", e)
		}
	}
}

func TestLogoutReasonsAndReceipts(t *testing.T) {
	for r, want := range map[events.ConnectFailureReason]string{401: "device_removed", 403: "primary_gone", 406: "banned", 400: "unknown", 409: "unknown"} {
		if got := LogoutReason(r); got != want {
			t.Errorf("%d: %s", r, got)
		}
	}
	for rt, want := range map[types.ReceiptType]string{types.ReceiptTypeDelivered: "delivered", types.ReceiptTypeRead: "read", types.ReceiptTypePlayed: "played", types.ReceiptTypeReadSelf: "read_self"} {
		if got, ok := ReceiptKind(rt); !ok || got != want {
			t.Errorf("%q: %s", rt, got)
		}
	}
	for _, rt := range []types.ReceiptType{types.ReceiptTypeRetry, types.ReceiptTypeSender, types.ReceiptTypeServerError, types.ReceiptTypeInactive} {
		if _, ok := ReceiptKind(rt); ok {
			t.Errorf("%q should not be reported", rt)
		}
	}
}

func TestErrorCodeMapping(t *testing.T) {
	send := map[error]string{
		whatsmeow.ErrNotConnected:                              "not_connected",
		whatsmeow.ErrMessageTimedOut:                           "timeout",
		context.DeadlineExceeded:                               "timeout",
		fmt.Errorf("x: %w", whatsmeow.ErrIQRateOverLimit):      "rate_limited",
		fmt.Errorf("%w 429", whatsmeow.ErrServerReturnedError): "rate_limited",
		fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError): "send_failed",
		errors.New("something new"):                            "send_failed",
	}
	for err, want := range send {
		if got := SendErrorCode(err); got != want {
			t.Errorf("send %v: %s, want %s", err, got, want)
		}
	}
	dl := map[error]string{
		whatsmeow.ErrMediaDownloadFailedWith404: "media_expired",
		whatsmeow.ErrMediaDownloadFailedWith410: "media_expired",
		whatsmeow.ErrInvalidMediaSHA256:         "media_unavailable",
		whatsmeow.ErrNotConnected:               "not_connected",
		context.DeadlineExceeded:                "timeout",
	}
	for err, want := range dl {
		if got := DownloadErrorCode(err); got != want {
			t.Errorf("download %v: %s, want %s", err, got, want)
		}
	}
}

func TestChatFromConversation(t *testing.T) {
	now := time.UnixMilli(1790330400000)
	conv := &waHistorySync.Conversation{Name: proto.String("Team"), UnreadCount: proto.Uint32(3), Pinned: proto.Uint32(1), Archived: proto.Bool(false),
		MuteEndTime: proto.Uint64(uint64(now.Add(time.Hour).UnixMilli())), ConversationTimestamp: proto.Uint64(1790330340)}
	c := ChatFromConversation(conv, "120363000000000001@g.us", true, now)
	if c.Kind != "group" || c.Name != "Team" || *c.UnreadCount != 3 || !*c.Pinned || *c.Archived || c.MutedUntil != now.Add(time.Hour).UnixMilli() || c.LastMessageAt != 1790330340000 {
		t.Fatalf("%+v", c)
	}
	conv.MuteEndTime = proto.Uint64(uint64(wstore.MutedForever.UnixMilli()))
	if c := ChatFromConversation(conv, "x", false, now); c.MutedUntil != -1 || c.Kind != "dm" {
		t.Fatalf("forever: %+v", c)
	}
	conv.MuteEndTime = proto.Uint64(uint64(now.Add(-time.Hour).UnixMilli()))
	if c := ChatFromConversation(conv, "x", false, now); c.MutedUntil != 0 {
		t.Fatal("expired mute reported")
	}
}

func TestConfigureDeviceAsksForAtMostTheHistoryWindow(t *testing.T) {
	ConfigureDevice("Voolo", 90)
	if wstore.DeviceProps.GetOs() != "Voolo" || wstore.DeviceProps.GetHistorySyncConfig().GetFullSyncDaysLimit() != 90 || wstore.DeviceProps.GetRequireFullSync() {
		t.Fatalf("%v", wstore.DeviceProps)
	}
}

// The real whatsmeow client dials only through the guarded transport: its
// first connection goes to web.whatsapp.com:443 through our dialer, and a
// dialer that refuses means nothing leaves the process.
func TestRealClientUsesGuardedTransport(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir(), make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dev, err := st.Container.GetFirstDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var dialed []string
	fakeDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return nil, errors.New("test: no network")
	}
	var blocked int
	var looked []string
	// A fake resolver too: nothing, not even a DNS query, leaves the process.
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		mu.Lock()
		looked = append(looked, host)
		mu.Unlock()
		return []net.IPAddr{{IP: net.ParseIP("157.240.1.1")}}, nil
	}
	opts := transport.Options{Dial: fakeDial, Lookup: lookup, OnBlock: func() { blocked++ }}
	cli := NewRealClient(dev, transport.NewClient(opts), transport.NewMediaClient(opts), nil).(realClient)
	cli.EnableAutoReconnect = false
	cli.InitialAutoReconnect = false
	if err := cli.Connect(); err == nil {
		t.Fatal("connected without a network")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(looked) == 0 || looked[0] != "web.whatsapp.com" || len(dialed) == 0 || dialed[0] != "157.240.1.1:443" {
		t.Fatalf("resolved %v, dialed %v", looked, dialed)
	}
	if blocked != 0 {
		t.Fatal("web.whatsapp.com was blocked")
	}
	if !cli.ManualHistorySyncDownload {
		t.Fatal("history must be downloaded at the pace of the ack window")
	}
}
