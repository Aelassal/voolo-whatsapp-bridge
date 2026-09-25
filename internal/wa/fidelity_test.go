// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

// Revision 2: a reply carries the quoted id, its author and a copy of its text
// (PROTOCOL.md §6.5), so the phone shows it as a reply.
func TestReplyCarriesQuoteContext(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f) // alice wrote 3EB0K
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "تمام", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX",
		"quotedMessageId": "3EB0K", "quotedSenderJid": bob, "quotedText": "hello"})
	h.expect(protocol.EvSendResult)
	ci := f.sent[0].msg.GetExtendedTextMessage().GetContextInfo()
	// The author the bridge saw wins over the client's.
	if ci.GetStanzaID() != "3EB0K" || ci.GetParticipant() != alice || ci.GetQuotedMessage().GetConversation() != "hello" {
		t.Fatalf("context %v", ci)
	}
	// A message the bridge never saw (older than this run): the client's author.
	h.advance(time.Second)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "x", "outboxId": "01M3C03V80N87VFZS5G0J0NFEY",
		"quotedMessageId": "3EB0OLD", "quotedSenderJid": alice})
	h.expect(protocol.EvSendResult)
	ci = f.sent[1].msg.GetExtendedTextMessage().GetContextInfo()
	if ci.GetStanzaID() != "3EB0OLD" || ci.GetParticipant() != alice || ci.GetQuotedMessage() != nil {
		t.Fatalf("context %v", ci)
	}
	// No quote: a plain conversation message.
	h.advance(time.Second)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "plain", "outboxId": "01M3C03V80N87VFZS5G0J0NFEZ"})
	h.expect(protocol.EvSendResult)
	if f.sent[2].msg.GetConversation() != "plain" || f.sent[2].msg.GetExtendedTextMessage() != nil {
		t.Fatalf("plain %v", f.sent[2].msg)
	}
}

// In a group that addresses people by @lid, the quote names the author as
// WhatsApp addressed them, not the phone number reported to the client.
func TestReplyUsesTheRawAuthorInLIDGroups(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := pairedFake()
		f.pnForLID[aliceLID] = mustParse(alice)
		return f
	})
	f := h.initPairedConnected()
	msg := textMsg(mustParse(group), mustParse(aliceLID), "3EB0G1", "في الجروب", false)
	f.dispatch(msg)
	m := field[protocol.MessageEvent](h.expect(protocol.EvMessage)).Message
	if m.SenderJID != alice {
		t.Fatalf("reported sender %s", m.SenderJID)
	}
	h.cmd("send_text", map[string]any{"chatJid": group, "text": "reply", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX",
		"quotedMessageId": "3EB0G1", "quotedSenderJid": alice, "quotedText": "في الجروب"})
	h.expect(protocol.EvSendResult)
	if p := f.sent[0].msg.GetExtendedTextMessage().GetContextInfo().GetParticipant(); p != aliceLID {
		t.Fatalf("participant %s, want the @lid", p)
	}
}

// A voice note or a file can be a reply too.
func TestMediaReplyCarriesContext(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	s := sealForTest(t, h.mediaDir, oggStream(3))
	h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFEX", "kind": "voice", "path": s.path,
		"key": s.key, "sha256": s.sha, "mime": "audio/ogg; codecs=opus", "quotedMessageId": "3EB0K", "quotedText": "hello"})
	h.expect(protocol.EvSendResult)
	ci := f.sent[0].msg.GetAudioMessage().GetContextInfo()
	if ci.GetStanzaID() != "3EB0K" || ci.GetQuotedMessage().GetConversation() != "hello" {
		t.Fatalf("voice context %v", ci)
	}
	h.advance(time.Second)
	d := sealForTest(t, h.mediaDir, []byte("%PDF-1.7 fake"))
	h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFEY", "kind": "file", "path": d.path,
		"key": d.key, "sha256": d.sha, "mime": "application/pdf", "fileName": "عقد.pdf", "quotedMessageId": "3EB0K"})
	h.expect(protocol.EvSendResult)
	if dm := f.sent[1].msg.GetDocumentMessage(); dm.GetContextInfo().GetStanzaID() != "3EB0K" || dm.GetFileName() != "عقد.pdf" {
		t.Fatalf("document %v", dm)
	}
}

