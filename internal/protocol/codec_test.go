// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readExamples(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples in %s: %v", dir, err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(b, []byte("\n")) || bytes.Count(b, []byte("\n")) != 1 {
			t.Fatalf("%s: an example is exactly one line ending in \\n", f)
		}
		out[filepath.Base(f)] = bytes.TrimSuffix(b, []byte("\n"))
	}
	return out
}

func TestEveryCommandExampleIsAccepted(t *testing.T) {
	seen := map[string]bool{}
	for name, line := range readExamples(t, "app-to-bridge") {
		env, err := ParseEnvelope(line)
		if err != nil {
			t.Fatalf("%s: envelope: %v", name, err)
		}
		if _, err := DecodeCommand(env.Type, env.Payload); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		seen[env.Type] = true
	}
	for _, c := range Commands() {
		if !seen[c] {
			t.Errorf("no example for command %s", c)
		}
	}
}

// eventTypes maps every event type to the struct that encodes it.
var eventTypes = map[string]func() any{
	EvHello: func() any { return &Hello{} }, EvReady: func() any { return &Ready{} },
	EvStatus: func() any { return &Status{} }, EvSignal: func() any { return &Signal{} },
	EvQR: func() any { return &QR{} }, EvPairCode: func() any { return &PairCode{} },
	EvPaired: func() any { return &Paired{} }, EvPairFailed: func() any { return &PairFailed{} },
	EvLoggedOut: func() any { return &LoggedOut{} }, EvSyncProgress: func() any { return &SyncProgress{} },
	EvChat: func() any { return &ChatEvent{} }, EvChatUpdate: func() any { return &ChatUpdate{} },
	EvContact: func() any { return &Contact{} }, EvGroup: func() any { return &Group{} },
	EvMessage: func() any { return &MessageEvent{} }, EvMsgUpdate: func() any { return &MessageUpdate{} },
	EvReaction: func() any { return &Reaction{} }, EvReceipt: func() any { return &Receipt{} },
	EvHistoryBatch: func() any { return &HistoryBatch{} }, EvMediaReady: func() any { return &MediaReady{} },
	EvSendResult: func() any { return &SendResult{} }, EvOK: func() any { return &Reply{} },
	EvPong: func() any { return &Reply{} }, EvError: func() any { return &ErrorEvent{} },
}

