package oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Frozen checker identity for the public synthetic suite (#544).
// Changing these after observing comparison results invalidates that
// comparison and requires a newly labeled evaluation.
const (
	CheckerID      = "oracle-checker-synth-v1"
	CheckerVersion = "1.0.0-synth"
	PolicyID       = "scoring-policy-synth-v1"
	ArtifactSchema = "mora.pilotreplay.oracle_artifact"
	ArtifactVer    = 1
	GradeSchema    = "mora.pilotreplay.oracle_grade"
	GradeVer       = 1
	HiddenSchema   = "mora.pilotreplay.oracle_hidden_spec"
	HiddenVer      = 1
)

// Grade dimensions. Scored separately; never collapsed into a single opaque
// "agent did well" bit without dimension evidence.
const (
	DimFailedTask            = "failed_task_outcome"
	DimValidMemoryConstraint = "valid_memory_constraint"
	DimUnrelatedRegression   = "unrelated_regression"
	DimNoRelevantMemory      = "no_relevant_memory_control"
)

// Dimension status values.
const (
	DimPass          = "pass"
	DimFail          = "fail"
	DimInconclusive  = "inconclusive"
	DimError         = "error"
	DimNotApplicable = "not_applicable"
)

// Missing / unsupported / grader-error policies. All forbid agent success.
const (
	PolicyInconclusiveNeverSuccess = "inconclusive_never_success"
	PolicyErrorNeverSuccess        = "error_never_success"
)

// AggregateRuleID documents how dimensions roll up to an OutcomeDocument.Kind.
const AggregateRuleID = "any_fail_is_fail; all_required_pass_is_pass; else_inconclusive; error_dominates_to_error"

// ScoringPolicy is the frozen scoring contract. Digest this; do not mutate
// after results are observed for a labeled evaluation.
type ScoringPolicy struct {
	PolicyID            string   `json:"policy_id"`
	CheckerID           string   `json:"checker_id"`
	CheckerVersion      string   `json:"checker_version"`
	MissingOutputPolicy string   `json:"missing_output_policy"`
	UnsupportedPolicy   string   `json:"unsupported_artifact_policy"`
	GraderErrorPolicy   string   `json:"grader_error_policy"`
	AggregateRule       string   `json:"aggregate_rule"`
	DimensionOrder      []string `json:"dimension_order"`
	// AllowedFeedbackFields are the only fields safe to surface toward
	// contender / repair authoring. Detail bodies and gold stay evaluator-only.
	AllowedFeedbackFields []string `json:"allowed_feedback_fields"`
	// ForbiddenFeedback lists material classes that must never appear in
	// AllowedFeedback or runner-visible payloads.
	ForbiddenFeedback []string `json:"forbidden_feedback"`
	ClaimsEfficacy    bool     `json:"claims_efficacy"` // must remain false
	Notes             string   `json:"notes,omitempty"`
}

// FrozenScoringPolicy returns the immutable public synthetic policy.
func FrozenScoringPolicy() ScoringPolicy {
	return ScoringPolicy{
		PolicyID:            PolicyID,
		CheckerID:           CheckerID,
		CheckerVersion:      CheckerVersion,
		MissingOutputPolicy: PolicyInconclusiveNeverSuccess,
		UnsupportedPolicy:   PolicyInconclusiveNeverSuccess,
		GraderErrorPolicy:   PolicyErrorNeverSuccess,
		AggregateRule:       AggregateRuleID,
		DimensionOrder: []string{
			DimFailedTask,
			DimValidMemoryConstraint,
			DimUnrelatedRegression,
			DimNoRelevantMemory,
		},
		AllowedFeedbackFields: []string{
			"outcome_kind",
			"dimension_statuses",
			"checker_id",
			"checker_version",
			"checker_digest",
			"policy_id",
			"artifact_digest",
			"scored",
		},
		ForbiddenFeedback: []string{
			"gold_labels",
			"expected_answers",
			"hidden_test_bodies",
			"reference_patches",
			"post_cutoff_evidence",
			"dimension_detail_bodies",
			"hidden_spec",
		},
		ClaimsEfficacy: false,
		Notes:          "Synthetic scoring policy for #544. Establishes checker sensitivity only; not repair benefit.",
	}
}

// PolicyDigest returns the SHA-256 hex digest of the canonical policy JSON.
func PolicyDigest(p ScoringPolicy) (string, error) {
	return canonicalDigest(p)
}

// CheckerFreezeRecord binds checker identity to digests for a labeled eval.
type CheckerFreezeRecord struct {
	CheckerID      string `json:"checker_id"`
	CheckerVersion string `json:"checker_version"`
	PolicyID       string `json:"policy_id"`
	PolicyDigest   string `json:"policy_digest"`
	// HiddenSpecDigest is the digest of the evaluator-side hidden spec used
	// for this labeled comparison. Public synthetic suite digests a synthetic
	// stand-in only; real private gold digests stay out of the public repo.
	HiddenSpecDigest string `json:"hidden_spec_digest"`
	// InputsDigest covers artifact schema + allowed feedback vocabulary.
	InputsDigest   string `json:"inputs_digest"`
	FrozenAtNote   string `json:"frozen_at_note"`
	ClaimsEfficacy bool   `json:"claims_efficacy"`
}

// FreezeChecker builds the freeze record for the public synthetic suite.
func FreezeChecker(hidden HiddenSpec) (CheckerFreezeRecord, error) {
	p := FrozenScoringPolicy()
	pd, err := PolicyDigest(p)
	if err != nil {
		return CheckerFreezeRecord{}, err
	}
	hd, err := HiddenSpecDigest(hidden)
	if err != nil {
		return CheckerFreezeRecord{}, err
	}
	inputs := map[string]any{
		"artifact_schema":         ArtifactSchema,
		"artifact_schema_version": ArtifactVer,
		"allowed_feedback":        p.AllowedFeedbackFields,
		"forbidden_feedback":      p.ForbiddenFeedback,
		"missing_output_policy":   p.MissingOutputPolicy,
		"unsupported_policy":      p.UnsupportedPolicy,
		"grader_error_policy":     p.GraderErrorPolicy,
		"dimension_order":         p.DimensionOrder,
	}
	id, err := canonicalDigest(inputs)
	if err != nil {
		return CheckerFreezeRecord{}, err
	}
	return CheckerFreezeRecord{
		CheckerID:        CheckerID,
		CheckerVersion:   CheckerVersion,
		PolicyID:         PolicyID,
		PolicyDigest:     pd,
		HiddenSpecDigest: hd,
		InputsDigest:     id,
		FrozenAtNote:     "Frozen before observing comparison results for the public synthetic suite (#544).",
		ClaimsEfficacy:   false,
	}, nil
}

// CheckerDigest is PolicyDigest || HiddenSpecDigest || InputsDigest combined.
func CheckerDigest(rec CheckerFreezeRecord) string {
	h := sha256.New()
	_, _ = h.Write([]byte(rec.PolicyDigest))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(rec.HiddenSpecDigest))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(rec.InputsDigest))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(rec.CheckerID))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(rec.CheckerVersion))
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalDigest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", errf(CodeInternal, "digest", "marshal: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// sortedKeys returns map keys in sorted order (deterministic digests / tests).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
