package imessage

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"testing"
)

func commitmentMemory(t *testing.T, times []string) memory.Memory {
	t.Helper()
	const id = "imessage_chat/same-thread-review"
	lines := []string{"Lucia: Can you send the review notes?", "Me: I sent the review notes.", "Lucia: Got the review notes, thanks."}
	body := "## 2026-08-05\n" + strings.Join(lines, "\n")
	entries := make([]map[string]any, 0, len(lines))
	cursor := 0
	for i, line := range lines {
		start := strings.Index(body[cursor:], line) + cursor
		end := start + len(line)
		cursor = end
		fromMe := i == 1
		sender := "Lucia"
		if fromMe {
			sender = "Me"
		}
		entries = append(entries, map[string]any{"evidence_ref": id + "#" + []string{"ask", "delivery", "ack"}[i], "at": times[i], "from_me": fromMe, "sender": sender, "block_start": start, "block_end": end})
	}
	return memory.Memory{ID: id, Type: "imessage", Provider: "imessage", Source: "same-thread-review", CreatedAt: times[len(times)-1], Text: body, Meta: map[string]any{"occurred_at": times[len(times)-1], "message_count": "3", "message_evidence_schema": 1, "message_evidence": entries}}
}
func TestCommitmentMessagesStrictEvidence(t *testing.T) {
	times := []string{"2026-08-05T10:00:00Z", "2026-08-05T10:05:00Z", "2026-08-05T10:06:00Z"}
	m := commitmentMemory(t, times)
	got, present := CommitmentMessages(m)
	if !present || len(got) != 3 || got[0].MessageRef != m.ID+"#ask" || got[0].Self || got[0].Body != "Can you send the review notes?" || !got[1].Self || got[1].BlockRef != "body" || got[2].At != times[2] {
		t.Fatalf("messages=%+v present=%v", got, present)
	}
	legacy := m
	delete(legacy.Meta, "message_evidence")
	delete(legacy.Meta, "message_evidence_schema")
	if got, present := CommitmentMessages(legacy); present || got != nil {
		t.Fatalf("legacy=%+v,%v", got, present)
	}
	for _, test := range []struct {
		name   string
		mutate func(*memory.Memory)
	}{{"bad schema", func(m *memory.Memory) { m.Meta["message_evidence_schema"] = 2 }}, {"diagnostic", func(m *memory.Memory) { m.Meta["message_evidence_diagnostics"] = []string{"missing"} }}, {"count", func(m *memory.Memory) { m.Meta["message_count"] = "bad" }}, {"backward time", func(m *memory.Memory) {
		m.Meta["message_evidence"].([]map[string]any)[1]["at"] = "2026-08-05T09:00:00Z"
	}}, {"sender mismatch", func(m *memory.Memory) { m.Meta["message_evidence"].([]map[string]any)[1]["sender"] = "Lucia" }}, {"coverage", func(m *memory.Memory) { m.Text += "\nuntrusted" }}} {
		t.Run(test.name, func(t *testing.T) {
			bad := commitmentMemory(t, times)
			test.mutate(&bad)
			if got, present := CommitmentMessages(bad); !present || len(got) != 0 {
				t.Fatalf("got=%+v present=%v", got, present)
			}
		})
	}
}
