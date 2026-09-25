// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/history"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

// enqueueHistory queues a history sync notification. whatsmeow is set to
// ManualHistorySyncDownload, so nothing is downloaded until the worker, which
// waits on the ack window, gets to it: a slow client slows the downloads, not
// the live events.
func (b *Bridge) enqueueHistory(s *session, n *waE2E.HistorySyncNotification) {
	s.hmu.Lock()
	s.hq = append(s.hq, n)
	s.hseen = true
	s.hmu.Unlock()
	select {
	case s.hsig <- struct{}{}:
	default:
	}
}

func (s *session) pop() *waE2E.HistorySyncNotification {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	if len(s.hq) == 0 {
		return nil
	}
	n := s.hq[0]
	s.hq[0] = nil
	s.hq = s.hq[1:]
	return n
}

func (s *session) queued() int {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	return len(s.hq)
}

func (b *Bridge) historyLoop(s *session) {
	for {
		if s.ctx.Err() != nil {
			return
		}
		n := s.pop()
		if n != nil {
			b.processHistorySafe(s, n)
			continue
		}
		s.hmu.Lock()
		waitIdle := s.hseen && !s.hdone
		s.hmu.Unlock()
		var idle <-chan time.Time
		if waitIdle {
			idle = time.After(b.cfg.HistoryIdle)
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.hsig:
		case <-idle:
			b.historyDone(s)
		}
	}
}

// processHistorySafe processes one notification; a panic drops only that
// notification, with a constant code on stderr, and the worker goes on.
func (b *Bridge) processHistorySafe(s *session, n *waE2E.HistorySyncNotification) {
	defer func() {
		if r := recover(); r != nil {
			b.cfg.Log.Error("panic_recovered", logx.Code("history"))
		}
	}()
	b.processHistory(s, n)
}

func (b *Bridge) historyDone(s *session) {
	s.hmu.Lock()
	already := s.hdone
	s.hdone = true
	s.hmu.Unlock()
	if !already {
		b.emit(protocol.EvSyncProgress, protocol.SyncProgress{Kind: "history", Done: true})
		b.cfg.Log.Info("history_done")
	}
}

func (b *Bridge) processHistory(s *session, n *waE2E.HistorySyncNotification) {
	ctx := s.ctx
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	hs, err := s.cli.DownloadHistorySync(dctx, n, true)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			b.cfg.Log.Warn("history_download_failed")
		}
		return
	}
	defer func() {
		if n.GetDirectPath() != "" {
			_ = s.cli.DeleteMedia(ctx, whatsmeow.MediaHistory, n.GetDirectPath(), n.GetFileEncSHA256(), n.GetEncHandle())
		}
	}()
	syncType, ok := SyncType(hs.GetSyncType())
	if !ok {
		return
	}
	var progress *int
	if p := hs.GetProgress(); p > 0 {
		progress = intPtr(int(min(p, 100)))
	}
	if syncType == "push_name" {
		for _, pn := range hs.GetPushnames() {
			if j, err := types.ParseJID(pn.GetID()); err == nil {
				b.contactChanged(s, j, types.EmptyJID)
			}
		}
	}
	for _, conv := range hs.GetConversations() {
		if ctx.Err() != nil {
			return
		}
		b.historyConversation(s, syncType, conv, progress)
	}
	if ctx.Err() != nil {
		return
	}
	b.emit(protocol.EvSyncProgress, protocol.SyncProgress{Kind: "history", Progress: progress, Done: false})
	b.sweepLIDs(s)
	if progress != nil && *progress >= 100 && s.queued() == 0 {
		b.historyDone(s)
	}
}

func (b *Bridge) reactionSender(s *session, key *waCommonKey, chat string) string {
	if key.fromMe {
		return b.ownJID(s)
	}
	if key.participant != "" {
		if j, err := types.ParseJID(key.participant); err == nil {
			return b.userJID(s.ctx, s, j, types.EmptyJID)
		}
		return ""
	}
	if !protocol.IsGroupJID(chat) {
		return chat
	}
	return ""
}

