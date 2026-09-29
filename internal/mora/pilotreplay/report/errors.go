package report

import "fmt"

// Stable error vocabulary for the report package.
const (
	CodeMissingCeiling      = "missing_ceiling"
	CodeSpendCeiling        = "spend_ceiling_required"
	CodeRunCeiling          = "run_ceiling_exceeded"
	CodeTimeCeiling         = "time_ceiling_required"
	CodeMatrixMismatch      = "matrix_mismatch"
	CodeScorerMismatch      = "scorer_mismatch"
	CodeHashMismatch        = "hash_mismatch"
	CodeDamageGate          = "damage_gate_failed"
	CodeClaimsEfficacy      = "claims_efficacy_forbidden"
	CodeMissingTrial        = "missing_trial_visibility"
	CodeCaseRepConfusion    = "case_rep_count_confusion"
	CodeUnauthorizedCompare = "unauthorized_real_comparison"
	CodePrivateEligibility  = "private_eligibility_required"
	CodeInvalidPlan         = "invalid_plan"
	CodeInvalidReport       = "invalid_report"
	CodeContractReject      = "contract_reject"
	CodeInternal            = "internal"
)

// Error is the single error type this package returns.
type Error struct {
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Message)
}

func errf(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}
