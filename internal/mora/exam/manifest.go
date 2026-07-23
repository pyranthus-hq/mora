package exam

import (
	"errors"
	"fmt"
	"reflect"
)

const (
	RuleIdentity          = "identity"
	RuleTimestamp         = "timestamp"
	RuleTransition        = "transition"
	RuleDirection         = "direction"
	RuleClosure           = "closure"
	RuleEvidenceSpan      = "evidence_span"
	RuleReplyChainQuotes  = "reply_chain_quotes"
	RuleSelfAttendee      = "self_is_attendee"
	RuleOneDefectArtifact = "one_defect_per_artifact"
	RuleClassBalance      = "class_balance"
	RulePersonaHygiene    = "persona_hygiene"
	RuleChannelGrain      = "channel_grain_connector_survivability"

	LintRealIdentity = "real_identity_ledger"
	LintCorpusBytes  = "real_identity_corpus"
	LintLabelLeak    = "label_leak"
	LintDateLeak     = "date_fingerprint"
	LintTitleLeak    = "title_fingerprint"
)

// Ledger schema versions. Version 1 is the obligations-v1 contract and its
// rendering is frozen — the pinned corpus hashes depend on it. Version 2
// unlocks realism features (structural quoting, wrapped bodies, composite
// artifacts). Version 3 preserves ordered Gmail per-message evidence. Each
// validated corpus stays bound to exactly one schema and renderer version.
const (
	SchemaV1 = 1
	SchemaV2 = 2
	SchemaV3 = 3
)

var RequiredValidatorRules = []string{
	RuleIdentity,
	RuleTimestamp,
	RuleTransition,
	RuleDirection,
	RuleClosure,
	RuleEvidenceSpan,
	RuleReplyChainQuotes,
	RuleSelfAttendee,
	RuleOneDefectArtifact,
	RuleClassBalance,
	RulePersonaHygiene,
	RuleChannelGrain,
}

var RequiredLints = []string{LintRealIdentity, LintCorpusBytes}

// Metric ids. There is exactly one per scored field on the Scorecard, and the
// registry meta-test fails by name if a field is added without one — so a number
// cannot enter the scoreboard without also declaring the sabotage that moves it.
const (
	MetricExtraction         = "extraction"
	MetricRecallUncapped     = "recall_uncapped"
	MetricCitationCoverage   = "citation_coverage"
	MetricCitationCorrect    = "citation_correct"
	MetricCounterparty       = "counterparty"
	MetricDedupCrossArtifact = "dedup_cross_artifact"
	MetricThirdPartyLeaks    = "third_party_leaks"
	MetricClosedLeaks        = "closed_leaks"
	MetricDupLeaks           = "dup_leaks"
	MetricNonObligationLeaks = "non_obligation_leaks"
	MetricCriticalIdentity   = "critical_identity"
	MetricCriticalDirection  = "critical_direction"
	MetricDirectionScorable  = "direction_scorable"
	MetricOwner              = "owner"
	MetricDirection          = "direction"
	MetricDueTime            = "due_time"
	MetricLifecycle          = "lifecycle"
	MetricClosureLinkage     = "closure_linkage"
	MetricCommitmentIdentity = "commitment_identity"
	MetricDedup              = "dedup"
	MetricCitationRoles      = "citation_roles"
	MetricLooseMatches       = "loose_matches"
	MetricUnmatched          = "unmatched"
)

// The slices every ratcheted metric must carry a floor for. A healthy corpus-wide
// average with one collapsed channel underneath it is a regression the global number
// structurally cannot see.
const (
	SliceChannel    = "channel"
	SliceBlockKind  = "block_kind"
	SliceOwner      = "owner"
	SliceDirection  = "direction"
	SliceState      = "state"
	SliceTransition = "transition"
)

// Archetypes of a labelled span that must not surface. They are what let owner,
// lifecycle and dedup be MEASURED with no typed product field to read.
const (
	ArchetypeNonObligation = "non_obligation"
	ArchetypeThirdParty    = "third_party"
	ArchetypeClosed        = "closed"
	ArchetypeDuplicate     = "duplicate"
	ArchetypeUnexpected    = "unexpected"
)

// The two policies a metric may never soften. A zero denominator is N/A and N/A is
// a failure (Finding 6); an invalid run is a hard failure, never a zero score.
const (
	PolicyNAIsFailure = "n_a_is_failure"
	PolicyHardFail    = "hard_fail"

	DirectionHigherBetter = "higher_better"
	DirectionLowerBetter  = "lower_better"

	UnitRatio = "ratio"
	UnitCount = "count"
	UnitFlag  = "flag"

	AggregationMicro    = "micro"
	AggregationPerClass = "per_class"
	AggregationAbsolute = "absolute"
)

