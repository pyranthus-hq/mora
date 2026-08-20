package mora

import (
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

// TestBudgetMarkdownDropsTailItemAndReportsSurvivors: with a budget one item-line
// short of the whole brief, the tail (gmail, rank 2) item is dropped from the
// rendered set and is NOT reported as a survivor, while the higher-rank calendar
// item survives.

// TestBudgetMarkdownShelfBoundedBySmallBudget (review finding, codex #3): the Urgent
// shelf is highest-priority but still budget-bounded — a budget too small for the whole
// shelf keeps only the fitting items as SURVIVORS and folds the rest into UrgentMore, so
// the "survived ⟹ rendered" invariant holds even when the shelf overflows the budget.

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

// TestBudgetMarkdownRenderedOutputFitsBudget: whatever survives, the rendered
// budgeted digest's kept item lines never exceed the budget (kept lines are always
// fully present, so the downstream safety clip never bites a committed id).
