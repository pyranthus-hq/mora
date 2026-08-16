package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestBoundedReadRequestedIsPresenceBased(t *testing.T) {
	if BoundedReadRequested(map[string]any{}) {
		t.Fatal("empty args requested")
	}
	for _, k := range []string{"match", "max_tokens", "occurrence"} {
		if !BoundedReadRequested(map[string]any{k: nil}) {
			t.Errorf("presence of %s ignored", k)
		}
	}
}
func TestApplyBoundedReadMatchAndOccurrence(t *testing.T) {
	m := memory.Memory{ID: "m", Text: "alpha target one. middle target two. omega"}
	out, r := ApplyBoundedRead(m, map[string]any{"match": "target", "occurrence": float64(2), "max_tokens": float64(4)})
	if !r.Matched || r.MatchCount != 2 || r.Occurrence != 2 || r.Budget != 4 {
		t.Fatalf("receipt=%+v", r)
	}
	if !strings.Contains(strings.ToLower(out.Text), "target") {
		t.Fatalf("excerpt=%q", out.Text)
	}
	if m.Text == out.Text {
		t.Fatal("input/full body returned")
	}
}
func TestApplyBoundedReadNoMatchIsHonest(t *testing.T) {
	m := memory.Memory{ID: "m", Text: "secret full body"}
	out, r := ApplyBoundedRead(m, map[string]any{"match": "absent"})
	if r.Matched || r.MatchCount != 0 || !r.Truncated || out.Text != "" {
		t.Fatalf("out=%q receipt=%+v", out.Text, r)
	}
}
func TestApplyBoundedReadWordBoundary(t *testing.T) {
	m := memory.Memory{ID: "m", Text: "category cat scatter cat"}
	_, r := ApplyBoundedRead(m, map[string]any{"match": "cat"})
	if r.MatchCount != 2 {
		t.Fatalf("count=%d", r.MatchCount)
	}
}
func TestApplyBoundedReadNoPhraseStillBounds(t *testing.T) {
	m := memory.Memory{ID: "m", Text: strings.Repeat("word ", 1000)}
	out, r := ApplyBoundedRead(m, map[string]any{"max_tokens": float64(2)})
	if !r.Matched || !r.Truncated || r.Budget != 2 || len([]rune(out.Text)) > 8 {
		t.Fatalf("len=%d receipt=%+v", len([]rune(out.Text)), r)
	}
}
func TestApplyBoundedReadBudgetCeiling(t *testing.T) {
	m := memory.Memory{ID: "m", Text: strings.Repeat("x ", 20000)}
	out, r := ApplyBoundedRead(m, map[string]any{"max_tokens": float64(20000)})
	if len([]rune(out.Text)) > boundedExcerptCharCap || r.Budget != 20000 {
		t.Fatalf("len=%d receipt=%+v", len([]rune(out.Text)), r)
	}
}
func TestBoundedReadReceiptJSONComposition(t *testing.T) {
	b, err := json.Marshal(BoundedReadReceipt{ID: "m", Matched: true, MatchCount: 1, Occurrence: 1, Budget: 9})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "evidence_ref") || !strings.Contains(s, `"match_count":1`) {
		t.Fatalf("json=%s", s)
	}
}
