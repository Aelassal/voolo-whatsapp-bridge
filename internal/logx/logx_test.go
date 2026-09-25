// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package logx

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var secrets = []string{
	"15550100002@s.whatsapp.net",
	"100000000000001@lid",
	"صباح الخير",
	"2@EXAMPLEONLYnotarealcode",
	"ABCD-2345",
	"0000000000000000000000000000000000000000000000000000000000000001",
	"/home/example/.config/app/store",
	"+201001234567",
}

func fixed() time.Time { return time.UnixMilli(1790330400000) }

func TestWhatsmeowArgumentsNeverReachStderr(t *testing.T) {
	var buf bytes.Buffer
	wa := NewWA(New(&buf, fixed), "Client")
	for _, s := range secrets {
		wa.Warnf("Failed to handle message from %s: %v", s, s)
		wa.Errorf("Error in %s", s)
		wa.Infof("info %s", s)
		wa.Debugf("debug %s", s)
		wa.Sub("Socket").Warnf("Dialing %s", s)
	}
	out := buf.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Fatalf("stderr contains %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "Failed to handle message from %s: %v") {
		t.Fatalf("constant format string missing:\n%s", out)
	}
	if !strings.Contains(out, "Client/Socket: Dialing %s") {
		t.Fatalf("sub-module format missing:\n%s", out)
	}
	if strings.Contains(out, "info %s") || strings.Contains(out, "debug %s") {
		t.Fatal("info/debug should be dropped")
	}
}

func TestLineShape(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, fixed)
	l.Info("store_opened", N(3))
	l.Error("send_failed", Code("rate_limited"))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	for _, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			switch k {
			case "t", "level", "event", "code", "n":
			default:
				t.Fatalf("unexpected field %q", k)
			}
		}
		if m["t"].(float64) != 1790330400000 {
			t.Fatal("bad t")
		}
	}
}

func TestEventNamesMustBeFixedIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, fixed)
	l.Info("hello 15550100002@s.whatsapp.net")
	if strings.Contains(buf.String(), "15550100002") || !strings.Contains(buf.String(), "invalid_event") {
		t.Fatalf("event not replaced: %s", buf.String())
	}
}

func TestScrubCode(t *testing.T) {
	cases := map[string]string{
		"rate_limited":             "rate_limited",
		"Client: Dialing %s":       "Client: Dialing %s",
		"got 15550100002":          "redacted",
		"x@y":                      "redacted",
		"+2010":                    "redacted",
		"code 405":                 "code 405",
		"line\nbreak\"quote{json}": "linebreakquotejson",
	}
	for in, want := range cases {
		if got := ScrubCode(in); got != want {
			t.Errorf("ScrubCode(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 500)
	if len(ScrubCode(long)) != maxCode {
		t.Error("not capped")
	}
}
