package mora

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

type namedExamScorecard struct {
	corpus  string
	surface string
	card    exam.Scorecard
}

// TestExamProductTarget keeps the dimensioned product contract executable over
// both corpora and every shipped surface.
//
// Gate 1 proves the exam is trustworthy; it does not claim the product passes it. This target stays red until #138.
func TestExamProductTarget(t *testing.T) {
	rows := recomputeExamProductScorecards(t)
	failures := examProductTargetFailures(rows)
	if os.Getenv("MORA_EXAM_PRODUCT_TARGET") == "1" {
		for _, failure := range failures {
			t.Error(failure)
		}
		return
	}

	const (
		wantRED      = true
		targetIssue  = "https://github.com/pyranthus-hq/mora/issues/138"
		dailyIssue   = "https://github.com/pyranthus-hq/mora/issues/154"
		knownRedEnds = "2026-10-14"
	)
	if !wantRED {
		for _, failure := range failures {
			t.Error(failure)
		}
		return
	}
	if len(failures) == 0 {
		t.Fatalf("FIXED: both corpora now meet the strict product target; flip wantRED:false and close %s", targetIssue)
	}
	if !hasTypedProductDeficiency(rows) {
		t.Fatalf("known-RED pin drifted: typed obligation rows are now scorable even though another target dimension remains red; inspect and update the dated pin for %s", targetIssue)
	}
	t.Logf("known RED through %s: %d strict product-target assertions fail across both corpora; tracked by %s and daily contract %s",
		knownRedEnds, len(failures), targetIssue, dailyIssue)
}

func recomputeExamProductScorecards(t *testing.T) []namedExamScorecard {
	t.Helper()
	var rows []namedExamScorecard
	for _, corpus := range []struct {
		name string
		root string
	}{
		{name: "obligations-v1", root: examFixtureRoot},
		{name: "obligations-v2", root: examFixtureV2Root},
	} {
		scorecards := runExamSurfaces(t, corpus.root)
		rows = append(rows,
			namedExamScorecard{corpus: corpus.name, surface: "daily CLI", card: scorecards.DailyCLI},
			namedExamScorecard{corpus: corpus.name, surface: "daily MCP", card: scorecards.DailyMCP},
			namedExamScorecard{corpus: corpus.name, surface: "event CLI", card: scorecards.EventCLI},
			namedExamScorecard{corpus: corpus.name, surface: "event MCP", card: scorecards.EventMCP},
		)
	}
	return rows
}

func examProductTargetFailures(rows []namedExamScorecard) []string {
	var failures []string
	for _, row := range rows {
		label := row.corpus + " " + row.surface
		card := row.card
		failures = append(failures, ratioAtLeastFailures(label+" extraction", card.Extraction, 0.90)...)
		failures = append(failures, ratioEqualFailures(label+" citation coverage", card.CitationCoverage, 1.0)...)
		failures = append(failures, ratioEqualFailures(label+" citation correctness", card.CitationCorrect, 1.0)...)
		for _, absolute := range []struct {
			name  string
			count int
		}{
			{name: "critical identity", count: card.CriticalIdentity},
			{name: "critical direction", count: card.CriticalDirection},
			{name: "third-party leaks", count: card.ThirdPartyLeaks},
			{name: "closed leaks", count: card.ClosedLeaks},
			{name: "duplicate leaks", count: card.DupLeaks},
			{name: "non-obligation leaks", count: card.NonObligationLeaks},
		} {
			if absolute.count != 0 {
				failures = append(failures, fmt.Sprintf("%s %s = %d, want 0", label, absolute.name, absolute.count))
			}
		}
		if !card.DirectionScorable {
			failures = append(failures, label+" direction_scorable = false, want true")
		}
		failures = append(failures, ratioAtLeastFailures(label+" direction", card.Direction, 0.90)...)
		failures = append(failures, ratioAtLeastFailures(label+" due time", card.DueTime, 0.90)...)
		failures = append(failures, ratioAtLeastFailures(label+" lifecycle", card.Lifecycle, 0.90)...)
		failures = append(failures, ratioAtLeastFailures(label+" closure linkage", card.ClosureLinkage, 0.90)...)
		if card.Owner == "" || card.Owner == exam.OwnerUnscorable {
			failures = append(failures, fmt.Sprintf("%s owner = %q, want a scorable owner result", label, card.Owner))
		}
	}
	return failures
}

func ratioAtLeastFailures(label string, got exam.PR, want float64) []string {
	if !got.Defined {
		return []string{label + " is unscorable, want a defined ratio"}
	}
	var failures []string
	if got.Precision < want {
		failures = append(failures, fmt.Sprintf("%s precision = %.6f, want >= %.2f", label, got.Precision, want))
	}
	if got.Recall < want {
		failures = append(failures, fmt.Sprintf("%s recall = %.6f, want >= %.2f", label, got.Recall, want))
	}
	return failures
}

func ratioEqualFailures(label string, got exam.PR, want float64) []string {
	if !got.Defined {
		return []string{label + " is unscorable, want a defined ratio"}
	}
	var failures []string
	if got.Precision != want {
		failures = append(failures, fmt.Sprintf("%s precision = %.6f, want %.2f", label, got.Precision, want))
	}
	if got.Recall != want {
		failures = append(failures, fmt.Sprintf("%s recall = %.6f, want %.2f", label, got.Recall, want))
	}
	return failures
}

func hasTypedProductDeficiency(rows []namedExamScorecard) bool {
	for _, row := range rows {
		card := row.card
		if !card.DirectionScorable ||
			!card.Direction.Defined ||
			!card.DueTime.Defined ||
			!card.Lifecycle.Defined ||
			!card.ClosureLinkage.Defined ||
			strings.TrimSpace(card.Owner) == "" ||
			card.Owner == exam.OwnerUnscorable {
			return true
		}
	}
	return false
}
