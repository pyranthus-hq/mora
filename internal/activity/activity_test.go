package activity

import (
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestDeriveConversationEvidenceParticipation(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	m := conversation("imessage", "imessage/chat", "one\ntwo\nthree", []map[string]any{
		{"evidence_ref": "imessage/chat#1", "at": "2026-09-09T09:00:00Z", "from_me": false, "sender": "Sam", "block_start": 0, "block_end": 3},
		{"evidence_ref": "imessage/chat#2", "at": "2026-09-10T12:00:00Z", "from_me": true, "sender": "Me", "block_start": 4, "block_end": 7},
		{"evidence_ref": "imessage/chat#3", "at": "2026-09-10T11:00:00Z", "from_me": false, "sender": "Pat", "block_start": 8, "block_end": 13},
	})
	m.Meta["is_group"] = true
	got := Derive(m, now)
	if !got.Eligible || got.EventSource != EventSourceMessageEvidence || got.EventAt.Format(time.RFC3339) != now.Format(time.RFC3339) {
		t.Fatalf("event projection = %+v", got)
	}
	p := got.Participation
	if p == nil || p.OwnShare != 1.0/3.0 || p.MessageEvidenceCount != 3 || p.LastOwnAt == nil || p.LastOwnAt.Format(time.RFC3339) != now.Format(time.RFC3339) || p.LatestSender != "Me" || p.IsGroup == nil || !*p.IsGroup {
		t.Fatalf("participation = %+v", p)
	}
}

func TestDeriveWhatsAppExplicitDirectAndNoOwn(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	m := conversation("whatsapp", "whatsapp/chat", "hello", []map[string]any{{
		"evidence_ref": "whatsapp/chat#1", "at": "2026-09-09T09:00:00Z", "from_me": false, "sender": "Sam", "block_start": 0, "block_end": 5,
	}})
	m.Meta["chat_kind"] = "direct"
	p := Derive(m, now).Participation
	if p == nil || p.OwnShare != 0 || p.LastOwnAt != nil || p.IsGroup == nil || *p.IsGroup || p.LatestSender != "Sam" || p.MessageEvidenceCount != 1 {
		t.Fatalf("participation = %+v", p)
	}
}

func TestDeriveDoesNotGuessFromInvalidOrPartialConversationEvidence(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	m := conversation("imessage", "imessage/chat", "hello", []map[string]any{{
		"evidence_ref": "imessage/chat#1", "at": "not-a-time", "from_me": true, "sender": "Me", "block_start": 0, "block_end": 5,
	}})
	m.Meta["occurred_at"] = "2026-09-09T09:00:00Z"
	got := Derive(m, now)
	if !got.Eligible || got.EventSource != EventSourceOccurredAt || got.Participation != nil || got.Automated != nil {
		t.Fatalf("invalid evidence must only fall back to explicit event: %+v", got)
	}
	m = conversation("whatsapp", "whatsapp/chat", "hello", []map[string]any{{
		"evidence_ref": "whatsapp/chat#1", "at": "2026-09-09T09:00:00Z", "from_me": false, "sender": "Sam", "block_start": 0, "block_end": 8,
	}})
	got = Derive(m, now)
	if got.Participation != nil || got.EventAt != nil { // span exceeds retained body
		t.Fatalf("partial evidence must not be retained: %+v", got)
	}
}

func TestDeriveTruncatedConversationDoesNotUseEvidence(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	m := conversation("imessage", "imessage/chat", "hello", []map[string]any{{
		"evidence_ref": "imessage/chat#1", "at": "2026-09-10T11:00:00Z", "from_me": true, "sender": "Me", "block_start": 0, "block_end": 5,
	}})
	m.Truncated = true
	m.Meta["occurred_at"] = "2026-09-09T09:00:00Z"
	got := Derive(m, now)
	if got.Participation != nil || got.EventSource != EventSourceOccurredAt || !got.Eligible {
		t.Fatalf("truncated evidence must not become participation: %+v", got)
	}
}

func TestDeriveUnknownGroupAndFutureEvidence(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	m := conversation("imessage", "imessage/chat", "hello", []map[string]any{{
		"evidence_ref": "imessage/chat#1", "at": "2026-09-11T00:00:00Z", "from_me": true, "sender": "Me", "block_start": 0, "block_end": 5,
	}})
	m.Meta["occurred_at"] = "2026-09-09T09:00:00Z"
	got := Derive(m, now)
	if got.Eligible || got.EventSource != EventSourceMessageEvidence || got.Participation == nil || got.Participation.IsGroup != nil {
		t.Fatalf("future evidence selects and excludes without group guess: %+v", got)
	}
}

func TestDeriveGmailNewestValidatedEvidenceThenOccurredAt(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	m := memory.Memory{ID: "gmail_thread/t", Provider: "gmail", Text: "From: one@example.com\nold\n\n---\n\nFrom: two@example.com\nnew", Meta: map[string]any{"messages": []map[string]any{
		{"message_ref": "gmail_thread/t#1", "sender": "one@example.com", "at": "2026-09-08T09:00:00Z"},
		{"message_ref": "gmail_thread/t#2", "sender": "two@example.com", "at": "2026-09-09T10:00:00Z"},
	}, "occurred_at": "2026-09-01T00:00:00Z"}}
	got := Derive(m, now)
	if got.EventSource != EventSourceMessageEvidence || got.EventAt.Format(time.RFC3339) != "2026-09-09T10:00:00Z" || got.Participation != nil {
		t.Fatalf("gmail evidence projection = %+v", got)
	}
	m.Meta["messages"] = []map[string]any{{"message_ref": "wrong#1", "sender": "one@example.com", "at": "2026-09-09T10:00:00Z"}}
	got = Derive(m, now)
	if got.EventSource != EventSourceOccurredAt || got.EventAt.Format(time.RFC3339) != "2026-09-01T00:00:00Z" {
		t.Fatalf("invalid Gmail evidence must fall back: %+v", got)
	}
}

func TestCalendarAndRangeOrderingBoundariesAndTies(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	from := mustTime(t, "2026-09-09T12:00:00Z")
	memories := []memory.Memory{
		{ID: "z", Provider: "calendar", CreatedAt: "2099-01-01T00:00:00Z", Meta: map[string]any{"occurred_at": "2026-09-10T12:00:00Z"}},
		{ID: "a", Provider: "applecal", Meta: map[string]any{"occurred_at": "2026-09-10T12:00:00Z"}},
		{ID: "lower", Provider: "calendar", Meta: map[string]any{"occurred_at": "2026-09-09T12:00:00Z"}},
		{ID: "old", Provider: "calendar", Meta: map[string]any{"occurred_at": "2026-09-09T11:59:59Z"}},
		{ID: "future", Provider: "calendar", Meta: map[string]any{"occurred_at": "2026-09-10T12:00:01Z"}},
		{ID: "write-only", Provider: "calendar", CreatedAt: "2026-09-10T11:00:00Z"},
	}
	got := SelectRange(memories, from, now)
	want := []string{"a", "z", "lower"}
	if len(got) != len(want) {
		t.Fatalf("got %d results: %+v", len(got), got)
	}
	for i, id := range want {
		if got[i].Memory.ID != id {
			t.Fatalf("result[%d] = %q, want %q", i, got[i].Memory.ID, id)
		}
	}
}

func conversation(provider, id, text string, evidence []map[string]any) memory.Memory {
	return memory.Memory{ID: id, Provider: provider, Text: text, Meta: map[string]any{"message_evidence": evidence}}
}
func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
