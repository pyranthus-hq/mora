package report

import (
	"sort"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// ScorerBinding pins the frozen checker identity for one assembled report.
// All trials in a report must match these digests/versions.
type ScorerBinding struct {
	CheckerID      string `json:"checker_id"`
	CheckerVersion string `json:"checker_version"`
	CheckerDigest  string `json:"checker_digest"`
	PolicyDigest   string `json:"policy_digest"`
	PolicyID       string `json:"policy_id,omitempty"`
}

// TrialRecord is one visible attempt row in an assembled report. Failed,
// missing, timed-out and unavailable trials remain visible; they are never
// silently dropped.
type TrialRecord struct {
	TrialID       string `json:"trial_id"`
	CaseID        string `json:"case_id"`
	CaseRole      string `json:"case_role"`
	ConditionKind string `json:"condition_kind"`
	ConditionID   string `json:"condition_id,omitempty"`
	RepIndex      int    `json:"rep_index"`
	AttemptStatus string `json:"attempt_status"`
	OutcomeKind   string `json:"outcome_kind"`
	// Scorer fields must match the report ScorerBinding.
	CheckerID      string `json:"checker_id"`
	CheckerVersion string `json:"checker_version"`
	CheckerDigest  string `json:"checker_digest"`
	PolicyDigest   string `json:"policy_digest"`
	ArtifactDigest string `json:"artifact_digest,omitempty"`
	// Dimension evidence (nil = not observed / missing).
	TaskSuccess           *bool `json:"task_success"`
	ConstraintPreserved   *bool `json:"constraint_preserved"`
	UnrelatedRegressionOK *bool `json:"unrelated_regression_ok"`
	NoRelevantMemoryOK    *bool `json:"no_relevant_memory_ok"`
	// DamageGateFailed is true when repair dropped a valid constraint.
	DamageGateFailed bool `json:"damage_gate_failed"`
	// Visible must remain true for failed/missing/negative rows.
	Visible bool   `json:"visible"`
	Notes   string `json:"notes,omitempty"`
}

// CountSummary distinguishes case count from repetition count. Repeated runs
// of one incident are not independent incidents.
type CountSummary struct {
	CaseCount       int `json:"case_count"`
	DistinctCases   int `json:"distinct_cases"` // alias clarity: unique case_ids
	RepetitionCount int `json:"repetition_count"`
	TrialCount      int `json:"trial_count"`
	VisibleTrials   int `json:"visible_trials"`
	FailedTrials    int `json:"failed_trials"`
	MissingTrials   int `json:"missing_trials"`
	NegativeTrials  int `json:"negative_trials"`
	DamageGateFails int `json:"damage_gate_fails"`
}

// AssembledReport is the offline assembly of trial rows + plan + scorer
// binding. Public fixtures keep ClaimsEfficacy=false and Synthetic=true.
type AssembledReport struct {
	Schema         string                  `json:"schema"`
	SchemaVersion  int                     `json:"schema_version"`
	ReportID       string                  `json:"report_id"`
	Plan           ComparisonPlan          `json:"plan"`
	Scorer         ScorerBinding           `json:"scorer"`
	Trials         []TrialRecord           `json:"trials"`
	Counts         CountSummary            `json:"counts"`
	Cost           contract.CostAccounting `json:"cost"`
	Limitations    []string                `json:"limitations"`
	ClaimsEfficacy bool                    `json:"claims_efficacy"` // must remain false
	Synthetic      bool                    `json:"synthetic"`
	Notes          string                  `json:"notes,omitempty"`
}

const (
	assembledSchema = "mora.pilotreplay.assembled_report"
	assembledVer    = 1
)

// NewAssembledReport constructs an empty assembled report shell.
func NewAssembledReport(reportID string, plan ComparisonPlan, scorer ScorerBinding) AssembledReport {
	return AssembledReport{
		Schema:         assembledSchema,
		SchemaVersion:  assembledVer,
		ReportID:       reportID,
		Plan:           plan,
		Scorer:         scorer,
		Trials:         []TrialRecord{},
		Limitations:    []string{},
		ClaimsEfficacy: false,
		Synthetic:      true,
		Cost: contract.CostAccounting{
			Currency:   "USD",
			FailClosed: true,
		},
	}
}

// Assemble validates trials against the scorer binding, enforces visibility of
// failed/missing/negatives, computes CountSummary and rejects efficacy claims.
func Assemble(reportID string, plan ComparisonPlan, scorer ScorerBinding, trials []TrialRecord) (AssembledReport, error) {
	r := NewAssembledReport(reportID, plan, scorer)
	if reportID == "" {
		return r, errf(CodeInvalidReport, "report_id", "required")
	}
	if err := plan.Validate(); err != nil {
		return r, err
	}
	if err := validateScorer(scorer); err != nil {
		return r, err
	}
	if trials == nil {
		trials = []TrialRecord{}
	}
	for i := range trials {
		if err := validateTrial(trials[i], scorer); err != nil {
			return r, err
		}
		// Failed / missing / negative must stay visible.
		if isFailedOrMissing(trials[i]) || isNegative(trials[i]) {
			if !trials[i].Visible {
				return r, errf(CodeMissingTrial, trials[i].TrialID, "failed/missing/negative trial must remain visible")
			}
		}
	}
	r.Trials = append([]TrialRecord(nil), trials...)
	r.Counts = summarizeCounts(trials)
	if r.ClaimsEfficacy {
		return r, errf(CodeClaimsEfficacy, "claims_efficacy", "assembled reports must not claim efficacy")
	}
	r.Limitations = defaultLimitations()
	r.Notes = "Offline assembly only. Real comparison waits for private eligibility + explicit spend authorization (Adit)."
	return r, nil
}

func validateScorer(s ScorerBinding) error {
	if s.CheckerID == "" {
		return errf(CodeScorerMismatch, "checker_id", "required")
	}
	if s.CheckerVersion == "" {
		return errf(CodeScorerMismatch, "checker_version", "required")
	}
	if s.CheckerDigest == "" {
		return errf(CodeHashMismatch, "checker_digest", "required")
	}
	if s.PolicyDigest == "" {
		return errf(CodeHashMismatch, "policy_digest", "required")
	}
	return nil
}

func validateTrial(t TrialRecord, scorer ScorerBinding) error {
	if t.TrialID == "" {
		return errf(CodeInvalidReport, "trial_id", "required")
	}
	if t.CaseID == "" {
		return errf(CodeInvalidReport, "case_id", "required")
	}
	if t.CheckerID != scorer.CheckerID || t.CheckerVersion != scorer.CheckerVersion {
		return errf(CodeScorerMismatch, t.TrialID, "trial scorer %s@%s != report %s@%s", t.CheckerID, t.CheckerVersion, scorer.CheckerID, scorer.CheckerVersion)
	}
	if t.CheckerDigest != scorer.CheckerDigest {
		return errf(CodeHashMismatch, t.TrialID+".checker_digest", "trial digest %q != report %q", t.CheckerDigest, scorer.CheckerDigest)
	}
	if t.PolicyDigest != scorer.PolicyDigest {
		return errf(CodeHashMismatch, t.TrialID+".policy_digest", "trial policy digest %q != report %q", t.PolicyDigest, scorer.PolicyDigest)
	}
	return nil
}

func isFailedOrMissing(t TrialRecord) bool {
	switch t.AttemptStatus {
	case contract.AttemptFailed, contract.AttemptTimedOut, contract.AttemptUnavailable, contract.AttemptSkipped:
		return true
	}
	switch t.OutcomeKind {
	case contract.OutcomeFail, contract.OutcomeError, contract.OutcomeUnavailable, contract.OutcomeTimedOut, contract.OutcomeSkipped, contract.OutcomeInconclusive:
		return true
	}
	return false
}

func isNegative(t TrialRecord) bool {
	if t.DamageGateFailed {
		return true
	}
	if t.TaskSuccess != nil && !*t.TaskSuccess {
		return true
	}
	if t.ConstraintPreserved != nil && !*t.ConstraintPreserved {
		return true
	}
	if t.OutcomeKind == contract.OutcomeFail {
		return true
	}
	return false
}

func summarizeCounts(trials []TrialRecord) CountSummary {
	cases := map[string]struct{}{}
	var s CountSummary
	s.TrialCount = len(trials)
	for _, t := range trials {
		cases[t.CaseID] = struct{}{}
		if t.Visible {
			s.VisibleTrials++
		}
		if t.AttemptStatus == contract.AttemptFailed || t.OutcomeKind == contract.OutcomeFail {
			s.FailedTrials++
		}
		if t.AttemptStatus == contract.AttemptUnavailable || t.AttemptStatus == contract.AttemptSkipped ||
			t.OutcomeKind == contract.OutcomeUnavailable || t.OutcomeKind == contract.OutcomeSkipped || t.OutcomeKind == contract.OutcomeInconclusive {
			s.MissingTrials++
		}
		if isNegative(t) {
			s.NegativeTrials++
		}
		if t.DamageGateFailed {
			s.DamageGateFails++
		}
		// Repetition count: every trial with RepIndex >= 0 counts as a rep slot.
		s.RepetitionCount++
	}
	s.DistinctCases = len(cases)
	s.CaseCount = len(cases)
	return s
}

// DistinctCaseIDs returns sorted unique case ids (for tests / summaries).
func DistinctCaseIDs(trials []TrialRecord) []string {
	m := map[string]struct{}{}
	for _, t := range trials {
		m[t.CaseID] = struct{}{}
	}
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func defaultLimitations() []string {
	return []string{
		"Public assembly uses synthetic fixtures only; no private artifacts.",
		"Case count ≠ repetition count: repeated runs of one incident are not independent incidents.",
		"Failed, missing, timed-out and unavailable trials remain visible.",
		"Negative results (task fail, constraint drop, damage-gate fail) are preserved.",
		"Planning default ≤45 runs is not approved spend.",
		"Real comparison waits for #542 private eligibility and explicit Adit spend authorization.",
		"claims_efficacy remains false; this report does not claim memory-edit benefit.",
	}
}

// ToContractReport projects an AssembledReport into the frozen contract
// ReportDocument shape (ids + matrix + cost + limitations). Trial bodies stay
// in AssembledReport; contract report is the public id envelope.
func (r AssembledReport) ToContractReport() contract.ReportDocument {
	doc := contract.NewReportDocument()
	doc.ReportID = r.ReportID
	seenCase := map[string]struct{}{}
	seenCond := map[string]struct{}{}
	for _, t := range r.Trials {
		if _, ok := seenCase[t.CaseID]; !ok {
			doc.CaseIDs = append(doc.CaseIDs, t.CaseID)
			seenCase[t.CaseID] = struct{}{}
		}
		if t.ConditionID != "" {
			if _, ok := seenCond[t.ConditionID]; !ok {
				doc.ConditionIDs = append(doc.ConditionIDs, t.ConditionID)
				seenCond[t.ConditionID] = struct{}{}
			}
		}
		doc.AttemptIDs = append(doc.AttemptIDs, t.TrialID)
		if t.OutcomeKind != "" {
			doc.OutcomeIDs = append(doc.OutcomeIDs, "outcome-"+t.TrialID)
		}
	}
	doc.Matrix = r.Plan.Matrix
	doc.Cost = r.Cost
	if doc.Cost.CeilingUSDMicros == 0 {
		doc.Cost.CeilingUSDMicros = r.Plan.SpendCeilingUSDMicros
		doc.Cost.FailClosed = true
		doc.Cost.Currency = "USD"
	}
	doc.AccessTable = contract.DefaultAccessTable()
	doc.Assumptions = contract.DefaultIsolationAssumptions()
	doc.Limitations = append([]string(nil), r.Limitations...)
	doc.ClaimsEfficacy = false
	doc.Notes = r.Notes
	return doc
}
