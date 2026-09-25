// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"image"
	_ "image/jpeg" // image sizes for send_media
	_ "image/png"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/media"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/transport"
)

// ready checks the common preconditions of commands that talk to WhatsApp.
func (b *Bridge) ready(id string) (*session, bool) {
	s := b.current()
	if s == nil {
		b.fail(id, protocol.ErrNotInitialized)
		return nil, false
	}
	if s.cli.Account().JID.IsEmpty() {
		b.fail(id, protocol.ErrNotPaired)
		return nil, false
	}
	if !s.cli.IsConnected() || !s.cli.IsLoggedIn() {
		b.fail(id, protocol.ErrNotConnected)
		return nil, false
	}
	return s, true
}

// The send backstop (PROTOCOL.md §6.5, review M6): compiled in, not
// configurable by the client, and above Voolo's own caps.
const (
	MinSendInterval  = time.Second      // at least this long between two sends
	MaxSendsInWindow = 30               // at most this many sends ...
	SendWindow       = 10 * time.Minute // ... in any window this long
)

// backstopAllows reports whether a send may start now. The caller holds the
// single send slot, so nothing else changes b.sends meanwhile.
func (b *Bridge) backstopAllows(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends = b.clampSends(b.sends, now)
	keep := b.sends[:0]
	for _, t := range b.sends {
		if now.Sub(t) < SendWindow {
			keep = append(keep, t)
		}
	}
	b.sends = keep
	if len(keep) >= MaxSendsInWindow {
		return false
	}
	if n := len(keep); n > 0 && now.Sub(keep[n-1]) < MinSendInterval {
		return false
	}
	return true
}

// clampSends moves send times that lie after now (the clock went back since
// they were taken) to one floor interval before now, and keeps their order.
// Such a send then counts for one window from the moment the bridge notices,
// so a clock set back never blocks sends for longer than SendWindow, and it
// does not trip the 1 s floor by itself (review R-L4).
func (b *Bridge) clampSends(sends []time.Time, now time.Time) []time.Time {
	limit := now.Add(-MinSendInterval)
	n := 0
	for i, t := range sends {
		if t.After(now) {
			sends[i] = limit
			n++
		}
	}
	if n > 0 {
		for i := 1; i < len(sends); i++ {
			if sends[i].Before(sends[i-1]) {
				sends[i] = sends[i-1]
			}
		}
		b.cfg.Log.Warn("send_clock_skew", logx.N(int64(n)))
	}
	return sends
}

func (b *Bridge) recordSend(t time.Time) {
	b.mu.Lock()
	b.sends = append(b.sends, t)
	b.mu.Unlock()
}

// beginSend runs the checks every send shares, in the order of PROTOCOL.md
// §6.5 (not_paired, not_connected, busy, unknown_chat, duplicate_outbox_id,
// rate_limited_local), and reserves the outbox id. On success it returns
// release, which frees the single send slot; it must run before the final
// reply is written, so a client that sends again right after send_result is
// not told busy. A refusal leaves the outbox id unused.
func (b *Bridge) beginSend(id, chatJID, outboxID string) (s *session, to types.JID, release func(), ok bool) {
	s, ok = b.ready(id)
	if !ok {
		return nil, types.EmptyJID, nil, false
	}
	if !b.sending.CompareAndSwap(false, true) {
		b.fail(id, protocol.ErrBusy)
		return nil, types.EmptyJID, nil, false
	}
	var once sync.Once
	free := func() { once.Do(func() { b.sending.Store(false) }) }
	defer func() {
		// A panic below frees the slot too.
		if !ok {
			free()
		}
	}()
	ok = false
	// A refusal frees the slot before its error is written (§6.5).
	refuse := func(code string) (*session, types.JID, func(), bool) {
		free()
		b.fail(id, code)
		if code == protocol.ErrDuplicateOutboxID || code == protocol.ErrRateLimitedLocal {
			b.cfg.Log.Warn("send_refused", logx.Code(code))
		}
		return nil, types.EmptyJID, nil, false
	}
	to, err := types.ParseJID(chatJID)
	if err != nil || !b.knownChat(chatJID) {
		return refuse(protocol.ErrUnknownChat)
	}
	known, err := s.st.OutboxKnown(s.ctx, outboxID)
	if err != nil {
		return refuse(protocol.ErrStoreIO)
	}
	if known {
		return refuse(protocol.ErrDuplicateOutboxID)
	}
	now := b.cfg.Now()
	if !b.backstopAllows(now) {
		return refuse(protocol.ErrRateLimitedLocal)
	}
	dup, err := s.st.ReserveOutbox(s.ctx, outboxID, now)
	if err != nil {
		return refuse(protocol.ErrStoreIO)
	}
	if dup {
		return refuse(protocol.ErrDuplicateOutboxID)
	}
	return s, to, free, true
}

