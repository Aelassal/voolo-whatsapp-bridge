// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

// PhoneLinkBrowser is the browser half of the "Browser (OS)" display name that
// WhatsApp requires for linking with a phone number (whatsmeow PairPhone; the
// server refuses other forms). Approved by the owner on 2026-09-25. QR links
// show init.deviceName ("Voolo") instead.
const PhoneLinkBrowser = "Chrome"

var phoneLinkOS = map[string]string{"windows": "Windows", "darwin": "Mac OS", "linux": "Linux"}

// PhoneLinkDisplayName is the name shown on the phone for a phone-code link,
// for example "Chrome (Windows)".
func PhoneLinkDisplayName() string {
	osName, ok := phoneLinkOS[runtime.GOOS]
	if !ok {
		osName = "Linux"
	}
	return PhoneLinkBrowser + " (" + osName + ")"
}

type pairing struct {
	replyTo string
	phone   string // empty for QR
	s       *session
	ctx     context.Context
	cancel  context.CancelFunc
	codes   chan []string
	rot     chan [2]string
}

func (b *Bridge) startPairing(id, phone string) {
	s := b.current()
	if !s.cli.Account().JID.IsEmpty() {
		b.fail(id, protocol.ErrAlreadyPaired)
		return
	}
	b.mu.Lock()
	if b.pair != nil {
		b.mu.Unlock()
		b.fail(id, protocol.ErrBusy)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	p := &pairing{replyTo: id, phone: phone, s: s, ctx: ctx, cancel: cancel, codes: make(chan []string, 4), rot: make(chan [2]string, 4)}
	b.pair = p
	b.mu.Unlock()
	b.cfg.Log.Info("pairing_started", logx.Code(map[bool]string{true: "phone", false: "qr"}[phone != ""]))
	b.async(func() { b.runPairing(p) })
}

// takePair clears p if it is still the current pairing.
func (b *Bridge) takePair(p *pairing) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pair != p || p == nil {
		return false
	}
	b.pair = nil
	p.cancel()
	return true
}

func (b *Bridge) currentPair(s *session) *pairing {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pair != nil && b.pair.s == s {
		return b.pair
	}
	return nil
}

func (b *Bridge) abortPair(p *pairing, code string) {
	if b.takePair(p) {
		p.s.cli.Disconnect()
		b.fail(p.replyTo, code)
		b.cfg.Log.Warn("pairing_failed", logx.Code(code))
	}
}

func (b *Bridge) runPairing(p *pairing) {
	if err := p.s.cli.Connect(); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
		b.abortPair(p, protocol.ErrPairFailed)
		return
	}
	var codes []string
	select {
	case codes = <-p.codes:
	case <-p.ctx.Done():
		return
	case <-time.After(b.cfg.FirstQRWait):
		b.abortPair(p, protocol.ErrPairFailed)
		return
	}
	if p.phone != "" {
		ctx, cancel := context.WithTimeout(p.ctx, 25*time.Second)
		code, err := p.s.cli.PairPhone(ctx, p.phone, true, whatsmeow.PairClientChrome, PhoneLinkDisplayName())
		cancel()
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			c := protocol.ErrPairFailed
			if errors.Is(err, whatsmeow.ErrPhoneNumberTooShort) || errors.Is(err, whatsmeow.ErrPhoneNumberIsNotInternational) || errors.Is(err, whatsmeow.ErrIQBadRequest) {
				c = protocol.ErrPhoneInvalid
			}
			b.abortPair(p, c)
			return
		}
		b.emit(protocol.EvPairCode, protocol.PairCode{ReplyTo: p.replyTo, Code: strings.ReplaceAll(code, "-", "")})
	}
	b.cycleCodes(p, codes)
}

