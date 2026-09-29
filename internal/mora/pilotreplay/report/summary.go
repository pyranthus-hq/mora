package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// PublicMethodSummary is the public-safe methodological record for #545.
// It must never contain private artifacts, spend receipts, repair bodies or
// efficacy claims. claims_efficacy remains false.
type PublicMethodSummary struct {
	SchemaVersion int      `json:"schema_version"`
	SummaryID     string   `json:"summary_id"`
	GeneratedAt   string   `json:"generated_at"` // RFC3339
	IssueRef      string   `json:"issue_ref"`    // "#545"
	ParentIssue   string   `json:"parent_issue"` // "#540"
	DependsOn     []string `json:"depends_on"`   // #541–#544

	// Methodological facts only.
	ConditionsCompared      []string `json:"conditions_compared"`
	ControlsRequired        []string `json:"controls_required"`
	PlanningDefaultMaxRuns  int      `json:"planning_default_max_runs"`
	PlanningIsApprovedSpend bool     `json:"planning_is_approved_spend"` // must be false

	PrivatePackageStatus string `json:"private_package_status"` // outside_public_repo
	RealRunsExecuted     bool   `json:"real_runs_executed"`     // false for public half
	SpendAuthorized      bool   `json:"spend_authorized"`       // false until Adit
	PrivateEligibility   string `json:"private_eligibility"`    // pending | ready | no_go

	Limitations        []string `json:"limitations"`
	ClaimsEfficacy     bool     `json:"claims_efficacy"` // must remain false
	SyntheticOnly      bool     `json:"synthetic_only"`
	PublicArtifactNote string   `json:"public_artifact_note"`
	HandoffNote        string   `json:"handoff_note,omitempty"`
}

// NewPublicMethodSummary returns the public half summary for #545.
func NewPublicMethodSummary(generatedAt time.Time) PublicMethodSummary {
	if generatedAt.IsZero() {
		generatedAt = time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	}
	return PublicMethodSummary{
		SchemaVersion: contract.SchemaVersion,
		SummaryID:     "summary-method-public-545",
		GeneratedAt:   generatedAt.UTC().Format(time.RFC3339),
		IssueRef:      "#545",
		ParentIssue:   "#540",
		DependsOn:     []string{"#541", "#542", "#543", "#544"},
		ConditionsCompared: []string{
			contract.ConditionOriginalMemory,
			contract.ConditionNoMemory,
			contract.ConditionReviewedMemoryEdit,
		},
		ControlsRequired: []string{
			contract.CaseRoleValidMemoryControl,
			contract.CaseRoleNoRelevantMemoryCtrl,
		},
		PlanningDefaultMaxRuns:  contract.PlanningMaxPlannedRuns,
		PlanningIsApprovedSpend: false,
		PrivatePackageStatus:    "outside_public_repo",
		RealRunsExecuted:        false,
		SpendAuthorized:         false,
		PrivateEligibility:      "pending",
		Limitations: []string{
			"Public PR = report plumbing + synthetic fixtures + methodological summary only.",
			"Planning default ≤45 runs is a schedule upper bound, not approved spend.",
			"Real comparison waits for #542 private eligibility and explicit Adit spend authorization.",
			"Failed/missing trials remain visible; case count ≠ repetition count.",
			"A repair that drops valid constraints fails the damage gate.",
			"No private repairs, artifacts or spend receipts appear in the public repo.",
		},
		ClaimsEfficacy:     false,
		SyntheticOnly:      true,
		PublicArtifactNote: "Synthetic report fixtures + comparison-plan validation only. See docs/experiments/memory-replay/report/.",
		HandoffNote:        "Private comparison (original / no-memory / edited) needs Adit: private package eligibility (#542), frozen matrix, explicit monetary/run/time ceilings, spend authorization, and isolation readiness. Do not substitute the planning default for approved spend.",
	}
}

// Validate checks public-summary invariants.
func (s PublicMethodSummary) Validate() error {
	if s.SchemaVersion != contract.SchemaVersion {
		return errf(CodeContractReject, "schema_version", "want %d", contract.SchemaVersion)
	}
	if strings.TrimSpace(s.SummaryID) == "" {
		return errf(CodeInvalidReport, "summary_id", "required")
	}
	if s.ClaimsEfficacy {
		return errf(CodeClaimsEfficacy, "claims_efficacy", "public methodological summary must not claim efficacy")
	}
	if s.PlanningIsApprovedSpend {
		return errf(CodeUnauthorizedCompare, "planning_is_approved_spend", "planning default must not claim approved spend")
	}
	if s.PrivatePackageStatus != "outside_public_repo" {
		return errf(CodeInvalidReport, "private_package_status", "public summaries must keep private package outside the public repo")
	}
	if s.RealRunsExecuted {
		return errf(CodeUnauthorizedCompare, "real_runs_executed", "public half must not claim real runs executed")
	}
	if s.SpendAuthorized {
		return errf(CodeUnauthorizedCompare, "spend_authorized", "public half must not claim spend authorized")
	}
	if !s.SyntheticOnly {
		return errf(CodeInvalidReport, "synthetic_only", "public methodological summary must be synthetic_only")
	}
	if s.DependsOn == nil || s.ConditionsCompared == nil || s.ControlsRequired == nil || s.Limitations == nil {
		return errf(CodeInvalidReport, "slices", "use [] when empty, never null")
	}
	return nil
}

// RenderMarkdown renders the public methodological summary for docs / PR bodies.
func (s PublicMethodSummary) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Public methodological summary (`%s`)\n\n", s.SummaryID)
	fmt.Fprintf(&b, "- **Issue:** %s (parent %s)\n", s.IssueRef, s.ParentIssue)
	fmt.Fprintf(&b, "- **Depends on:** %s\n", strings.Join(s.DependsOn, ", "))
	fmt.Fprintf(&b, "- **Generated at:** %s\n", s.GeneratedAt)
	fmt.Fprintf(&b, "- **Conditions compared:** %s\n", strings.Join(s.ConditionsCompared, ", "))
	fmt.Fprintf(&b, "- **Controls required:** %s\n", strings.Join(s.ControlsRequired, ", "))
	fmt.Fprintf(&b, "- **Planning default max runs:** %d (approved spend: %v)\n", s.PlanningDefaultMaxRuns, s.PlanningIsApprovedSpend)
	fmt.Fprintf(&b, "- **Private package:** `%s`\n", s.PrivatePackageStatus)
	fmt.Fprintf(&b, "- **Real runs executed:** %v\n", s.RealRunsExecuted)
	fmt.Fprintf(&b, "- **Spend authorized:** %v\n", s.SpendAuthorized)
	fmt.Fprintf(&b, "- **Private eligibility:** `%s`\n", s.PrivateEligibility)
	fmt.Fprintf(&b, "- **Synthetic only:** %v\n", s.SyntheticOnly)
	fmt.Fprintf(&b, "- **Claims efficacy:** %v (must be false)\n\n", s.ClaimsEfficacy)
	b.WriteString("## Limitations\n\n")
	for _, l := range s.Limitations {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	if s.PublicArtifactNote != "" {
		fmt.Fprintf(&b, "\n## Public artifact note\n\n%s\n", s.PublicArtifactNote)
	}
	if s.HandoffNote != "" {
		fmt.Fprintf(&b, "\n## Handoff (Adit)\n\n%s\n", s.HandoffNote)
	}
	return b.String()
}
