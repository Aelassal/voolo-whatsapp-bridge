// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package protocol

// Event types (PROTOCOL.md §5).
const (
	EvHello        = "hello"
	EvReady        = "ready"
	EvStatus       = "status"
	EvSignal       = "signal"
	EvQR           = "qr"
	EvPairCode     = "pair_code"
	EvPaired       = "paired"
	EvPairFailed   = "pair_failed"
	EvLoggedOut    = "logged_out"
	EvSyncProgress = "sync_progress"
	EvChat         = "chat"
	EvChatUpdate   = "chat_update"
	EvContact      = "contact"
	EvGroup        = "group"
	EvMessage      = "message"
	EvMsgUpdate    = "message_update"
	EvReaction     = "reaction"
	EvReceipt      = "receipt"
	EvHistoryBatch = "history_batch"
	EvMediaReady   = "media_ready"
	EvSendResult   = "send_result"
	EvOK           = "ok"
	EvPong         = "pong"
	EvError        = "error"
)

// Hello is §5.1.
type Hello struct {
	Bridge   string `json:"bridge"`
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	// Features lists what this bridge offers beyond the first release of v1
	// (revision 2, PROTOCOL.md §5.1). A client uses a feature only when listed.
	Features []string `json:"features,omitempty"`
}

// Features of revision 2 (PROTOCOL.md §5.1), in the order they were added.
const (
	FeatureReplyContext = "reply_context"
	FeatureForward      = "forward"
	FeatureSendVideo    = "send_video"
	FeatureFetchAll     = "fetch_all_media"
)

// Features returns the features this bridge offers.
func Features() []string {
	return []string{FeatureReplyContext, FeatureForward, FeatureSendVideo, FeatureFetchAll}
}

// Account is ready.account.
type Account struct {
	JID      string `json:"jid"`
	PushName string `json:"pushName,omitempty"`
}

// Ready is §5.2.
type Ready struct {
	ReplyTo string   `json:"replyTo"`
	Paired  bool     `json:"paired"`
	Account *Account `json:"account,omitempty"`
}

// Connection states (§5.3).
const (
	StateUnpaired     = "unpaired"
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateReconnecting = "reconnecting"
	StateStopped      = "stopped"
)

// Status is §5.3.
type Status struct {
	State string `json:"state"`
}

// Signal kinds (§5.4).
const (
	SigRateLimited      = "rate_limited"
	SigTempBanned       = "temp_banned"
	SigStreamReplaced   = "stream_replaced"
	SigClientOutdated   = "client_outdated"
	SigConnectFailure   = "connect_failure"
	SigStreamError      = "stream_error"
	SigKeepaliveTimeout = "keepalive_timeout"
)

// Signal is §5.4. Code is a number for temp_banned/connect_failure/rate_limited
// and a string for stream_error, so it is typed any.
type Signal struct {
	Kind        string `json:"kind"`
	Code        any    `json:"code,omitempty"`
	ExpiresInMs int64  `json:"expiresInMs,omitempty"`
}

