// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package history

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

func TestWindowOfFour(t *testing.T) {
	w := NewWindow(WindowSize)
	ctx := context.Background()
	for i := int64(1); i <= 4; i++ {
		seq, err := w.Acquire(ctx)
		if err != nil || seq != i {
			t.Fatalf("seq %d err %v", seq, err)
		}
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := w.Acquire(blocked); err == nil {
		t.Fatal("fifth batch went out without an ack")
	}
	got := make(chan int64)
	go func() { s, _ := w.Acquire(ctx); got <- s }()
	select {
	case <-got:
		t.Fatal("not blocked")
	case <-time.After(30 * time.Millisecond):
	}
	if n := w.Ack(1); n != 1 {
		t.Fatalf("released %d", n)
	}
	select {
	case s := <-got:
		if s != 5 {
			t.Fatalf("seq %d", s)
		}
	case <-time.After(time.Second):
		t.Fatal("ack did not release the window")
	}
	// A repeated or unknown ack changes nothing; a later ack releases all earlier ones.
	if w.Ack(1) != 0 || w.Ack(99) != 4 || w.Outstanding() != 0 {
		t.Fatal("cumulative ack broken")
	}
}

func msgs(chat string, n int, ts func(i int) int64) []protocol.Message {
	out := make([]protocol.Message, n)
	for i := range out {
		out[i] = protocol.Message{ChatJID: chat, ID: "M" + strings.Repeat("0", 3) + string(rune('A'+i%26)), SenderJID: chat, TS: ts(i), Kind: "text", Text: "x"}
	}
	return out
}

func TestCapsDays(t *testing.T) {
	now := time.UnixMilli(1790330400000)
	day := int64(24 * time.Hour / time.Millisecond)
	c := &Caps{Days: 90, MaxPerChat: 20000, Now: func() time.Time { return now }}
	in := []protocol.Message{
		{ID: "d89", TS: now.UnixMilli() - 89*day},
		{ID: "d90", TS: now.UnixMilli() - 90*day},
		{ID: "d90+1ms", TS: now.UnixMilli() - 90*day - 1},
		{ID: "d91", TS: now.UnixMilli() - 91*day},
	}
	kept, dropped := c.Filter("a@s.whatsapp.net", in)
	if dropped != 2 || len(kept) != 2 || kept[0].ID != "d90" || kept[1].ID != "d89" {
		t.Fatalf("kept %+v dropped %d", kept, dropped)
	}
}

func TestCapsPerChatAcrossChunks(t *testing.T) {
	now := time.UnixMilli(1790330400000)
	c := &Caps{Days: 90, MaxPerChat: 20000, Now: func() time.Time { return now }}
	ts := func(base int64) func(int) int64 {
		return func(i int) int64 { return now.UnixMilli() - base - int64(i) }
	}
	// 19,999 in a first chunk, then 2 more: exactly one fits.
	k1, d1 := c.Filter("a", msgs("a", 19999, ts(0)))
	k2, d2 := c.Filter("a", msgs("a", 2, ts(100000)))
	if len(k1) != 19999 || d1 != 0 || len(k2) != 1 || d2 != 1 {
		t.Fatalf("19,999 + 2: kept %d/%d dropped %d/%d", len(k1), len(k2), d1, d2)
	}
	if k2[0].TS != now.UnixMilli()-100000 {
		t.Fatal("the newest message of the chunk should be kept")
	}
	k3, d3 := c.Filter("a", msgs("a", 1, ts(200000)))
	if len(k3) != 0 || d3 != 1 {
		t.Fatal("20,001st message kept")
	}
	// Other chats have their own count; 20,000 exactly fits, 20,001 does not.
	kb, _ := c.Filter("b", msgs("b", 20000, ts(0)))
	kc, dc := c.Filter("c", msgs("c", 20001, ts(0)))
	if len(kb) != 20000 || len(kc) != 20000 || dc != 1 {
		t.Fatalf("b %d c %d/%d", len(kb), len(kc), dc)
	}
	// Output is in ascending time order.
	for i := 1; i < len(kc); i++ {
		if kc[i].TS < kc[i-1].TS {
			t.Fatal("not ascending")
		}
	}
}

func TestSplitRespectsCountAndBytes(t *testing.T) {
	chat := protocol.Chat{JID: "a@s.whatsapp.net", Kind: "dm", Name: strings.Repeat("ن", 512)}
	small := msgs(chat.JID, 450, func(i int) int64 { return int64(i) })
	b := Split(chat, small)
	if len(b) != 3 || len(b[0]) != 200 || len(b[1]) != 200 || len(b[2]) != 50 {
		t.Fatalf("count split: %d batches", len(b))
	}
	big := make([]protocol.Message, 30)
	for i := range big {
		big[i] = protocol.Message{ChatJID: chat.JID, ID: "B", SenderJID: chat.JID, Kind: "text", Text: strings.Repeat("\u0001", protocol.MaxTextChars)} // 6 bytes each when escaped
	}
	w := protocol.NewWriter(io.Discard, nil)
	for _, batch := range Split(chat, big) {
		line, err := w.Encode(protocol.EvHistoryBatch, protocol.HistoryBatch{Seq: 1 << 40, SyncType: "initial", Chat: chat, Messages: batch, Progress: new(int)})
		if err != nil {
			t.Fatal(err)
		}
		if len(line)-1 > protocol.MaxBatchBytes {
			t.Fatalf("batch of %d messages is %d bytes", len(batch), len(line))
		}
	}
	if len(Split(chat, nil)) != 0 {
		t.Fatal("empty chat gives a batch")
	}
}
