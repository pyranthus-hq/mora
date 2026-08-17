package contextintent

import (
	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"testing"
)

func TestIntentRoutingAndQualifiers(t *testing.T) {
	for _, tt := range []struct {
		q    string
		want Intent
	}{{"What do I owe on Alpha?", OpenLoops}, {"What's still open?", OpenLoops}, {"What materially changed across Alpha?", CurrentState}, {"open source search", Generic}, {"closed captions", Generic}, {"recently read newsletter", Generic}, {"project Alpha", Generic}, {"Find the email saying what do I owe", Generic}} {
		if got := Of(tt.q); got != tt.want {
			t.Fatalf("Of(%q)=%q want %q", tt.q, got, tt.want)
		}
	}
	got := QualifierTerms("What do I owe on Alpha alpha and βeta?", OpenLoops, map[string]bool{"and": true})
	if strings.Join(got, ",") != "alpha,βeta" {
		t.Fatalf("terms=%q", got)
	}
	if !TermsMatch(got, "ALPHA and βeta") || TermsMatch(got, "alpha only") {
		t.Fatal("term matching drift")
	}
}
func TestCurrentItemsRanksFiltersAndLimits(t *testing.T) {
	items := []memory.Memory{{ID: "bulk", Scope: "global", Title: "Alpha", Meta: map[string]any{"service": true}}, {ID: "project-service", Scope: "project:alpha", Title: "Alpha", Meta: map[string]any{"service": true}}, {ID: "global", Scope: "global", Text: "Alpha"}, {ID: "project", Scope: " PROJECT:Alpha ", Tags: []string{"Alpha"}}, {ID: "other", Scope: "project:beta", Title: "Beta"}}
	service := func(m memory.Memory) bool { return m.Meta["service"] == true }
	got := CurrentItems(items, []string{"alpha"}, 3, service)
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	if strings.Join(ids, ",") != "project,global,project-service" {
		t.Fatalf("ids=%v", ids)
	}
	if Rank(memory.Memory{Scope: "global"}, true) != 3 || Rank(memory.Memory{Scope: "project:x"}, true) != 2 || Rank(memory.Memory{Scope: "global"}, false) != 1 || Rank(memory.Memory{Scope: " project:x "}, false) != 0 {
		t.Fatal("rank drift")
	}
}
func TestOpenItemsEligibilityAndOrdering(t *testing.T) {
	memories := map[string]memory.Memory{"new": {ID: "new", Scope: "project:a", Title: "Alpha"}, "old": {ID: "old", Scope: "project:a", Text: "Alpha"}, "invalid": {ID: "invalid", Scope: "project:a", Text: "Alpha"}, "blocked": {ID: "blocked", Scope: "project:a", Text: "Alpha"}}
	record := func(id, mid, at, state, duplicate string) commitment.Record {
		return commitment.Record{ID: id, Summary: "Alpha task", State: state, DuplicateOf: duplicate, OpenedBy: commitment.Span{MemoryID: mid, OccurredAt: at}}
	}
	inventory := map[string][]commitment.Record{"new": {record("b", "new", "2026-08-01T12:00:00Z", commitment.Open, "")}, "old": {record("a", "old", "2026-08-01T10:00:00Z", commitment.Open, ""), record("closed", "old", "2026-08-01T11:00:00Z", commitment.Closed, "")}, "invalid": {record("c", "invalid", "not-a-time", commitment.Open, "")}, "blocked": {record("d", "blocked", "2026-08-01T13:00:00Z", commitment.Open, "")}, "missing": {record("e", "missing", "2026-08-01T14:00:00Z", commitment.Open, "")}}
	got := OpenItems(inventory, memories, []string{"alpha"}, "project:a", func(m memory.Memory) bool { return m.ID != "blocked" })
	if len(got) != 3 || got[0].ID != "b" || got[1].ID != "a" || got[2].ID != "c" {
		t.Fatalf("got=%+v", got)
	}
}
func TestRenderOpen(t *testing.T) {
	if RenderOpen(nil, 0) != "" {
		t.Fatal("zero budget")
	}
	if got := RenderOpen(nil, 100); got != "# Open commitments\nNo open commitments matched this request.\n" {
		t.Fatalf("empty=%q", got)
	}
	records := []commitment.Record{{Summary: "Send notes", StateUncertain: true, Direction: commitment.OwedBySelf, Due: commitment.Due{Kind: commitment.DueRelative}, OpenedBy: commitment.Span{MemoryID: "m1", MessageRef: "m1#x", OccurredAt: "2026-08-01T10:00:00Z"}}}
	want := "# Open commitments\n- Send notes [open; source freshness uncertain; owed_by_self] [due: relative]\n  Evidence: m1#x at 2026-08-01T10:00:00Z\n"
	if got := RenderOpen(records, 1000); got != want {
		t.Fatalf("render=%q", got)
	}
	if got := RenderOpen(records, 12); len([]rune(got)) != 12 {
		t.Fatalf("bounded=%q", got)
	}
}