// Feature forward: the message is marked forwarded, text and media alike.
func TestForwardMarksTheMessage(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.knownChat(f)
	h.cmd("send_text", map[string]any{"chatJid": alice, "text": "forwarded text", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX", "forwarded": true})
	h.expect(protocol.EvSendResult)
	et := f.sent[0].msg.GetExtendedTextMessage()
	if et.GetText() != "forwarded text" || !et.GetContextInfo().GetIsForwarded() || et.GetContextInfo().GetForwardingScore() != 1 ||
		et.GetContextInfo().GetStanzaID() != "" {
		t.Fatalf("forward %v", f.sent[0].msg)
	}
	h.advance(time.Second)
	s := sealForTest(t, h.mediaDir, jpegForTest(t))
	h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFEY", "kind": "image", "path": s.path,
		"key": s.key, "sha256": s.sha, "mime": "image/jpeg", "caption": "صورة", "forwarded": true})
	h.expect(protocol.EvSendResult)
	im := f.sent[1].msg.GetImageMessage()
	if !im.GetContextInfo().GetIsForwarded() || im.GetCaption() != "صورة" || im.GetWidth() != 4 {
		t.Fatalf("image forward %v", im)
	}
}

// jpegForTest is a tiny real JPEG (4×2) so the bridge can read its size.
func jpegForTest(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Feature send_video: a video message with its facts; the caps of init.
func TestSendVideoAndCaps(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.init(map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 16 << 20, "voiceMaxSeconds": 3600,
		"videoMaxBytes": 64, "fileMaxBytes": 32})
	f := h.fake()
	h.expectState(protocol.StateConnecting)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connects > 0 })
	f.dispatch(&events.Connected{})
	h.expectState(protocol.StateConnected)
	h.knownChat(f)
	video := bytes.Repeat([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p'}, 8) // 64 bytes
	s := sealForTest(t, h.mediaDir, video)
	h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFEX", "kind": "video", "path": s.path,
		"key": s.key, "sha256": s.sha, "mime": "video/mp4", "caption": "رحلة", "durationS": 42, "width": 1280, "height": 720})
	h.expect(protocol.EvSendResult)
	vm := f.sent[0].msg.GetVideoMessage()
	if vm.GetMimetype() != "video/mp4" || vm.GetSeconds() != 42 || vm.GetWidth() != 1280 || vm.GetHeight() != 720 || vm.GetCaption() != "رحلة" ||
		vm.GetFileLength() != 64 {
		t.Fatalf("video %v", vm)
	}
	h.advance(time.Second)
	big := sealForTest(t, h.mediaDir, append(video, 0))
	id := h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFEY", "kind": "video", "path": big.path,
		"key": big.key, "sha256": big.sha, "mime": "video/mp4"})
	h.expectError(id, protocol.ErrMediaTooLarge)
	notVideo := sealForTest(t, h.mediaDir, video)
	id = h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFEZ", "kind": "video", "path": notVideo.path,
		"key": notVideo.key, "sha256": notVideo.sha, "mime": "image/jpeg"})
	h.expectError(id, protocol.ErrMediaInvalid)
	doc := sealForTest(t, h.mediaDir, bytes.Repeat([]byte("d"), 33))
	id = h.cmd("send_media", map[string]any{"chatJid": alice, "outboxId": "01M3C03V80N87VFZS5G0J0NFF0", "kind": "file", "path": doc.path,
		"key": doc.key, "sha256": doc.sha, "mime": "application/pdf"})
	h.expectError(id, protocol.ErrMediaTooLarge)
	if len(f.sent) != 1 {
		t.Fatalf("sent %d", len(f.sent))
	}
}

// Without the new limits a file keeps the first release's 32 MiB cap.
func TestFileCapDefault(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.initPairedConnected()
	h.b.mu.Lock()
	lim := h.b.lim
	h.b.mu.Unlock()
	if lim.FileMaxBytes != 32<<20 || lim.VideoMaxBytes != 16<<20 {
		t.Fatalf("limits %+v", lim)
	}
}