// QR is §5.5.
type QR struct {
	ReplyTo   string `json:"replyTo"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
	Index     int    `json:"index"`
}

// PairCode is §5.6.
type PairCode struct {
	ReplyTo string `json:"replyTo"`
	Code    string `json:"code"`
}

// Paired is §5.7.
type Paired struct {
	JID          string `json:"jid"`
	LID          string `json:"lid,omitempty"`
	PushName     string `json:"pushName,omitempty"`
	BusinessName string `json:"businessName,omitempty"`
}

// Pair failure reasons (§5.8).
const (
	PairTimeout        = "timeout"
	PairRejected       = "rejected"
	PairClientOutdated = "client_outdated"
	PairError          = "error"
)

// PairFailed is §5.8.
type PairFailed struct {
	ReplyTo string `json:"replyTo"`
	Reason  string `json:"reason"`
}

// Logout reasons (§5.9).
const (
	LogoutUser          = "user"
	LogoutDeviceRemoved = "device_removed"
	LogoutPrimaryGone   = "primary_gone"
	LogoutBanned        = "banned"
	LogoutUnknown       = "unknown"
)

// LoggedOut is §5.9.
type LoggedOut struct {
	Reason string `json:"reason"`
	Code   int    `json:"code,omitempty"`
}

// SyncProgress is §5.10.
type SyncProgress struct {
	Kind     string `json:"kind"`
	Progress *int   `json:"progress,omitempty"`
	Done     bool   `json:"done"`
}

// Chat is the chat object of §5.11.
type Chat struct {
	JID           string `json:"jid"`
	Kind          string `json:"kind"`
	Name          string `json:"name,omitempty"`
	UnreadCount   *int   `json:"unreadCount,omitempty"`
	Pinned        *bool  `json:"pinned,omitempty"`
	MutedUntil    int64  `json:"mutedUntil,omitempty"`
	Archived      *bool  `json:"archived,omitempty"`
	LastMessageAt int64  `json:"lastMessageAt,omitempty"`
}

// ChatEvent is §5.11.
type ChatEvent struct {
	Chat Chat `json:"chat"`
}

// ChatUpdate is §5.12.
type ChatUpdate struct {
	JID         string  `json:"jid"`
	Name        *string `json:"name,omitempty"`
	UnreadCount *int    `json:"unreadCount,omitempty"`
	MarkedRead  *bool   `json:"markedRead,omitempty"`
	Pinned      *bool   `json:"pinned,omitempty"`
	MutedUntil  *int64  `json:"mutedUntil,omitempty"`
	Archived    *bool   `json:"archived,omitempty"`
	AliasOf     string  `json:"aliasOf,omitempty"`
}

// Contact is §5.13.
type Contact struct {
	JID          string `json:"jid"`
	Name         string `json:"name,omitempty"`
	PushName     string `json:"pushName,omitempty"`
	BusinessName string `json:"businessName,omitempty"`
}

// Participant is one group member.
type Participant struct {
	JID     string `json:"jid"`
	IsAdmin bool   `json:"isAdmin,omitempty"`
}

// MaxParticipants is the cap on group.participants.
const MaxParticipants = 2048

// Group is §5.14.
type Group struct {
	JID          string        `json:"jid"`
	Name         string        `json:"name"`
	Topic        string        `json:"topic,omitempty"`
	Participants []Participant `json:"participants"`
}

// Message kinds (§5.15).
const (
	KindText        = "text"
	KindImage       = "image"
	KindVideo       = "video"
	KindSticker     = "sticker"
	KindVoice       = "voice"
	KindAudio       = "audio"
	KindDocument    = "document"
	KindLocation    = "location"
	KindContact     = "contact"
	KindPoll        = "poll"
	KindSystem      = "system"
	KindUnsupported = "unsupported"
)

// Quoted is message.quoted.
type Quoted struct {
	ID        string `json:"id"`
	SenderJID string `json:"senderJid,omitempty"`
}

// Media is a media descriptor (a description only).
type Media struct {
	Mime      string `json:"mime"`
	SizeBytes int64  `json:"sizeBytes"`
	DurationS int    `json:"durationS,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	FileName  string `json:"fileName,omitempty"`
}

// MaxMentions caps message.mentions.
const MaxMentions = 64

// Message is §5.15.
type Message struct {
	ChatJID   string   `json:"chatJid"`
	ID        string   `json:"id"`
	SenderJID string   `json:"senderJid"`
	FromMe    bool     `json:"fromMe"`
	TS        int64    `json:"ts"`
	Kind      string   `json:"kind"`
	Text      string   `json:"text,omitempty"`
	PushName  string   `json:"pushName,omitempty"`
	Quoted    *Quoted  `json:"quoted,omitempty"`
	Mentions  []string `json:"mentions,omitempty"`
	Forwarded bool     `json:"forwarded,omitempty"`
	Edited    bool     `json:"edited,omitempty"`
	Media     *Media   `json:"media,omitempty"`
}

// MessageEvent is the message event payload.
type MessageEvent struct {
	Message Message `json:"message"`
}

// MessageUpdate is §5.16.
type MessageUpdate struct {
	ChatJID   string `json:"chatJid"`
	MessageID string `json:"messageId"`
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	At        int64  `json:"at"`
	By        string `json:"by,omitempty"`
}

// Reaction is §5.17.
type Reaction struct {
	ChatJID   string `json:"chatJid"`
	MessageID string `json:"messageId"`
	SenderJID string `json:"senderJid"`
	Emoji     string `json:"emoji"`
	At        int64  `json:"at"`
}

// Receipt kinds (§5.18).
const (
	ReceiptDelivered = "delivered"
	ReceiptRead      = "read"
	ReceiptPlayed    = "played"
	ReceiptReadSelf  = "read_self"
)

// MaxReceiptIDs caps receipt.messageIds.
const MaxReceiptIDs = 256

// Receipt is §5.18.
type Receipt struct {
	ChatJID    string   `json:"chatJid"`
	MessageIDs []string `json:"messageIds"`
	Kind       string   `json:"kind"`
	SenderJID  string   `json:"senderJid,omitempty"`
	At         int64    `json:"at"`
}

