// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package protocol

import (
	"slices"
	"strings"
	"testing"
)

// Revision 2 (PROTOCOL.md §5.1, §6.5, §6.6): the reply fields.
func TestQuoteFields(t *testing.T) {
	base := `"chatJid":"15550100002@s.whatsapp.net","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"`
	cases := []struct {
		extra string
		ok    bool
	}{
		{`"quotedMessageId":"3EB0K"`, true},
		{`"quotedMessageId":"3EB0K","quotedSenderJid":"15550100003@s.whatsapp.net","quotedText":"أيوه — yes"`, true},
		{`"quotedMessageId":"3EB0K","quotedSenderJid":"100000000000002@lid"`, true},
		{`"quotedSenderJid":"15550100003@s.whatsapp.net"`, false}, // an author without the id
		{`"quotedText":"x"`, false},
		{`"quotedMessageId":"3EB0K","quotedSenderJid":"120363000000000001@g.us"`, false}, // a group is not an author
		{`"quotedMessageId":"3EB0K","quotedText":""`, true},                              // empty is absent
		{`"quotedMessageId":"3EB0K","quotedText":"` + strings.Repeat("ب", MaxQuotedTextChars) + `"`, true},
		{`"quotedMessageId":"3EB0K","quotedText":"` + strings.Repeat("ب", MaxQuotedTextChars+1) + `"`, false},
		{`"quotedMessageId":"bad id"`, false},
		{`"quotedMessageId":"3EB0K","quotedSenderJid":null`, false},
	}
	for _, c := range cases {
		err := cmd(CmdSendText, "{"+base+","+c.extra+"}")
		if (err == nil) != c.ok {
			t.Errorf("send_text %s: err=%v, want ok=%v", c.extra, err, c.ok)
		}
	}
	media := `"chatJid":"15550100002@s.whatsapp.net","outboxId":"01M3C03V80N87VFZS5G0J0NFEX","kind":"image","path":"/tmp/m/0a.bin","key":"` +
		strings.Repeat("1f", 32) + `","sha256":"` + strings.Repeat("aa", 32) + `","mime":"image/jpeg"`
	if err := cmd(CmdSendMedia, "{"+media+`,"quotedMessageId":"3EB0K","quotedSenderJid":"15550100003@s.whatsapp.net","quotedText":"Photo"}`); err != nil {
		t.Fatalf("send_media with a quote refused: %v", err)
	}
	if err := cmd(CmdSendMedia, "{"+media+`,"quotedText":"Photo"}`); err == nil {
		t.Fatal("send_media quotedText without the id accepted")
	}
}

func TestHelloAnnouncesFeatures(t *testing.T) {
	if !slices.Contains(Features(), FeatureReplyContext) {
		t.Fatalf("features %v", Features())
	}
}

// Revision 2, feature forward: one flag, never together with a quote.
func TestForwardFlag(t *testing.T) {
	base := `"chatJid":"15550100002@s.whatsapp.net","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"`
	if err := cmd(CmdSendText, "{"+base+`,"forwarded":true}`); err != nil {
		t.Fatalf("forward refused: %v", err)
	}
	if err := cmd(CmdSendText, "{"+base+`,"forwarded":true,"quotedMessageId":"3EB0K"}`); err == nil {
		t.Fatal("a forwarded reply accepted")
	}
	if err := cmd(CmdSendText, "{"+base+`,"forwarded":"yes"}`); err == nil {
		t.Fatal("forwarded as a string accepted")
	}
	for _, e := range []string{`"forwardTo":["15550100003@s.whatsapp.net"]`, `"forwardChats":2`, `"chatJids":["15550100003@s.whatsapp.net"]`} {
		if err := cmd(CmdSendText, "{"+base+`,"forwarded":true,`+e+"}"); err == nil {
			t.Errorf("a forward to several chats accepted: %s", e)
		}
	}
	if !slices.Contains(Features(), FeatureForward) {
		t.Fatal("forward not announced")
	}
}
