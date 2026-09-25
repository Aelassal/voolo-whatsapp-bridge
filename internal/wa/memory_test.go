// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

// TestMemoryUnderFakeSync measures the heap and RSS after a history sync of
// 50 chats × 200 messages and 500 live messages through the fake client.
// It is a measurement, not a check: run it with VOOLO_MEM_REPORT=1.
func TestMemoryUnderFakeSync(t *testing.T) {
	if os.Getenv("VOOLO_MEM_REPORT") == "" {
		t.Skip("set VOOLO_MEM_REPORT=1 to measure")
	}
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	var convs []*waHistorySync.Conversation
	for c := 0; c < 50; c++ {
		chat := "155503" + strings.Repeat("0", 4) + itoa(10+c) + "@s.whatsapp.net"
		conv := &waHistorySync.Conversation{ID: proto.String(chat)}
		for i := 0; i < 200; i++ {
			conv.Messages = append(conv.Messages, histMsg(chat, "3EB0M"+itoa(c)+"X"+itoa(i), h.now.Add(-time.Duration(i)*time.Minute), strings.Repeat("نص ", 20)))
		}
		convs = append(convs, conv)
	}
	notif := &waE2E.HistorySyncNotification{}
	f.history[notif] = &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(), Conversations: convs, Progress: proto.Uint32(100)}
	f.dispatch(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: mustParse(me), Sender: mustParse(me), IsFromMe: true}, ID: "3EB0N"},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: notif}}})
	batches := 0
	for {
		e := h.next()
		if e.Type == protocol.EvHistoryBatch {
			batches++
			h.cmd("ack", map[string]any{"seq": field[protocol.HistoryBatch](e).Seq})
		}
		if e.Type == protocol.EvSyncProgress && field[protocol.SyncProgress](e).Done {
			break
		}
	}
	for i := 0; i < 500; i++ {
		f.dispatch(textMsg(mustParse(alice), mustParse(alice), "3EB0LV"+itoa(i), "live "+itoa(i), false))
	}
	for i := 0; i < 500; i++ {
		h.expect(protocol.EvMessage)
	}
	delete(f.history, notif)
	convs = nil
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss := ""
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "VmRSS") || strings.HasPrefix(l, "VmHWM") {
				rss += strings.Join(strings.Fields(l), " ") + "; "
			}
		}
	}
	t.Logf("batches=%d heapInuse=%.1f MiB heapSys=%.1f MiB %s", batches, float64(ms.HeapInuse)/(1<<20), float64(ms.HeapSys)/(1<<20), rss)
}
