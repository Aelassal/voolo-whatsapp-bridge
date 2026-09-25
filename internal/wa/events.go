// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/media"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

// onEvent is the single whatsmeow event handler of a session. Pairing is done
// here too, not with GetQRChannel, so no handler is added or removed while
// events are being dispatched (ADR-017).
func (b *Bridge) onEvent(s *session, evt any) {
	if b.current() != s || s.ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			b.cfg.Log.Error("panic_recovered", logx.Code("event"))
		}
	}()
	if sig, ok := SignalFor(evt); ok {
		b.emit(protocol.EvSignal, sig)
		b.cfg.Log.Warn("signal", logx.Code(sig.Kind))
	}
	switch e := evt.(type) {
	case *events.QR:
		b.pairQR(s, e.Codes)
	case *events.RotateADVSecret:
		b.pairRotate(e.OldSecret, e.NewSecret)
	case *events.PairSuccess:
		b.pairSuccess(s, e)
	case *events.PairError:
		b.pairEnd(s, protocol.PairError)
	case *events.QRScannedWithoutMultidevice:
		b.pairEnd(s, protocol.PairRejected)
	case *events.Connected:
		s.offlineDone.Store(false)
		b.setState(protocol.StateConnected)
		b.sweepLIDs(s)
		s.groupsOnce.Do(func() { b.async(func() { b.loadGroups(s) }) })
	case *events.Disconnected:
		if b.pairEnd(s, protocol.PairTimeout) {
			return
		}
		b.setState(protocol.StateReconnecting)
	case *events.LoggedOut:
		reason := LogoutReason(e.Reason)
		b.cfg.Log.Warn("logged_out", logx.Code(reason))
		b.async(func() { b.loggedOutBy(s, reason, int(e.Reason), "") })
	case *events.StreamReplaced, *events.TemporaryBan:
		b.setState(protocol.StateStopped)
	case *events.ConnectFailure:
		b.pairEnd(s, protocol.PairError)
		b.setState(protocol.StateStopped)
	case *events.ClientOutdated:
		b.clientOutdated(s)
	case *events.Message:
		b.onMessage(s, e)
	case *events.UndecryptableMessage:
		b.onUndecryptable(s, e)
	case *events.Receipt:
		b.onReceipt(s, e)
	case *events.OfflineSyncPreview:
		b.emit(protocol.EvSyncProgress, protocol.SyncProgress{Kind: "offline", Done: false})
	case *events.OfflineSyncCompleted:
		if s.offlineDone.CompareAndSwap(false, true) {
			b.emit(protocol.EvSyncProgress, protocol.SyncProgress{Kind: "offline", Done: true})
		}
	case *events.Pin:
		b.chatUpdate(s, e.JID, func(u *protocol.ChatUpdate) { u.Pinned = boolPtr(e.Action.GetPinned()) })
	case *events.Archive:
		b.chatUpdate(s, e.JID, func(u *protocol.ChatUpdate) { u.Archived = boolPtr(e.Action.GetArchived()) })
	case *events.Mute:
		b.chatUpdate(s, e.JID, func(u *protocol.ChatUpdate) {
			var v int64
			if e.Action.GetMuted() {
				v = mutedUntilFrom(e.Action.GetMuteEndTimestamp(), b.cfg.Now())
				if v == 0 {
					v = -1
				}
			}
			u.MutedUntil = &v
		})
	case *events.MarkChatAsRead:
		if e.Action.GetRead() {
			b.chatUpdate(s, e.JID, func(u *protocol.ChatUpdate) { u.MarkedRead = boolPtr(true); u.UnreadCount = intPtr(0) })
		}
	case *events.PushName:
		b.contactChanged(s, e.JID, e.JIDAlt)
	case *events.Contact:
		b.contactChanged(s, e.JID, types.EmptyJID)
	case *events.BusinessName:
		b.contactChanged(s, e.JID, types.EmptyJID)
	case *events.GroupInfo:
		jid := e.JID
		b.async(func() { b.refreshGroup(s, jid, true) })
	case *events.JoinedGroup:
		info := e.GroupInfo
		b.async(func() { b.joinedGroup(s, &info) })
	}
}

func (b *Bridge) ownJID(s *session) string {
	a := s.cli.Account()
	if a.JID.IsEmpty() {
		return ""
	}
	return a.JID.String()
}

