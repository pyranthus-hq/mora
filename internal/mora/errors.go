package mora

import "fmt"

// This file is the single source of truth for Mora's published error taxonomy:
// the error codes an unattended agent may branch on, the class each code belongs
// to, and the process exit code a class maps to.
//
// internal/mora/eval/error-code-registry.json is the PUBLISHED mirror of what is
// declared here. TestErrorCodeRegistryMatchesSource parses this file with go/ast
// and fails if the two drift in either direction — a code declared but not
// registered, or registered but not declared.

// Error classes. Every published code belongs to exactly one class, and the
// class (not the code) is what a process exit status is derived from. The
// vocabulary is closed: adding a class is a taxonomy change that must land in
// error-code-registry.json in the same commit.
const (
	errClassUsage      = "usage"
	errClassConnector  = "connector"
	errClassPermission = "permission"
	errClassConsent    = "consent"
	errClassData       = "data"
	errClassIndex      = "index"
	errClassInternal   = "internal"
)

// CON-07's five discriminations, plus the backfill value for a failure nothing
// typed. These are the `error_class` a connector failure carries — an ORTHOGONAL
// axis to the three existing state vocabularies, not a fourth one. The
// many-to-one mapping into sourceHealth.State is documented in
// docs/architecture/08-cli-and-ux.md.
const (
	connectorClassMalformed    = "malformed"
	connectorClassUnavailable  = "unavailable"
	connectorClassUnauthorized = "unauthorized"
	connectorClassStale        = "stale"
	connectorClassEmpty        = "empty"
	connectorClassUnclassified = "unclassified"
)

// The published error codes. Dotted lowercase, <domain>.<condition>. A code is
// a contract: renaming one is a breaking change, adding one is additive.
const (
	errCodeUsageUnknownFlag     = "usage.unknown_flag"
	errCodeUsageUnknownValue    = "usage.unknown_value"
	errCodeUsageMissingArgument = "usage.missing_argument"

	errCodeConnectorMalformed    = "connector.malformed_response"
	errCodeConnectorUnavailable  = "connector.unavailable"
	errCodeConnectorUnauthorized = "connector.unauthorized"
	errCodeConnectorStale        = "connector.stale"
	errCodeConnectorEmpty        = "connector.empty"
	errCodeConnectorUnclassified = "connector.unclassified"

	// A consent gate Mora DIRECTLY OBSERVED in its own governance ledger. This
	// is not the `permission` class: that class is reserved for an observed
	// OPERATING-SYSTEM refusal, and Mora still infers those from error prose.
	errCodeConsentRequired = "consent.required"

	errCodeDataNotFound = "data.not_found"
	errCodeDataCorrupt  = "data.corrupt"

	errCodeIndexUnavailable    = "index.unavailable"
	errCodeIndexSchemaMismatch = "index.schema_mismatch"

	errCodeInternalUnexpected = "internal.unexpected"
)

// Process exit codes.
//
// 1, 2, and 10 shipped before this taxonomy existed and are grandfathered with
// their exact shipped meanings. Phase 01 Plan 06 Task 1 put the one candidate
// change (doctor --strict unhealthy, 1 -> 2) to the maintainer, who chose to
// leave it at 1: a process exit status is the one contract in this phase with no
// additive migration path.
//
// 3 through 9 are permanently reserved-unused. A future phase allocates at
// exitCodeFirstAllocatable or above.
const (
	// exitCodeGenericFailure is what every error class maps to today.
	exitCodeGenericFailure = 1
	// exitCodePulseUnhealthy is `doctor --pulse`'s shipped "sick, not broken"
	// status (doctor.go returns exitCodeError{code: 2}; pinned by
	// TestDoctorPulseAlarmsAndExitsTwo in health_test.go). Declared here so the
	// registry can publish it; doctor.go is deliberately NOT edited by this plan.
	exitCodePulseUnhealthy = 2

	// exitCodeReservedLow..exitCodeReservedHigh is the permanently unused band.
	exitCodeReservedLow  = 3
	exitCodeReservedHigh = 9

	// exitCodeFirstAllocatable is the first status a future phase may claim.
	exitCodeFirstAllocatable = 11
)

// grandfatheredExitCodes is the closed set of process exit codes Mora ships,
// with the meaning each one already had. It exists so the registry file and the
// source cannot drift: TestExitCodeAllocationIsGrandfathered compares them.
// loopSkipExitCode lives in loop.go (= 10) and is referenced, not redeclared.
var grandfatheredExitCodes = map[int]string{
	exitCodeGenericFailure: "generic failure",
	exitCodePulseUnhealthy: "doctor --pulse: a source, the index, or a producer is unhealthy",
	loopSkipExitCode:       "loop begin: this period already succeeded",
}

// moraErrorCodeClass binds every published code to its class. This is the table
// the registry file mirrors and the table classForErrorCode reads.
var moraErrorCodeClass = map[string]string{
	errCodeUsageUnknownFlag:     errClassUsage,
	errCodeUsageUnknownValue:    errClassUsage,
	errCodeUsageMissingArgument: errClassUsage,

	errCodeConnectorMalformed:    errClassConnector,
	errCodeConnectorUnavailable:  errClassConnector,
	errCodeConnectorUnauthorized: errClassConnector,
	errCodeConnectorStale:        errClassConnector,
	errCodeConnectorEmpty:        errClassConnector,
	errCodeConnectorUnclassified: errClassConnector,

	errCodeConsentRequired: errClassConsent,

	errCodeDataNotFound: errClassData,
	errCodeDataCorrupt:  errClassData,

	errCodeIndexUnavailable:    errClassIndex,
	errCodeIndexSchemaMismatch: errClassIndex,

	errCodeInternalUnexpected: errClassInternal,
}

