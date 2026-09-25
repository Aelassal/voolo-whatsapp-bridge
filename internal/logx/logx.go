// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package logx writes content-free diagnostics to stderr (PROTOCOL.md §10.4).
//
// Every line is a JSON object {"t","level","event","code"?,"n"?}. Event names
// and codes are fixed strings chosen by this program; anything that could carry
// user content (message text, names, phone numbers, JIDs, QR strings, pairing
// codes, keys, paths) is never passed in. As a second line of defence, codes are
// scrubbed: characters outside a small set are removed, and a code that looks
// like it holds an address or a number sequence is replaced by "redacted".
package logx

import (
	"encoding/json"
	"io"
	"regexp"
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

var eventRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// Opt adds an optional field to a line.
type Opt func(*line)

// Code attaches a fixed code (an error code, a state, a constant format string).
func Code(c string) Opt { return func(l *line) { l.Code = ScrubCode(c) } }

// N attaches a count.
func N(n int64) Opt { return func(l *line) { v := n; l.N = &v } }

// Log writes one line. An event name that is not a fixed identifier is replaced.
func (l *Logger) Log(level Level, event string, opts ...Opt) {
	if l == nil {
		return
	}
	if !eventRe.MatchString(event) {
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
	codeKeep = regexp.MustCompile(`[^A-Za-z0-9 _.:/%,()'\-]`)
)

// ScrubCode reduces s to a safe diagnostic code. It keeps letters, digits and a
// few punctuation marks, caps the length, and replaces the whole value with
// "redacted" when it contains '@', '+' followed by digits, or a run of five or
// more digits (a phone number, a JID or an id could hide there).
func ScrubCode(s string) string {
	if strings.ContainsAny(s, "@+") || digitRun.MatchString(s) {
		return "redacted"
	}
	s = codeKeep.ReplaceAllString(s, "")
	if len(s) > maxCode {
		s = s[:maxCode]
	}
	return strings.TrimSpace(s)
}
