package report

import "github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"

// Synthetic scorer binding — mirrors public oracle identity by value only.
// Digests are fixed stand-ins for offline report tests; they are NOT a claim
// that a private checker was run.
const (
	SynthCheckerID      = "oracle-checker-synth-v1"
	SynthCheckerVersion = "1.0.0-synth"
	SynthPolicyID       = "scoring-policy-synth-v1"
	// Fixed 64-char hex stand-ins (not derived from private gold).
	SynthCheckerDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	SynthPolicyDigest  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// SyntheticScorer returns the public fixture scorer binding.
func SyntheticScorer() ScorerBinding {
	return ScorerBinding{
		CheckerID:      SynthCheckerID,
		CheckerVersion: SynthCheckerVersion,
		CheckerDigest:  SynthCheckerDigest,
		PolicyDigest:   SynthPolicyDigest,
		PolicyID:       SynthPolicyID,
	}
}

func boolPtr(v bool) *bool { return &v }

func baseTrial(id, caseID, role, condKind, condID string, rep int) TrialRecord {
	s := SyntheticScorer()
	return TrialRecord{
		TrialID:        id,
		CaseID:         caseID,
		CaseRole:       role,
		ConditionKind:  condKind,
		ConditionID:    condID,
		RepIndex:       rep,
		AttemptStatus:  contract.AttemptSucceeded,
		OutcomeKind:    contract.OutcomePass,
		CheckerID:      s.CheckerID,
		CheckerVersion: s.CheckerVersion,
		CheckerDigest:  s.CheckerDigest,
		PolicyDigest:   s.PolicyDigest,
		ArtifactDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Visible:        true,
		Notes:          "Synthetic trial fixture for #545 offline report tests.",
	}
}

// FixtureValidMemoryPreserved is the valid-memory control with constraint intact.
func FixtureValidMemoryPreserved() TrialRecord {
	t := baseTrial(
		"trial-synth-valid-memory-ok-001",
		"case-synth-valid-memory-001",
		contract.CaseRoleValidMemoryControl,
		contract.ConditionReviewedMemoryEdit,
		"cond-edit-case-synth-valid-memory-001",
		0,
	)
	t.ConstraintPreserved = boolPtr(true)
	t.TaskSuccess = boolPtr(true)
	t.UnrelatedRegressionOK = boolPtr(true)
	t.Notes = "Valid-memory control: constraint preserved under edited condition"
	return t
}

// FixtureValidMemoryDropped fails the damage gate by dropping the constraint.
func FixtureValidMemoryDropped() TrialRecord {
	t := baseTrial(
		"trial-synth-valid-memory-drop-001",
		"case-synth-valid-memory-001",
		contract.CaseRoleValidMemoryControl,
		contract.ConditionReviewedMemoryEdit,
		"cond-edit-case-synth-valid-memory-001",
		1,
	)
	t.ConstraintPreserved = boolPtr(false)
	t.TaskSuccess = boolPtr(true) // appears to "win" by dropping constraint
	t.UnrelatedRegressionOK = boolPtr(true)
	t.OutcomeKind = contract.OutcomeFail
	t.DamageGateFailed = true
	t.Notes = "Valid-memory control NEGATIVE: constraint dropped → damage gate fail"
	return t
}

// FixtureNoRelevantMemoryOK succeeds without requiring the proposed memory edit.
func FixtureNoRelevantMemoryOK() TrialRecord {
	t := baseTrial(
		"trial-synth-nrm-ok-001",
		"case-synth-nrm-001",
		contract.CaseRoleNoRelevantMemoryCtrl,
		contract.ConditionOriginalMemory,
		"cond-original-case-synth-nrm-001",
		0,
	)
	t.NoRelevantMemoryOK = boolPtr(true)
	t.TaskSuccess = boolPtr(true)
	t.ConstraintPreserved = boolPtr(true)
	t.Notes = "No-relevant-memory control: task succeeds without proposed memory edit"
	return t
}

// FixtureNoRelevantMemoryNegative fails when success requires the memory edit.
func FixtureNoRelevantMemoryNegative() TrialRecord {
	t := baseTrial(
		"trial-synth-nrm-neg-001",
		"case-synth-nrm-001",
		contract.CaseRoleNoRelevantMemoryCtrl,
		contract.ConditionReviewedMemoryEdit,
		"cond-edit-case-synth-nrm-001",
		0,
	)
	t.NoRelevantMemoryOK = boolPtr(false)
	t.TaskSuccess = boolPtr(false)
	t.OutcomeKind = contract.OutcomeFail
	t.Notes = "No-relevant-memory NEGATIVE: success claimed only via required memory edit"
	return t
}

// FixtureFailureOriginal is a failure-case trial under original_memory.
func FixtureFailureOriginal() TrialRecord {
	t := baseTrial(
		"trial-synth-failure-orig-001",
		"case-synth-failure-001",
		contract.CaseRoleFailure,
		contract.ConditionOriginalMemory,
		"cond-original-case-synth-failure-001",
		0,
	)
	t.TaskSuccess = boolPtr(false)
	t.ConstraintPreserved = boolPtr(true)
	t.OutcomeKind = contract.OutcomeFail
	t.AttemptStatus = contract.AttemptSucceeded // harness finished; oracle failed
	t.Notes = "Failure case under original_memory: task fail preserved as negative"
	return t
}

// FixtureFailureMissing is an unavailable / missing trial that must stay visible.
func FixtureFailureMissing() TrialRecord {
	t := baseTrial(
		"trial-synth-failure-missing-001",
		"case-synth-failure-001",
		contract.CaseRoleFailure,
		contract.ConditionNoMemory,
		"cond-none-case-synth-failure-001",
		0,
	)
	t.AttemptStatus = contract.AttemptUnavailable
	t.OutcomeKind = contract.OutcomeUnavailable
	t.TaskSuccess = nil
	t.ConstraintPreserved = nil
	t.Notes = "Missing/unavailable trial — must remain visible in assembled report"
	return t
}

// FixtureEditDamageDrop is reviewed_memory_edit that drops constraints (damage).
func FixtureEditDamageDrop() TrialRecord {
	t := baseTrial(
		"trial-synth-edit-damage-001",
		"case-synth-failure-001",
		contract.CaseRoleFailure,
		contract.ConditionReviewedMemoryEdit,
		"cond-edit-case-synth-failure-001",
		0,
	)
	t.TaskSuccess = boolPtr(true)
	t.ConstraintPreserved = boolPtr(false)
	t.UnrelatedRegressionOK = boolPtr(true)
	t.OutcomeKind = contract.OutcomeFail
	t.DamageGateFailed = true
	t.Notes = "Edited condition NEGATIVE: task pass via constraint drop → damage gate"
	return t
}

// FixtureScorerMismatch deliberately uses a wrong checker digest.
func FixtureScorerMismatch() TrialRecord {
	t := FixtureFailureOriginal()
	t.TrialID = "trial-synth-scorer-mismatch-001"
	t.CheckerDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	t.Notes = "Deliberate scorer digest mismatch for rejection tests"
	return t
}

// SyntheticComparisonPlan returns a plan with explicit ceilings suitable for
// offline validation. IsApprovedSpend remains false; AuthorizeRealComparison
// still fails without private eligibility + spend auth ref.
func SyntheticComparisonPlan() ComparisonPlan {
	p := NewComparisonPlan("plan-synth-545-001")
	// Explicit ceilings — planning default ≤45 is NOT approved spend.
	p.SpendCeilingUSDMicros = 1 // 1 micro-USD synthetic ceiling for shape tests
	p.RunCeiling = contract.PlanningMaxPlannedRuns
	p.TimeCeilingSeconds = 3600
	p.EstimatedCostPerRunUSDMicros = 0 // plumbing-only; no estimated paid cost
	p.IsApprovedSpend = false
	p.PrivateEligibilityReady = false
	p.SpendAuthorizationRef = ""
	p.StoppingRule = "stop at ceiling, first damage-gate fail, or Adit halt"
	p.MissingRunPolicy = "record_unavailable"
	p.Notes = "Synthetic plan for #545. Ceilings present for Validate; real comparison still Adit-gated."
	return p
}

// SyntheticControlBundle returns both required controls (preserve + NRM) plus
// their negatives and a missing trial — the offline fixture set for assembly.
func SyntheticControlBundle() []TrialRecord {
	return []TrialRecord{
		FixtureFailureOriginal(),
		FixtureFailureMissing(),
		FixtureValidMemoryPreserved(),
		FixtureValidMemoryDropped(),
		FixtureNoRelevantMemoryOK(),
		FixtureNoRelevantMemoryNegative(),
		FixtureEditDamageDrop(),
	}
}
