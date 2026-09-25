// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Command types (PROTOCOL.md §6). There are no others, and none will be added
// that send to several chats, schedule, repeat, template or broadcast.
const (
	CmdInit       = "init"
	CmdPairQR     = "pair_qr"
	CmdPairPhone  = "pair_phone"
	CmdLogout     = "logout"
	CmdSendText   = "send_text"
	CmdSendMedia  = "send_media"
	CmdMarkRead   = "mark_read"
	CmdFetchMedia = "fetch_media"
	CmdAck        = "ack"
	CmdPing       = "ping"
	CmdShutdown   = "shutdown"
)

// Ceilings the bridge enforces whatever the client asks for (§6.1, ADR-012 S5
// as amended by the owner on 2026-09-25).
const (
	MaxHistoryDays       = 90
	MaxHistoryPerChat    = 20000
	MaxImageBytes        = 16 << 20 // 16 MiB
	MaxVoiceSeconds      = 3600     // 60 minutes
	MaxVoiceBytes        = 32 << 20 // 32 MiB
	MaxFileBytes         = 32 << 20 // send_media kind "file"
	MaxTextChars         = 65536
	MaxNameChars         = 512
	MaxJIDLen            = 128
	MaxMarkRead          = 50
	MaxDeviceNameChars   = 32
	MaxPathBytes         = 4096
	MaxFileNameChars     = 255
	MaxCaptionChars      = 65536
	DefaultVoiceMaxBytes = MaxVoiceBytes
)

// Limits is init.limits.
type Limits struct {
	HistoryDays       int   `json:"historyDays"`
	HistoryMaxPerChat int   `json:"historyMaxPerChat"`
	ImageMaxBytes     int64 `json:"imageMaxBytes"`
	VoiceMaxSeconds   int   `json:"voiceMaxSeconds"`
	// VoiceMaxBytes is optional (added within v1 on 2026-09-25); absent means MaxVoiceBytes.
	VoiceMaxBytes *int64 `json:"voiceMaxBytes,omitempty"`
}

// VoiceBytes returns the effective voice size limit.
func (l Limits) VoiceBytes() int64 {
	if l.VoiceMaxBytes == nil {
		return DefaultVoiceMaxBytes
	}
	return *l.VoiceMaxBytes
}

// Init is the init payload.
type Init struct {
	StoreKey   string `json:"storeKey"`
	StoreDir   string `json:"storeDir"`
	MediaDir   string `json:"mediaDir"`
	DeviceName string `json:"deviceName"`
	Limits     Limits `json:"limits"`
}

// PairPhone is the pair_phone payload.
type PairPhone struct {
	Phone string `json:"phone"`
}

// SendText is the send_text payload.
type SendText struct {
	ChatJID         string `json:"chatJid"`
	Text            string `json:"text"`
	OutboxID        string `json:"outboxId"`
	QuotedMessageID string `json:"quotedMessageId,omitempty"`
}

