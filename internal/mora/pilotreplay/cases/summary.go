package cases

import (
	"fmt"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// RedactedEligibilitySummary is the public, redacted record of a qualification
// decision. It must never contain private repository identities, absolute
// private paths, vault contents, credentials, or raw traces.
type RedactedEligibilitySummary struct {
	SchemaVersion int      `json:"schema_version"`
	SummaryID     string   `json:"summary_id"`
	GeneratedAt   string   `json:"generated_at"` // RFC3339
	IssueRef      string   `json:"issue_ref"`    // e.g. "#542"
	Decision      string   `json:"decision"`     // eligible | no_go | prospective_capture | public_protocol_only
	RolesCovered  []string `json:"roles_covered"`
	ControlsNoted bool     `json:"controls_noted"`
	MissingInputs []string `json:"missing_inputs"`
	Exclusions    []string `json:"exclusions"`
	Limitations   []string `json:"limitations"`
	// PrivatePackageStatus is always "outside_public_repo" for public artifacts.
	PrivatePackageStatus string `json:"private_package_status"`
	PermissionStatus     string `json:"permission_status"` // recorded | unknown | not_applicable_public_half
	SyntheticOnly        bool   `json:"synthetic_only"`
	// ClaimsMemoryCausedFailure must remain false in public summaries.
	ClaimsMemoryCausedFailure bool   `json:"claims_memory_caused_failure"`
	PublicArtifactNote        string `json:"public_artifact_note"`
	HandoffNote               string `json:"handoff_note,omitempty"`
}

// NewPublicProtocolSummary returns the redacted summary for the public half
// of #542 (protocol + synthetic fixtures only; private go/no-go deferred).
func NewPublicProtocolSummary(generatedAt time.Time) RedactedEligibilitySummary {
	if generatedAt.IsZero() {
		generatedAt = time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	}
	return RedactedEligibilitySummary{
		SchemaVersion: contract.SchemaVersion,
		SummaryID:     "summary-elig-public-protocol-542",
		GeneratedAt:   generatedAt.UTC().Format(time.RFC3339),
		IssueRef:      "#542",
		Decision:      "public_protocol_only",
		RolesCovered: []string{
			contract.CaseRoleFailure,
			contract.CaseRoleValidMemoryControl,
			contract.CaseRoleNoRelevantMemoryCtrl,
		},
		ControlsNoted: true,
		MissingInputs: []string{
			"private_candidate_inventory",
			"authorized_permission_record",
			"private_frozen_case_package",
		},
		Exclusions: []string{
			"No real incident IDs, repo names, or source paths are published.",
			"Unfavorable or incomplete private candidates are preserved only in the private selection record.",
		},
		Limitations: []string{
			"Public fixtures are synthetic and cannot support historical attribution.",
			"Closure of the public PR is protocol + fixtures only.",
			"Go/no-go for the private packet is left to Adit / an authorized owner.",
			"No model execution, spend, or outreach is authorized by this summary.",
		},
		PrivatePackageStatus:      "outside_public_repo",
		PermissionStatus:          "not_applicable_public_half",
		SyntheticOnly:             true,
		ClaimsMemoryCausedFailure: false,
		PublicArtifactNote:        "Synthetic manifests + qualification protocol only. See docs/experiments/memory-replay/cases/.",
		HandoffNote:               "Private inventory of 5–10 candidates + permission review needs Adit / authorized owner. Do not invent cases to meet quota.",
	}
}

// Validate checks public-summary invariants (no efficacy / causation claims).
func (s RedactedEligibilitySummary) Validate() error {
	if s.SchemaVersion != contract.SchemaVersion {
		return errf(CodeContractReject, "schema_version", "want %d", contract.SchemaVersion)
	}
	if strings.TrimSpace(s.SummaryID) == "" {
		return errf(CodeMissingRequiredHash, "summary_id", "required")
	}
	if s.ClaimsMemoryCausedFailure {
		return errf(CodeContractReject, "claims_memory_caused_failure", "public eligibility summary must not claim memory caused a failure")
	}
	if !s.SyntheticOnly && s.Decision == "public_protocol_only" {
		return errf(CodeContractReject, "synthetic_only", "public_protocol_only summaries must be synthetic_only")
	}
	if s.PrivatePackageStatus != "outside_public_repo" {
		return errf(CodeUnknownPermission, "private_package_status", "public summaries must keep private package outside the public repo")
	}
	if s.MissingInputs == nil || s.Exclusions == nil || s.Limitations == nil || s.RolesCovered == nil {
		return errf(CodeMissingRequiredHash, "slices", "use [] when empty, never null")
	}
	return nil
}

// RenderMarkdown renders a redacted summary for docs / PR bodies.
func (s RedactedEligibilitySummary) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Redacted eligibility summary (`%s`)\n\n", s.SummaryID)
	fmt.Fprintf(&b, "- **Issue:** %s\n", s.IssueRef)
	fmt.Fprintf(&b, "- **Generated at:** %s\n", s.GeneratedAt)
	fmt.Fprintf(&b, "- **Decision:** `%s`\n", s.Decision)
	fmt.Fprintf(&b, "- **Roles covered:** %s\n", strings.Join(s.RolesCovered, ", "))
	fmt.Fprintf(&b, "- **Controls noted:** %v\n", s.ControlsNoted)
	fmt.Fprintf(&b, "- **Private package:** `%s`\n", s.PrivatePackageStatus)
	fmt.Fprintf(&b, "- **Permission status:** `%s`\n", s.PermissionStatus)
	fmt.Fprintf(&b, "- **Synthetic only:** %v\n", s.SyntheticOnly)
	fmt.Fprintf(&b, "- **Claims memory caused failure:** %v (must be false)\n\n", s.ClaimsMemoryCausedFailure)
	b.WriteString("## Missing inputs\n\n")
	for _, m := range s.MissingInputs {
		fmt.Fprintf(&b, "- %s\n", m)
	}
	b.WriteString("\n## Exclusions\n\n")
	for _, e := range s.Exclusions {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	b.WriteString("\n## Limitations\n\n")
	for _, l := range s.Limitations {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	if s.PublicArtifactNote != "" {
		fmt.Fprintf(&b, "\n## Public artifact note\n\n%s\n", s.PublicArtifactNote)
	}
	if s.HandoffNote != "" {
		fmt.Fprintf(&b, "\n## Handoff\n\n%s\n", s.HandoffNote)
	}
	return b.String()
}
