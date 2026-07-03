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

// mdReserveBytes mirrors budgetDigestForMarkdown's chrome reservation (frame + shelf +
// open-tasks + per-section heading & "+N more" line), so a test can compute the exact
// item budget that remains.
func mdReserveBytes(d Digest) int {
	r := len(renderDigestHeader(d)) + len(renderDigestFreshness(d)) +
		len(renderDigestUrgentShelf(d)) + len(renderDigestStaleTasks(d))
	for _, s := range d.Sections {
		r += len(renderDigestSectionHeading(s)) + len(renderDigestMoreLine(len(s.Items)+s.MoreCount))
	}
	return r
}

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
	// A budget that reserves the chrome + exactly the calendar item, leaving no room
	// for the gmail (tail) item.
	budget := mdReserveBytes(d) + len(renderDigestItemLine(d.Sections[0].Items[0]))

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

// TestBudgetMarkdownShelfBoundedBySmallBudget (review finding, codex #3): the Urgent
// shelf is highest-priority but still budget-bounded — a budget too small for the whole
// shelf keeps only the fitting items as SURVIVORS and folds the rest into UrgentMore, so
// the "survived ⟹ rendered" invariant holds even when the shelf overflows the budget.
func TestBudgetMarkdownShelfBoundedBySmallBudget(t *testing.T) {
	d := Digest{
		Generated: "2026-07-02T00:00:00Z",
		Urgent: []DigestItem{
			{ID: "u1", Title: "Urgent one", Snippet: "sign by eod", Source: "gmail", Change: "new"},
			{ID: "u2", Title: "Urgent two", Snippet: "sign by eod", Source: "gmail", Change: "new"},
			{ID: "u3", Title: "Urgent three", Snippet: "sign by eod", Source: "gmail", Change: "new"},
		},
	}
	// Budget fits the header + shelf chrome + a single urgent item line.
	budget := len(renderDigestHeader(d)) + 64 + len(renderDigestItemLine(d.Urgent[0]))

	bd, survived := budgetDigestForMarkdown(d, budget)

	kept := 0
	for _, it := range d.Urgent {
		if survived[it.ID] {
			kept++
		}
	}
	if kept == 0 || kept == len(d.Urgent) {
		t.Fatalf("small budget must PARTIALLY bound the shelf; kept=%d of %d", kept, len(d.Urgent))
	}
	// The invariant: EVERY survived id renders in the budgeted output (no clipped survivor).
	out := renderDigest(bd, budget)
	for id := range survived {
		if !strings.Contains(out, "(id: "+id+")") {
			t.Fatalf("survived id %q missing from the rendered brief (survived⟹rendered violated):\n%s", id, out)
		}
	}
	if bd.UrgentMore != len(d.Urgent)-kept {
		t.Fatalf("overflow urgent items must fold into UrgentMore; got %d want %d", bd.UrgentMore, len(d.Urgent)-kept)
	}
}

// TestBriefSurfacedCountIncludesUrgent (review finding, codex #2): an urgent-only delta
// must not be treated as empty (which would drop the shelf via the 24h window fallback).
func TestBriefSurfacedCountIncludesUrgent(t *testing.T) {
	d := Digest{Urgent: []DigestItem{{ID: "g1", Title: "Deadline"}}}
	if briefSurfacedItemCount(d) == 0 {
		t.Fatalf("a digest whose only surfaced item is on the urgent shelf must not count as empty")
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
