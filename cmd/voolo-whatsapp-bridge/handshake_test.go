// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

const key1 = "0000000000000000000000000000000000000000000000000000000000000001"

type proc struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan protocol.Envelope
	raw    *safeBuf
	stderr *safeBuf
	done   chan int
	ids    *protocol.IDSource
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func start(t *testing.T, initTimeout time.Duration) *proc {
	t.Helper()
	store.RestrictUmask()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &proc{t: t, in: inW, lines: make(chan protocol.Envelope, 1000), raw: &safeBuf{}, stderr: &safeBuf{}, done: make(chan int, 1), ids: protocol.NewIDSource(nil)}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 2<<20), 2<<20)
		for sc.Scan() {
			_, _ = p.raw.Write(append(append([]byte(nil), sc.Bytes()...), '\n'))
			env, err := protocol.ParseEnvelope(sc.Bytes())
			if err != nil {
				t.Errorf("stdout carried a non-protocol line: %q", sc.Text())
				continue
			}
			p.lines <- env
		}
		close(p.lines)
	}()
	go func() {
		code := run(nil, inR, outW, p.stderr, deps{initTimeout: initTimeout})
		outW.Close()
		p.done <- code
	}()
	t.Cleanup(func() { inW.Close() })
	return p
}

func (p *proc) send(line string) { _, _ = io.WriteString(p.in, line+"\n") }

func (p *proc) cmd(typ string, payload any) string {
	id := p.ids.New()
	b, _ := json.Marshal(payload)
	p.send(fmt.Sprintf(`{"v":1,"id":%q,"type":%q,"ts":1790330400000,"payload":%s}`, id, typ, b))
	return id
}

func (p *proc) next() protocol.Envelope {
	p.t.Helper()
	select {
	case e, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("stdout closed; output:\n%s", p.raw.String())
		}
		return e
	case <-time.After(10 * time.Second):
		p.t.Fatalf("timeout; output:\n%s", p.raw.String())
	}
	return protocol.Envelope{}
}

func (p *proc) expect(typ string) protocol.Envelope {
	p.t.Helper()
	for {
		if e := p.next(); e.Type == typ {
			return e
		}
	}
}

func (p *proc) exit() int {
	p.t.Helper()
	select {
	case c := <-p.done:
		return c
	case <-time.After(10 * time.Second):
		p.t.Fatal("bridge did not exit")
	}
	return -1
}

func payload[T any](e protocol.Envelope) T {
	var v T
	_ = json.Unmarshal(e.Payload, &v)
	return v
}

func initPayload(storeDir, mediaDir, key string) map[string]any {
	return map[string]any{"storeKey": key, "storeDir": storeDir, "mediaDir": mediaDir, "deviceName": "Voolo",
		"limits": map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 16 << 20, "voiceMaxSeconds": 3600}}
}

func TestHelloIsFirst(t *testing.T) {
	p := start(t, 5*time.Second)
	e := p.next()
	h := payload[protocol.Hello](e)
	if e.Type != "hello" || h.Bridge != "voolo-whatsapp-bridge" || h.Protocol != 1 || h.OS != runtime.GOOS || h.Arch != runtime.GOARCH || h.Version == "" {
		t.Fatalf("first line %s %s", e.Type, e.Payload)
	}
	p.in.Close()
	if c := p.exit(); c != 0 {
		t.Fatalf("exit %d", c)
	}
}

