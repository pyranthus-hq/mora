package exam

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
	MetricDirection          = "direction"
	MetricDueTime            = "due_time"
	MetricLifecycle          = "lifecycle"
	MetricClosureLinkage     = "closure_linkage"
	MetricLooseMatches       = "loose_matches"
	MetricUnmatched          = "unmatched"
)

// The slices every ratcheted metric must carry a floor for. A corpus-wide 0.91 with
// the iMessage channel at 0.20 is a regression the global number cannot see.
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

var everySlice = []string{SliceChannel, SliceBlockKind, SliceOwner, SliceDirection, SliceState, SliceTransition}

var RequiredMetrics = []MetricSpec{
	{
		ID: MetricExtraction, Field: "Extraction", Version: 1,
		Description: "obligation extraction P/R — meeting at quote grain, daily at artifact grain",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowEmptyBrief, RowEveryQuestion, RowCopyTheInput, RowAuthoredToQuoted, RowCitationSpanMove, RowDailyEmpty},
	},
	{
		ID: MetricRecallUncapped, Field: "RecallUncapped", Version: 1,
		Description: "the same meeting run at a raised per-attendee cap — makes the ranker cap a number, not a hidden confound",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowEmptyBrief, RowAuthoredToQuoted},
	},
	{
		ID: MetricCitationCoverage, Field: "CitationCoverage", Version: 1,
		Description: "surfaced lines carrying a citation that resolves in the corpus",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowDailyCitation},
	},
	{
		ID: MetricCitationCorrect, Field: "CitationCorrect", Version: 1,
		Description: "the cited memory is the memory that carries the gold evidence — SEPARATE from coverage",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowUnsupportedCitation, RowDailyCitation},
	},
	{
		ID: MetricCounterparty, Field: "Counterparty", Version: 1,
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
		SabotageCases: []string{RowClosedAsOpen, RowGateDisableSweep},
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
		SabotageCases: []string{RowOracle, RowConstantClassifier},
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
		ID: MetricLifecycle, Field: "Lifecycle", Version: 1,
		Description: "BORN-RED: open | closed | superseded; no production surface carries one",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowClosedAsOpen, RowOracle},
	},
	{
		ID: MetricClosureLinkage, Field: "ClosureLinkage", Version: 1,
		Description: "BORN-RED: the memory that closed the obligation; no production surface carries one",
		Direction:   DirectionHigherBetter, Unit: UnitRatio, Aggregation: AggregationMicro,
		ZeroDenominatorPolicy: PolicyNAIsFailure, InvalidRunPolicy: PolicyHardFail, RequiredSlices: everySlice,
		SabotageCases: []string{RowClosedAsOpen, RowOracle},
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

// Red-team row ids. Row (t) — the graph-state-insensitive scorer — needs the
// two-state merge run, which lands with the correction-flywheel fixture; it is
// deliberately NOT registered here, because a manifested row with no baseline is a
// named failure and a silently registered row that nothing exercises is worse.
const (
	RowSyntheticGibberish  = "a_synthetic_gibberish"
	RowEmptyBrief          = "b_empty_brief"
	RowEveryQuestion       = "c_every_question"
	RowCopyTheInput        = "d_copy_the_input"
	RowIdentityFlip        = "e_identity_flip"
	RowDirectionFlip       = "f_direction_flip"
	RowUnsupportedCitation = "g_unsupported_citation"
	RowConstantClassifier  = "h_constant_classifier"
	RowDailyEmpty          = "i_daily_empty"
	RowDailyCitation       = "j_daily_citation"
	RowOracle              = "k_oracle"
	RowClosedAsOpen        = "l_closed_as_open"
	RowGoldOwnerFlip       = "m_gold_owner_flip"
	RowCitationSpanMove    = "n_citation_span_move"
	RowAuthoredToQuoted    = "o_authored_to_quoted"
	RowRemovedSource       = "p_removed_source"
	RowDuplicateNoise      = "q_duplicate_noise"
	RowInputOrder          = "r_input_order"
	RowGateDisableSweep    = "s_gate_disable_sweep"
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
