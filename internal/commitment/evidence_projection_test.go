package commitment

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"reflect"
	"strings"
	"testing"
)

func gmailEvidenceMemory() memory.Memory {
	return memory.Memory{ID: "gmail_thread/t1", Type: "email", Provider: "gmail", Source: "gmail:me", CreatedAt: "2026-01-01T10:05:00Z", Text: "From: sam@example.com\n\nCan you send the deck?\n\n---\n\nFrom: me@example.com\n\nI sent the deck.", Meta: map[string]any{"occurred_at": "2026-01-01T10:05:00Z", "from": []string{"sam@example.com", "me@example.com"}, "to": []string{"me@example.com", "sam@example.com"}, "names": map[string]string{"sam@example.com": "Sam Rivera"}, "messages": []map[string]any{{"message_ref": "gmail_thread/t1#m1", "sender": "sam@example.com", "at": "2026-01-01T10:00:00Z", "block_refs": []string{"body1"}}, {"message_ref": "gmail_thread/t1#m2", "sender": "me@example.com", "at": "2026-01-01T10:05:00Z", "block_refs": []string{"body2"}}}}}
}
func TestEvidenceFromMemoriesGmailAndManual(t *testing.T) {
	self := map[string]bool{"me@example.com": true}
	m := gmailEvidenceMemory()
	got := EvidenceFromMemories([]memory.Memory{m}, self)
	if len(got) != 2 || got[0].Party != PartyCounterparty || got[0].MessageRef != "gmail_thread/t1#m1" || got[0].BlockRef != "body1" || got[1].Party != PartySelf || got[1].Text != "I sent the deck." || got[1].Citation.Date() != "2026-01-01T10:05:00Z" {
		t.Fatalf("gmail=%+v", got)
	}
	legacy := m
	legacy.Meta["messages"] = "malformed"
	legacy.Text = "From: sam@example.com\n\nI sent the deck."
	got = EvidenceFromMemories([]memory.Memory{legacy}, self)
	if len(got) != 1 || got[0].MessageRef != "" || got[0].Party != PartyCounterparty {
		t.Fatalf("legacy=%+v", got)
	}
	manual := memory.Memory{ID: "manual/1", Type: "note", Source: "manual", CreatedAt: "2026-01-01T09:00:00Z", Text: "I sent the notes."}
	deleted := manual
	deleted.ID = "deleted"
	deleted.DeletedAt = "2026-01-01T10:00:00Z"
	m = gmailEvidenceMemory()
	got = EvidenceFromMemories([]memory.Memory{m, manual, deleted}, self)
	if len(got) != 3 || got[0].MemoryID != "manual/1" || got[0].Party != PartySelf {
		t.Fatalf("ordered=%+v", got)
	}
	bad := manual
	bad.ID = ""
	if got := EvidenceFromMemories([]memory.Memory{bad}, self); len(got) != 0 {
		t.Fatalf("invalid citation=%+v", got)
	}
}
func TestEvidenceFromMemoriesIMessage(t *testing.T) {
	legacy := memory.Memory{ID: "imessage_chat/legacy", Type: "imessage", Provider: "imessage", Source: "chat", CreatedAt: "2026-01-01T10:00:00Z", Text: "## date\nLucia: I sent the deck.\nMe: Got the deck.\n* system\nmalformed", Meta: map[string]any{"participants": []map[string]string{{"handle": "+100", "name": "Lucia"}}}}
	got := EvidenceFromMemories([]memory.Memory{legacy}, nil)
	if len(got) != 2 || got[0].Party != PartySelf || got[1].Party != PartyCounterparty {
		t.Fatalf("legacy imessage=%+v", got)
	}
	const id = "imessage_chat/stable"
	line := "Lucia: I sent the deck."
	body := "## 2026-01-01\n" + line
	start := strings.Index(body, line)
	stable := memory.Memory{ID: id, Type: "imessage", Provider: "imessage", Source: "chat", CreatedAt: "2026-01-01T10:00:00Z", Text: body, Meta: map[string]any{"occurred_at": "2026-01-01T10:00:00Z", "message_count": "1", "participants": []map[string]string{{"handle": "+100", "name": "Lucia"}}, "message_evidence_schema": 1, "message_evidence": []map[string]any{{"evidence_ref": id + "#m1", "at": "2026-01-01T10:00:00Z", "from_me": false, "sender": "Lucia", "block_start": start, "block_end": start + len(line)}}}}
	got = EvidenceFromMemories([]memory.Memory{stable}, nil)
	if len(got) != 1 || got[0].MessageRef != id+"#m1" || got[0].Citation.Date() != "2026-01-01T10:00:00Z" {
		t.Fatalf("stable=%+v", got)
	}
}
func TestEvidenceProjectionHelpers(t *testing.T) {
	m := gmailEvidenceMemory()
	if len(GmailMessages(m)) != 2 || len(GmailBodyParts(m)) != 2 || FirstGmailSender(m) != "sam@example.com" {
		t.Fatal("gmail helpers")
	}
	if GmailMessages(memory.Memory{Meta: map[string]any{"messages": make(chan int)}}) != nil || GmailMessages(memory.Memory{Meta: map[string]any{"messages": "bad"}}) != nil {
		t.Fatal("malformed messages")
	}
	if GmailAuthoredBlockRef(GmailMessage{}, "body") != "" || GmailAuthoredBlockRef(GmailMessage{BlockRefs: []string{"x"}}, "On Tue, Sam wrote:\n> quote") != "" {
		t.Fatal("authored block")
	}
	if FirstGmailSender(memory.Memory{Text: "Subject: x", Meta: map[string]any{"from": []string{"a@x"}}}) != "" || FirstGmailSender(memory.Memory{Text: "From: Bob", Meta: map[string]any{"from": []string{"a@x"}}}) != "" {
		t.Fatal("sender guard")
	}
	turns := ConversationTurns("\n# heading\n* system\nno colon\nLucia:\nLucia: hello\nMe: yes")
	if !reflect.DeepEqual(turns, []Turn{{Body: "hello"}, {Self: true, Body: "yes"}}) {
		t.Fatalf("turns=%+v", turns)
	}
	for _, tt := range []struct {
		m    memory.Memory
		want string
	}{{memory.Memory{Source: "s", Provider: "p", Type: "t"}, "s"}, {memory.Memory{Provider: "p", Type: "t"}, "p"}, {memory.Memory{Type: "t"}, "t"}} {
		if got := SourceOf(tt.m); got != tt.want {
			t.Errorf("source=%q", got)
		}
	}
}