// connectorErrorClassByCode binds every connector code to the CON-07
// discrimination it expresses. CON-07 is literally the statement that these six
// values are pairwise distinct and that a connector failure resolves to exactly
// one of them.
var connectorErrorClassByCode = map[string]string{
	errCodeConnectorMalformed:    connectorClassMalformed,
	errCodeConnectorUnavailable:  connectorClassUnavailable,
	errCodeConnectorUnauthorized: connectorClassUnauthorized,
	errCodeConnectorStale:        connectorClassStale,
	errCodeConnectorEmpty:        connectorClassEmpty,
	errCodeConnectorUnclassified: connectorClassUnclassified,
}

// retryableByErrorCode is the machine retry policy published beside each code
// in error-code-registry.json. Keeping the complete table here makes a drift in
// either direction fail TestErrorCodeRegistryMatchesSource instead of leaving
// receipt retry advice as duplicated prose.
var retryableByErrorCode = map[string]bool{
	errCodeUsageUnknownFlag:      false,
	errCodeUsageUnknownValue:     false,
	errCodeUsageMissingArgument:  false,
	errCodeConnectorMalformed:    false,
	errCodeConnectorUnavailable:  true,
	errCodeConnectorUnauthorized: false,
	errCodeConnectorStale:        true,
	errCodeConnectorEmpty:        false,
	errCodeConnectorUnclassified: false,
	errCodeConsentRequired:       false,
	errCodeDataNotFound:          false,
	errCodeDataCorrupt:           false,
	errCodeIndexUnavailable:      true,
	errCodeIndexSchemaMismatch:   false,
	errCodeInternalUnexpected:    false,
}

func retryableForErrorCode(code string) bool { return retryableByErrorCode[code] }

// exitCodeByClass is the class -> process exit code table the registry
// publishes. Every class maps to the generic failure status today; the entries
// are written out one per class rather than collapsed to a single return so that
// allocating a distinct status for one class later is a one-line, reviewable
// change instead of a rewrite.
var exitCodeByClass = map[string]int{
	errClassUsage:      exitCodeGenericFailure,
	errClassConnector:  exitCodeGenericFailure,
	errClassPermission: exitCodeGenericFailure,
	errClassConsent:    exitCodeGenericFailure,
	errClassData:       exitCodeGenericFailure,
	errClassIndex:      exitCodeGenericFailure,
	errClassInternal:   exitCodeGenericFailure,
}

// classForErrorCode reports the class a published code belongs to. A code with
// no registry row resolves to the internal class: reaching that branch means
// Mora constructed a code it never published, which is a Mora bug rather than a
// user or connector fault.
func classForErrorCode(code string) string {
	if class, ok := moraErrorCodeClass[code]; ok {
		return class
	}
	return errClassInternal
}

// connectorErrorClassOf reports the CON-07 discrimination a persisted error code
// expresses. An empty or unrecognized code is `unclassified`, which is the
// read-time backfill rule for records written before this taxonomy shipped —
// Mora never rewrites those files on disk to add a field.
func connectorErrorClassOf(code string) string {
	if class, ok := connectorErrorClassByCode[code]; ok {
		return class
	}
	return connectorClassUnclassified
}

// syncErrorCodeOrUnclassified applies that backfill rule to one persisted sync
// record: a typed code wins, a failure with no typed code reads as
// connector.unclassified, and a record with no failure at all stays empty (an
// omitempty field must not sprout a value on a healthy source).
func syncErrorCodeOrUnclassified(code, lastError string) string {
	if code != "" {
		return code
	}
	if lastError != "" {
		return errCodeConnectorUnclassified
	}
	return ""
}

// moraError is the machine-readable error contract for CLI failures.
type moraError struct {
	Code   string
	Class  string
	Source string
	Msg    string
	cause  error
}

func newMoraError(code, class string, cause error, format string, args ...any) moraError {
	return moraError{
		Code:   code,
		Class:  class,
		Source: "mora",
		Msg:    fmt.Sprintf(format, args...),
		cause:  cause,
	}
}

// newCodedError builds a moraError from a published code alone, deriving the
// class from the taxonomy table. Prefer it at new construction sites: it makes
// pairing a code with the wrong class impossible.
func newCodedError(code string, cause error, format string, args ...any) moraError {
	return newMoraError(code, classForErrorCode(code), cause, format, args...)
}

func (e moraError) Error() string { return e.Msg }

// Unwrap returns the wrapped cause so errors.Is/errors.As still reach the
// existing package sentinels (errIndexUnmarkable, errEmbedderUnavailable, and
// the share sentinels) through a moraError wrap.
func (e moraError) Unwrap() error { return e.cause }

func (e moraError) ExitCode() int { return exitCodeForClass(e.Class) }

// exitCodeForClass implements the published class -> exit code table. An
// unknown class maps to the generic failure status rather than inventing one.
func exitCodeForClass(class string) int {
	if code, ok := exitCodeByClass[class]; ok {
		return code
	}
	return exitCodeGenericFailure
}
