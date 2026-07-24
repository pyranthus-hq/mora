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
	examProductMetricRequired                 examProductMetricRequirement = "REQUIRED"
	examProductMetricLegacyUnscorable         examProductMetricRequirement = "LEGACY_UNSCORABLE"
	examProductMetricNoGoldSamplesUnscorable  examProductMetricRequirement = "NO_GOLD_SAMPLES_UNSCORABLE"
	examProductLegacyUnscorableEvidence                                    = "frozen schema-v1/v2 vaults lack immutable message/block refs; commitmentID must not fabricate them"
	examProductLegacyInventoryRecallEvidence                               = "frozen schema-v1/v2 vaults lack immutable message/block refs; empty-ID commitments cannot safely join inventory lifecycle/closure gold"
	examProductV1DailySelectionEvidence                                    = "schema-v1 OBLIGATIONS.md defines no daily surface-relevance rule; gold placement is ledger-explicit"
	examProductNoDuplicateGoldSamplesEvidence                              = "corpus has zero duplicate_of gold samples; ratio is undefined and absolute duplicate leak counts remain enforced"
)

type examProductMetricState struct {
	requirement examProductMetricRequirement
	reason      string
}

// examProductScorecardState makes corpus and surface capability part of every
// strict row. Component states keep the measurable half of a ratio required while
// classifying only the structurally impossible half as legacy-unscorable. Evidence-
// joined ratios become required once the corpus supplies immutable message/block
// refs; commitmentID must never fabricate refs merely to make a ratio green.
type examProductScorecardState struct {
	schemaVersion       int
	extractionPrecision examProductMetricState
	lifecycleRecall     examProductMetricState
	closureRecall       examProductMetricState
	commitmentIdentity  examProductMetricState
	dedup               examProductMetricState
	citationRoles       examProductMetricState
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
		t.Fatalf("known-RED pin drifted: typed obligation rows now meet their required floors even though another target dimension remains red; inspect and update the dated pin for %s", targetIssue)
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
		scorecards := runExamSurfaces(t, corpus.root)
		rows = append(rows,
			namedExamScorecard{corpus: corpus.name, surface: "daily CLI", state: examProductScorecardStateForLedger(ledger, exam.SurfaceDaily), card: scorecards.DailyCLI},
			namedExamScorecard{corpus: corpus.name, surface: "daily MCP", state: examProductScorecardStateForLedger(ledger, exam.SurfaceDaily), card: scorecards.DailyMCP},
			namedExamScorecard{corpus: corpus.name, surface: "event CLI", state: examProductScorecardStateForLedger(ledger, exam.SurfaceMeeting), card: scorecards.EventCLI},
			namedExamScorecard{corpus: corpus.name, surface: "event MCP", state: examProductScorecardStateForLedger(ledger, exam.SurfaceMeeting), card: scorecards.EventMCP},
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
		wantExtractionPrecision := examProductMetricState{requirement: examProductMetricRequired}
		if row.state.schemaVersion == exam.SchemaV1 && row.card.Surface == exam.SurfaceDaily {
			wantExtractionPrecision = examProductMetricState{
				requirement: examProductMetricLegacyUnscorable,
				reason:      examProductV1DailySelectionEvidence,
			}
		}
		if row.state.extractionPrecision != wantExtractionPrecision {
			t.Errorf("%s %s extraction precision state = %+v, want %+v",
				row.corpus, row.surface, row.state.extractionPrecision, wantExtractionPrecision)
		}
		wantInventoryRecall := examProductMetricState{requirement: examProductMetricRequired}
		if row.state.schemaVersion < exam.SchemaV3 {
			wantInventoryRecall = examProductMetricState{
				requirement: examProductMetricLegacyUnscorable,
				reason:      examProductLegacyInventoryRecallEvidence,
			}
		}
		if row.state.lifecycleRecall != wantInventoryRecall {
			t.Errorf("%s %s lifecycle recall state = %+v, want %+v",
				row.corpus, row.surface, row.state.lifecycleRecall, wantInventoryRecall)
		}
		if row.state.closureRecall != wantInventoryRecall {
			t.Errorf("%s %s closure recall state = %+v, want %+v",
				row.corpus, row.surface, row.state.closureRecall, wantInventoryRecall)
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
				if metric.name == "dedup" {
					continue
				}
				if metric.state.requirement != examProductMetricRequired || metric.state.reason != "" {
					t.Errorf("%s %s %s state = %+v, want required with no exemption",
						row.corpus, row.surface, metric.name, metric.state)
				}
			}
			if row.state.dedup.requirement != examProductMetricNoGoldSamplesUnscorable ||
				row.state.dedup.reason != examProductNoDuplicateGoldSamplesEvidence {
				t.Errorf("%s %s dedup state = %+v, want explicit zero-gold-samples classification",
					row.corpus, row.surface, row.state.dedup)
			}
			if row.card.Dedup.Defined {
				t.Errorf("%s %s dedup = %+v, want undefined with zero duplicate_of gold samples",
					row.corpus, row.surface, row.card.Dedup)
			}
			if row.card.DupLeaks != 0 || row.card.DedupCrossArtifact != 0 {
				t.Errorf("%s %s duplicate leak absolutes = DupLeaks:%d DedupCrossArtifact:%d, want 0/0",
					row.corpus, row.surface, row.card.DupLeaks, row.card.DedupCrossArtifact)
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

func examProductScorecardStateForLedger(ledger exam.Ledger, surface string) examProductScorecardState {
	required := examProductMetricState{requirement: examProductMetricRequired}
	var evidence examProductMetricState
	switch ledger.Version {
	case exam.SchemaV1, exam.SchemaV2:
		evidence = examProductMetricState{
			requirement: examProductMetricLegacyUnscorable,
			reason:      examProductLegacyUnscorableEvidence,
		}
	default:
		if ledger.Version >= exam.SchemaV3 {
			evidence = examProductMetricState{requirement: examProductMetricRequired}
		}
	}
	dedup := evidence
	if ledger.Version >= exam.SchemaV3 {
		hasDuplicateGold := false
		for _, commitment := range ledger.Commitments {
			if commitment.DuplicateOf != "" {
				hasDuplicateGold = true
				break
			}
		}
		if !hasDuplicateGold {
			dedup = examProductMetricState{
				requirement: examProductMetricNoGoldSamplesUnscorable,
				reason:      examProductNoDuplicateGoldSamplesEvidence,
			}
		}
	}
	extractionPrecision := required
	if ledger.Version == exam.SchemaV1 && surface == exam.SurfaceDaily {
		extractionPrecision = examProductMetricState{
			requirement: examProductMetricLegacyUnscorable,
			reason:      examProductV1DailySelectionEvidence,
		}
	}
	inventoryRecall := required
	if ledger.Version < exam.SchemaV3 {
		inventoryRecall = examProductMetricState{
			requirement: examProductMetricLegacyUnscorable,
			reason:      examProductLegacyInventoryRecallEvidence,
		}
	}
	return examProductScorecardState{
		schemaVersion:       ledger.Version,
		extractionPrecision: extractionPrecision,
		lifecycleRecall:     inventoryRecall,
		closureRecall:       inventoryRecall,
		commitmentIdentity:  evidence,
		dedup:               dedup,
		citationRoles:       evidence,
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
		failures = append(failures, ratioComponentsAtLeastFailures(
			label+" extraction", card.Extraction, 0.90,
			row.state.extractionPrecision,
			examProductMetricState{requirement: examProductMetricRequired},
		)...)
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
		failures = append(failures, ratioComponentsAtLeastFailures(
			label+" lifecycle", card.Lifecycle, 0.90,
			examProductMetricState{requirement: examProductMetricRequired},
			row.state.lifecycleRecall,
		)...)
		failures = append(failures, ratioComponentsAtLeastFailures(
			label+" closure linkage", card.ClosureLinkage, 0.90,
			examProductMetricState{requirement: examProductMetricRequired},
			row.state.closureRecall,
		)...)
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
		} {
			failures = append(failures, classRecallAtLeastFailures(label+" "+dimension.name, dimension.row, 0.90)...)
		}
		if row.state.lifecycleRecall.requirement == examProductMetricRequired {
			failures = append(failures, classRecallAtLeastFailures(label+" lifecycle", card.Lifecycle, 0.90)...)
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

func ratioComponentsAtLeastFailures(
	label string,
	got exam.PR,
	want float64,
	precisionState, recallState examProductMetricState,
) []string {
	var failures []string
	check := func(component string, state examProductMetricState, value float64) {
		switch state.requirement {
		case examProductMetricRequired:
			if state.reason != "" {
				failures = append(failures, fmt.Sprintf("%s %s required state carries stale unscorable reason %q",
					label, component, state.reason))
				return
			}
			if !got.Defined {
				failures = append(failures, fmt.Sprintf("%s %s is unscorable, want a defined ratio", label, component))
				return
			}
			if value < want {
				failures = append(failures, fmt.Sprintf("%s %s = %.6f, want >= %.2f", label, component, value, want))
			}
		case examProductMetricLegacyUnscorable:
			if state.reason == "" {
				failures = append(failures, fmt.Sprintf("%s %s legacy-unscorable state has no reason", label, component))
			}
		default:
			failures = append(failures, fmt.Sprintf("%s %s has unsupported component state %q",
				label, component, state.requirement))
		}
	}
	check("precision", precisionState, got.Precision)
	check("recall", recallState, got.Recall)
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
	case examProductMetricNoGoldSamplesUnscorable:
		if schemaVersion < exam.SchemaV3 {
			return []string{fmt.Sprintf("%s state = %s at schema %d, want %s with the immutable-ref reason",
				label, state.requirement, schemaVersion, examProductMetricLegacyUnscorable)}
		}
		if state.reason != examProductNoDuplicateGoldSamplesEvidence {
			return []string{fmt.Sprintf("%s zero-gold-samples reason = %q, want %q",
				label, state.reason, examProductNoDuplicateGoldSamplesEvidence)}
		}
		if got.Defined {
			return []string{label + " is defined with zero duplicate_of gold samples; ratio must remain explicitly unscorable"}
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
			ratioBelowProductTarget(card.Direction) ||
			ratioBelowProductTarget(card.DueTime) ||
			ratioComponentBelowProductTarget(card.Lifecycle, row.state.lifecycleRecall) ||
			ratioComponentBelowProductTarget(card.ClosureLinkage, row.state.closureRecall) ||
			ratioBelowProductTarget(card.Owner) ||
			requiredMetricBelowProductTarget(row.state.commitmentIdentity, card.CommitmentIdentity) ||
			requiredMetricBelowProductTarget(row.state.dedup, card.Dedup) ||
			requiredMetricBelowProductTarget(row.state.citationRoles, card.CitationRoles) {
			return true
		}
	}
	return false
}

func ratioBelowProductTarget(row exam.PR) bool {
	return !row.Defined || row.Precision < 0.90 || row.Recall < 0.90
}

func ratioComponentBelowProductTarget(row exam.PR, recallState examProductMetricState) bool {
	return !row.Defined ||
		row.Precision < 0.90 ||
		recallState.requirement == examProductMetricRequired && row.Recall < 0.90
}

func requiredMetricBelowProductTarget(state examProductMetricState, row exam.PR) bool {
	return state.requirement == examProductMetricRequired && ratioBelowProductTarget(row)
}