// deliver sends one message, once. It is never retried by the bridge. The
// send counts toward the backstop from here, where it is handed to WhatsApp
// (lead decision 2026-09-25: a send refused before this point does not count).
func (b *Bridge) deliver(s *session, id, outboxID string, to types.JID, msg *waE2E.Message, release func()) {
	now := b.cfg.Now()
	b.recordSend(now)
	if err := s.st.MarkDispatched(s.ctx, outboxID, now); err != nil {
		b.cfg.Log.Warn("store_write_failed", logx.Code("outbox"))
	}
	ctx, cancel := context.WithTimeout(s.ctx, b.cfg.SendTimeout+5*time.Second)
	defer cancel()
	resp, err := s.cli.SendMessage(ctx, to, msg, whatsmeow.SendRequestExtra{Timeout: b.cfg.SendTimeout})
	release()
	if err != nil {
		code := SendErrorCode(err)
		if code == protocol.ErrRateLimited {
			b.emit(protocol.EvSignal, protocol.Signal{Kind: protocol.SigRateLimited, Code: 429})
		}
		b.fail(id, code)
		b.cfg.Log.Warn("send_failed", logx.Code(code))
		return
	}
	if err := s.st.SetOutboxMessage(s.ctx, outboxID, resp.ID); err != nil {
		b.cfg.Log.Warn("store_write_failed", logx.Code("outbox"))
	}
	at := resp.Timestamp
	if at.IsZero() {
		at = b.cfg.Now()
	}
	b.senders.put(to.String()+"|"+resp.ID, b.ownJID(s))
	b.emit(protocol.EvSendResult, protocol.SendResult{ReplyTo: id, OutboxID: outboxID, MessageID: resp.ID, At: at.UnixMilli()})
	b.cfg.Log.Info("send_ok")
}

// contextInfo is the reply part of a send (PROTOCOL.md §6.5, revision 2):
// the quoted message's id, its author and a copy of its text. The author is
// the JID WhatsApp gave for that message when the bridge saw it (a group may
// address people by @lid), else the client's quotedSenderJid, else the
// reported sender. The copy is what WhatsApp's clients show above the reply;
// it goes only to the chat that already holds the quoted message.
func (b *Bridge) contextInfo(chat string, q protocol.Quote) *waE2E.ContextInfo {
	if q.Forwarded {
		// Forwarded once by this user (PROTOCOL.md §6.5, feature forward).
		return &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(1)}
	}
	if q.QuotedMessageID == "" {
		return nil
	}
	ci := &waE2E.ContextInfo{StanzaID: proto.String(q.QuotedMessageID)}
	key := chat + "|" + q.QuotedMessageID
	if p, ok := b.rawSenders.get(key); ok {
		ci.Participant = proto.String(p)
	} else if q.QuotedSenderJID != "" {
		ci.Participant = proto.String(q.QuotedSenderJID)
	} else if p, ok := b.senders.get(key); ok {
		ci.Participant = proto.String(p)
	}
	if q.QuotedText != "" {
		ci.QuotedMessage = &waE2E.Message{Conversation: proto.String(q.QuotedText)}
	}
	return ci
}