func TestEveryEventExampleRoundTrips(t *testing.T) {
	seen := map[string]bool{}
	for name, line := range readExamples(t, "bridge-to-app") {
		env, err := ParseEnvelope(line)
		if err != nil {
			t.Fatalf("%s: envelope: %v", name, err)
		}
		mk, ok := eventTypes[env.Type]
		if !ok {
			t.Fatalf("%s: unknown event type %s", name, env.Type)
		}
		v := mk()
		dec := json.NewDecoder(bytes.NewReader(env.Payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Fatalf("%s: our struct does not cover the example: %v", name, err)
		}
		w := NewWriter(io.Discard, func() time.Time { return time.UnixMilli(env.TS) })
		out, err := w.Encode(env.Type, v)
		if err != nil {
			t.Fatal(err)
		}
		var a, b map[string]any
		_ = json.Unmarshal(env.Payload, &a)
		var got struct {
			Payload json.RawMessage `json:"payload"`
		}
		_ = json.Unmarshal(out, &got)
		_ = json.Unmarshal(got.Payload, &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: round trip differs\nwant %s\ngot  %s", name, env.Payload, got.Payload)
		}
		seen[env.Type] = true
	}
	for typ := range eventTypes {
		if !seen[typ] {
			t.Errorf("no example for event %s", typ)
		}
	}
}

func TestInvalidExamples(t *testing.T) {
	ex := readExamples(t, "invalid")
	expect := map[string]string{
		"send_text-extra-field.json":     ErrBadRequest,
		"send_text-schedule.json":        ErrBadRequest,
		"send_text-empty.json":           ErrBadRequest,
		"pair_phone-leading-zero.json":   ErrPhoneInvalid,
		"init-short-key.json":            ErrBadRequest,
		"init-limits-above-ceiling.json": ErrBadRequest,
		"mark_read-too-many.json":        ErrBadRequest,
	}
	for name, code := range expect {
		env, err := ParseEnvelope(ex[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err = DecodeCommand(env.Type, env.Payload)
		var br *BadRequest
		switch {
		case code == ErrBadRequest && !errors.As(err, &br):
			t.Errorf("%s: want bad_request, got %v", name, err)
		case code == ErrPhoneInvalid && !errors.Is(err, PhoneInvalid):
			t.Errorf("%s: want phone_invalid, got %v", name, err)
		}
	}
	if _, err := ParseEnvelope(ex["extra-envelope-field.json"]); !errors.Is(err, ErrDrop) {
		t.Errorf("extra envelope field: want drop, got %v", err)
	}
	var ve *VersionError
	if _, err := ParseEnvelope(ex["wrong-version.json"]); !errors.As(err, &ve) || ve.ID == "" {
		t.Errorf("wrong version: want VersionError with id, got %v", err)
	}
}

func TestEnvelopeStrictness(t *testing.T) {
	good := `{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":{}}`
	if _, err := ParseEnvelope([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		``, `null`, `[]`, `"x"`, `{}`, `not json`, good + `x`, good + `{}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":null}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":[]}`,
		`{"v":1.0,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":{}}`,
		`{"v":"1","id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":{}}`,
		`{"v":1,"id":"not-a-ulid","type":"ping","ts":1790330400000,"payload":{}}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359I","type":"ping","ts":1790330400000,"payload":{}}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"Ping","ts":1790330400000,"payload":{}}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1.5,"payload":{}}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1e3,"payload":{}}`,
		`{"V":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":{}}`,
		`{"v":1,"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":{}}`,
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":{"a":1,"a":2}}`,
	}
	for _, b := range bad {
		if _, err := ParseEnvelope([]byte(b)); !errors.Is(err, ErrDrop) {
			t.Errorf("%q: want drop, got %v", b, err)
		}
	}
}

func cmd(typ, payload string) error {
	_, err := DecodeCommand(typ, json.RawMessage(payload))
	return err
}

func TestCommandStrictness(t *testing.T) {
	cases := []struct {
		typ, payload string
		ok           bool
	}{
		{CmdPing, `{}`, true},
		{CmdPing, `{"x":1}`, false},
		{CmdSendText, `{"chatJid":"15550100002@s.whatsapp.net","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, true},
		{CmdSendText, `{"chatjid":"15550100002@s.whatsapp.net","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, false},
		{CmdSendText, `{"ChatJid":"15550100002@s.whatsapp.net","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, false},
		{CmdSendText, `{"chatJid":"15550100002@s.whatsapp.net","text":123,"outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, false},
		{CmdSendText, `{"chatJid":"15550100002@s.whatsapp.net","text":null,"outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, false},
		{CmdSendText, `{"chatJid":"status@broadcast","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, false},
		{CmdSendText, `{"chatJid":"120363000000000001@newsletter","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, false},
		{CmdSendText, `{"chatJid":"15550100002@s.whatsapp.net","text":"hi","outboxId":"01M3C03V80N87VFZS5G0J0NFEX","quotedMessageId":"../x"}`, false},
		{CmdSendText, `{"chatJid":"15550100002@s.whatsapp.net","text":"` + strings.Repeat("ب", MaxTextChars) + `","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, true},
		{CmdSendText, `{"chatJid":"15550100002@s.whatsapp.net","text":"` + strings.Repeat("a", MaxTextChars+1) + `","outboxId":"01M3C03V80N87VFZS5G0J0NFEX"}`, false},
		{CmdAck, `{"seq":0}`, false},
		{CmdAck, `{"seq":1.5}`, false},
		{CmdAck, `{"seq":"1"}`, false},
		{CmdMarkRead, `{"chatJid":"120363000000000001@g.us","messageIds":["3EB0"]}`, false},
		{CmdMarkRead, `{"chatJid":"15550100002@s.whatsapp.net","messageIds":[]}`, false},
		{CmdMarkRead, `{"chatJid":"15550100002@s.whatsapp.net","messageIds":["3EB0"]}`, true},
		{CmdFetchMedia, `{"chatJid":"15550100002@s.whatsapp.net","messageId":"3EB0","extra":1}`, false},
		{CmdPairPhone, `{"phone":"+201001234567"}`, false},
		{CmdPairPhone, `{"phone":"123456"}`, false},
		{CmdPairPhone, `{"phone":"1234567890123456"}`, false},
		{CmdPairPhone, `{"phone":"201001234567"}`, true},
	}
	for _, c := range cases {
		err := cmd(c.typ, c.payload)
		if (err == nil) != c.ok {
			p := c.payload
			if len(p) > 80 {
				p = p[:80]
			}
			t.Errorf("%s %s: ok=%v err=%v", c.typ, p, c.ok, err)
		}
	}
}

func initPayload(mod func(m map[string]any)) string {
	m := map[string]any{
		"storeKey": strings.Repeat("ab", 32), "storeDir": "/tmp/s", "mediaDir": "/tmp/m", "deviceName": "Voolo",
		"limits": map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 16 << 20, "voiceMaxSeconds": 3600},
	}
	if mod != nil {
		mod(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestInitValidation(t *testing.T) {
	lim := func(k string, v any) func(map[string]any) {
		return func(m map[string]any) { m["limits"].(map[string]any)[k] = v }
	}
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	cases := []struct {
		name string
		mod  func(map[string]any)
		ok   bool
	}{
		{"ceilings", nil, true},
		{"history 91 days", lim("historyDays", 91), false},
		{"history 0 days", lim("historyDays", 0), false},
		{"20001 per chat", lim("historyMaxPerChat", 20001), false},
		{"image above 16 MiB", lim("imageMaxBytes", 16<<20+1), false},
		{"voice above 60 min", lim("voiceMaxSeconds", 3601), false},
		{"voice bytes at ceiling", lim("voiceMaxBytes", 32<<20), true},
		{"voice bytes above ceiling", lim("voiceMaxBytes", 32<<20+1), false},
		{"voice bytes zero", lim("voiceMaxBytes", 0), false},
		{"limits extra field", lim("fetchHistory", true), false},
		{"uppercase key", set("storeKey", strings.Repeat("AB", 32)), false},
		{"relative store", set("storeDir", "store"), false},
		{"dotdot store", set("storeDir", "/tmp/../etc"), false},
		{"empty device name", set("deviceName", ""), false},
		{"long device name", set("deviceName", strings.Repeat("x", 33)), false},
		{"control char name", set("deviceName", "Voo\u0007lo"), false},
		{"missing limits", func(m map[string]any) { delete(m, "limits") }, false},
	}
	for _, c := range cases {
		if err := cmd(CmdInit, initPayload(c.mod)); (err == nil) != c.ok {
			t.Errorf("%s: ok=%v err=%v", c.name, c.ok, err)
		}
	}
	v, _ := DecodeCommand(CmdInit, json.RawMessage(initPayload(nil)))
	if got := v.(*Init).Limits.VoiceBytes(); got != MaxVoiceBytes {
		t.Errorf("default voice bytes = %d", got)
	}
}

func TestLineReaderCapsAndCounts(t *testing.T) {
	long := strings.Repeat("x", MaxLineBytes) // plus newline = one byte too many
	exact := strings.Repeat("y", MaxLineBytes-1)
	in := "a\r\n" + long + "\n" + "b\n" + exact + "\n" + "partial"
	lr := NewLineReader(strings.NewReader(in))
	var got []string
	for {
		l, err := lr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(l))
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != exact {
		t.Fatalf("lines: %d %q", len(got), got[:min(len(got), 2)])
	}
	if lr.Dropped.Load() != 2 {
		t.Fatalf("dropped = %d, want 2 (over-long + partial)", lr.Dropped.Load())
	}
}

func TestWriterRefusesOverlongLinesAndEmitsOneLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, nil)
	if err := w.Emit(EvMessage, MessageEvent{Message: Message{Text: "a\nb"}}); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatal("raw newline in output")
	}
	if err := w.Emit(EvMessage, MessageEvent{Message: Message{Text: strings.Repeat("z", MaxLineBytes)}}); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("want ErrLineTooLong, got %v", err)
	}
	env, err := ParseEnvelope(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	if err != nil || env.Type != EvMessage {
		t.Fatalf("own output does not parse: %v", err)
	}
}

func TestULIDs(t *testing.T) {
	s := NewIDSource(nil)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := s.New()
		if !IsULID(id) || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
	}
	fixed := NewIDSource(func() time.Time { return time.UnixMilli(0) })
	if id := fixed.New(); !strings.HasPrefix(id, "0000000000") {
		t.Fatalf("time part wrong: %s", id)
	}
}