// userJID returns a person's JID in protocol form, preferring the phone
// number when WhatsApp gave it (alt) or the store knows the mapping.
func (b *Bridge) userJID(ctx context.Context, s *session, j, alt types.JID) string {
	j = j.ToNonAD()
	if j.Server == types.HiddenUserServer {
		if alt.Server == types.DefaultUserServer && alt.User != "" {
			return alt.ToNonAD().String()
		}
		if pn := s.cli.PNForLID(ctx, j); !pn.IsEmpty() {
			return pn.String()
		}
	}
	if j.Server != types.DefaultUserServer && j.Server != types.HiddenUserServer {
		return ""
	}
	return j.String()
}

// chatJID normalises a chat JID; ok=false for chats that are never reported.
// When a reported @lid chat learns its phone number, chat_update{aliasOf}
// is sent once and the phone-number JID is used from then on.
func (b *Bridge) chatJID(ctx context.Context, s *session, j, alt types.JID) (string, bool) {
	j = j.ToNonAD()
	if !ReportableChat(j) {
		return "", false
	}
	if j.Server != types.HiddenUserServer {
		return j.String(), true
	}
	pn := types.EmptyJID
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		pn = alt.ToNonAD()
	} else {
		pn = s.cli.PNForLID(ctx, j)
	}
	if pn.IsEmpty() {
		return j.String(), true
	}
	b.alias(ctx, s, j.String(), pn.String())
	return pn.String(), true
}

func (b *Bridge) alias(ctx context.Context, s *session, lid, pn string) {
	b.mu.Lock()
	pending, reported := b.chats[lid]
	if reported && pending {
		b.chats[lid] = false
		b.chats[pn] = false
	}
	b.mu.Unlock()
	if reported && pending {
		if err := s.st.ResolveLIDChat(ctx, lid, pn); err != nil {
			b.cfg.Log.Warn("store_write_failed", logx.Code("alias"))
		}
		b.emit(protocol.EvChatUpdate, protocol.ChatUpdate{JID: pn, AliasOf: lid})
	}
}

// sweepLIDs checks every reported @lid chat for a newly known phone number.
func (b *Bridge) sweepLIDs(s *session) {
	b.mu.Lock()
	var pending []string
	for j, p := range b.chats {
		if p {
			pending = append(pending, j)
		}
	}
	b.mu.Unlock()
	for _, l := range pending {
		lid, err := types.ParseJID(l)
		if err != nil {
			continue
		}
		if pn := s.cli.PNForLID(s.ctx, lid); !pn.IsEmpty() {
			b.alias(s.ctx, s, l, pn.String())
		}
	}
}

// markReported records chats as reported; it returns the ones that are new.
func (b *Bridge) markReported(ctx context.Context, s *session, jids ...string) []string {
	b.mu.Lock()
	var fresh []string
	for _, j := range jids {
		if _, ok := b.chats[j]; !ok {
			b.chats[j] = len(j) > 4 && j[len(j)-4:] == "@lid"
			fresh = append(fresh, j)
		}
	}
	b.mu.Unlock()
	if len(fresh) > 0 {
		if err := s.st.AddChats(ctx, fresh); err != nil {
			b.cfg.Log.Warn("store_write_failed", logx.Code("chats"))
		}
	}
	return fresh
}

func (b *Bridge) knownChat(jid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.chats[jid]; ok {
		return true
	}
	_, ok := b.groupCache[jid]
	return ok
}

// ensureChat sends chat (and group/contact) the first time a chat is seen live.
func (b *Bridge) ensureChat(ctx context.Context, s *session, jid string, isGroup bool) {
	if len(b.markReported(ctx, s, jid)) == 1 {
		ch := protocol.Chat{JID: jid, Kind: "dm"}
		if isGroup {
			ch.Kind = "group"
			b.mu.Lock()
			if gi := b.groupCache[jid]; gi != nil {
				ch.Name = truncate(gi.Name, protocol.MaxNameChars)
			}
			b.mu.Unlock()
		} else if ci := s.cli.Contact(ctx, mustJID(jid)); ci.Found {
			ch.Name = truncate(firstNonEmpty(ci.FullName, ci.FirstName, ci.PushName, ci.BusinessName), protocol.MaxNameChars)
		}
		b.emit(protocol.EvChat, protocol.ChatEvent{Chat: ch})
	}
	if isGroup {
		b.ensureGroup(s, jid)
	} else {
		b.ensureContact(ctx, s, jid)
	}
}

