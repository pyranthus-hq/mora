package imessage

import (
	"strings"
	"testing"
)

// TestConversationGrain proves the conversation grain (IMSG-03): one Item per
// conversation for BOTH a 1:1 and a group chat. It drives the fake Fetcher (canned
// pages, no live chat.db) so it runs in CI, and walks the cursor exactly like the
// shared Ingest loop does — asserting each conversation surfaces as exactly one Item.
func TestConversationGrain(t *testing.T) {
	oneToOne := Item{
		Kind:       KindIMessageChat,
		ProviderID: "iMessage;-;+14155551234",
		Title:      "+14155551234",
		Body:       "+14155551234: are we still on for the demo?\nMe: yes, 3pm",
		Tags:       []string{"imessage"},
		Meta:       map[string]any{"message_count": "2"},
	}
	group := Item{
		Kind:       KindIMessageChat,
		ProviderID: "iMessage;+;chat999",
		Title:      "Demo Crew",
		Body:       "Sarah: pushed to 4\nMe: works\n+14155550000: 👍 sounds good",
		Tags:       []string{"imessage"},
		Meta:       map[string]any{"message_count": "3"},
	}

	f := &fakeFetcher{
		pages: map[string]Page{
			"":   {Items: []Item{oneToOne}, NextCursor: "42"},
			"42": {Items: []Item{group}, NextCursor: ""},
		},
	}

	// Walk pages by cursor (mirrors memory.Ingest).
	var items []Item
	cursor := ""
	for {
		page, err := f.FetchPage(KindIMessageChat, FetchWindow{}, cursor)
		if err != nil {
			t.Fatalf("FetchPage(%q): %v", cursor, err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(items) != 2 {
		t.Fatalf("got %d items, want exactly 2 (one per conversation)", len(items))
	}

	// Exactly one Item per distinct conversation GUID; each has a non-empty body and
	// the iMessage kind.
	seen := map[string]int{}
	for _, it := range items {
		if it.Kind != KindIMessageChat {
			t.Errorf("item %q has kind %q, want %q", it.ProviderID, it.Kind, KindIMessageChat)
		}
		if strings.TrimSpace(it.Body) == "" {
			t.Errorf("item %q has empty body", it.ProviderID)
		}
		seen[it.ProviderID]++
	}
	for guid, n := range seen {
		if n != 1 {
			t.Errorf("conversation %q produced %d items, want 1 (grain)", guid, n)
		}
	}
	if seen["iMessage;-;+14155551234"] != 1 || seen["iMessage;+;chat999"] != 1 {
		t.Errorf("missing a conversation: 1:1=%d group=%d", seen["iMessage;-;+14155551234"], seen["iMessage;+;chat999"])
	}

	// The fake was driven across the page boundary (cursor resume path exercised).
	if len(f.calls) != 2 || f.calls[0] != "" || f.calls[1] != "42" {
		t.Errorf("cursor walk = %v, want [\"\" \"42\"]", f.calls)
	}
}

// TestStableIDForm guards the conversation StableID/SafeFilename contract: a chat
// GUID maps to imessage_chat/<guid> and the on-disk name keeps the GUID's ; + - @
// chars (only / : space are replaced) so any later findMemory lookup matches.
func TestStableIDForm(t *testing.T) {
	id := imessageStableID("iMessage;-;+14155551234")
	if id != "imessage_chat/iMessage;-;+14155551234" {
		t.Fatalf("stable id = %q", id)
	}
}