// MetricSpec is the sensitivity contract for one number. SabotageCases is ENFORCED,
// not documented: a metric that names no red-team row is decoration, and
// TestEveryMetricHasASabotageCase fails by metric name.
type MetricSpec struct {
	ID                    string
	Field                 string
	Version               int
	Description           string
	Direction             string
	Unit                  string
	Aggregation           string
	ZeroDenominatorPolicy string
	InvalidRunPolicy      string
	RequiredSlices        []string
	SabotageCases         []string
}

// ValidateMetricManifest is shared by the package-level sensitivity test and
// the Gate 1 integrity exit. Keeping the registry contract here prevents those
// two trust checks from drifting into subtly different definitions.
func ValidateMetricManifest() error {
	registered := map[string]bool{}
	for _, id := range RequiredRedTeamRows {
		registered[id.Name] = true
	}
	var problems []error
	for _, spec := range RequiredMetrics {
		if spec.Version < 1 || spec.Version > ScorerVersion {
			problems = append(problems, fmt.Errorf("metric %q version = %d, want 1..scorer version %d", spec.ID, spec.Version, ScorerVersion))
		}
		if len(spec.SabotageCases) == 0 {
			problems = append(problems, fmt.Errorf("EVAL_BROKEN: metric %q declares no sabotage case", spec.ID))
			continue
		}
		for _, row := range spec.SabotageCases {
			if !registered[row] {
				problems = append(problems, fmt.Errorf("EVAL_BROKEN: metric %q names sabotage row %q, which has no registered baseline", spec.ID, row))
			}
		}
		if spec.ZeroDenominatorPolicy != PolicyNAIsFailure {
			problems = append(problems, fmt.Errorf("metric %q zero-denominator policy = %q, want %q", spec.ID, spec.ZeroDenominatorPolicy, PolicyNAIsFailure))
		}
		if spec.InvalidRunPolicy != PolicyHardFail {
			problems = append(problems, fmt.Errorf("metric %q invalid-run policy = %q, want %q", spec.ID, spec.InvalidRunPolicy, PolicyHardFail))
		}
		if len(spec.RequiredSlices) == 0 {
			problems = append(problems, fmt.Errorf("metric %q declares no required slices; a global average must never hide a collapsed slice", spec.ID))
		}
	}
	return errors.Join(problems...)
}

// ValidateMetricRegistryCoverage proves that every numeric Scorecard field is
// registered before ValidateMetricManifest proves what sabotages it.
func ValidateMetricRegistryCoverage() error {
	registered := map[string]bool{}
	var problems []error
	for _, spec := range RequiredMetrics {
		if registered[spec.Field] {
			problems = append(problems, fmt.Errorf("two metrics claim scorecard field %q", spec.Field))
		}
		registered[spec.Field] = true
	}
	nonMetrics := map[string]bool{"ScorerVersion": true, "Surface": true}
	scorecard := reflect.TypeOf(Scorecard{})
	for i := 0; i < scorecard.NumField(); i++ {
		name := scorecard.Field(i).Name
		if !nonMetrics[name] && !registered[name] {
			problems = append(problems, fmt.Errorf("EVAL_BROKEN: scorecard field %q has no MetricSpec, so nothing proves it can move", name))
		}
	}
	for field := range registered {
		if _, ok := scorecard.FieldByName(field); !ok {
			problems = append(problems, fmt.Errorf("metric registry names %q, which is not a scorecard field", field))
		}
	}
	for field := range nonMetrics {
		if _, ok := scorecard.FieldByName(field); !ok {
			problems = append(problems, fmt.Errorf("the non-metric exclusion list names %q, which is not a scorecard field", field))
		}
	}
	return errors.Join(problems...)
}

var everySlice = []string{SliceChannel, SliceBlockKind, SliceOwner, SliceDirection, SliceState, SliceTransition}

