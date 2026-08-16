package digest

import (
	"strings"
	"testing"
)

func budgetFixture() Model {
	return Model{Generated: "2026-07-02T00:00:00Z", Sections: []Section{{Source: "calendar", Label: "Calendar", State: "delta", Items: []Item{{ID: "c1", Title: "Standup", Snippet: "daily sync", Source: "calendar", Change: "new"}}}, {Source: "gmail", Label: "Emails", State: "delta", Items: []Item{{ID: "g1", Title: "UrgentApproval", Snippet: "sign the MSA by 5pm today", Source: "gmail", Change: "new"}}}}}
}
func reserve(d Model) int {
	r := len(Header(d)) + len(Freshness(d)) + len(UrgentShelf(d)) + len(StaleTasks(d))
	for _, s := range d.Sections {
		r += len(SectionHeading(s)) + len(MoreLine(len(s.Items)+s.MoreCount))
	}
	return r
}
func TestBudgetMarkdownDropsTailItemAndReportsSurvivors(t *testing.T) {
	d := budgetFixture()
	budget := reserve(d) + len(ItemLine(d.Sections[0].Items[0]))
	bd, survived := Budget(d, budget, 16000, 1)
	if survived["g1"] || !survived["c1"] {
		t.Fatalf("survived=%v", survived)
	}
	out := Render(bd, 1<<20)
	if strings.Contains(out, "UrgentApproval") || !strings.Contains(out, "Standup") {
		t.Fatalf("out=%s", out)
	}
}
func TestBudgetMarkdownShelfBoundedBySmallBudget(t *testing.T) {
	d := Model{Generated: "2026-07-02T00:00:00Z", Urgent: []Item{{ID: "u1", Title: "Urgent one", Snippet: "sign by eod", Change: "new"}, {ID: "u2", Title: "Urgent two", Snippet: "sign by eod", Change: "new"}, {ID: "u3", Title: "Urgent three", Snippet: "sign by eod", Change: "new"}}}
	budget := len(Header(d)) + 64 + len(ItemLine(d.Urgent[0]))
	bd, survived := Budget(d, budget, 16000, 1)
	if len(survived) == 0 || len(survived) == len(d.Urgent) {
		t.Fatalf("survived=%v", survived)
	}
	out := Render(bd, budget)
	for id := range survived {
		if !strings.Contains(out, "(id: "+id+")") {
			t.Fatalf("id=%s out=%s", id, out)
		}
	}
	if bd.UrgentMore != len(d.Urgent)-len(survived) {
		t.Fatalf("more=%d", bd.UrgentMore)
	}
}
func TestBudgetMarkdownAllFitAllSurvive(t *testing.T) {
	d := budgetFixture()
	bd, s := Budget(d, 1<<20, 16000, 1)
	if !s["c1"] || !s["g1"] || len(bd.Sections) != 2 || len(bd.Sections[0].Items) != 1 || len(bd.Sections[1].Items) != 1 {
		t.Fatalf("bd=%+v survived=%v", bd, s)
	}
}
func TestBudgetMarkdownKeptLinesWithinBudget(t *testing.T) {
	d := budgetFixture()
	full := Render(d, 1<<20)
	budget := len(full) - len(ItemLine(d.Sections[1].Items[0]))
	bd, s := Budget(d, budget, 16000, 1)
	out := Render(bd, 1<<20)
	for _, section := range bd.Sections {
		for _, it := range section.Items {
			if !s[it.ID] || !strings.Contains(out, "(id: "+it.ID+")") {
				t.Fatalf("item=%s out=%s s=%v", it.ID, out, s)
			}
		}
	}
}
func TestRenderDigestStructureAndObligations(t *testing.T) {
	d := Model{Generated: "2026-07-02T00:00:00Z", SinceHours: 24, HealthBanner: "⚠ health", Freshness: map[string]string{"gmail": "now", "calendar": "earlier"}, Urgent: []Item{{ID: "u", Title: "Urgent", Snippet: "now", Direction: "owed_by_self", Owner: Atom{Kind: "address", Value: "me@x"}, CounterpartyLabel: " Sam ", DueAt: "today", Lifecycle: "open", ClosureRef: "none"}}, UrgentMore: 2, Sections: []Section{{Label: "Emails", State: "baseline", MoreCount: 1, Items: []Item{{ID: "g", Title: "Mail", Snippet: "body", Change: "updated", Obligations: []Obligation{{CommitmentID: "c", Summary: "  send   thing ", Owner: Atom{Kind: "address", Value: "me@x"}, Direction: "owed_by_self", DueAt: "none", Lifecycle: "open", ClosureRef: "none", Citations: []Citation{{Role: "opener", MemoryID: "g", CommitmentID: "c"}}}}}}}}, StaleTasks: []string{"old task"}}
	out := RenderBody(d)
	for _, want := range []string{"last 24h", "⚠ health", "calendar earlier · gmail now", "⚠ Urgent (1)", "+2 more urgent", "Emails — baseline (2)", "[updated] Mail", "counterparty=Sam", "summary=send thing", "citation: role=opener", "Open tasks (1 stale)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}
func TestHeadingAndChangeLabels(t *testing.T) {
	for state, want := range map[string]string{"no changes since last brief": "no changes", "stale": "stale", "unavailable": "unavailable", "delta": "Emails (0)"} {
		got := Heading(Section{Label: "Emails", State: state})
		if !strings.Contains(got, want) {
			t.Fatalf("state=%s got=%s", state, got)
		}
	}
	if ChangePrefix("new") != "[new] " || ChangePrefix("updated") != "[updated] " || ChangePrefix("x") != "" {
		t.Fatal("prefix")
	}
}
func TestBudgetAllElidedExplanationAndEmptySection(t *testing.T) {
	d := budgetFixture()
	d.Sections = append(d.Sections, Section{Source: "empty", Label: "Empty", Items: nil})
	bd, s := Budget(d, 1, 16000, 1)
	if len(s) != 0 || bd.EmptyExplanation != "all items elided by token budget" || bd.Sections[0].ElidedByBudget != 1 || !bd.Sections[0].Truncated || bd.Sections[2].Items == nil {
		t.Fatalf("bd=%+v s=%v", bd, s)
	}
}
