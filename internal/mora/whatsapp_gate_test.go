package mora

import (
	"strings"
	"testing"
	"time"
)

func whatsappTestMemory(lane string) Memory {
	return Memory{
		ID: "whatsapp_conversation/15551234567@s.whatsapp.net", Type: "whatsapp", Provider: "whatsapp",
		ProviderID: "15551234567@s.whatsapp.net", Title: "Riya", CreatedAt: "2026-08-13T12:00:00Z",
		Text: "Riya: Can you send the report tomorrow?",
		Meta: map[string]any{
			"relevance_lane": lane, "chat_kind": "direct", "occurred_at": "2026-08-13T12:00:00Z",
			"participants": []map[string]string{{"handle": "15551234567@s.whatsapp.net", "name": "Riya"}},
		},
	}
}

func TestWhatsAppIntelligenceLaneCannotPromote(t *testing.T) {
	m := whatsappTestMemory("intelligence")
	if got := classifyCommitments(m, Config{}); len(got) != 0 {
		t.Fatalf("group intelligence produced commitments: %#v", got)
	}
	m.Text = "Riya: final notice, send this immediately"
	if ok, _ := isUrgent(m, time.Date(2026, 8, 13, 12, 5, 0, 0, time.UTC)); ok {
		t.Fatal("group intelligence reached urgent shelf")
	}
}

func TestWhatsAppDirectLaneUsesConversationCommitmentPipeline(t *testing.T) {
	m := whatsappTestMemory("personal_action")
	got := classifyCommitments(m, Config{})
	if len(got) == 0 || got[0].Direction != commitOwedBySelf {
		t.Fatalf("direct request did not become an owner obligation: %#v", got)
	}
}

// TestWhatsAppGateFailsClosedOnInconsistentMetadata — personal_action is only
// honored on a consistent DIRECT tuple. A group memory with a spoofed or lost
// lane, missing chat_kind, or malformed metadata types must stay informational:
// no commitments, never urgent.
func TestWhatsAppGateFailsClosedOnInconsistentMetadata(t *testing.T) {
	urgentText := "Riya: final notice, send this immediately"
	cases := []struct {
		name   string
		mutate func(*Memory)
	}{
		{"group chat_kind with personal_action lane", func(m *Memory) { m.Meta["chat_kind"] = "group" }},
		{"missing chat_kind", func(m *Memory) { delete(m.Meta, "chat_kind") }},
		{"malformed chat_kind type", func(m *Memory) { m.Meta["chat_kind"] = 7 }},
		{"malformed lane type", func(m *Memory) { m.Meta["relevance_lane"] = true }},
		{"nil meta", func(m *Memory) { m.Meta = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := whatsappTestMemory("personal_action")
			tc.mutate(&m)
			if got := classifyCommitments(m, Config{}); len(got) != 0 {
				t.Fatalf("inconsistent gate metadata produced commitments: %#v", got)
			}
			m.Text = urgentText
			if ok, _ := isUrgent(m, time.Date(2026, 8, 13, 12, 5, 0, 0, time.UTC)); ok {
				t.Fatal("inconsistent gate metadata reached urgent shelf")
			}
		})
	}
}

func TestWhatsAppDigestCarriesInspectableGate(t *testing.T) {
	m := whatsappTestMemory("intelligence")
	m.Meta["inclusion_rationale"] = "group conversation with substantive content; informational only"
	it := digestItemFor(Config{}, m, "whatsapp", "new")
	line := renderDigestArtifactLine(it)
	if !strings.Contains(line, "[intelligence: group conversation") {
		t.Fatalf("gate rationale missing: %s", line)
	}
}
