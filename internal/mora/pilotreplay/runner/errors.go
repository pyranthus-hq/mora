package runner

import "fmt"

// Stable runner error codes.
const (
	CodeWorkspace       = "workspace_error"
	CodeResetBleed      = "reset_bleed"
	CodeHashMismatch    = "initial_hash_mismatch"
	CodeAdmitReject     = "admit_reject"
	CodeGateDenied      = "gate_denied"
	CodeBudgetExhausted = "budget_exhausted"
	CodeTimeout         = "timeout"
	CodeMalformedOutput = "malformed_output"
	CodeMissingExposure = "missing_exposure_capture"
	CodePathRejected    = "rejected_path_access"
	CodeUnavailable     = "provider_unavailable"
	CodeIsolationBreach = "isolation_breach"
	CodeOracleLeak      = "oracle_material_in_contender"
	CodeProviderBlocked = "provider_blocked"
	CodeInternal        = "internal"
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