func (b *Bridge) sendText(id string, c *protocol.SendText) {
	s, to, release, ok := b.beginSend(id, c.ChatJID, c.OutboxID)
	if !ok {
		return
	}
	defer release()
	msg := &waE2E.Message{}
	if ci := b.contextInfo(c.ChatJID, c.Quote); ci != nil {
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: proto.String(c.Text), ContextInfo: ci}
	} else {
		msg.Conversation = proto.String(c.Text)
	}
	b.deliver(s, id, c.OutboxID, to, msg, release)
}

func (b *Bridge) sendMedia(id string, c *protocol.SendMedia) {
	b.mu.Lock()
	lim, dir := b.lim, b.init.MediaDir
	b.mu.Unlock()
	// The hand-off file is deleted in every case, also when the send is
	// refused before the file is read (§6.6, review L4).
	defer media.Discard(dir, c.Path)
	s, to, release, ok := b.beginSend(id, c.ChatJID, c.OutboxID)
	if !ok {
		return
	}
	defer release()
	data, err := media.Open(dir, c.Path, c.Key, c.SHA256, media.MaxSendBytes(c.Kind, lim))
	if err != nil {
		code := protocol.ErrMediaInvalid
		if errors.Is(err, media.ErrTooBig) {
			code = protocol.ErrMediaTooLarge
		}
		release()
		b.fail(id, code)
		return
	}
	var mt whatsmeow.MediaType
	switch c.Kind {
	case "image":
		mt = whatsmeow.MediaImage
	case "voice":
		mt = whatsmeow.MediaAudio
	case "video":
		mt = whatsmeow.MediaVideo
		if !strings.HasPrefix(c.Mime, "video/") {
			release()
			b.fail(id, protocol.ErrMediaInvalid)
			return
		}
	default:
		mt = whatsmeow.MediaDocument
	}
	secs := 0
	if c.Kind == "voice" {
		// A voice note is Ogg Opus with a duration read from the file; anything
		// else could not be held to voiceMaxSeconds (review L10).
		secs = media.OggDurationSeconds(data)
		if !media.VoiceMime(c.Mime) || secs <= 0 {
			release()
			b.fail(id, protocol.ErrMediaInvalid)
			return
		}
		if secs > lim.VoiceMaxSeconds {
			release()
			b.fail(id, protocol.ErrMediaTooLarge)
			return
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, b.cfg.SendTimeout)
	up, err := s.cli.Upload(ctx, data, mt)
	cancel()
	if err != nil {
		code := SendErrorCode(err)
		if code == protocol.ErrSendFailed {
			code = protocol.ErrTimeout // an upload failure sent nothing, but we cannot tell a slow upload apart
			if errors.Is(err, whatsmeow.ErrNotConnected) {
				code = protocol.ErrNotConnected
			}
		}
		release()
		b.fail(id, code)
		return
	}
	ci := b.contextInfo(c.ChatJID, c.Quote)
	msg := &waE2E.Message{}
	switch c.Kind {
	case "image":
		im := &waE2E.ImageMessage{Mimetype: proto.String(c.Mime), URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength), ContextInfo: ci}
		if c.Caption != "" {
			im.Caption = proto.String(c.Caption)
		}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			im.Width, im.Height = proto.Uint32(uint32(cfg.Width)), proto.Uint32(uint32(cfg.Height))
		}
		msg.ImageMessage = im
	case "video":
		vm := &waE2E.VideoMessage{Mimetype: proto.String(c.Mime), URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength), ContextInfo: ci}
		if c.Caption != "" {
			vm.Caption = proto.String(c.Caption)
		}
		if c.DurationS > 0 {
			vm.Seconds = proto.Uint32(uint32(c.DurationS))
		}
		if c.Width > 0 && c.Height > 0 {
			vm.Width, vm.Height = proto.Uint32(uint32(c.Width)), proto.Uint32(uint32(c.Height))
		}
		msg.VideoMessage = vm
	case "voice":
		msg.AudioMessage = &waE2E.AudioMessage{Mimetype: proto.String(c.Mime), URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
			PTT: proto.Bool(true), Seconds: proto.Uint32(uint32(secs)), ContextInfo: ci}
		if wf := protocol.WaveformBytes(c.Waveform); wf != nil {
			msg.AudioMessage.Waveform = wf
		}
	default:
		name := c.FileName
		if name == "" {
			name = "file"
			if exts, _ := mime.ExtensionsByType(c.Mime); len(exts) > 0 {
				name += exts[0]
			}
		}
		dm := &waE2E.DocumentMessage{Mimetype: proto.String(c.Mime), URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
			FileName: proto.String(name), Title: proto.String(name), ContextInfo: ci}
		if c.Caption != "" {
			dm.Caption = proto.String(c.Caption)
		}
		msg.DocumentMessage = dm
	}
	b.deliver(s, id, c.OutboxID, to, msg, release)
}