func TestBeforeInitAndGarbage(t *testing.T) {
	p := start(t, 5*time.Second)
	p.expect("hello")
	for _, g := range []string{"", "garbage", "{}", "[1,2]", `{"v":1}`, strings.Repeat("x", 100),
		`{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1,"payload":{},"extra":1}`, "\x00\xff\xfe"} {
		p.send(g)
	}
	p.send(strings.Repeat("y", protocol.MaxLineBytes+10)) // over-long: dropped
	id := p.cmd("ping", map[string]any{})
	e := p.next()
	if e.Type != "pong" || payload[protocol.Reply](e).ReplyTo != id {
		t.Fatalf("garbage produced output or ping failed: %s %s", e.Type, e.Payload)
	}
	id = p.cmd("send_text", map[string]any{"chatJid": "15550100002@s.whatsapp.net", "text": "hi", "outboxId": "01M3C03V80N87VFZS5G0J0NFEX"})
	if e := payload[protocol.ErrorEvent](p.expect("error")); e.Code != "not_initialized" || e.ReplyTo != id {
		t.Fatalf("%+v", e)
	}
	id = p.cmd("send_bulk", map[string]any{})
	if e := payload[protocol.ErrorEvent](p.expect("error")); e.Code != "unknown_command" || e.ReplyTo != id {
		t.Fatalf("%+v", e)
	}
	p.send(`{"v":2,"id":"01M3C03X59KPWHW34P8YXK68WH","type":"ping","ts":1,"payload":{}}`)
	if e := payload[protocol.ErrorEvent](p.expect("error")); e.Code != "unsupported_version" || e.ReplyTo != "01M3C03X59KPWHW34P8YXK68WH" {
		t.Fatalf("%+v", e)
	}
	p.in.Close()
	if c := p.exit(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	if !strings.Contains(p.stderr.String(), `"line_dropped"`) {
		t.Fatalf("drops not counted on stderr:\n%s", p.stderr.String())
	}
}

func TestInitTimeoutExits2(t *testing.T) {
	p := start(t, 200*time.Millisecond)
	p.expect("hello")
	p.cmd("ping", map[string]any{}) // a ping does not count as init
	if c := p.exit(); c != 2 {
		t.Fatalf("exit %d, want 2", c)
	}
}

func TestInitReadyThenEOFExitsCleanly(t *testing.T) {
	dir := t.TempDir()
	sd, md := filepath.Join(dir, "store"), filepath.Join(dir, "media")
	p := start(t, 5*time.Second)
	p.expect("hello")
	id := p.cmd("init", initPayload(sd, md, key1))
	r := payload[protocol.Ready](p.expect("ready"))
	if r.ReplyTo != id || r.Paired {
		t.Fatalf("ready %+v", r)
	}
	if s := payload[protocol.Status](p.expect("status")); s.State != "unpaired" {
		t.Fatalf("status %+v", s)
	}
	p.in.Close() // Voolo crashed: stdin EOF
	if s := payload[protocol.Status](p.expect("status")); s.State != "stopped" {
		t.Fatalf("status %+v", s)
	}
	if c := p.exit(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	// The store was closed and unlocked: it opens again with the same key.
	p2 := start(t, 5*time.Second)
	p2.expect("hello")
	p2.cmd("init", initPayload(sd, md, key1))
	p2.expect("ready")
	id = p2.cmd("shutdown", map[string]any{})
	if s := payload[protocol.Status](p2.expect("status")); s.State != "unpaired" {
		t.Fatalf("status %+v", s)
	}
	if s := payload[protocol.Status](p2.expect("status")); s.State != "stopped" {
		t.Fatalf("status %+v", s)
	}
	if e := p2.expect("ok"); payload[protocol.Reply](e).ReplyTo != id {
		t.Fatal("shutdown ok")
	}
	if c := p2.exit(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	// Wrong key on the existing store: fatal error, exit 3, store untouched.
	before, _ := os.ReadFile(filepath.Join(sd, store.DBFile))
	p3 := start(t, 5*time.Second)
	p3.expect("hello")
	id = p3.cmd("init", initPayload(sd, md, strings.Repeat("ab", 32)))
	e := payload[protocol.ErrorEvent](p3.expect("error"))
	if e.Code != "store_key_invalid" || !e.Fatal || e.ReplyTo != id {
		t.Fatalf("%+v", e)
	}
	if c := p3.exit(); c != 3 {
		t.Fatalf("exit %d, want 3", c)
	}
	after, _ := os.ReadFile(filepath.Join(sd, store.DBFile))
	if !bytes.Equal(before, after) {
		t.Fatal("store changed by a wrong key")
	}
	for _, out := range []string{p.stderr.String(), p2.stderr.String(), p3.stderr.String()} {
		checkStderr(t, out, key1, strings.Repeat("ab", 32), sd, md)
	}
}

func TestShutdownWithBadPayloadIsRefused(t *testing.T) {
	p := start(t, 5*time.Second)
	p.expect("hello")
	id := p.cmd("shutdown", map[string]any{"force": true})
	if e := payload[protocol.ErrorEvent](p.expect("error")); e.Code != "bad_request" || e.ReplyTo != id {
		t.Fatalf("%+v", e)
	}
	id = p.cmd("ping", map[string]any{})
	if payload[protocol.Reply](p.expect("pong")).ReplyTo != id {
		t.Fatal("still running")
	}
	p.cmd("shutdown", map[string]any{})
	p.expect("ok")
	if c := p.exit(); c != 0 {
		t.Fatalf("exit %d", c)
	}
}

// stderr carries only {"t","level","event","code"?,"n"?} lines with no content.
func checkStderr(t *testing.T, out string, secrets ...string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stderr line is not JSON: %q", line)
		}
		for k := range m {
			if k != "t" && k != "level" && k != "event" && k != "code" && k != "n" {
				t.Fatalf("stderr field %q in %q", k, line)
			}
		}
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Fatalf("stderr contains %q", s)
		}
	}
}

func TestFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if c := run([]string{"--version"}, nil, &out, &errOut, defaultDeps()); c != 0 || !strings.Contains(out.String(), version) {
		t.Fatalf("--version: %d %q", c, out.String())
	}
	out.Reset()
	if c := run([]string{"--license"}, nil, &out, &errOut, defaultDeps()); c != 0 || !strings.Contains(out.String(), "GNU GENERAL PUBLIC LICENSE") || !strings.Contains(out.String(), "Version 3") {
		t.Fatalf("--license: %d", c)
	}
	out.Reset()
	if c := run([]string{"--source"}, nil, &out, &errOut, defaultDeps()); c != 0 || !strings.HasPrefix(out.String(), "https://github.com/Aelassal/voolo-whatsapp-bridge") {
		t.Fatalf("--source: %q", out.String())
	}
	old := version
	version = "1.2.3"
	out.Reset()
	run([]string{"--source"}, nil, &out, &errOut, defaultDeps())
	version = old
	if strings.TrimSpace(out.String()) != "https://github.com/Aelassal/voolo-whatsapp-bridge/tree/v1.2.3" {
		t.Fatalf("--source for a release: %q", out.String())
	}
	if c := run([]string{"--proxy", "x"}, nil, &out, &errOut, defaultDeps()); c != exitUsage {
		t.Fatalf("unknown flag exit %d", c)
	}
}

// The real binary: stdin EOF before init exits 0, and stdout starts with hello.
func TestBinaryStdinEOF(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "bridge")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("exit: %v\nstderr: %s", err, stderr.String())
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("EOF exit too slow")
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"type":"hello"`) {
		t.Fatalf("stdout: %q", stdout.String())
	}
	checkStderr(t, stderr.String())
}
