// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package logx

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
	"+15550100001",
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
	AllowCodes("rate_limited")
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

// Review L1 (mutation mut-4a): an event name is one of the fixed names listed
// in PROTOCOL.md §10.4. A well-formed name built at run time, for example
// from key characters, is replaced too.
func TestEventNamesAreAFixedSet(t *testing.T) {
	key := "9f3ab7c2e81d4f06a5b9c3d7e2f14a8b6c0d9e3f7a2b5c8d1e4f7a0b3c6d9e2f"
	for _, ev := range []string{"init_" + key[:40], "abcdef0123456789", "status_x", "ready2"} {
		var buf bytes.Buffer
		New(&buf, fixed).Info(ev)
		if !strings.Contains(buf.String(), `"event":"invalid_event"`) {
			t.Fatalf("%q was written as an event name: %s", ev, buf.String())
		}
	}
	for _, ev := range Events() {
		var buf bytes.Buffer
		New(&buf, fixed).Info(ev)
		if !strings.Contains(buf.String(), `"event":"`+ev+`"`) {
			t.Fatalf("listed event %q replaced", ev)
		}
	}
}

// Review L1: random 32-byte keys, as hex or base64, whole or in pieces, never
// survive Code(), and as hex never survive the format scrubber either: no
// 16-character piece of one reaches the output (100,000 keys).
func TestScrubCodeRedactsRandomKeys(t *testing.T) {
	n := 100000
	if testing.Short() {
		n = 5000
	}
	k := make([]byte, 32)
	for i := 0; i < n; i++ {
		_, _ = rand.Read(k)
		hx := hex.EncodeToString(k)
		for _, enc := range []string{hx, strings.ToUpper(hx), base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k)} {
			for _, in := range []string{enc, "key " + enc, enc[:20], "x:" + enc[5:30] + " y", enc[3:19]} {
				var buf bytes.Buffer
				New(&buf, fixed).Info("status", Code(in))
				outs := []string{buf.String()}
				if enc == hx || enc == strings.ToUpper(hx) {
					outs = append(outs, ScrubCode(in))
				}
				src := strings.TrimPrefix(strings.TrimPrefix(in, "key "), "x:")
				for _, out := range outs {
					for j := 0; j+16 <= len(src); j++ {
						if piece := src[j : j+16]; strings.Contains(out, piece) {
							t.Fatalf("input %q gave %q, which keeps %q", in, out, piece)
						}
					}
				}
			}
		}
	}
	AllowCodes("rate_limited_local", "duplicate_outbox_id")
	for _, c := range []string{"rate_limited_local", "duplicate_outbox_id", "too_long", "media"} {
		var buf bytes.Buffer
		New(&buf, fixed).Info("status", Code(c))
		if !strings.Contains(buf.String(), `"code":"`+c+`"`) {
			t.Errorf("Code(%q) not kept: %s", c, buf.String())
		}
	}
	// Ordinary diagnostic codes and whatsmeow format strings stay readable.
	for _, s := range []string{"Client/Socket: Failed to handle frame: %v", "Database: Upgrading database to v%d", "Client: Initial connection failed but reconnecting in background (%v)"} {
		if ScrubCode(s) != s {
			t.Errorf("ScrubCode(%q) = %q", s, ScrubCode(s))
		}
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
	long := strings.Repeat("go on ", 100) // 600 characters, no hex run (a long hex run is redacted)
	if len(ScrubCode(long)) != maxCode {
		t.Error("not capped")
	}
}