func TestEvidenceProjectionPartyAndOrderingBranches(t *testing.T) {
	self := map[string]bool{"me@example.com": true}
	unknown := gmailEvidenceMemory()
	unknown.Text += "\n\n---\n\nFrom: bob@example.com\n\nI sent something."
	unknown.Meta["messages"] = append(unknown.Meta["messages"].([]map[string]any), map[string]any{"message_ref": "gmail_thread/t1#m3", "sender": "bob@example.com", "at": "2026-01-01T10:06:00Z", "block_refs": []string{"body3"}})
	if got := EvidenceFromMemories([]memory.Memory{unknown}, self); len(got) != 2 {
		t.Fatalf("unknown author admitted: %+v", got)
	}
	legacySelf := memory.Memory{ID: "gmail_thread/self", Type: "email", Provider: "gmail", CreatedAt: "2026-01-01T10:00:00Z", Text: "From: me@example.com\n\nI sent the deck.", Meta: map[string]any{"from": []string{"me@example.com"}, "to": []string{"sam@example.com"}}}
	if got := EvidenceFromMemories([]memory.Memory{legacySelf}, self); len(got) != 1 || got[0].Party != PartySelf {
		t.Fatalf("legacy self=%+v", got)
	}
	const id = "imessage_chat/self"
	line := "Me: I sent the deck."
	body := "## 2026-01-01\n" + line
	start := strings.Index(body, line)
	imSelf := memory.Memory{ID: id, Type: "imessage", Provider: "imessage", CreatedAt: "2026-01-01T10:00:00Z", Text: body, Meta: map[string]any{"message_count": "1", "participants": []map[string]string{{"handle": "+100", "name": "Lucia"}}, "message_evidence_schema": 1, "message_evidence": []map[string]any{{"evidence_ref": id + "#m1", "at": "2026-01-01T10:00:00Z", "from_me": true, "sender": "Me", "block_start": start, "block_end": start + len(line)}}}}
	if got := EvidenceFromMemories([]memory.Memory{imSelf}, self); len(got) != 1 || got[0].Party != PartySelf {
		t.Fatalf("stable self=%+v", got)
	}
	a := memory.Memory{ID: "manual/b", Type: "note", Source: "manual", CreatedAt: "2026-01-01T08:00:00Z", Text: "I sent b."}
	b := a
	b.ID = "manual/a"
	b.Text = "I sent a."
	got := EvidenceFromMemories([]memory.Memory{a, b}, self)
	if len(got) != 2 || got[0].MemoryID != "manual/a" {
		t.Fatalf("memory order=%+v", got)
	}
}
