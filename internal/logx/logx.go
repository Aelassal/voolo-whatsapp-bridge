// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package logx writes content-free diagnostics to stderr (PROTOCOL.md §10.4).
//
// Every line is a JSON object {"t","level","event","code"?,"n"?}. Event names
// come from a fixed list (Events, PROTOCOL.md §10.4); any other name is
// written as "invalid_event". Codes are fixed strings chosen by this program;
// anything that could carry user content (message text, names, phone numbers,
// JIDs, QR strings, pairing codes, keys, paths) is never passed in. As a second
// line of defence, codes are scrubbed: characters outside a small set are
// removed, and a code that looks like it holds an address, a number sequence,
// a hex or base64 run (a key, a hash, an id) is replaced by "redacted".
package logx

import (
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level of a diagnostic line.
type Level string

// Levels.
const (
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

// Logger writes diagnostic lines. It is safe for concurrent use.
type Logger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// New returns a logger writing to w. now may be nil (time.Now).
func New(w io.Writer, now func() time.Time) *Logger {
	if now == nil {
		now = time.Now
	}
	return &Logger{w: w, now: now}
}

// Discard is a logger that writes nothing.
func Discard() *Logger { return New(io.Discard, nil) }

type line struct {
	T     int64  `json:"t"`
	Level Level  `json:"level"`
	Event string `json:"event"`
	Code  string `json:"code,omitempty"`
	N     *int64 `json:"n,omitempty"`
}

// events is the complete list of event names (PROTOCOL.md §10.4). It is a
// constant of the program: a name built at run time is never written.
var events = map[string]bool{}

func init() {
	for _, e := range []string{
		// process and stdio
		"started", "stdin_eof", "shutdown", "init_timeout", "line_dropped", "emit_failed", "panic", "panic_recovered",
		"stop_timeout", "exit_logged_out",
		// init and store
		"ready", "fatal", "store_open_failed", "store_close_failed", "store_wiped", "store_wipe_failed", "store_write_failed",
		"media_prune_failed", "media_stale_deleted",
		// commands and connection
		"command_refused", "status", "signal", "connect_failed", "host_blocked", "logged_out",
		"pairing_started", "pairing_failed", "pairing_timeout", "paired",
		"version_refreshed", "version_refresh_failed",
		"send_ok", "send_failed", "send_refused", "fetch_failed",
		"group_info_failed", "joined_groups_failed",
		"history_capped", "history_done", "history_download_failed",
		// whatsmeow's own warnings and errors (constant format string as code)
		"whatsmeow",
	} {
		events[e] = true
	}
}

// Events returns the fixed event names, for documentation and tests.
func Events() []string {
	out := make([]string, 0, len(events))
	for e := range events {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// Opt adds an optional field to a line.
type Opt func(*line)

// codes is the complete list of values Code may write: this package's own
// words plus the protocol's codes, registered by AllowCodes at start-up.
var (
	codesMu sync.RWMutex
	codes   = map[string]bool{
		"too_long": true, "invalid": true, "event": true, "history": true, "command": true,
		"alias": true, "chats": true, "media": true, "outbox": true, "phone": true, "qr": true, "async": true,
	}
)

// AllowCodes adds fixed codes (constants of the program, registered from
// package init functions).
func AllowCodes(cs ...string) {
	codesMu.Lock()
	defer codesMu.Unlock()
	for _, c := range cs {
		codes[c] = true
	}
}

// Code attaches a fixed code chosen by this program: an error code, a state,
// a signal kind, a command or event type, a short word. A value that is not
// one of the registered codes is written as "redacted", so a key, a hash, an
// id, a JID or a phone number passed as a code by mistake never appears.
func Code(c string) Opt {
	codesMu.RLock()
	ok := codes[c]
	codesMu.RUnlock()
	return func(l *line) {
		if ok {
			l.Code = c
		} else {
			l.Code = "redacted"
		}
	}
}

// Format attaches a constant format string of whatsmeow's logger, scrubbed by
// ScrubCode. Its arguments are never passed in.
func Format(f string) Opt { return func(l *line) { l.Code = ScrubCode(f) } }

// N attaches a count.
func N(n int64) Opt { return func(l *line) { v := n; l.N = &v } }

// Log writes one line. An event name that is not a fixed identifier is replaced.
func (l *Logger) Log(level Level, event string, opts ...Opt) {
	if l == nil {
		return
	}
	if !events[event] {
		event = "invalid_event"
	}
	ln := line{T: l.now().UnixMilli(), Level: level, Event: event}
	for _, o := range opts {
		o(&ln)
	}
	b, err := json.Marshal(ln)
	if err != nil {
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	_, _ = l.w.Write(b)
	l.mu.Unlock()
}

// Info writes an info line.
func (l *Logger) Info(event string, opts ...Opt) { l.Log(Info, event, opts...) }

// Warn writes a warn line.
func (l *Logger) Warn(event string, opts ...Opt) { l.Log(Warn, event, opts...) }

// Error writes an error line.
func (l *Logger) Error(event string, opts ...Opt) { l.Log(Error, event, opts...) }

const maxCode = 160

var (
	digitRun = regexp.MustCompile(`[0-9]{5,}`)
	hexRun   = regexp.MustCompile(`[0-9A-Fa-f]{16,}`)
	tokenRun = regexp.MustCompile(`[A-Za-z0-9/_=-]{16,}`)
	codeKeep = regexp.MustCompile(`[^A-Za-z0-9 _.:/%,()'\-]`)
)

// secretLike reports whether s holds something that could be a key, a hash,
// an id or an address: '@', '+', a run of five or more digits, a run of 16 or
// more hex digits, or a run of 16 or more base64 characters with a digit in
// it or with both upper and lower case letters.
func secretLike(s string) bool {
	if strings.ContainsAny(s, "@+") || digitRun.MatchString(s) || hexRun.MatchString(s) {
		return true
	}
	for _, t := range tokenRun.FindAllString(s, -1) {
		if strings.ContainsAny(t, "0123456789") || (strings.ToLower(t) != t && strings.ToUpper(t) != t) {
			return true
		}
	}
	return false
}

// ScrubCode reduces s to a safe diagnostic code. It keeps letters, digits and a
// few punctuation marks, caps the length, and replaces the whole value with
// "redacted" when it looks like it holds a secret or an address (secretLike),
// before or after the other characters are removed.
func ScrubCode(s string) string {
	if secretLike(s) {
		return "redacted"
	}
	s = codeKeep.ReplaceAllString(s, "")
	if secretLike(s) {
		return "redacted"
	}
	if len(s) > maxCode {
		s = s[:maxCode]
	}
	return strings.TrimSpace(s)
}