var RequiredMetrics = []MetricSpec{
	{
		ID: MetricExtraction, Field: "Extraction", Version: 1,
		Description: "obligation extraction P/R — meeting at quote grain, daily at artifact grain",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowEmptyBrief, RowEveryQuestion, RowCopyTheInput, RowAuthoredToQuoted, RowCitationSpanMove, RowDailyEmpty, RowGraphStateInsensitive},
	},
	{
		ID: MetricRecallUncapped, Field: "RecallUncapped", Version: 1,
		Description: "the same meeting run at a raised per-attendee cap — makes the ranker cap a number, not a hidden confound",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowEmptyBrief},
	},
	{
		ID: MetricCitationCoverage, Field: "CitationCoverage", Version: 2,
		Description: "surfaced lines carrying a citation that resolves in the corpus",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowDailyCitation},
	},
	{
		ID: MetricCitationCorrect, Field: "CitationCorrect", Version: 2,
		Description: "the cited memory is the memory that carries the gold evidence — SEPARATE from coverage",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowUnsupportedCitation, RowDailyCitation},
	},
	{
		ID: MetricCounterparty, Field: "Counterparty", Version: 2,
		Description: "per-class recall over the person each line is attributed to — never accuracy",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationPerClass,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowIdentityFlip},
	},
	{
		ID: MetricDedupCrossArtifact, Field: "DedupCrossArtifact", Version: 1,
		Description: "a DuplicateOf pair surfacing from two DIFFERENT memory ids — the only dedup counter that can be nonzero",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowCopyTheInput},
	},
	{
		ID: MetricThirdPartyLeaks, Field: "ThirdPartyLeaks", Version: 1,
		Description: "a commitment owed by a third party presented to the user",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowGoldOwnerFlip, RowGateDisableSweep},
	},
	{
		ID: MetricClosedLeaks, Field: "ClosedLeaks", Version: 1,
		Description: "a gold-CLOSED obligation presented as current — the consequential open/closed failure",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowClosedAsOpen, RowInventoryOriginEscape, RowGateDisableSweep},
	},
	{
		ID: MetricDupLeaks, Field: "DupLeaks", Version: 1,
		Description: "a duplicate of a commitment presented as a second obligation",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowCopyTheInput},
	},
	{
		ID: MetricNonObligationLeaks, Field: "NonObligationLeaks", Version: 1,
		Description: "a labelled non-obligation (footer, marketing, quoted context, …) surfaced as an obligation",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowEveryQuestion, RowCopyTheInput, RowGateDisableSweep},
	},
	{
		ID: MetricCriticalIdentity, Field: "CriticalIdentity", Version: 1,
		Description: "a line attributed to the wrong person — the #135 sev-1 class",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowIdentityFlip, RowGateDisableSweep},
	},
	{
		ID: MetricCriticalDirection, Field: "CriticalDirection", Version: 1,
		Description: "a wrong TYPED direction, or a third-party obligation presented in the user-owed lane",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowDirectionFlip, RowGoldOwnerFlip, RowGateDisableSweep},
	},
	{
		ID: MetricDirectionScorable, Field: "DirectionScorable", Version: 1,
		Description: "false while every prediction is \"unknown\" — blocks a vacuous zero from reading as a measurement",
		Direction:   DirectionHigherBetter, Unit: UnitFlag, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowOracle},
	},
	{
		ID: MetricOwner, Field: "Owner", Version: 2,
		Description: "BORN-RED: direct predicted owner, scored per class against commitment ownership",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationPerClass,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowOwnerPredictionFlip},
	},
	{
		ID: MetricDirection, Field: "Direction", Version: 1,
		Description: "BORN-RED: per-class direction recall; the real adapters can only emit \"unknown\"",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationPerClass,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowDirectionFlip, RowConstantClassifier, RowOracle},
	},
	{
		ID: MetricDueTime, Field: "DueTime", Version: 1,
		Description: "BORN-RED: the typed due value; no production surface carries one",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowOracle, RowClosedAsOpen},
	},
	{
		ID: MetricLifecycle, Field: "Lifecycle", Version: 2,
		Description: "BORN-RED: open | closed | superseded over the complete commitment inventory",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowClosedAsOpen, RowInventoryLifecycleFlip, RowOracle},
	},
	{
		ID: MetricClosureLinkage, Field: "ClosureLinkage", Version: 2,
		Description: "BORN-RED: the memory that closed each commitment in the complete inventory",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowClosedAsOpen, RowOracle},
	},
	{
		ID: MetricCommitmentIdentity, Field: "CommitmentIdentity", Version: 2,
		Description: "BORN-RED: immutable commitment identity anchored to opening evidence",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowCommitmentIdentityFlip},
	},
	{
		ID: MetricDedup, Field: "Dedup", Version: 2,
		Description: "BORN-RED: DuplicateOf points from each labelled copy to its canonical commitment",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowDuplicatePointerFlip},
	},
	{
		ID: MetricCitationRoles, Field: "CitationRoles", Version: 2,
		Description: "BORN-RED: opener, closure, and supporting-copy citations retain their typed roles",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationPerClass,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowCitationRoleFlip},
	},
	{
		ID: MetricLooseMatches, Field: "LooseMatches", Version: 1,
		Description: "a hit that needed containment instead of equality — brittleness is REPORTED, never absorbed",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowCopyTheInput},
	},
	{
		ID: MetricUnmatched, Field: "Unmatched", Version: 1,
		Description: "a surfaced line matching NO labelled span in the whole ledger — grounding failure",
		Direction:   DirectionLowerBetter, Unit: UnitCount, Aggregation: AggregationAbsolute,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowSyntheticGibberish},
	},
}

