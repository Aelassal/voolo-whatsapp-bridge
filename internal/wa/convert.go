// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

// truncate cuts s to at most n characters, at a character boundary.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// ReportableChat reports whether a chat JID is ever reported: people and
// groups only; never broadcast lists, status or newsletters.
func ReportableChat(j types.JID) bool {
	switch j.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return j.User != ""
	}
	return false
}

// MediaKeys is what a later fetch_media needs; it never goes on the pipe.
type MediaKeys struct {
	DirectPath    string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
}

// Content is the protocol view of a message body.
type Content struct {
	Kind      string
	Text      string
	Media     *protocol.Media
	Keys      *MediaKeys
	QuotedID  string
	QuotedBy  string // raw JID string, normalised by the caller
	Mentions  []string
	Forwarded bool
}

type mediaMsg interface {
	GetDirectPath() string
	GetMediaKey() []byte
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
	GetMimetype() string
	GetFileLength() uint64
}

func withMedia(c *Content, m mediaMsg) {
	c.Media = &protocol.Media{Mime: truncate(m.GetMimetype(), 255), SizeBytes: int64(m.GetFileLength())}
	if m.GetDirectPath() != "" && len(m.GetMediaKey()) > 0 {
		c.Keys = &MediaKeys{DirectPath: m.GetDirectPath(), MediaKey: m.GetMediaKey(), FileSHA256: m.GetFileSHA256(), FileEncSHA256: m.GetFileEncSHA256()}
	}
}

func withContext(c *Content, ci *waE2E.ContextInfo) {
	if ci == nil {
		return
	}
	if id := ci.GetStanzaID(); id != "" && protocol.IsMessageID(id) {
		c.QuotedID = id
		c.QuotedBy = ci.GetParticipant()
	}
	c.Mentions = ci.GetMentionedJID()
	c.Forwarded = ci.GetIsForwarded()
}

// ContentOf maps a (unwrapped) message body. ok=false means the message
// carries nothing to show (protocol messages, reactions, poll votes, key
// distribution only): the caller handles or ignores those separately.
func ContentOf(m *waE2E.Message, viewOnce bool) (c Content, ok bool) {
	if m == nil {
		return c, false
	}
	switch {
	case m.GetProtocolMessage() != nil, m.GetReactionMessage() != nil, m.GetEncReactionMessage() != nil,
		m.GetPollUpdateMessage() != nil, m.GetKeepInChatMessage() != nil, m.GetPinInChatMessage() != nil,
		onlyMeta(m):
		return c, false
	}
	if viewOnce {
		// View-once media is never described or downloadable (as in WhatsApp Web).
		return Content{Kind: protocol.KindUnsupported}, true
	}
	switch {
	case m.Conversation != nil:
		c.Kind, c.Text = protocol.KindText, m.GetConversation()
	case m.ExtendedTextMessage != nil:
		e := m.GetExtendedTextMessage()
		c.Kind, c.Text = protocol.KindText, e.GetText()
		withContext(&c, e.GetContextInfo())
	case m.ImageMessage != nil:
		im := m.GetImageMessage()
		c.Kind, c.Text = protocol.KindImage, im.GetCaption()
		withMedia(&c, im)
		c.Media.Width, c.Media.Height = int(im.GetWidth()), int(im.GetHeight())
		withContext(&c, im.GetContextInfo())
	case m.VideoMessage != nil:
		v := m.GetVideoMessage()
		c.Kind, c.Text = protocol.KindVideo, v.GetCaption()
		withMedia(&c, v)
		c.Media.DurationS, c.Media.Width, c.Media.Height = int(v.GetSeconds()), int(v.GetWidth()), int(v.GetHeight())
		withContext(&c, v.GetContextInfo())
	case m.PtvMessage != nil:
		v := m.GetPtvMessage()
		c.Kind = protocol.KindVideo
		withMedia(&c, v)
		c.Media.DurationS = int(v.GetSeconds())
		withContext(&c, v.GetContextInfo())
	case m.AudioMessage != nil:
		a := m.GetAudioMessage()
		c.Kind = protocol.KindAudio
		if a.GetPTT() {
			c.Kind = protocol.KindVoice
		}
		withMedia(&c, a)
		c.Media.DurationS = int(a.GetSeconds())
		withContext(&c, a.GetContextInfo())
	case m.StickerMessage != nil:
		s := m.GetStickerMessage()
		c.Kind = protocol.KindSticker
		withMedia(&c, s)
		c.Media.Width, c.Media.Height = int(s.GetWidth()), int(s.GetHeight())
		withContext(&c, s.GetContextInfo())
	case m.DocumentMessage != nil:
		d := m.GetDocumentMessage()
		c.Kind, c.Text = protocol.KindDocument, d.GetCaption()
		withMedia(&c, d)
		c.Media.FileName = truncate(d.GetFileName(), protocol.MaxNameChars)
		withContext(&c, d.GetContextInfo())
	case m.LocationMessage != nil, m.LiveLocationMessage != nil:
		c.Kind = protocol.KindLocation
	case m.ContactMessage != nil, m.ContactsArrayMessage != nil:
		c.Kind = protocol.KindContact
	case m.PollCreationMessage != nil:
		c.Kind, c.Text = protocol.KindPoll, m.GetPollCreationMessage().GetName()
	case m.PollCreationMessageV2 != nil:
		c.Kind, c.Text = protocol.KindPoll, m.GetPollCreationMessageV2().GetName()
	case m.PollCreationMessageV3 != nil:
		c.Kind, c.Text = protocol.KindPoll, m.GetPollCreationMessageV3().GetName()
	default:
		c.Kind = protocol.KindUnsupported
	}
	c.Text = truncate(c.Text, protocol.MaxTextChars)
	return c, true
}

