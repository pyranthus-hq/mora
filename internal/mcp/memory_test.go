package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func decisionFixture(_, asOf, durability, flip, review string) *memory.DecisionValidity {
	return &memory.DecisionValidity{AsOf: asOf, Durability: durability, FlipConditions: []string{flip}, ReviewBy: review}
}

func TestMemoryFromArgsDefaultsAndExactFields(t *testing.T) {
	now := time.Date(2026, 8, 13, 2, 3, 4, 0, time.FixedZone("x", 3600))
	got, err := MemoryFromArgs(map[string]any{"title": "Fact", "text": "Body"}, now, decisionFixture)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "global" || got.Type != "insight" || got.Title != "Fact" || got.Text != "Body" || got.Source != "mcp" || got.CreatedAt != "2026-08-13T02:03:04+01:00" || got.Decision != nil {
		t.Fatalf("memory=%+v", got)
	}
	got, err = MemoryFromArgs(map[string]any{"scope": "work", "type": "note", "title": "T", "text": "X", "source": "agent"}, now, decisionFixture)
	if err != nil || got.Scope != "work" || got.Type != "note" || got.Source != "agent" {
		t.Fatalf("explicit=(%+v,%v)", got, err)
	}
}

func TestMemoryFromArgsRequiresTitleAndText(t *testing.T) {
	for _, args := range []map[string]any{{"text": "x"}, {"title": "x"}, {"title": 7, "text": "x"}} {
		if _, err := MemoryFromArgs(args, time.Time{}, decisionFixture); err == nil || err.Error() != "title and text required" {
			t.Errorf("args=%v err=%v", args, err)
		}
	}
}

func TestMemoryFromArgsDecisionDelegatesAllValidityFields(t *testing.T) {
	called := false
	builder := func(created, asOf, durability, flip, review string) *memory.DecisionValidity {
		called = true
		got := strings.Join([]string{created, asOf, durability, flip, review}, "|")
		want := "2026-08-13T00:00:00Z|2026-08-01|durable|if x; if y|2027-01-01"
		if got != want {
			t.Fatalf("builder=%q", got)
		}
		return &memory.DecisionValidity{AsOf: asOf}
	}
	args := map[string]any{"type": "decision", "title": "D", "text": "Do", "as_of": "2026-08-01", "durability": "durable", "flip_conditions": "if x; if y", "review_by": "2027-01-01"}
	got, err := MemoryFromArgs(args, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), builder)
	if err != nil || !called || got.Decision == nil || got.Decision.AsOf != "2026-08-01" {
		t.Fatalf("got=(%+v,%v) called=%v", got, err, called)
	}
}

func TestMemoryFromArgsRejectsDecisionFieldsForOtherTypes(t *testing.T) {
	for _, field := range []string{"as_of", "durability", "flip_conditions", "review_by"} {
		args := map[string]any{"title": "T", "text": "X", field: "value"}
		if _, err := MemoryFromArgs(args, time.Time{}, decisionFixture); err == nil || err.Error() != "decision validity fields require type=decision" {
			t.Errorf("field=%s err=%v", field, err)
		}
	}
}
