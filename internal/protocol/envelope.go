// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package protocol implements the wire format of PROTOCOL.md v1: JSON lines on
// stdin/stdout, the five-field envelope, strict decoding of commands and
// encoding of events.
package protocol

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the protocol version (PROTOCOL.md §2).
const Version = 1

// MaxLineBytes is the maximum size of one line, newline included (§1).
const MaxLineBytes = 1 << 20

var (
	typeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
)

// IsULID reports whether s is a 26-character Crockford base32 ULID.
func IsULID(s string) bool { return ulidRe.MatchString(s) }

// Envelope is one decoded line.
type Envelope struct {
	V       int64
	ID      string
	Type    string
	TS      int64
	Payload json.RawMessage
}

// ErrDrop means the line is not a valid envelope and must be dropped and counted.
var ErrDrop = errors.New("invalid envelope")

// VersionError means the envelope is well-formed but v is not 1.
type VersionError struct{ ID string }

func (e *VersionError) Error() string { return "unsupported version" }

var envelopeFields = []string{"v", "id", "type", "ts", "payload"}

// ParseEnvelope decodes one line (without its newline). It returns ErrDrop for
// anything that is not exactly a five-field envelope with the right types, and
// a *VersionError when only the version is wrong.
func ParseEnvelope(line []byte) (Envelope, error) {
	var env Envelope
	fields, err := objectFields(line)
	if err != nil || len(fields) != len(envelopeFields) {
		return env, ErrDrop
	}
	for _, k := range envelopeFields {
		if _, ok := fields[k]; !ok {
			return env, ErrDrop
		}
	}
	v, ok := jsonInt(fields["v"])
	if !ok {
		return env, ErrDrop
	}
	if err := json.Unmarshal(fields["id"], &env.ID); err != nil || !IsULID(env.ID) {
		return env, ErrDrop
	}
	if err := json.Unmarshal(fields["type"], &env.Type); err != nil || !typeRe.MatchString(env.Type) {
		return env, ErrDrop
	}
	ts, ok := jsonInt(fields["ts"])
	if !ok {
		return env, ErrDrop
	}
	p := bytes.TrimSpace(fields["payload"])
	if len(p) == 0 || p[0] != '{' {
		return env, ErrDrop
	}
	if _, err := objectFields(p); err != nil {
		return env, ErrDrop
	}
	env.V, env.TS, env.Payload = v, ts, p
	if v != Version {
		return env, &VersionError{ID: env.ID}
	}
	return env, nil
}

// jsonInt parses a JSON number that is written as an integer (no fraction or exponent).
func jsonInt(raw json.RawMessage) (int64, bool) {
	s := string(bytes.TrimSpace(raw))
	if s == "" || s == "-" {
		return 0, false
	}
	for i, c := range s {
		if !(c >= '0' && c <= '9') && !(i == 0 && c == '-') {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// objectFields returns the members of a JSON object, refusing duplicate names,
// trailing data and non-objects. Names are compared exactly (case-sensitive).
func objectFields(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not an object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := tok.(string)
		if !ok {
			return nil, errors.New("bad member name")
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("duplicate member")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out[name] = raw
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return out, nil
}

// LineReader splits a stream into lines of at most MaxLineBytes (newline
// included). Longer lines are discarded up to the next newline and counted.
// A trailing partial line at end of input is discarded.
type LineReader struct {
	r       *bufio.Reader
	max     int
	Dropped atomic.Int64
}

// NewLineReader wraps r.
func NewLineReader(r io.Reader) *LineReader {
	return &LineReader{r: bufio.NewReaderSize(r, 64*1024), max: MaxLineBytes}
}

// Next returns the next line without its trailing "\n" or "\r\n". It returns
// io.EOF at end of input.
func (lr *LineReader) Next() ([]byte, error) {
	for {
		var buf []byte
		tooLong := false
		for {
			chunk, err := lr.r.ReadSlice('\n')
			if !tooLong {
				if len(buf)+len(chunk) > lr.max {
					tooLong = true
					buf = nil
				} else {
					buf = append(buf, chunk...)
				}
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if err == io.EOF {
					if len(buf) > 0 || tooLong {
						lr.Dropped.Add(1)
					}
					return nil, io.EOF
				}
				return nil, err
			}
			break
		}
		if tooLong {
			lr.Dropped.Add(1)
			continue
		}
		buf = buf[:len(buf)-1]
		if n := len(buf); n > 0 && buf[n-1] == '\r' {
			buf = buf[:n-1]
		}
		return buf, nil
	}
}

// ErrLineTooLong is returned by Writer when an encoded line would exceed MaxLineBytes.
var ErrLineTooLong = errors.New("line too long")

// Writer encodes events as lines. It is safe for concurrent use; each event is
// written with a single Write call.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
	ids *IDSource
}

// NewWriter returns a writer to w. now may be nil.
func NewWriter(w io.Writer, now func() time.Time) *Writer {
	if now == nil {
		now = time.Now
	}
	return &Writer{w: w, now: now, ids: NewIDSource(now)}
}

type outEnvelope struct {
	V       int    `json:"v"`
	ID      string `json:"id"`
	Type    string `json:"type"`
	TS      int64  `json:"ts"`
	Payload any    `json:"payload"`
}

// Encode returns the line for an event, newline included.
func (w *Writer) Encode(typ string, payload any) ([]byte, error) {
	if payload == nil {
		payload = struct{}{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(outEnvelope{V: Version, ID: w.ids.New(), Type: typ, TS: w.now().UnixMilli(), Payload: payload}); err != nil {
		return nil, err
	}
	if b.Len() > MaxLineBytes {
		return nil, ErrLineTooLong
	}
	return b.Bytes(), nil
}

// Emit writes one event line.
func (w *Writer) Emit(typ string, payload any) error {
	line, err := w.Encode(typ, payload)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(line)
	return err
}

// EncodedSize returns the JSON size of v (used to size history batches).
func EncodedSize(v any) int {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return MaxLineBytes
	}
	return b.Len() - 1
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// IDSource makes ULIDs: 48 bits of milliseconds and 80 random bits.
type IDSource struct {
	now func() time.Time
}

// NewIDSource returns an id source using now for the time part.
func NewIDSource(now func() time.Time) *IDSource {
	if now == nil {
		now = time.Now
	}
	return &IDSource{now: now}
}

// New returns a fresh ULID.
func (s *IDSource) New() string {
	var b [16]byte
	ms := uint64(s.now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic("crypto/rand failed")
	}
	// 128 bits -> 26 base32 characters (the first carries 3 bits).
	var out [26]byte
	var acc uint64
	var bits uint
	idx := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 && idx >= 0 {
			out[idx] = crockford[acc&31]
			acc >>= 5
			bits -= 5
			idx--
		}
	}
	if idx == 0 {
		out[0] = crockford[acc&31]
	}
	return string(out[:])
}