// SendMedia is the send_media payload.
type SendMedia struct {
	ChatJID  string `json:"chatJid"`
	OutboxID string `json:"outboxId"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Key      string `json:"key"`
	SHA256   string `json:"sha256"`
	Mime     string `json:"mime"`
	Caption  string `json:"caption,omitempty"`
	FileName string `json:"fileName,omitempty"`
}

// MarkRead is the mark_read payload.
type MarkRead struct {
	ChatJID    string   `json:"chatJid"`
	MessageIDs []string `json:"messageIds"`
	SenderJID  string   `json:"senderJid,omitempty"`
}

// FetchMedia is the fetch_media payload.
type FetchMedia struct {
	ChatJID   string `json:"chatJid"`
	MessageID string `json:"messageId"`
}

// Ack is the ack payload.
type Ack struct {
	Seq int64 `json:"seq"`
}

// Empty is the payload of commands without fields.
type Empty struct{}

// fieldSpec lists the members a payload object may have; true means required.
type fieldSpec map[string]bool

type commandSpec struct {
	fields fieldSpec
	nested map[string]fieldSpec
	decode func(json.RawMessage) (any, error)
}

var limitsSpec = fieldSpec{"historyDays": true, "historyMaxPerChat": true, "imageMaxBytes": true, "voiceMaxSeconds": true, "voiceMaxBytes": false}

var commandSpecs = map[string]commandSpec{
	CmdInit: {
		fields: fieldSpec{"storeKey": true, "storeDir": true, "mediaDir": true, "deviceName": true, "limits": true},
		nested: map[string]fieldSpec{"limits": limitsSpec},
		decode: decodeAs[Init],
	},
	CmdPairQR:     {fields: fieldSpec{}, decode: decodeAs[Empty]},
	CmdPairPhone:  {fields: fieldSpec{"phone": true}, decode: decodeAs[PairPhone]},
	CmdLogout:     {fields: fieldSpec{}, decode: decodeAs[Empty]},
	CmdSendText:   {fields: fieldSpec{"chatJid": true, "text": true, "outboxId": true, "quotedMessageId": false}, decode: decodeAs[SendText]},
	CmdSendMedia:  {fields: fieldSpec{"chatJid": true, "outboxId": true, "kind": true, "path": true, "key": true, "sha256": true, "mime": true, "caption": false, "fileName": false}, decode: decodeAs[SendMedia]},
	CmdMarkRead:   {fields: fieldSpec{"chatJid": true, "messageIds": true, "senderJid": false}, decode: decodeAs[MarkRead]},
	CmdFetchMedia: {fields: fieldSpec{"chatJid": true, "messageId": true}, decode: decodeAs[FetchMedia]},
	CmdAck:        {fields: fieldSpec{"seq": true}, decode: decodeAs[Ack]},
	CmdPing:       {fields: fieldSpec{}, decode: decodeAs[Empty]},
	CmdShutdown:   {fields: fieldSpec{}, decode: decodeAs[Empty]},
}

// Commands returns the names of every v1 command.
func Commands() []string {
	out := make([]string, 0, len(commandSpecs))
	for k := range commandSpecs {
		out = append(out, k)
	}
	return out
}

// IsCommand reports whether typ is a v1 command.
func IsCommand(typ string) bool { _, ok := commandSpecs[typ]; return ok }

func decodeAs[T any](raw json.RawMessage) (any, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

// BadRequest is returned by DecodeCommand when a payload fails strict decoding.
type BadRequest struct{ Reason string }

func (e *BadRequest) Error() string { return "bad_request: " + e.Reason }

// PhoneInvalid is returned for a pair_phone number that is not E.164 without '+'.
var PhoneInvalid = errors.New("phone_invalid")

func bad(reason string) error { return &BadRequest{Reason: reason} }

func checkFields(raw json.RawMessage, spec fieldSpec, nested map[string]fieldSpec) error {
	fields, err := objectFields(raw)
	if err != nil {
		return bad("payload is not a plain object")
	}
	for name := range fields {
		if _, ok := spec[name]; !ok {
			return bad("unknown field")
		}
	}
	for name, required := range spec {
		v, ok := fields[name]
		if required && !ok {
			return bad("missing field")
		}
		if ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return bad("null value")
		}
	}
	for name, sub := range nested {
		if v, ok := fields[name]; ok {
			if err := checkFields(v, sub, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// DecodeCommand strictly decodes and validates the payload of a known command.
// It returns *BadRequest (or PhoneInvalid for pair_phone) on failure.
func DecodeCommand(typ string, payload json.RawMessage) (any, error) {
	spec, ok := commandSpecs[typ]
	if !ok {
		return nil, bad("unknown command")
	}
	// Text that is not valid UTF-8, raw or as a lone surrogate escape, is
	// refused: encoding/json would silently turn it into U+FFFD (review L2).
	if !utf8.Valid(payload) || loneSurrogate(payload) {
		return nil, bad("invalid UTF-8")
	}
	if err := checkFields(payload, spec.fields, spec.nested); err != nil {
		return nil, err
	}
	v, err := spec.decode(payload)
	if err != nil {
		return nil, bad("wrong type")
	}
	if err := validate(v); err != nil {
		return nil, err
	}
	return v, nil
}

// loneSurrogate reports whether raw JSON holds a \uXXXX escape of a UTF-16
// surrogate that is not part of a high-low pair. Outside strings a backslash
// is not valid JSON, so escapes are found without tracking string state.
func loneSurrogate(raw []byte) bool {
	hex4 := func(i int) (rune, bool) {
		if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
			return 0, false
		}
		var r rune
		for _, c := range raw[i+2 : i+6] {
			switch {
			case c >= '0' && c <= '9':
				r = r<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				r = r<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				r = r<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		r, ok := hex4(i)
		if !ok {
			i++ // a two-character escape such as \\ or \"
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			lo, ok := hex4(i + 6)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 11
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		default:
			i += 5
		}
	}
	return false
}

var (
	storeKeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex64Re    = storeKeyRe
	phoneRe    = regexp.MustCompile(`^[1-9][0-9]{6,14}$`)
	chatJIDRe  = regexp.MustCompile(`^(?:[0-9]{1,20}@s\.whatsapp\.net|[0-9]{1,20}@lid|[0-9]{1,20}(?:-[0-9]{1,20})?@g\.us)$`)
	userJIDRe  = regexp.MustCompile(`^(?:[0-9]{1,20}@s\.whatsapp\.net|[0-9]{1,20}@lid)$`)
	msgIDRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	mimeRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}(?:; ?[a-z0-9_-]{1,32}=[A-Za-z0-9_.-]{1,64})*$`)
)

