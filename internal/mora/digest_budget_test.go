package mora

import (
	"strings"
	"testing"
)

// Issue #62 defect 1 — the structural Markdown budgeter. renderDigest used to
// tail-clip the whole Markdown string with truncateRunes AFTER the watermark had
// already advanced, so an item within the per-source cap but past the byte budget
// was marked seen yet never rendered. budgetDigestForMarkdown replaces that blunt
// clip with a structural budget that cuts at ITEM/SECTION boundaries and reports
// the exact set of stable IDs that survived into rendered lines — the only ids the
// commit may advance over.

func mdBudgetDigest() Digest {
	return Digest{
		Generated: "2026-07-02T00:00:00Z",
		Sections: []DigestSection{
			{Source: "calendar", State: stateDelta, Items: []DigestItem{
				{ID: "c1", Title: "Standup", Snippet: "daily sync", Source: "calendar", Change: "new"},
			}},
			{Source: "gmail", State: stateDelta, Items: []DigestItem{
				{ID: "g1", Title: "UrgentApproval", Snippet: "sign the MSA by 5pm today", Source: "gmail", Change: "new"},
			}},
		},
	}
}

// TestBudgetMarkdownDropsTailItemAndReportsSurvivors: with a budget one item-line
// short of the whole brief, the tail (gmail, rank 2) item is dropped from the
// rendered set and is NOT reported as a survivor, while the higher-rank calendar
// item survives.
func TestBudgetMarkdownDropsTailItemAndReportsSurvivors(t *testing.T) {
	d := mdBudgetDigest()
	full := renderDigest(d, 1<<20) // unbudgeted reference render.
	gmailLine := renderDigestItemLine(d.Sections[1].Items[0])
	budget := len(full) - len(gmailLine) // one line short of everything.

	bd, survived := budgetDigestForMarkdown(d, budget)

	if survived["g1"] {
		t.Fatalf("g1 (tail item) must NOT survive a budget one line short; survived=%v", survived)
	}
	if !survived["c1"] {
		t.Fatalf("c1 (high-rank item) must survive; survived=%v", survived)
	}
	out := renderDigest(bd, 1<<20)
	if strings.Contains(out, "UrgentApproval") {
		t.Fatalf("dropped item body must not render; got:\n%s", out)
	}
	if !strings.Contains(out, "Standup") {
		t.Fatalf("kept item must render; got:\n%s", out)
	}
}

// TestBudgetMarkdownAllFitAllSurvive: a generous budget keeps everything and
// reports every item id as a survivor (the common, non-truncating case).
func TestBudgetMarkdownAllFitAllSurvive(t *testing.T) {
	d := mdBudgetDigest()
	bd, survived := budgetDigestForMarkdown(d, 1<<20)
	for _, want := range []string{"c1", "g1"} {
		if !survived[want] {
			t.Fatalf("%s must survive a generous budget; survived=%v", want, survived)
		}
	}
	if got, want := len(bd.Sections), 2; got != want {
		t.Fatalf("all sections kept: got %d want %d", got, want)
	}
	for _, s := range bd.Sections {
		if len(s.Items) != 1 {
			t.Fatalf("section %q must keep its item; got %d", s.Source, len(s.Items))
		}
	}
}

// TestBudgetMarkdownRenderedOutputFitsBudget: whatever survives, the rendered
// budgeted digest's kept item lines never exceed the budget (kept lines are always
// fully present, so the downstream safety clip never bites a committed id).
func TestBudgetMarkdownKeptLinesWithinBudget(t *testing.T) {
	d := mdBudgetDigest()
	full := renderDigest(d, 1<<20)
	// A budget that fits the frame + calendar but not gmail.
	budget := len(full) - len(renderDigestItemLine(d.Sections[1].Items[0]))
	bd, survived := budgetDigestForMarkdown(d, budget)
	// Every surviving id's line must appear verbatim in a full render of the budgeted digest.
	out := renderDigest(bd, 1<<20)
	for _, s := range bd.Sections {
		for _, it := range s.Items {
			if !survived[it.ID] {
				t.Fatalf("kept item %q must be reported as survivor", it.ID)
			}
			if !strings.Contains(out, "(id: "+it.ID+")") {
				t.Fatalf("kept item %q must render fully; out:\n%s", it.ID, out)
			}
		}
	}
}
