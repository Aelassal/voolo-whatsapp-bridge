// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package history turns the phone's history sync into history_batch events:
// the S5 caps (days and messages per chat), splitting into batches of at most
// 200 messages and 786,432 bytes, and the acknowledgement window of 4 batches
// (PROTOCOL.md §7).
package history

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

// WindowSize is the number of unacknowledged batches allowed in flight.
const WindowSize = 4

// Window hands out batch sequence numbers and blocks when WindowSize batches
// are waiting for an ack. Live events never go through it.
type Window struct {
	mu          sync.Mutex
	size        int
	next        int64
	outstanding map[int64]struct{}
	changed     chan struct{}
}

// NewWindow returns a window of the given size; seq starts at 1.
func NewWindow(size int) *Window {
	return &Window{size: size, next: 1, outstanding: map[int64]struct{}{}, changed: make(chan struct{})}
}

// Acquire waits until fewer than size batches are unacknowledged, then
// reserves and returns the next seq.
func (w *Window) Acquire(ctx context.Context) (int64, error) {
	for {
		w.mu.Lock()
		if len(w.outstanding) < w.size {
			seq := w.next
			w.next++
			w.outstanding[seq] = struct{}{}
			w.mu.Unlock()
			return seq, nil
		}
		ch := w.changed
		w.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// Ack acknowledges seq and every earlier batch. Unknown or repeated seqs are
// ignored. It returns how many batches it released.
func (w *Window) Ack(seq int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for s := range w.outstanding {
		if s <= seq {
			delete(w.outstanding, s)
			n++
		}
	}
	if n > 0 {
		close(w.changed)
		w.changed = make(chan struct{})
	}
	return n
}

// Reset forgets every unacknowledged batch (after a logout) and wakes any
// waiter. Sequence numbers keep increasing for the life of the process.
func (w *Window) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.outstanding = map[int64]struct{}{}
	close(w.changed)
	w.changed = make(chan struct{})
}

// Outstanding returns the number of unacknowledged batches.
func (w *Window) Outstanding() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.outstanding)
}

// Caps drops history older than Days and beyond MaxPerChat messages per chat,
// counted across every history chunk of this process run. It is safe for
// concurrent use.
type Caps struct {
	Days       int
	MaxPerChat int
	Now        func() time.Time

	mu     sync.Mutex
	counts map[string]int
}

// Filter returns the messages of one chat that fit the caps, in ascending
// time order, and how many it dropped. When a chunk would pass the per-chat
// cap, the newest messages are kept.
func (c *Caps) Filter(chatJID string, msgs []protocol.Message) ([]protocol.Message, int) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	cutoff := now().Add(-time.Duration(c.Days) * 24 * time.Hour).UnixMilli()
	kept := make([]protocol.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.TS >= cutoff {
			kept = append(kept, m)
		}
	}
	dropped := len(msgs) - len(kept)
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].TS > kept[j].TS })
	c.mu.Lock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	room := c.MaxPerChat - c.counts[chatJID]
	if room < 0 {
		room = 0
	}
	if len(kept) > room {
		dropped += len(kept) - room
		kept = kept[:room]
	}
	c.counts[chatJID] += len(kept)
	c.mu.Unlock()
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].TS < kept[j].TS })
	return kept, dropped
}

// envelopeOverhead bounds everything in a history_batch line except the
// messages and the chat: the envelope, seq, syncType, progress and brackets.
const envelopeOverhead = 512

// Split cuts one chat's messages into batches of at most
// protocol.MaxBatchMessages messages and protocol.MaxBatchBytes encoded bytes
// (with the chat repeated in each). A chat with no messages gives no batch.
func Split(chat protocol.Chat, msgs []protocol.Message) [][]protocol.Message {
	budget := protocol.MaxBatchBytes - envelopeOverhead - protocol.EncodedSize(chat)
	var out [][]protocol.Message
	var cur []protocol.Message
	size := 0
	for _, m := range msgs {
		ms := protocol.EncodedSize(m) + 1 // comma
		if len(cur) > 0 && (len(cur) >= protocol.MaxBatchMessages || size+ms > budget) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, m)
		size += ms
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