// onlyMeta reports whether a message holds nothing but transport metadata
// (a sender key distribution, context info), i.e. no body to show.
func onlyMeta(m *waE2E.Message) bool {
	body := false
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		switch fd.Name() {
		case "senderKeyDistributionMessage", "messageContextInfo", "fastRatchetKeySenderKeyDistributionMessage":
			return true
		}
		body = true
		return false
	})
	return !body
}

// SyncType maps a history sync type; ok=false for types that carry no chats.
func SyncType(t waHistorySync.HistorySync_HistorySyncType) (string, bool) {
	switch t {
	case waHistorySync.HistorySync_INITIAL_BOOTSTRAP:
		return "initial", true
	case waHistorySync.HistorySync_RECENT:
		return "recent", true
	case waHistorySync.HistorySync_FULL:
		return "full", true
	case waHistorySync.HistorySync_PUSH_NAME:
		return "push_name", true
	case waHistorySync.HistorySync_ON_DEMAND:
		return "on_demand", true
	}
	return "", false
}

// ReceiptKind maps a receipt type; ok=false for types that are not reported.
func ReceiptKind(t types.ReceiptType) (string, bool) {
	switch t {
	case types.ReceiptTypeDelivered:
		return protocol.ReceiptDelivered, true
	case types.ReceiptTypeRead:
		return protocol.ReceiptRead, true
	case types.ReceiptTypePlayed:
		return protocol.ReceiptPlayed, true
	case types.ReceiptTypeReadSelf:
		return protocol.ReceiptReadSelf, true
	}
	return "", false
}

// LogoutReason maps a whatsmeow logout reason (§5.9).
func LogoutReason(r events.ConnectFailureReason) string {
	switch r {
	case events.ConnectFailureLoggedOut:
		return protocol.LogoutDeviceRemoved
	case events.ConnectFailureMainDeviceGone:
		return protocol.LogoutPrimaryGone
	case events.ConnectFailureUnknownLogout:
		return protocol.LogoutBanned
	}
	return protocol.LogoutUnknown
}