func (b *Bridge) markRead(id string, c *protocol.MarkRead) {
	s, ok := b.ready(id)
	if !ok {
		return
	}
	chat, err := types.ParseJID(c.ChatJID)
	if err != nil {
		b.fail(id, protocol.ErrBadRequest)
		return
	}
	// Read receipts only for chats the bridge reported (review L3) ...
	if !b.knownChat(c.ChatJID) {
		b.fail(id, protocol.ErrUnknownChat)
		return
	}
	// ... and, in a group, only for messages of the named sender where the
	// bridge knows who wrote them.
	if c.SenderJID != "" {
		for _, mid := range c.MessageIDs {
			if by, known := b.senders.get(c.ChatJID + "|" + mid); known && by != c.SenderJID {
				b.fail(id, protocol.ErrBadRequest)
				return
			}
		}
	}
	sender := types.EmptyJID
	if c.SenderJID != "" {
		sender, _ = types.ParseJID(c.SenderJID)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
	defer cancel()
	if err := s.cli.MarkRead(ctx, c.MessageIDs, b.cfg.Now(), chat, sender); err != nil {
		code := SendErrorCode(err)
		if code == protocol.ErrSendFailed || code == protocol.ErrRateLimited {
			code = protocol.ErrInternal
		}
		b.fail(id, code)
		return
	}
	b.emit(protocol.EvOK, protocol.Reply{ReplyTo: id})
}

func (b *Bridge) fetchMedia(id string, c *protocol.FetchMedia) {
	s := b.current()
	b.mu.Lock()
	lim, dir := b.lim, b.init.MediaDir
	b.mu.Unlock()
	d, found, err := s.st.GetMedia(s.ctx, c.ChatJID, c.MessageID)
	if err != nil {
		b.fail(id, protocol.ErrStoreIO)
		return
	}
	if !found {
		b.fail(id, protocol.ErrUnknownMessage)
		return
	}
	if !media.Fetchable(d.Kind) {
		b.fail(id, protocol.ErrMediaUnavailable)
		return
	}
	if media.CheckFetch(d.Kind, d.SizeBytes, d.DurationS, lim) != nil {
		b.fail(id, protocol.ErrMediaTooLarge)
		return
	}
	if _, ok := b.ready(id); !ok {
		return
	}
	select {
	case b.fetchSem <- struct{}{}:
		defer func() { <-b.fetchSem }()
	case <-s.ctx.Done():
		return
	}
	mt := downloadType(d.Kind)
	// The transport cuts the download off past the cap (plus WhatsApp's
	// encryption overhead), whatever size the sender declared (review M2).
	maxBytes := media.MaxFetchBytes(d.Kind, lim)
	ctx, cancel := context.WithTimeout(transport.WithBodyLimit(s.ctx, maxBytes+media.DownloadOverhead), b.cfg.FetchTimeout)
	data, err := s.cli.DownloadMediaWithPath(ctx, d.DirectPath, d.FileEncSHA256, d.FileSHA256, d.MediaKey, mt, "", false)
	cancel()
	if err != nil {
		code := DownloadErrorCode(err)
		b.fail(id, code)
		b.cfg.Log.Warn("fetch_failed", logx.Code(code))
		return
	}
	durationS := d.DurationS
	if d.Kind == "voice" {
		durationS = voiceDuration(d.DurationS, media.OggDurationSeconds(data))
	}
	if media.CheckFetch(d.Kind, int64(len(data)), durationS, lim) != nil {
		clear(data)
		b.fail(id, protocol.ErrMediaTooLarge)
		return
	}
	sealed, err := media.Seal(dir, data)
	clear(data)
	if err != nil {
		b.fail(id, protocol.ErrInternal)
		return
	}
	b.emit(protocol.EvMediaReady, protocol.MediaReady{ReplyTo: id, MessageID: c.MessageID, Path: sealed.Path, Key: sealed.Key,
		SHA256: sealed.SHA256, Mime: d.Mime, SizeBytes: sealed.SizeBytes, DurationS: durationS})
}

// downloadType is WhatsApp's media type of a reported kind: it names the keys
// the file was encrypted with.
func downloadType(kind string) whatsmeow.MediaType {
	switch kind {
	case "voice", "audio":
		return whatsmeow.MediaAudio
	case "video":
		return whatsmeow.MediaVideo
	case "document":
		return whatsmeow.MediaDocument
	}
	return whatsmeow.MediaImage // image, sticker
}

// VoiceDurationMargin is how far a received voice file may read shorter than
// its declared length and still be taken at its own word.
const VoiceDurationMargin = 2

// voiceDuration is the duration reported and capped for a received voice
// note: the file's own where it can be read (review L10), but never much less
// than the sender declared, since the file is the sender's word too (review
// R-L2). An unreadable file keeps the declared length.
func voiceDuration(declared, file int) int {
	if file <= 0 || declared-file > VoiceDurationMargin {
		return max(declared, file)
	}
	return file
}

// The set_pin backstop (PROTOCOL.md §6.12): compiled in, like the send one.
const (
	MinPinInterval  = time.Second
	MaxPinsInWindow = 20
	PinWindow       = 10 * time.Minute
)

// pinAllowed reports whether a set_pin may write now, and records it.
func (b *Bridge) pinAllowed(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := b.pins[:0]
	for _, t := range b.pins {
		if d := now.Sub(t); d >= 0 && d < PinWindow {
			keep = append(keep, t)
		}
	}
	b.pins = keep
	if len(keep) >= MaxPinsInWindow || (len(keep) > 0 && now.Sub(keep[len(keep)-1]) < MinPinInterval) {
		return false
	}
	b.pins = append(b.pins, now)
	return true
}

// setPin pins or unpins one reported chat in WhatsApp's app state, so the
// phone and every linked device show it (revision 2, feature set_pin). It is
// never repeated by the bridge.
func (b *Bridge) setPin(id string, c *protocol.SetPin) {
	s, ok := b.ready(id)
	if !ok {
		return
	}
	jid, err := types.ParseJID(c.ChatJID)
	if err != nil || !b.knownChat(c.ChatJID) {
		b.fail(id, protocol.ErrUnknownChat)
		return
	}
	if !b.pinAllowed(b.cfg.Now()) {
		b.fail(id, protocol.ErrRateLimitedLocal)
		b.cfg.Log.Warn("pin_failed", logx.Code(protocol.ErrRateLimitedLocal))
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
	defer cancel()
	if err := s.cli.SendAppState(ctx, appstate.BuildPin(jid, c.Pinned)); err != nil {
		code := SendErrorCode(err)
		if code == protocol.ErrSendFailed {
			code = protocol.ErrInternal
		}
		b.fail(id, code)
		b.cfg.Log.Warn("pin_failed", logx.Code(code))
		return
	}
	b.emit(protocol.EvOK, protocol.Reply{ReplyTo: id})
	b.cfg.Log.Info("pin_ok")
}

// AvatarInterval is the least time between two profile picture queries
// (PROTOCOL.md §6.13): the bridge waits, it does not refuse.
const AvatarInterval = time.Second

// fetchAvatar answers fetch_avatar (revision 2, feature avatar): the preview
// of one reported chat's profile picture, as a §8 hand-off file, or its state
// (unchanged, none, hidden). One at a time, at most one query a second.
func (b *Bridge) fetchAvatar(id string, c *protocol.FetchAvatar) {
	s, ok := b.ready(id)
	if !ok {
		return
	}
	jid, err := types.ParseJID(c.JID)
	if err != nil || !b.knownChat(c.JID) {
		b.fail(id, protocol.ErrUnknownChat)
		return
	}
	b.picMu.Lock()
	defer b.picMu.Unlock()
	b.mu.Lock()
	wait := AvatarInterval - b.cfg.Now().Sub(b.lastPic)
	dir := b.init.MediaDir
	b.mu.Unlock()
	if wait > 0 && wait <= AvatarInterval {
		select {
		case <-time.After(wait):
		case <-s.ctx.Done():
			return
		}
	}
	b.mu.Lock()
	b.lastPic = b.cfg.Now()
	b.mu.Unlock()
	answer := protocol.Avatar{ReplyTo: id, JID: c.JID}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	info, err := s.cli.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{Preview: true, ExistingID: c.KnownID})
	switch {
	case errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized):
		answer.State = protocol.AvatarHidden
	case errors.Is(err, whatsmeow.ErrProfilePictureNotSet):
		answer.State = protocol.AvatarNone
	case err != nil:
		code := SendErrorCode(err)
		if code != protocol.ErrRateLimited && code != protocol.ErrTimeout && code != protocol.ErrNotConnected {
			code = protocol.ErrMediaUnavailable
		}
		b.fail(id, code)
		b.cfg.Log.Warn("avatar_failed", logx.Code(code))
		return
	case info == nil:
		answer.State = protocol.AvatarUnchanged
		answer.ID = c.KnownID
	default:
		data, err := s.cli.FetchPicture(ctx, info.URL, protocol.MaxAvatarBytes)
		mime := http.DetectContentType(data)
		if err != nil || (mime != "image/jpeg" && mime != "image/png" && mime != "image/webp") || !protocol.IsPictureID(info.ID) {
			code := protocol.ErrMediaUnavailable
			if errors.Is(err, transport.ErrBodyTooLarge) {
				code = protocol.ErrMediaTooLarge
			}
			clear(data)
			b.fail(id, code)
			b.cfg.Log.Warn("avatar_failed", logx.Code(code))
			return
		}
		sealed, err := media.Seal(dir, data)
		clear(data)
		if err != nil {
			b.fail(id, protocol.ErrInternal)
			return
		}
		answer.State, answer.ID, answer.Path, answer.Key, answer.SHA256, answer.Mime, answer.SizeBytes =
			protocol.AvatarSet, info.ID, sealed.Path, sealed.Key, sealed.SHA256, mime, sealed.SizeBytes
	}
	b.emit(protocol.EvAvatar, answer)
	b.cfg.Log.Info("avatar_ok", logx.Code(answer.State))
}