// History batch limits (§7).
const (
	MaxBatchMessages = 200
	MaxBatchBytes    = 786432
)

// HistoryBatch is §5.19.
type HistoryBatch struct {
	Seq      int64     `json:"seq"`
	SyncType string    `json:"syncType"`
	Chat     Chat      `json:"chat"`
	Messages []Message `json:"messages"`
	Progress *int      `json:"progress,omitempty"`
}

// MediaReady is §5.20.
type MediaReady struct {
	ReplyTo   string `json:"replyTo"`
	MessageID string `json:"messageId"`
	Path      string `json:"path"`
	Key       string `json:"key"`
	SHA256    string `json:"sha256"`
	Mime      string `json:"mime"`
	SizeBytes int64  `json:"sizeBytes"`
	DurationS int    `json:"durationS,omitempty"`
}

// SendResult is §5.21.
type SendResult struct {
	ReplyTo   string `json:"replyTo"`
	OutboxID  string `json:"outboxId"`
	MessageID string `json:"messageId"`
	At        int64  `json:"at"`
}

// Reply is the payload of ok and pong.
type Reply struct {
	ReplyTo string `json:"replyTo"`
}

// ErrorEvent is §5.22 error.
type ErrorEvent struct {
	ReplyTo   string `json:"replyTo,omitempty"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Fatal     bool   `json:"fatal,omitempty"`
}

// Error codes (§9).
const (
	ErrBadRequest         = "bad_request"
	ErrUnknownCommand     = "unknown_command"
	ErrUnsupportedVersion = "unsupported_version"
	ErrNotInitialized     = "not_initialized"
	ErrAlreadyInitialized = "already_initialized"
	ErrStoreKeyInvalid    = "store_key_invalid"
	ErrStoreLocked        = "store_locked"
	ErrStoreIO            = "store_io"
	ErrNotPaired          = "not_paired"
	ErrAlreadyPaired      = "already_paired"
	ErrPhoneInvalid       = "phone_invalid"
	ErrPairFailed         = "pair_failed"
	ErrNotConnected       = "not_connected"
	ErrBusy               = "busy"
	ErrDuplicateOutboxID  = "duplicate_outbox_id"
	ErrUnknownChat        = "unknown_chat"
	ErrUnknownMessage     = "unknown_message"
	ErrRateLimited        = "rate_limited"
	ErrSendFailed         = "send_failed"
	ErrTimeout            = "timeout"
	ErrMediaTooLarge      = "media_too_large"
	ErrMediaExpired       = "media_expired"
	ErrMediaUnavailable   = "media_unavailable"
	ErrMediaInvalid       = "media_invalid"
	ErrHostBlocked        = "host_blocked"
	ErrClientOutdated     = "client_outdated"
	ErrInternal           = "internal"
	// ErrRateLimitedLocal is the bridge's own send backstop (§6.5): not a
	// WhatsApp answer, and nothing was sent.
	ErrRateLimitedLocal = "rate_limited_local"
)

var retryable = map[string]bool{
	ErrStoreIO:      true,
	ErrPairFailed:   true,
	ErrNotConnected: true,
	ErrBusy:         true,
	ErrRateLimited:  true,

	ErrRateLimitedLocal: true,
}

// ErrorCodes returns every error code of §9.
func ErrorCodes() []string {
	return []string{ErrBadRequest, ErrUnknownCommand, ErrUnsupportedVersion, ErrNotInitialized, ErrAlreadyInitialized,
		ErrStoreKeyInvalid, ErrStoreLocked, ErrStoreIO, ErrNotPaired, ErrAlreadyPaired, ErrPhoneInvalid, ErrPairFailed,
		ErrNotConnected, ErrBusy, ErrDuplicateOutboxID, ErrUnknownChat, ErrUnknownMessage, ErrRateLimited, ErrRateLimitedLocal,
		ErrSendFailed, ErrTimeout, ErrMediaTooLarge, ErrMediaExpired, ErrMediaUnavailable, ErrMediaInvalid, ErrHostBlocked,
		ErrClientOutdated, ErrInternal}
}

// Retryable reports the §9 "retryable" column for a code.
func Retryable(code string) bool { return retryable[code] }

// NewError builds an error payload with the §9 retryable flag.
func NewError(replyTo, code string) ErrorEvent {
	return ErrorEvent{ReplyTo: replyTo, Code: code, Retryable: Retryable(code)}
}