// cycleCodes shows each QR code for its lifetime (the first 60 s, the others
// 20 s), applies ADV secret rotations, and ends the pairing with
// pair_failed{timeout} when the codes run out. For a phone-code link the same
// clock bounds the pairing window, but no qr event is sent.
func (b *Bridge) cycleCodes(p *pairing, codes []string) {
	idx := 0
	for len(codes) > 0 {
		code := codes[0]
		codes = codes[1:]
		ttl := b.cfg.QRNext
		if idx == 0 {
			ttl = b.cfg.QRFirst
		}
		if p.phone == "" && len(code) <= 4096 {
			b.emit(protocol.EvQR, protocol.QR{ReplyTo: p.replyTo, Code: code, ExpiresAt: b.cfg.Now().Add(ttl).UnixMilli(), Index: idx})
		}
		idx++
		timer := time.NewTimer(ttl)
		select {
		case <-timer.C:
		case r := <-p.rot:
			timer.Stop()
			rotated := []string{strings.Replace(code, r[0], r[1], 1)}
			for _, c := range codes {
				rotated = append(rotated, strings.Replace(c, r[0], r[1], 1))
			}
			codes = rotated
		case nc := <-p.codes:
			timer.Stop()
			codes = nc
		case <-p.ctx.Done():
			timer.Stop()
			return
		}
	}
	if b.takePair(p) {
		p.s.cli.Disconnect()
		b.emit(protocol.EvPairFailed, protocol.PairFailed{ReplyTo: p.replyTo, Reason: protocol.PairTimeout})
		b.cfg.Log.Info("pairing_timeout")
	}
}

func (b *Bridge) pairQR(s *session, codes []string) {
	if p := b.currentPair(s); p != nil && len(codes) > 0 {
		select {
		case p.codes <- append([]string(nil), codes...):
		default:
		}
	}
}

func (b *Bridge) pairRotate(oldSecret, newSecret string) {
	b.mu.Lock()
	p := b.pair
	b.mu.Unlock()
	if p != nil {
		select {
		case p.rot <- [2]string{oldSecret, newSecret}:
		default:
		}
	}
}

func (b *Bridge) pairSuccess(s *session, e *events.PairSuccess) {
	if p := b.currentPair(s); p != nil {
		b.takePair(p)
	}
	ev := protocol.Paired{JID: e.ID.ToNonAD().String(), BusinessName: truncate(e.BusinessName, protocol.MaxNameChars)}
	if !e.LID.IsEmpty() {
		ev.LID = e.LID.ToNonAD().String()
	}
	b.emit(protocol.EvPaired, ev)
	b.cfg.Log.Info("paired")
	b.setState(protocol.StateConnecting)
}

// pairEnd ends a running pairing with pair_failed; false if none was running.
func (b *Bridge) pairEnd(s *session, reason string) bool {
	p := b.currentPair(s)
	if p == nil || !b.takePair(p) {
		return false
	}
	b.emit(protocol.EvPairFailed, protocol.PairFailed{ReplyTo: p.replyTo, Reason: reason})
	b.cfg.Log.Warn("pairing_failed", logx.Code(reason))
	b.async(func() { s.cli.Disconnect() })
	return true
}

// clientOutdated handles WhatsApp's 405: refresh the Web version once and
// reconnect; a second rejection is fatal (exit 5).
func (b *Bridge) clientOutdated(s *session) {
	b.mu.Lock()
	b.outdated++
	n := b.outdated
	b.mu.Unlock()
	b.pairEnd(s, protocol.PairClientOutdated)
	if n > 1 || b.cfg.RefreshVersion == nil {
		b.failFatal("", protocol.ErrClientOutdated, ExitClientOutdated)
		return
	}
	b.async(func() {
		ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
		defer cancel()
		if err := b.cfg.RefreshVersion(ctx); err != nil {
			b.cfg.Log.Error("version_refresh_failed")
			b.failFatal("", protocol.ErrClientOutdated, ExitClientOutdated)
			return
		}
		b.cfg.Log.Info("version_refreshed")
		if !s.cli.Account().JID.IsEmpty() {
			s.cli.Disconnect()
			b.connect(s)
		}
	})
}
