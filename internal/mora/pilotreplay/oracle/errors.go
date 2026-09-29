package oracle

import "fmt"

// Stable error vocabulary for the oracle package.
const (
	CodeMissingArtifact     = "missing_artifact"
	CodeMalformedArtifact   = "malformed_artifact"
	CodeUnsupportedArtifact = "unsupported_artifact"
	CodeGraderError         = "grader_error"
	CodePolicyMismatch      = "policy_mismatch"
	CodeIsolationBreach     = "isolation_breach"
	CodeGoldLeak            = "gold_leak"
	CodeInvalidInput        = "invalid_input"
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