// SignalFor maps a provider event to a signal (§5.4); ok=false otherwise.
func SignalFor(evt any) (protocol.Signal, bool) {
	switch e := evt.(type) {
	case *events.TemporaryBan:
		s := protocol.Signal{Kind: protocol.SigTempBanned, Code: int(e.Code)}
		if e.Expire > 0 {
			s.ExpiresInMs = e.Expire.Milliseconds()
		}
		return s, true
	case *events.StreamReplaced:
		return protocol.Signal{Kind: protocol.SigStreamReplaced}, true
	case *events.ClientOutdated:
		return protocol.Signal{Kind: protocol.SigClientOutdated}, true
	case *events.ConnectFailure:
		if int(e.Reason) == 429 {
			return protocol.Signal{Kind: protocol.SigRateLimited, Code: 429}, true
		}
		return protocol.Signal{Kind: protocol.SigConnectFailure, Code: int(e.Reason)}, true
	case *events.StreamError:
		return protocol.Signal{Kind: protocol.SigStreamError, Code: truncate(e.Code, 32)}, true
	case *events.KeepAliveTimeout:
		return protocol.Signal{Kind: protocol.SigKeepaliveTimeout}, true
	}
	return protocol.Signal{}, false
}

// SendErrorCode maps a send/upload error to a protocol error code.
func SendErrorCode(err error) string {
	var iq *whatsmeow.IQError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return protocol.ErrNotConnected
	case errors.Is(err, whatsmeow.ErrMessageTimedOut), errors.Is(err, whatsmeow.ErrIQTimedOut),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return protocol.ErrTimeout
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		return protocol.ErrRateLimited
	case errors.As(err, &iq) && iq.Code == 429:
		return protocol.ErrRateLimited
	case errors.Is(err, whatsmeow.ErrServerReturnedError):
		if serverErrorCode(err) == 429 {
			return protocol.ErrRateLimited
		}
		return protocol.ErrSendFailed
	}
	return protocol.ErrSendFailed
}

func serverErrorCode(err error) int {
	s := err.Error()
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(s[i+1:])
	return n
}

// DownloadErrorCode maps a media download error.
func DownloadErrorCode(err error) string {
	switch {
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410),
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403), errors.Is(err, whatsmeow.ErrNoURLPresent):
		return protocol.ErrMediaExpired
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return protocol.ErrNotConnected
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, whatsmeow.ErrIQTimedOut):
		return protocol.ErrTimeout
	}
	return protocol.ErrMediaUnavailable
}

// mutedUntilFrom converts a WhatsApp mute end time (milliseconds, or seconds
// in older payloads; -1 or far future for "forever") to the protocol value.
func mutedUntilFrom(v int64, now time.Time) int64 {
	switch {
	case v == 0:
		return 0
	case v < 0:
		return -1
	}
	if v < 1e12 { // seconds
		v *= 1000
	}
	if v > now.AddDate(50, 0, 0).UnixMilli() {
		return -1
	}
	return v
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// ChatFromConversation maps history metadata. jid is the normalised chat JID.
func ChatFromConversation(conv *waHistorySync.Conversation, jid string, isGroup bool, now time.Time) protocol.Chat {
	ch := protocol.Chat{JID: jid, Kind: "dm"}
	if isGroup {
		ch.Kind = "group"
	}
	name := conv.GetName()
	if name == "" {
		name = conv.GetDisplayName()
	}
	ch.Name = truncate(name, protocol.MaxNameChars)
	ch.UnreadCount = intPtr(int(conv.GetUnreadCount()))
	ch.Pinned = boolPtr(conv.GetPinned() > 0)
	ch.Archived = boolPtr(conv.GetArchived())
	if mu := mutedUntilFrom(int64(conv.GetMuteEndTime()), now); mu != 0 && (mu == -1 || mu > now.UnixMilli()) {
		ch.MutedUntil = mu
	}
	ts := conv.GetConversationTimestamp()
	if l := conv.GetLastMsgTimestamp(); l > ts {
		ts = l
	}
	if ts > 0 {
		ch.LastMessageAt = int64(ts) * 1000
	}
	return ch
}
