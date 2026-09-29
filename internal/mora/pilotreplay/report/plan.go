package report

import (
	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// ComparisonPlan is the pre-execution plan that must validate before any real
// provider invocation. Planning defaults (≤45 runs) are NOT approved spend.
type ComparisonPlan struct {
	PlanID string `json:"plan_id"`
	// Matrix is the proposed case×condition×rep schedule.
	Matrix contract.PlanningMatrix `json:"matrix"`
	// Explicit ceilings. Missing any ceiling fails validation.
	SpendCeilingUSDMicros int64 `json:"spend_ceiling_usd_micros"`
	RunCeiling            int   `json:"run_ceiling"`
	TimeCeilingSeconds    int   `json:"time_ceiling_seconds"`
	// EstimatedCostPerRunUSDMicros is used only to resolve the matrix against
	// the spend ceiling offline. Zero is allowed for plumbing-only plans that
	// still must declare a positive spend ceiling.
	EstimatedCostPerRunUSDMicros int64 `json:"estimated_cost_per_run_usd_micros"`
	// ResolvedMaxRuns is filled by ResolveAgainstCeilings (matrix size capped
	// by run ceiling). Zero until Resolve is called.
	ResolvedMaxRuns int `json:"resolved_max_runs"`
	// ResolvedEstimatedSpendUSDMicros = ResolvedMaxRuns * EstimatedCostPerRun.
	ResolvedEstimatedSpendUSDMicros int64 `json:"resolved_estimated_spend_usd_micros"`
	// IsApprovedSpend must remain false unless an explicit Adit authorization
	// exists outside this public package. Public fixtures keep this false.
	IsApprovedSpend bool `json:"is_approved_spend"`
	// PrivateEligibilityReady is true only after #542 private go/no-go.
	PrivateEligibilityReady bool `json:"private_eligibility_ready"`
	// SpendAuthorizationRef points at an external Adit authorization record.
	// Empty means no real comparison is authorized.
	SpendAuthorizationRef string `json:"spend_authorization_ref,omitempty"`
	// StoppingRule is required disclosure (e.g. "stop at ceiling or first
	// damage-gate fail"). Empty rejects AuthorizeRealComparison.
	StoppingRule string `json:"stopping_rule,omitempty"`
	// MissingRunPolicy mirrors contract.RetryPolicy.MissingRunPolicy.
	MissingRunPolicy string `json:"missing_run_policy"`
	Notes            string `json:"notes,omitempty"`
}

// NewComparisonPlan returns a plan with the contract planning default matrix
// and empty ceilings (must be filled; empty ceilings fail Validate).
func NewComparisonPlan(planID string) ComparisonPlan {
	return ComparisonPlan{
		PlanID:           planID,
		Matrix:           contract.DefaultPlanningMatrix(),
		IsApprovedSpend:  false,
		MissingRunPolicy: "record_unavailable",
		Notes:            "Planning default ≤45 runs is NOT approved spend. Freeze actual matrix and ceilings before any provider call.",
	}
}

// Validate checks the plan shape offline. It does NOT authorize real runs.
//
// Rules:
//   - every ceiling (spend / run / time) must be present and positive
//   - matrix must validate (planning default must not claim approved spend
//     or statistical power)
//   - matrix.MaxPlannedRuns must not exceed RunCeiling
//   - if EstimatedCostPerRunUSDMicros > 0, estimated spend must not exceed
//     SpendCeilingUSDMicros
//   - IsApprovedSpend on the public plan path must remain false unless a
//     SpendAuthorizationRef is also present (still not a live authorization)
func (p *ComparisonPlan) Validate() error {
	if p.PlanID == "" {
		return errf(CodeInvalidPlan, "plan_id", "required")
	}
	if err := p.Matrix.Validate("matrix"); err != nil {
		if ce, ok := err.(*contract.Error); ok {
			return errf(CodeContractReject, ce.Field, "%s", ce.Message)
		}
		return errf(CodeContractReject, "matrix", "%v", err)
	}
	if p.SpendCeilingUSDMicros <= 0 {
		return errf(CodeMissingCeiling, "spend_ceiling_usd_micros", "explicit monetary spend ceiling required; do not infer from planning default")
	}
	if p.RunCeiling <= 0 {
		return errf(CodeMissingCeiling, "run_ceiling", "explicit run ceiling required")
	}
	if p.TimeCeilingSeconds <= 0 {
		return errf(CodeMissingCeiling, "time_ceiling_seconds", "explicit time ceiling required")
	}
	if p.Matrix.MaxPlannedRuns > p.RunCeiling {
		return errf(CodeRunCeiling, "matrix.max_planned_runs", "planned %d exceeds run ceiling %d", p.Matrix.MaxPlannedRuns, p.RunCeiling)
	}
	if p.EstimatedCostPerRunUSDMicros < 0 {
		return errf(CodeInvalidPlan, "estimated_cost_per_run_usd_micros", "must be >= 0")
	}
	if p.EstimatedCostPerRunUSDMicros > 0 {
		est := int64(p.Matrix.MaxPlannedRuns) * p.EstimatedCostPerRunUSDMicros
		if est > p.SpendCeilingUSDMicros {
			return errf(CodeSpendCeiling, "estimated_spend", "estimated %d micros exceeds spend ceiling %d", est, p.SpendCeilingUSDMicros)
		}
	}
	if p.IsApprovedSpend && p.SpendAuthorizationRef == "" {
		return errf(CodeUnauthorizedCompare, "is_approved_spend", "cannot claim approved spend without spend_authorization_ref (Adit-gated)")
	}
	switch p.MissingRunPolicy {
	case "record_unavailable", "exclude_with_note", "fail_closed":
		// ok
	case "":
		return errf(CodeInvalidPlan, "missing_run_policy", "required")
	default:
		return errf(CodeInvalidPlan, "missing_run_policy", "unknown policy %q", p.MissingRunPolicy)
	}
	return nil
}

// ResolveAgainstCeilings computes ResolvedMaxRuns and estimated spend from the
// explicit matrix and ceilings. Validate must pass first. Planning default
// ≤45 is still not approved spend.
func (p *ComparisonPlan) ResolveAgainstCeilings() error {
	if err := p.Validate(); err != nil {
		return err
	}
	runs := p.Matrix.MaxPlannedRuns
	if runs > p.RunCeiling {
		return errf(CodeRunCeiling, "resolved_max_runs", "cannot resolve: planned %d > run ceiling %d", runs, p.RunCeiling)
	}
	p.ResolvedMaxRuns = runs
	p.ResolvedEstimatedSpendUSDMicros = int64(runs) * p.EstimatedCostPerRunUSDMicros
	if p.ResolvedEstimatedSpendUSDMicros > p.SpendCeilingUSDMicros {
		return errf(CodeSpendCeiling, "resolved_estimated_spend_usd_micros", "resolved %d exceeds ceiling %d", p.ResolvedEstimatedSpendUSDMicros, p.SpendCeilingUSDMicros)
	}
	return nil
}

// AuthorizeRealComparison fails closed unless private eligibility and an
// explicit spend authorization are present. Public plumbing never passes.
func (p *ComparisonPlan) AuthorizeRealComparison() error {
	if err := p.ResolveAgainstCeilings(); err != nil {
		return err
	}
	if !p.PrivateEligibilityReady {
		return errf(CodePrivateEligibility, "private_eligibility_ready", "real comparison waits for #542 private eligibility (Adit)")
	}
	if p.SpendAuthorizationRef == "" {
		return errf(CodeUnauthorizedCompare, "spend_authorization_ref", "explicit spend authorization required (Adit); planning default is not approved spend")
	}
	if !p.IsApprovedSpend {
		return errf(CodeUnauthorizedCompare, "is_approved_spend", "spend authorization recorded but is_approved_spend still false")
	}
	if p.StoppingRule == "" {
		return errf(CodeInvalidPlan, "stopping_rule", "stopping rule required before real comparison")
	}
	return nil
}