// Feature fetch_all_media: videos, documents, stickers and audio files are
// described with a download descriptor and fetched on request, each under its
// own cap and with WhatsApp's media type for its keys.
func TestFetchVideoDocumentSticker(t *testing.T) {
	h := newHarness(t, pairedFake)
	h.init(map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 1000, "voiceMaxSeconds": 3600,
		"videoMaxBytes": 2000, "fileMaxBytes": 3000})
	f := h.fake()
	h.expectState(protocol.StateConnecting)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connects > 0 })
	f.dispatch(&events.Connected{})
	h.expectState(protocol.StateConnected)
	video := bytes.Repeat([]byte("v"), 1500)
	doc := bytes.Repeat([]byte("d"), 2500)
	sticker := []byte("RIFF....WEBPVP8 ")
	f.downloads["/v/video"], f.downloads["/v/doc"], f.downloads["/v/sticker"] = video, doc, sticker
	info := func(id string) types.MessageInfo {
		return types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(alice), Sender: mustParse(alice)}, ID: id, Timestamp: h.now}
	}
	f.dispatch(&events.Message{Info: info("3EB0VID"), Message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"),
		FileLength: proto.Uint64(1500), Seconds: proto.Uint32(12), DirectPath: proto.String("/v/video"), MediaKey: []byte{1}}}})
	f.dispatch(&events.Message{Info: info("3EB0DOC"), Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String("application/pdf"),
		FileLength: proto.Uint64(2500), FileName: proto.String("عقد.pdf"), DirectPath: proto.String("/v/doc"), MediaKey: []byte{1}}}})
	f.dispatch(&events.Message{Info: info("3EB0STK"), Message: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp"),
		FileLength: proto.Uint64(uint64(len(sticker))), DirectPath: proto.String("/v/sticker"), MediaKey: []byte{1}}}})
	f.dispatch(&events.Message{Info: info("3EB0BIG"), Message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"),
		FileLength: proto.Uint64(2001), DirectPath: proto.String("/v/video"), MediaKey: []byte{1}}}})
	for range 4 {
		h.expect(protocol.EvMessage)
	}
	for id, want := range map[string][]byte{"3EB0VID": video, "3EB0DOC": doc, "3EB0STK": sticker} {
		rid := h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": id})
		mr := field[protocol.MediaReady](h.expect(protocol.EvMediaReady))
		if mr.ReplyTo != rid || mr.SizeBytes != int64(len(want)) {
			t.Fatalf("%s: %+v", id, mr)
		}
	}
	h.expectError(h.cmd("fetch_media", map[string]any{"chatJid": alice, "messageId": "3EB0BIG"}), protocol.ErrMediaTooLarge)
	// Each download is cut off at its own cap plus WhatsApp's overhead.
	f.mu.Lock()
	limits := append([]int64(nil), f.dlLimits...)
	f.mu.Unlock()
	if len(limits) != 3 {
		t.Fatalf("downloads %v", limits)
	}
	seen := map[int64]bool{}
	for _, l := range limits {
		seen[l] = true
	}
	for _, want := range []int64{2000 + 26, 3000 + 26, 1000 + 26} {
		if !seen[want] {
			t.Fatalf("body limits %v, missing %d", limits, want)
		}
	}
}

func TestDownloadTypes(t *testing.T) {
	for kind, want := range map[string]whatsmeow.MediaType{"image": whatsmeow.MediaImage, "sticker": whatsmeow.MediaImage, "voice": whatsmeow.MediaAudio,
		"audio": whatsmeow.MediaAudio, "video": whatsmeow.MediaVideo, "document": whatsmeow.MediaDocument} {
		if got := downloadType(kind); got != want {
			t.Errorf("%s: %s", kind, got)
		}
	}
}

// Feature set_pin: one app-state patch per user action, for a reported chat,
// under its own backstop; errors mapped, nothing repeated.
func TestSetPin(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	h.expectError(h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": true}), protocol.ErrUnknownChat)
	h.knownChat(f)
	id := h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": true})
	if reply(h.expect(protocol.EvOK)) != id {
		t.Fatal("ok replyTo")
	}
	f.mu.Lock()
	p := f.patches[0]
	f.mu.Unlock()
	if len(p.Mutations) != 1 || p.Mutations[0].Index[0] != appstate.IndexPin || p.Mutations[0].Index[1] != alice ||
		!p.Mutations[0].Value.GetPinAction().GetPinned() {
		t.Fatalf("patch %+v", p)
	}
	// Within a second: refused, nothing written.
	h.expectError(h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": false}), protocol.ErrRateLimitedLocal)
	h.advance(time.Second)
	h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": false})
	h.expect(protocol.EvOK)
	f.mu.Lock()
	if len(f.patches) != 2 || f.patches[1].Mutations[0].Value.GetPinAction().GetPinned() {
		t.Fatalf("unpin %+v", f.patches)
	}
	f.mu.Unlock()
	// At most 20 in ten minutes.
	for range 18 {
		h.advance(time.Second)
		h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": true})
		h.expect(protocol.EvOK)
	}
	h.advance(time.Second)
	h.expectError(h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": true}), protocol.ErrRateLimitedLocal)
	h.advance(10 * time.Minute)
	f.mu.Lock()
	f.patchErr = whatsmeow.ErrIQTimedOut
	f.mu.Unlock()
	h.expectError(h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": true}), protocol.ErrTimeout)
	h.advance(time.Second)
	f.mu.Lock()
	f.patchErr = errors.New("server said no")
	f.mu.Unlock()
	h.expectError(h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": true}), protocol.ErrInternal)
	f.mu.Lock()
	n := len(f.patches)
	f.mu.Unlock()
	if n != 20 {
		t.Fatalf("patches written %d", n)
	}
	f.Disconnect()
	h.expectError(h.cmd("set_pin", map[string]any{"chatJid": alice, "pinned": true}), protocol.ErrNotConnected)
}
