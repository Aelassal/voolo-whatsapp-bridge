// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package protocol

import (
	"sort"
	"strings"
	"testing"
)

// S2: the command set is exactly the eleven v1 commands. Adding one is a
// protocol change that needs PROTOCOL.md, both sides and a security review.
func TestCommandSetIsExactlyV1(t *testing.T) {
	got := Commands()
	sort.Strings(got)
	// Revision 2 added set_pin (one chat, the user's action) and fetch_avatar
	// (one chat's picture), and nothing that sends.
	want := []string{"ack", "fetch_avatar", "fetch_media", "init", "logout", "mark_read", "pair_phone", "pair_qr", "ping", "send_media", "send_text",
		"set_pin", "shutdown"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("command set changed:\n got %v\nwant %v", got, want)
	}
	for _, banned := range []string{"fetch_history", "send_bulk", "broadcast", "schedule", "set_presence", "presence", "get_contacts", "send_status", "create_group"} {
		if IsCommand(banned) {
			t.Errorf("%s must not exist", banned)
		}
	}
}

// S2: a send takes exactly one chat, no time, no template, no recipient list.
func TestSendRefusesBulkScheduleTemplateFields(t *testing.T) {
	base := `"chatJid":"15550100002@s.whatsapp.net","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"`
	extras := []string{
		`"recipients":["15550100002@s.whatsapp.net","15550100003@s.whatsapp.net"]`,
		`"chatJids":["15550100002@s.whatsapp.net"]`,
		`"to":"15550100003@s.whatsapp.net"`,
		`"sendAt":1790334000000`,
		`"delayMs":1000`,
		`"repeat":3`,
		`"templateId":"welcome"`,
		`"template":{"name":"x"}`,
		`"broadcast":true`,
	}
	for _, e := range extras {
		if err := cmd(CmdSendText, "{"+base+","+e+"}"); err == nil {
			t.Errorf("send_text accepted %s", e)
		}
	}
	if err := cmd(CmdSendText, `{"chatJid":["15550100002@s.whatsapp.net"],"text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`); err == nil {
		t.Error("send_text accepted a list as chatJid")
	}
	mediaBase := `"chatJid":"15550100002@s.whatsapp.net","outboxId":"01M3C03V80N87VFZS5G0J0NFEX","kind":"image","path":"/tmp/m/0a.bin","key":"` +
		strings.Repeat("1f", 32) + `","sha256":"` + strings.Repeat("aa", 32) + `","mime":"image/jpeg"`
	if err := cmd(CmdSendMedia, "{"+mediaBase+"}"); err != nil {
		t.Fatalf("valid send_media refused: %v", err)
	}
	for _, e := range extras {
		if err := cmd(CmdSendMedia, "{"+mediaBase+","+e+"}"); err == nil {
			t.Errorf("send_media accepted %s", e)
		}
	}
}