type waCommonKey struct {
	fromMe      bool
	participant string
}

func (b *Bridge) historyConversation(s *session, syncType string, conv *waHistorySync.Conversation, progress *int) {
	ctx := s.ctx
	cj, err := types.ParseJID(conv.GetID())
	if err != nil {
		return
	}
	alt := types.EmptyJID
	if pn := conv.GetPnJID(); pn != "" {
		alt, _ = types.ParseJID(pn)
	}
	chat, ok := b.chatJID(ctx, s, cj, alt)
	if !ok {
		return
	}
	isGroup := cj.Server == types.GroupServer
	var msgs []protocol.Message
	descs := map[string]store.MediaDesc{}
	reactions := map[string][]protocol.Reaction{}
	for _, hm := range conv.GetMessages() {
		wm := hm.GetMessage()
		if wm == nil || wm.GetMessage() == nil {
			continue // stubs (system notices without a body) are not reported in v1
		}
		evt, err := s.cli.ParseWebMessage(cj, wm)
		if err != nil {
			continue
		}
		m, desc, ok := b.toMessage(ctx, s, evt, chat)
		if !ok {
			continue
		}
		msgs = append(msgs, m)
		if desc != nil {
			descs[m.ID] = *desc
		}
		for _, r := range wm.GetReactions() {
			if r.GetText() == "" {
				continue
			}
			sender := b.reactionSender(s, &waCommonKey{fromMe: r.GetKey().GetFromMe(), participant: r.GetKey().GetParticipant()}, chat)
			if !protocol.IsUserJID(sender) {
				continue
			}
			at := r.GetSenderTimestampMS()
			if at <= 0 {
				at = m.TS
			}
			reactions[m.ID] = append(reactions[m.ID], protocol.Reaction{ChatJID: chat, MessageID: m.ID, SenderJID: sender, Emoji: truncate(r.GetText(), 32), At: at})
		}
	}
	b.mu.Lock()
	caps := b.caps
	b.mu.Unlock()
	kept, dropped := caps.Filter(chat, msgs)
	if dropped > 0 {
		b.cfg.Log.Info("history_capped", logx.N(int64(dropped)))
	}
	if len(kept) == 0 {
		return
	}
	b.markReported(ctx, s, chat)
	meta := ChatFromConversation(conv, chat, isGroup, b.cfg.Now())
	if !isGroup && meta.Name == "" {
		if ci := s.cli.Contact(ctx, mustJID(chat)); ci.Found {
			meta.Name = truncate(firstNonEmpty(ci.FullName, ci.FirstName, ci.PushName, ci.BusinessName), protocol.MaxNameChars)
		}
	}
	var keptDescs []store.MediaDesc
	for _, m := range kept {
		if d, ok := descs[m.ID]; ok {
			keptDescs = append(keptDescs, d)
		}
	}
	if err := s.st.PutMedia(ctx, keptDescs); err != nil {
		b.cfg.Log.Warn("store_write_failed", logx.Code("media"))
	}
	for _, batch := range history.Split(meta, kept) {
		seq, err := b.window.Acquire(ctx)
		if err != nil {
			return
		}
		b.emit(protocol.EvHistoryBatch, protocol.HistoryBatch{Seq: seq, SyncType: syncType, Chat: meta, Messages: batch, Progress: progress})
	}
	senders := map[string]bool{}
	for _, m := range kept {
		for _, r := range reactions[m.ID] {
			b.emit(protocol.EvReaction, r)
		}
		if isGroup && !m.FromMe {
			senders[m.SenderJID] = true
		}
	}
	if isGroup {
		b.ensureGroup(s, chat)
		for j := range senders {
			b.ensureContact(ctx, s, j)
		}
	} else {
		b.ensureContact(ctx, s, chat)
	}
}
