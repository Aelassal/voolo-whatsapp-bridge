// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"testing"
	"time"

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