func (b *Bridge) logout(id string) {
	s := b.current()
	if s.cli.Account().JID.IsEmpty() {
		b.fail(id, protocol.ErrNotPaired)
		return
	}
	if !s.cli.IsConnected() {
		b.fail(id, protocol.ErrNotConnected)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
	err := s.cli.Logout(ctx)
	cancel()
	if err != nil {
		code := SendErrorCode(err)
		if code != protocol.ErrNotConnected && code != protocol.ErrTimeout {
			code = protocol.ErrInternal
		}
		b.fail(id, code)
		return
	}
	// Stop, which the caller runs next, deletes the store and replies ok.
	b.loggedOutBy(s, protocol.LogoutUser, 0, id)
}

// lru is a small bounded map (message → sender, for quoted replies).
type lru struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[string]*list.Element
}

type lruEntry struct{ k, v string }

func newLRU(max int) *lru { return &lru{max: max, ll: list.New(), m: map[string]*list.Element{}} }

func (l *lru) put(k, v string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.m[k]; ok {
		e.Value.(*lruEntry).v = v
		l.ll.MoveToFront(e)
		return
	}
	l.m[k] = l.ll.PushFront(&lruEntry{k, v})
	if l.ll.Len() > l.max {
		old := l.ll.Back()
		l.ll.Remove(old)
		delete(l.m, old.Value.(*lruEntry).k)
	}
}

func (l *lru) get(k string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.m[k]; ok {
		l.ll.MoveToFront(e)
		return e.Value.(*lruEntry).v, true
	}
	return "", false
}