// IsChatJID reports whether s is a chat JID accepted by the protocol (§3).
func IsChatJID(s string) bool { return len(s) <= MaxJIDLen && chatJIDRe.MatchString(s) }

// IsUserJID reports whether s is a person's JID (§3).
func IsUserJID(s string) bool { return len(s) <= MaxJIDLen && userJIDRe.MatchString(s) }

// IsMessageID reports whether s is a message id (§3).
func IsMessageID(s string) bool { return msgIDRe.MatchString(s) }

// IsGroupJID reports whether s is a group chat JID.
func IsGroupJID(s string) bool { return strings.HasSuffix(s, "@g.us") }

func textOK(s string, minChars, maxChars int) bool {
	if !utf8.ValidString(s) {
		return false
	}
	n := utf8.RuneCountInString(s)
	return n >= minChars && n <= maxChars
}

func cleanAbs(p string) (string, bool) {
	if p == "" || len(p) > MaxPathBytes || strings.ContainsRune(p, 0) || !filepath.IsAbs(p) {
		return "", false
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", false
		}
	}
	return filepath.Clean(p), true
}

func validate(v any) error {
	switch c := v.(type) {
	case *Init:
		if !storeKeyRe.MatchString(c.StoreKey) {
			return bad("storeKey")
		}
		var ok bool
		if c.StoreDir, ok = cleanAbs(c.StoreDir); !ok {
			return bad("storeDir")
		}
		if c.MediaDir, ok = cleanAbs(c.MediaDir); !ok {
			return bad("mediaDir")
		}
		if !textOK(c.DeviceName, 1, MaxDeviceNameChars) || strings.IndexFunc(c.DeviceName, unicode.IsControl) >= 0 {
			return bad("deviceName")
		}
		l := &c.Limits
		if l.HistoryDays < 1 || l.HistoryDays > MaxHistoryDays ||
			l.HistoryMaxPerChat < 1 || l.HistoryMaxPerChat > MaxHistoryPerChat ||
			l.ImageMaxBytes < 1 || l.ImageMaxBytes > MaxImageBytes ||
			l.VoiceMaxSeconds < 1 || l.VoiceMaxSeconds > MaxVoiceSeconds ||
			(l.VoiceMaxBytes != nil && (*l.VoiceMaxBytes < 1 || *l.VoiceMaxBytes > MaxVoiceBytes)) {
			return bad("limits")
		}
	case *PairPhone:
		if !phoneRe.MatchString(c.Phone) {
			return PhoneInvalid
		}
	case *SendText:
		if !IsChatJID(c.ChatJID) || !IsULID(c.OutboxID) || !textOK(c.Text, 1, MaxTextChars) {
			return bad("send_text")
		}
		if c.QuotedMessageID != "" && !IsMessageID(c.QuotedMessageID) {
			return bad("quotedMessageId")
		}
	case *SendMedia:
		if !IsChatJID(c.ChatJID) || !IsULID(c.OutboxID) {
			return bad("send_media")
		}
		switch c.Kind {
		case "image", "voice", "file":
		default:
			return bad("kind")
		}
		var ok bool
		if c.Path, ok = cleanAbs(c.Path); !ok {
			return bad("path")
		}
		if !hex64Re.MatchString(c.Key) || !hex64Re.MatchString(c.SHA256) || !mimeRe.MatchString(c.Mime) {
			return bad("key, sha256 or mime")
		}
		if c.Caption != "" && !textOK(c.Caption, 1, MaxCaptionChars) {
			return bad("caption")
		}
		if c.FileName != "" && (!textOK(c.FileName, 1, MaxFileNameChars) || strings.ContainsAny(c.FileName, "/\\\x00") ||
			c.FileName == "." || c.FileName == ".." || strings.IndexFunc(c.FileName, unicode.IsControl) >= 0) {
			return bad("fileName")
		}
	case *MarkRead:
		if !IsChatJID(c.ChatJID) || len(c.MessageIDs) < 1 || len(c.MessageIDs) > MaxMarkRead {
			return bad("mark_read")
		}
		for _, id := range c.MessageIDs {
			if !IsMessageID(id) {
				return bad("messageIds")
			}
		}
		if c.SenderJID != "" && !IsUserJID(c.SenderJID) {
			return bad("senderJid")
		}
		if IsGroupJID(c.ChatJID) && c.SenderJID == "" {
			return bad("senderJid required in groups")
		}
	case *FetchMedia:
		if !IsChatJID(c.ChatJID) || !IsMessageID(c.MessageID) {
			return bad("fetch_media")
		}
	case *Ack:
		if c.Seq < 1 {
			return bad("seq")
		}
	}
	return nil
}