func mustJID(s string) types.JID {
	j, _ := types.ParseJID(s)
	return j
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// ensureContact reports a display name once for a person who appears in a
// reported chat or as a group sender. The address book is never reported.
func (b *Bridge) ensureContact(ctx context.Context, s *session, jid string) {
	if jid == "" || jid == b.ownJID(s) {
		return
	}
	b.mu.Lock()
	done := b.contacts[jid]
	b.mu.Unlock()
	if done {
		return
	}
	b.sendContact(ctx, s, jid)
}

func (b *Bridge) sendContact(ctx context.Context, s *session, jid string) {
	j, err := types.ParseJID(jid)
	if err != nil {
		return
	}
	ci := s.cli.Contact(ctx, j)
	c := protocol.Contact{
		JID:          jid,
		Name:         truncate(firstNonEmpty(ci.FullName, ci.FirstName), protocol.MaxNameChars),
		PushName:     truncate(ci.PushName, protocol.MaxNameChars),
		BusinessName: truncate(ci.BusinessName, protocol.MaxNameChars),
	}
	if c.Name == "" && c.PushName == "" && c.BusinessName == "" {
		return
	}
	b.mu.Lock()
	b.contacts[jid] = true
	b.mu.Unlock()
	b.emit(protocol.EvContact, c)
}

// contactChanged re-reports a name only for people already reported.
func (b *Bridge) contactChanged(s *session, j, alt types.JID) {
	jid := b.userJID(s.ctx, s, j, alt)
	b.mu.Lock()
	_, isChat := b.chats[jid]
	was := b.contacts[jid]
	b.mu.Unlock()
	if isChat || was {
		b.sendContact(s.ctx, s, jid)
	}
}

func (b *Bridge) chatUpdate(s *session, j types.JID, set func(*protocol.ChatUpdate)) {
	jid, ok := b.chatJID(s.ctx, s, j, types.EmptyJID)
	if !ok {
		return
	}
	u := protocol.ChatUpdate{JID: jid}
	set(&u)
	b.emit(protocol.EvChatUpdate, u)
}

func (b *Bridge) onReceipt(s *session, e *events.Receipt) {
	kind, ok := ReceiptKind(e.Type)
	if !ok || (e.IsFromMe && kind != protocol.ReceiptReadSelf) {
		return
	}
	alt := types.EmptyJID
	if !e.IsGroup {
		alt = e.SenderAlt
	}
	chat, ok := b.chatJID(s.ctx, s, e.Chat, alt)
	if !ok {
		return
	}
	ids := make([]string, 0, len(e.MessageIDs))
	for _, id := range e.MessageIDs {
		if protocol.IsMessageID(id) {
			ids = append(ids, id)
		}
	}
	r := protocol.Receipt{ChatJID: chat, Kind: kind, At: e.Timestamp.UnixMilli()}
	if e.IsGroup && kind != protocol.ReceiptReadSelf {
		r.SenderJID = b.userJID(s.ctx, s, e.Sender, e.SenderAlt)
	}
	for len(ids) > 0 {
		n := min(len(ids), protocol.MaxReceiptIDs)
		r.MessageIDs = ids[:n]
		ids = ids[n:]
		b.emit(protocol.EvReceipt, r)
	}
}

// toMessage maps a parsed message; ok=false when there is nothing to report.
func (b *Bridge) toMessage(ctx context.Context, s *session, e *events.Message, chat string) (protocol.Message, *store.MediaDesc, bool) {
	c, ok := ContentOf(e.Message, e.IsViewOnce)
	if !ok || !protocol.IsMessageID(e.Info.ID) {
		return protocol.Message{}, nil, false
	}
	m := protocol.Message{ChatJID: chat, ID: e.Info.ID, FromMe: e.Info.IsFromMe, TS: e.Info.Timestamp.UnixMilli(),
		Kind: c.Kind, Text: c.Text, Forwarded: c.Forwarded, Edited: e.IsEdit, Media: c.Media}
	if m.FromMe {
		m.SenderJID = b.ownJID(s)
	} else {
		m.SenderJID = b.userJID(ctx, s, e.Info.Sender, e.Info.SenderAlt)
		m.PushName = truncate(e.Info.PushName, protocol.MaxNameChars)
	}
	if m.SenderJID == "" && !protocol.IsGroupJID(chat) {
		m.SenderJID = chat
	}
	if !protocol.IsUserJID(m.SenderJID) {
		return protocol.Message{}, nil, false
	}
	if c.QuotedID != "" {
		q := &protocol.Quoted{ID: c.QuotedID}
		if c.QuotedBy != "" {
			if qj, err := types.ParseJID(c.QuotedBy); err == nil {
				if u := b.userJID(ctx, s, qj, types.EmptyJID); protocol.IsUserJID(u) {
					q.SenderJID = u
				}
			}
		}
		m.Quoted = q
	}
	for _, raw := range c.Mentions {
		if len(m.Mentions) >= protocol.MaxMentions {
			break
		}
		if mj, err := types.ParseJID(raw); err == nil {
			if u := b.userJID(ctx, s, mj, types.EmptyJID); protocol.IsUserJID(u) {
				m.Mentions = append(m.Mentions, u)
			}
		}
	}
	var desc *store.MediaDesc
	if c.Keys != nil && media.Fetchable(c.Kind) && c.Media != nil {
		desc = &store.MediaDesc{ChatJID: chat, MessageID: m.ID, Kind: c.Kind, Mime: c.Media.Mime, SizeBytes: c.Media.SizeBytes,
			DurationS: c.Media.DurationS, DirectPath: c.Keys.DirectPath, MediaKey: c.Keys.MediaKey,
			FileSHA256: c.Keys.FileSHA256, FileEncSHA256: c.Keys.FileEncSHA256, MsgTS: m.TS}
	}
	b.senders.put(chat+"|"+m.ID, m.SenderJID)
	if raw := e.Info.Sender.ToNonAD(); !raw.IsEmpty() && (raw.Server == types.DefaultUserServer || raw.Server == types.HiddenUserServer) {
		b.rawSenders.put(chat+"|"+m.ID, raw.String())
	}
	return m, desc, true
}

func (b *Bridge) onMessage(s *session, e *events.Message) {
	ctx := s.ctx
	if pm := e.Message.GetProtocolMessage(); pm != nil {
		if n := pm.GetHistorySyncNotification(); n != nil && e.Info.IsFromMe {
			b.enqueueHistory(s, n)
			return
		}
	}
	alt := e.Info.SenderAlt
	if e.Info.IsFromMe {
		alt = e.Info.RecipientAlt
	}
	if e.Info.IsGroup {
		alt = types.EmptyJID
	}
	chat, ok := b.chatJID(ctx, s, e.Info.Chat, alt)
	if !ok {
		return
	}
	by := b.ownJID(s)
	if !e.Info.IsFromMe {
		by = b.userJID(ctx, s, e.Info.Sender, e.Info.SenderAlt)
	}
	at := e.Info.Timestamp.UnixMilli()
	if pm := e.Message.GetProtocolMessage(); pm != nil {
		target := pm.GetKey().GetID()
		if !protocol.IsMessageID(target) {
			return
		}
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			b.emit(protocol.EvMsgUpdate, protocol.MessageUpdate{ChatJID: chat, MessageID: target, Kind: "revoke", At: at, By: by})
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			c, ok := ContentOf(pm.GetEditedMessage(), false)
			if !ok {
				return
			}
			if ts := pm.GetTimestampMS(); ts > 0 {
				at = ts
			}
			b.emit(protocol.EvMsgUpdate, protocol.MessageUpdate{ChatJID: chat, MessageID: target, Kind: "edit", Text: c.Text, At: at, By: by})
		}
		return
	}
	if r := e.Message.GetReactionMessage(); r != nil {
		target := r.GetKey().GetID()
		if !protocol.IsMessageID(target) || !protocol.IsUserJID(by) {
			return
		}
		if ts := r.GetSenderTimestampMS(); ts > 0 {
			at = ts
		}
		b.emit(protocol.EvReaction, protocol.Reaction{ChatJID: chat, MessageID: target, SenderJID: by, Emoji: truncate(r.GetText(), 32), At: at})
		return
	}
	m, desc, ok := b.toMessage(ctx, s, e, chat)
	if !ok {
		return
	}
	b.ensureChat(ctx, s, chat, e.Info.IsGroup)
	if desc != nil {
		if err := s.st.PutMedia(ctx, []store.MediaDesc{*desc}); err != nil {
			b.cfg.Log.Warn("store_write_failed", logx.Code("media"))
		}
	}
	b.emit(protocol.EvMessage, protocol.MessageEvent{Message: m})
	if e.Info.IsGroup && !m.FromMe {
		b.ensureContact(ctx, s, m.SenderJID)
	}
}

