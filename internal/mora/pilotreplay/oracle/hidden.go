package oracle

import (
	"strings"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// MarkerCheck is one executable key/value expectation against TaskMarkers.
type MarkerCheck struct {
	Key           string `json:"key"`
	RequiredValue string `json:"required_value"`
	// AbsentWhenSet: if true, the key must be missing or not equal to RequiredValue
	// for the check to pass (used for known-fault markers that must be cleared).
	MustBeAbsent bool `json:"must_be_absent,omitempty"`
}

// FileTokenCheck requires a token to appear (or not) inside a file body.
type FileTokenCheck struct {
	Path        string `json:"path"`
	Token       string `json:"token"`
	MustContain bool   `json:"must_contain"`
	Description string `json:"description,omitempty"`
}

// NoRelevantMemorySpec freezes the control: success must not require the
// proposed memory edit.
type NoRelevantMemorySpec struct {
	// SuccessMarkers must all pass for the control task.
	SuccessMarkers []MarkerCheck `json:"success_markers"`
	// ForbiddenMarkers must remain absent (e.g. "required_memory_edit=true").
	ForbiddenMarkers []MarkerCheck `json:"forbidden_markers"`
	SuccessRule      string        `json:"success_rule"`
}

// HiddenSpec is evaluator-only material. It must never appear in contender or
// repair payloads. Public fixtures use synthetic stand-ins only; real private
// gold stays outside the public repository.
type HiddenSpec struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	SpecID        string `json:"spec_id"`
	Synthetic     bool   `json:"synthetic"`
	// TaskPassMarkers: required for DimFailedTask = pass.
	TaskPassMarkers []MarkerCheck `json:"task_pass_markers"`
	// TaskFailMarkers: if present as stated, DimFailedTask = fail (known fault).
	TaskFailMarkers []MarkerCheck `json:"task_fail_markers"`
	// ConstraintChecks: valid-memory constraint preservation.
	ConstraintChecks []FileTokenCheck `json:"constraint_checks"`
	// RegressionGuards: unrelated behavior that must still hold.
	RegressionGuards []FileTokenCheck `json:"regression_guards"`
	// NoRelevantMemory: control-case executable checks.
	NoRelevantMemory NoRelevantMemorySpec `json:"no_relevant_memory"`
	// ApplicableRoles lists which case roles this spec grades.
	ApplicableRoles []string `json:"applicable_roles"`
	Notes           string   `json:"notes,omitempty"`
}

// NewHiddenSpec returns a HiddenSpec with schema header filled.
func NewHiddenSpec() HiddenSpec {
	return HiddenSpec{
		Schema:           HiddenSchema,
		SchemaVersion:    HiddenVer,
		Synthetic:        true,
		TaskPassMarkers:  []MarkerCheck{},
		TaskFailMarkers:  []MarkerCheck{},
		ConstraintChecks: []FileTokenCheck{},
		RegressionGuards: []FileTokenCheck{},
		NoRelevantMemory: NoRelevantMemorySpec{
			SuccessMarkers:   []MarkerCheck{},
			ForbiddenMarkers: []MarkerCheck{},
		},
		ApplicableRoles: []string{
			contract.CaseRoleFailure,
			contract.CaseRoleValidMemoryControl,
			contract.CaseRoleNoRelevantMemoryCtrl,
		},
	}
}

// Validate checks HiddenSpec shape. Synthetic flag must be true for in-repo fixtures.
func (h *HiddenSpec) Validate() error {
	if h == nil {
		return errf(CodeInvalidInput, "hidden_spec", "nil")
	}
	if h.Schema != HiddenSchema {
		return errf(CodeInvalidInput, "schema", "want %q, got %q", HiddenSchema, h.Schema)
	}
	if h.SchemaVersion != HiddenVer {
		return errf(CodeInvalidInput, "schema_version", "want %d, got %d", HiddenVer, h.SchemaVersion)
	}
	if strings.TrimSpace(h.SpecID) == "" {
		return errf(CodeInvalidInput, "spec_id", "required")
	}
	if !h.Synthetic {
		return errf(CodeGoldLeak, "synthetic", "public-repo HiddenSpec must be synthetic=true; real private gold stays outside the public repository")
	}
	return nil
}

// HiddenSpecDigest digests the evaluator-side spec (synthetic stand-in in public).
func HiddenSpecDigest(h HiddenSpec) (string, error) {
	if err := h.Validate(); err != nil {
		return "", err
	}
	return canonicalDigest(h)
}

// ContenderMustNotSee returns resource names that must stay deny for contender.
func ContenderMustNotSee() []string {
	return []string{
		"oracle_hidden_tests",
		"oracle_expected_answers",
		"oracle_reference_patches",
		"post_cutoff_evidence",
		"hidden_spec",
		"gold_labels",
		"dimension_detail_bodies",
	}
}

// RepairMustNotSee returns resource names that must stay deny for repair.
func RepairMustNotSee() []string {
	return []string{
		"oracle_hidden_tests",
		"oracle_expected_answers",
		"oracle_reference_patches",
		"post_cutoff_evidence",
		"hidden_spec",
		"gold_labels",
		"dimension_detail_bodies",
	}
}
