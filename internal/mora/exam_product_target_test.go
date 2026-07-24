package mora

import (
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

type namedExamScorecard struct {
	corpus  string
	surface string
	state   examProductScorecardState
	card    exam.Scorecard
}

type examProductMetricRequirement string

const (
	examProductMetricRequired           examProductMetricRequirement = "REQUIRED"
	examProductMetricLegacyUnscorable   examProductMetricRequirement = "LEGACY_UNSCORABLE"
	examProductLegacyUnscorableEvidence                              = "frozen schema-v1/v2 vaults lack immutable message/block refs; commitmentID must not fabricate them"
)

type examProductMetricState struct {
	requirement examProductMetricRequirement
	reason      string
}

// examProductScorecardState makes corpus capability part of every strict row.
// The three evidence-joined ratios are required once schema v3 supplies immutable
// message/block refs. Frozen v1/v2 vaults cannot support those joins, and
// commitmentID must never fabricate refs merely to make an undefined ratio green.
type examProductScorecardState struct {
	schemaVersion      int
	commitmentIdentity examProductMetricState
	dedup              examProductMetricState
	citationRoles      examProductMetricState
}

// TestExamProductTarget keeps the dimensioned product contract executable over
// all three corpora and every shipped surface.
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
		t.Fatalf("FIXED: all three corpora now meet the strict product target; flip wantRED:false and close %s", targetIssue)
	}
	if !hasTypedProductDeficiency(rows) {
		t.Fatalf("known-RED pin drifted: typed obligation rows are now scorable even though another target dimension remains red; inspect and update the dated pin for %s", targetIssue)
	}
	t.Logf("known RED through %s: %d strict product-target assertions fail across all three corpora; tracked by %s and daily contract %s",
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
		{name: "obligations-v3", root: examFixtureV3Root},
	} {
		ledger := loadExamLedgerFromRoot(t, corpus.root)
		state := examProductScorecardStateForSchema(ledger.Version)
		scorecards := runExamSurfaces(t, corpus.root)
		rows = append(rows,
			namedExamScorecard{corpus: corpus.name, surface: "daily CLI", state: state, card: scorecards.DailyCLI},
			namedExamScorecard{corpus: corpus.name, surface: "daily MCP", state: state, card: scorecards.DailyMCP},
			namedExamScorecard{corpus: corpus.name, surface: "event CLI", state: state, card: scorecards.EventCLI},
			namedExamScorecard{corpus: corpus.name, surface: "event MCP", state: state, card: scorecards.EventMCP},
		)
	}
	return rows
}

func TestExamProductTargetCorpusScope(t *testing.T) {
	rows := recomputeExamProductScorecards(t)
	if len(rows) != 12 {
		t.Fatalf("product scorecard rows = %d, want 3 corpora x 4 surfaces", len(rows))
	}
	seen := map[string]int{}
	for _, row := range rows {
		seen[row.corpus]++
		metrics := []struct {
			name  string
			state examProductMetricState
		}{
			{name: "commitment identity", state: row.state.commitmentIdentity},
			{name: "dedup", state: row.state.dedup},
			{name: "citation roles", state: row.state.citationRoles},
		}
		switch row.state.schemaVersion {
		case exam.SchemaV1, exam.SchemaV2:
			for _, metric := range metrics {
				if metric.state.requirement != examProductMetricLegacyUnscorable ||
					metric.state.reason != examProductLegacyUnscorableEvidence {
					t.Errorf("%s %s %s state = %+v, want explicit legacy-unscorable immutable-ref classification",
						row.corpus, row.surface, metric.name, metric.state)
				}
			}
		case exam.SchemaV3:
			for _, metric := range metrics {
				if metric.state.requirement != examProductMetricRequired || metric.state.reason != "" {
					t.Errorf("%s %s %s state = %+v, want required with no exemption",
						row.corpus, row.surface, metric.name, metric.state)
				}
			}
			if !row.card.CommitmentIdentity.Defined {
				t.Errorf("%s %s commitment identity = %+v, want defined on schema v3",
					row.corpus, row.surface, row.card.CommitmentIdentity)
			}
		default:
			t.Errorf("%s %s schema version = %d, want 1, 2, or 3", row.corpus, row.surface, row.state.schemaVersion)
		}
	}
	for _, corpus := range []string{"obligations-v1", "obligations-v2", "obligations-v3"} {
		if seen[corpus] != 4 {
			t.Errorf("%s scorecard rows = %d, want four shipped surfaces", corpus, seen[corpus])
		}
	}
}

func examProductScorecardStateForSchema(schemaVersion int) examProductScorecardState {
	var evidence examProductMetricState
	switch schemaVersion {
	case exam.SchemaV1, exam.SchemaV2:
		evidence = examProductMetricState{
			requirement: examProductMetricLegacyUnscorable,
			reason:      examProductLegacyUnscorableEvidence,
		}
	default:
		if schemaVersion >= exam.SchemaV3 {
			evidence = examProductMetricState{requirement: examProductMetricRequired}
		}
	}
	return examProductScorecardState{
		schemaVersion:      schemaVersion,
		commitmentIdentity: evidence,
		dedup:              evidence,
		citationRoles:      evidence,
	}
}

