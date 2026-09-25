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

// Revision 2, feature send_video: the video kind, its display facts, and the
// two new optional limits with their ceilings.
func TestVideoKindAndLimits(t *testing.T) {
	media := func(kind, extra string) string {
		return `{"chatJid":"15550100002@s.whatsapp.net","outboxId":"01M3C03V80N87VFZS5G0J0NFEX","kind":"` + kind + `","path":"/tmp/m/0a.bin","key":"` +
			strings.Repeat("1f", 32) + `","sha256":"` + strings.Repeat("aa", 32) + `","mime":"video/mp4"` + extra + `}`
	}
	cases := []struct {
		payload string
		ok      bool
	}{
		{media("video", ``), true},
		{media("video", `,"durationS":42,"width":1280,"height":720,"caption":"رحلة"`), true},
		{media("video", `,"durationS":-1`), false},
		{media("video", `,"durationS":86401`), false},
		{media("video", `,"width":16385,"height":10`), false},
		{media("image", `,"durationS":4`), false}, // facts only for a video
		{media("file", `,"width":4`), false},
		{media("gif", ``), false},
	}
	for _, c := range cases {
		if err := cmd(CmdSendMedia, c.payload); (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.payload, err, c.ok)
		}
	}
	lim := func(extra string) string {
		return `{"storeKey":"` + strings.Repeat("ab", 32) + `","storeDir":"/tmp/s","mediaDir":"/tmp/m","deviceName":"Voolo","limits":{"historyDays":1,"historyMaxPerChat":1,"imageMaxBytes":1,"voiceMaxSeconds":1` + extra + `}}`
	}
	for extra, ok := range map[string]bool{
		``:                                    true,
		`,"videoMaxBytes":67108864`:           true,
		`,"videoMaxBytes":67108865`:           false,
		`,"fileMaxBytes":104857600`:           true,
		`,"fileMaxBytes":104857601`:           false,
		`,"fileMaxBytes":0`:                   false,
		`,"videoMaxBytes":1,"fileMaxBytes":1`: true,
	} {
		v, err := DecodeCommand(CmdInit, []byte(lim(extra)))
		if (err == nil) != ok {
			t.Errorf("limits %s: err=%v, want ok=%v", extra, err, ok)
		}
		if err == nil && extra == `` {
			l := v.(*Init).Limits
			if l.VideoBytes() != DefaultVideoMaxBytes || l.FileBytes() != DefaultFileMaxBytes {
				t.Errorf("defaults %d %d", l.VideoBytes(), l.FileBytes())
			}
		}
	}
	if !slices.Contains(Features(), FeatureSendVideo) {
		t.Fatal("send_video not announced")
	}
}

func TestSetPinStrict(t *testing.T) {
	for payload, ok := range map[string]bool{
		`{"chatJid":"15550100002@s.whatsapp.net","pinned":true}`:                true,
		`{"chatJid":"120363000000000001@g.us","pinned":false}`:                  true,
		`{"chatJid":"15550100002@s.whatsapp.net"}`:                              false,
		`{"chatJid":"status@broadcast","pinned":true}`:                          false,
		`{"chatJids":["15550100002@s.whatsapp.net"],"pinned":true}`:             false,
		`{"chatJid":"15550100002@s.whatsapp.net","pinned":true,"archive":true}`: false,
		`{"chatJid":"15550100002@s.whatsapp.net","pinned":"true"}`:              false,
	} {
		if err := cmd(CmdSetPin, payload); (err == nil) != ok {
			t.Errorf("%s: %v", payload, err)
		}
	}
}