func (b *Bridge) onUndecryptable(s *session, e *events.UndecryptableMessage) {
	if e.IsUnavailable && e.UnavailableType == events.UnavailableTypeViewOnce {
		return
	}
	alt := types.EmptyJID
	if !e.Info.IsGroup {
		alt = e.Info.SenderAlt
	}
	chat, ok := b.chatJID(s.ctx, s, e.Info.Chat, alt)
	if !ok || !protocol.IsMessageID(e.Info.ID) {
		return
	}
	m := protocol.Message{ChatJID: chat, ID: e.Info.ID, FromMe: e.Info.IsFromMe, TS: e.Info.Timestamp.UnixMilli(), Kind: protocol.KindUnsupported}
	if m.FromMe {
		m.SenderJID = b.ownJID(s)
	} else {
		m.SenderJID = b.userJID(s.ctx, s, e.Info.Sender, e.Info.SenderAlt)
	}
	if !protocol.IsUserJID(m.SenderJID) {
		return
	}
	b.ensureChat(s.ctx, s, chat, e.Info.IsGroup)
	b.emit(protocol.EvMessage, protocol.MessageEvent{Message: m})
}

// Groups ---------------------------------------------------------------

func groupEvent(gi *types.GroupInfo) protocol.Group {
	g := protocol.Group{JID: gi.JID.ToNonAD().String(), Name: truncate(gi.Name, protocol.MaxNameChars), Topic: truncate(gi.Topic, protocol.MaxNameChars), Participants: []protocol.Participant{}}
	for _, p := range gi.Participants {
		if len(g.Participants) >= protocol.MaxParticipants {
			break
		}
		j := p.JID
		if !p.PhoneNumber.IsEmpty() {
			j = p.PhoneNumber
		}
		js := j.ToNonAD().String()
		if !protocol.IsUserJID(js) {
			continue
		}
		g.Participants = append(g.Participants, protocol.Participant{JID: js, IsAdmin: p.IsAdmin || p.IsSuperAdmin})
	}
	return g
}

