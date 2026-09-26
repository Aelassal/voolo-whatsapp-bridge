// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package wa

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
)

// Revision 3, feature lid_names (PROTOCOL.md §5.13–§5.15): WhatsApp writes a
// mention as "@<digits>" in the text and names the person in mentionedJid,
// more and more often by linked id (@lid). The client needs the pair to show
// a name, so the bridge reports it next to the phone number it already gives.

const (
	bobLID   = "100000000000003@lid"
	carolLID = "100000000000009@lid" // nobody knows Carol's phone number
)

func mentionMsg(chat, sender types.JID, id, text string, mentioned ...string) *events.Message {
	m := textMsg(chat, sender, id, text, false)
	m.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text),
		ContextInfo: &waE2E.ContextInfo{MentionedJID: mentioned}}}
	return m
}

func TestMentionLIDsAreMappedToPhoneNumbers(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := pairedFake()
		f.pnForLID[aliceLID] = mustParse(alice)
		f.contacts[alice] = types.ContactInfo{Found: true, FullName: "Alice Example", PushName: "Alice"}
		return f
	})
	f := h.initPairedConnected()
	text := "@100000000000002 @100000000000009 بكرة؟"
	f.dispatch(mentionMsg(mustParse(group), mustParse(bob), "3EB0M1", text, aliceLID, carolLID, me))
	m := field[protocol.MessageEvent](h.expect(protocol.EvMessage)).Message
	if m.Text != text {
		t.Fatalf("the text must never change: %q", m.Text)
	}
	if want := []string{alice, carolLID, me}; !slices.Equal(m.Mentions, want) {
		t.Fatalf("mentions %v, want %v", m.Mentions, want)
	}
	if len(m.MentionLIDs) != 1 || m.MentionLIDs[0] != (protocol.MentionLID{LID: aliceLID, JID: alice}) {
		t.Fatalf("mentionLids %+v", m.MentionLIDs)
	}
	// A mentioned person with a known name is reported once, with the linked id.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no contact for the mentioned person")
		default:
		}
		c := field[protocol.Contact](h.expect(protocol.EvContact))
		if c.JID == alice {
			if c.Name != "Alice Example" || c.LID != aliceLID {
				t.Fatalf("contact %+v", c)
			}
			break
		}
	}
}

func TestMentionByPhoneNumberHasNoLIDPair(t *testing.T) {
	h := newHarness(t, pairedFake)
	f := h.initPairedConnected()
	f.dispatch(mentionMsg(mustParse(group), mustParse(bob), "3EB0M2", "@15550100002 hi", alice))
	m := field[protocol.MessageEvent](h.expect(protocol.EvMessage)).Message
	if !slices.Equal(m.Mentions, []string{alice}) || m.MentionLIDs != nil {
		t.Fatalf("message %+v", m)
	}
	raw, _ := json.Marshal(m)
	if json.Valid(raw) && containsKey(raw, "mentionLids") {
		t.Fatalf("empty mentionLids must be left out: %s", raw)
	}
}

func TestGroupParticipantsCarryTheirLinkedID(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := pairedFake()
		f.pnForLID[carolLID] = types.EmptyJID
		f.pnForLID["100000000000004@lid"] = mustParse("15550100004@s.whatsapp.net")
		f.groups = []*types.GroupInfo{{JID: mustParse(group), GroupName: types.GroupName{Name: "Team"}, Participants: []types.GroupParticipant{
			{JID: mustParse(me), IsAdmin: true},
			// LID-addressed group: the JID is the linked id, the phone number is known.
			{JID: mustParse(bobLID), PhoneNumber: mustParse(bob), LID: mustParse(bobLID)},
			// Phone-addressed group that also names the linked id.
			{JID: mustParse(alice), LID: mustParse(aliceLID)},
			// Only a linked id, whose number the store knows.
			{JID: mustParse("100000000000004@lid")},
			// Only a linked id, number unknown.
			{JID: mustParse(carolLID)},
		}}}
		return f
	})
	f := h.initPairedConnected()
	time.Sleep(50 * time.Millisecond) // joined groups load
	f.dispatch(textMsg(mustParse(group), mustParse(bob), "3EB0M3", "hi", false))
	g := field[protocol.Group](h.expect(protocol.EvGroup))
	want := []protocol.Participant{
		{JID: me, IsAdmin: true},
		{JID: bob, LID: bobLID},
		{JID: alice, LID: aliceLID},
		{JID: "15550100004@s.whatsapp.net", LID: "100000000000004@lid"},
		{JID: carolLID},
	}
	if !slices.Equal(g.Participants, want) {
		t.Fatalf("participants\n got %+v\nwant %+v", g.Participants, want)
	}
}

func TestReadyAccountCarriesOwnLID(t *testing.T) {
	h := newHarness(t, func() *fakeClient {
		f := pairedFake()
		f.account.LID = mustParse("100000000000001@lid")
		return f
	})
	id := h.cmd("init", map[string]any{"storeKey": testKey, "storeDir": h.storeDir, "mediaDir": h.mediaDir, "deviceName": "Voolo",
		"limits": map[string]any{"historyDays": 90, "historyMaxPerChat": 20000, "imageMaxBytes": 16 << 20, "voiceMaxSeconds": 3600}})
	r := field[protocol.Ready](h.expect(protocol.EvReady))
	if r.ReplyTo != id || r.Account == nil || r.Account.JID != me || r.Account.LID != "100000000000001@lid" {
		t.Fatalf("ready %+v", r)
	}
}

func TestFeaturesListLIDNames(t *testing.T) {
	if !slices.Contains(protocol.Features(), protocol.FeatureLIDNames) {
		t.Fatalf("features %v", protocol.Features())
	}
}

func containsKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