func examProductTargetFailures(rows []namedExamScorecard) []string {
	var failures []string
	for _, row := range rows {
		label := row.corpus + " " + row.surface
		card := row.card
		if card.ScorerVersion != exam.ScorerVersion {
			failures = append(failures, fmt.Sprintf("%s scorer version = %d, want %d", label, card.ScorerVersion, exam.ScorerVersion))
		}
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
			{name: "cross-artifact duplicates", count: card.DedupCrossArtifact},
			{name: "non-obligation leaks", count: card.NonObligationLeaks},
			{name: "loose matches", count: card.LooseMatches},
			{name: "unmatched", count: card.Unmatched},
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
		failures = append(failures, ratioAtLeastFailures(label+" owner", card.Owner, 0.90)...)
		failures = append(failures, scopedRatioAtLeastFailures(
			label+" commitment identity", row.state.schemaVersion, row.state.commitmentIdentity, card.CommitmentIdentity, 0.90,
		)...)
		failures = append(failures, scopedRatioAtLeastFailures(
			label+" dedup", row.state.schemaVersion, row.state.dedup, card.Dedup, 0.90,
		)...)
		failures = append(failures, scopedRatioAtLeastFailures(
			label+" citation roles", row.state.schemaVersion, row.state.citationRoles, card.CitationRoles, 0.90,
		)...)
		for _, dimension := range []struct {
			name string
			row  exam.PR
		}{
			{name: "owner", row: card.Owner},
			{name: "direction", row: card.Direction},
			{name: "due time", row: card.DueTime},
			{name: "lifecycle", row: card.Lifecycle},
		} {
			failures = append(failures, classRecallAtLeastFailures(label+" "+dimension.name, dimension.row, 0.90)...)
		}
		if row.state.citationRoles.requirement == examProductMetricRequired {
			failures = append(failures, classRecallAtLeastFailures(label+" citation roles", card.CitationRoles, 0.90)...)
		}
		if card.Surface == exam.SurfaceMeeting {
			failures = append(failures, ratioAtLeastFailures(label+" counterparty", card.Counterparty, 0.90)...)
			failures = append(failures, classRecallAtLeastFailures(label+" counterparty", card.Counterparty, 0.90)...)
		}
	}
	return failures
}

func scopedRatioAtLeastFailures(label string, schemaVersion int, state examProductMetricState, got exam.PR, want float64) []string {
	switch state.requirement {
	case examProductMetricRequired:
		if schemaVersion < exam.SchemaV3 {
			return []string{fmt.Sprintf("%s state = %s at schema %d, want %s with the immutable-ref reason",
				label, state.requirement, schemaVersion, examProductMetricLegacyUnscorable)}
		}
		if state.reason != "" {
			return []string{fmt.Sprintf("%s required state carries stale unscorable reason %q", label, state.reason)}
		}
		return ratioAtLeastFailures(label, got, want)
	case examProductMetricLegacyUnscorable:
		if schemaVersion >= exam.SchemaV3 {
			return []string{fmt.Sprintf("%s state = %s at schema %d, want %s",
				label, state.requirement, schemaVersion, examProductMetricRequired)}
		}
		if state.reason != examProductLegacyUnscorableEvidence {
			return []string{fmt.Sprintf("%s legacy-unscorable reason = %q, want %q",
				label, state.reason, examProductLegacyUnscorableEvidence)}
		}
		if got.Defined {
			return []string{label + " is defined on a legacy corpus without immutable refs; commitmentID evidence must not be fabricated"}
		}
		return nil
	default:
		return []string{fmt.Sprintf("%s has no metric requirement at schema %d; unscorable rows must never be silently skipped",
			label, schemaVersion)}
	}
}

func classRecallAtLeastFailures(label string, got exam.PR, want float64) []string {
	if len(got.RecallByClass) == 0 {
		return []string{label + " has no per-class recall, want explicit class floors"}
	}
	var failures []string
	classes := make([]string, 0, len(got.RecallByClass))
	for class := range got.RecallByClass {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		recall := got.RecallByClass[class]
		if recall < want {
			failures = append(failures, fmt.Sprintf("%s class %q recall = %.6f, want >= %.2f", label, class, recall, want))
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
			!card.Owner.Defined ||
			requiredMetricUndefined(row.state.commitmentIdentity, card.CommitmentIdentity) ||
			requiredMetricUndefined(row.state.dedup, card.Dedup) ||
			requiredMetricUndefined(row.state.citationRoles, card.CitationRoles) {
			return true
		}
	}
	return false
}

func requiredMetricUndefined(state examProductMetricState, row exam.PR) bool {
	return state.requirement == examProductMetricRequired && !row.Defined
}