func (b *Bridge) ensureGroup(s *session, jid string) {
	b.mu.Lock()
	if b.groupsSent[jid] {
		b.mu.Unlock()
		return
	}
	gi := b.groupCache[jid]
	loaded := b.groupsLoaded
	if gi != nil || loaded {
		// Either reported now, or fetched once below; never asked for twice.
		b.groupsSent[jid] = true
	}
	b.mu.Unlock()
	if gi != nil {
		b.emit(protocol.EvGroup, groupEvent(gi))
		return
	}
	if loaded {
		// Not among the joined groups at connect time: ask for this one group.
		b.async(func() { b.refreshGroup(s, mustJID(jid), false) })
	}
}

var groupFetchMu sync.Mutex

// refreshGroup fetches one group's metadata (a single query, one at a time).
func (b *Bridge) refreshGroup(s *session, j types.JID, changed bool) {
	jid := j.ToNonAD().String()
	b.mu.Lock()
	_, reported := b.chats[jid]
	old := b.groupCache[jid]
	b.mu.Unlock()
	if !reported {
		return
	}
	groupFetchMu.Lock()
	defer groupFetchMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	gi, err := s.cli.GetGroupInfo(ctx, j)
	if err != nil || gi == nil {
		b.cfg.Log.Warn("group_info_failed")
		return
	}
	b.mu.Lock()
	b.groupCache[jid] = gi
	b.groupsSent[jid] = true
	b.mu.Unlock()
	b.emit(protocol.EvGroup, groupEvent(gi))
	if changed && old != nil && old.Name != gi.Name {
		name := truncate(gi.Name, protocol.MaxNameChars)
		b.emit(protocol.EvChatUpdate, protocol.ChatUpdate{JID: jid, Name: &name})
	}
}

func (b *Bridge) joinedGroup(s *session, gi *types.GroupInfo) {
	jid := gi.JID.ToNonAD().String()
	b.mu.Lock()
	b.groupCache[jid] = gi
	b.mu.Unlock()
	b.ensureChat(s.ctx, s, jid, true)
}

// loadGroups fetches every joined group once after the first connection
// (one query, as WhatsApp Web does) and reports those of reported chats.
func (b *Bridge) loadGroups(s *session) {
	ctx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	defer cancel()
	infos, err := s.cli.GetJoinedGroups(ctx)
	if err != nil {
		b.cfg.Log.Warn("joined_groups_failed")
	}
	b.mu.Lock()
	for _, gi := range infos {
		if gi != nil {
			b.groupCache[gi.JID.ToNonAD().String()] = gi
		}
	}
	b.groupsLoaded = true
	var pending []string
	for j := range b.chats {
		if protocol.IsGroupJID(j) && !b.groupsSent[j] {
			pending = append(pending, j)
		}
	}
	b.mu.Unlock()
	for _, j := range pending {
		b.ensureGroup(s, j)
	}
}