// Red-team row ids.
const (
	RowSyntheticGibberish     = "a_synthetic_gibberish"
	RowEmptyBrief             = "b_empty_brief"
	RowEveryQuestion          = "c_every_question"
	RowCopyTheInput           = "d_copy_the_input"
	RowIdentityFlip           = "e_identity_flip"
	RowDirectionFlip          = "f_direction_flip"
	RowUnsupportedCitation    = "g_unsupported_citation"
	RowConstantClassifier     = "h_constant_classifier"
	RowDailyEmpty             = "i_daily_empty"
	RowDailyCitation          = "j_daily_citation"
	RowOracle                 = "k_oracle"
	RowClosedAsOpen           = "l_closed_as_open"
	RowGoldOwnerFlip          = "m_gold_owner_flip"
	RowCitationSpanMove       = "n_citation_span_move"
	RowAuthoredToQuoted       = "o_authored_to_quoted"
	RowRemovedSource          = "p_removed_source"
	RowDuplicateNoise         = "q_duplicate_noise"
	RowInputOrder             = "r_input_order"
	RowGateDisableSweep       = "s_gate_disable_sweep"
	RowGraphStateInsensitive  = "t_graph_state_insensitive"
	RowOwnerPredictionFlip    = "u_owner_prediction_flip"
	RowCommitmentIdentityFlip = "v_commitment_identity_flip"
	RowDuplicatePointerFlip   = "w_duplicate_pointer_flip"
	RowCitationRoleFlip       = "x_citation_role_flip"
	RowInventoryLifecycleFlip = "y_inventory_lifecycle_flip"
	RowInventoryOriginEscape  = "z_inventory_origin_escape"
)

// RedTeamRowID is version-pinned by (surface, name). TestScorerRedTeam iterates THIS
// list, so deleting a subtest cannot delete the failure.
type RedTeamRowID struct {
	Surface string
	Name    string
}

var RequiredRedTeamRows = []RedTeamRowID{
	{SurfaceMeeting, RowSyntheticGibberish},
	{SurfaceMeeting, RowEmptyBrief},
	{SurfaceMeeting, RowEveryQuestion},
	{SurfaceMeeting, RowCopyTheInput},
	{SurfaceMeeting, RowIdentityFlip},
	{SurfaceMeeting, RowDirectionFlip},
	{SurfaceMeeting, RowUnsupportedCitation},
	{SurfaceMeeting, RowConstantClassifier},
	{SurfaceDaily, RowDailyEmpty},
	{SurfaceDaily, RowDailyCitation},
	{SurfaceMeeting, RowOracle},
	{SurfaceDaily, RowOracle},
	{SurfaceMeeting, RowClosedAsOpen},
	{SurfaceMeeting, RowGoldOwnerFlip},
	{SurfaceMeeting, RowCitationSpanMove},
	{SurfaceMeeting, RowAuthoredToQuoted},
	{SurfaceMeeting, RowRemovedSource},
	{SurfaceDaily, RowRemovedSource},
	{SurfaceMeeting, RowDuplicateNoise},
	{SurfaceDaily, RowDuplicateNoise},
	{SurfaceMeeting, RowInputOrder},
	{SurfaceDaily, RowInputOrder},
	{SurfaceMeeting, RowGateDisableSweep},
	{SurfaceMeeting, RowGraphStateInsensitive},
	{SurfaceMeeting, RowOwnerPredictionFlip},
	{SurfaceMeeting, RowCommitmentIdentityFlip},
	{SurfaceMeeting, RowDuplicatePointerFlip},
	{SurfaceMeeting, RowCitationRoleFlip},
	{SurfaceMeeting, RowInventoryLifecycleFlip},
	{SurfaceMeeting, RowInventoryOriginEscape},
}

// ProductionExclusionGates are the seventeen gates reachable from
// buildMeetingBriefFromEvent. Disabling any of them must cost something SCORED — a
// gate whose removal moves no number is not protecting anything.
var ProductionExclusionGates = []string{
	"classifyMeetingBriefEvidence",
	"isMeetingNotification",
	"assignedToThirdParty",
	"memoryIsServiceOnly",
	"userOwnedOpenLoop",
	"meetingBriefIsTwoPartyExchange",
	"relationalEvidenceIDs",
	"meetingBriefResolveAttribution",
	"stripURLs",
	"unwrapHardWraps",
	"senderAuthoredBody",
	"stripSpeakerPrefix",
	"isForwardedSubject",
	"isLeadInFragment",
	"stripNoiseTokens",
	"gmailActionableAsk",
	"containsPhrase",
}

func knownSlice(slice string) bool {
	for _, s := range everySlice {
		if s == slice {
			return true
		}
	}
	return false
}
