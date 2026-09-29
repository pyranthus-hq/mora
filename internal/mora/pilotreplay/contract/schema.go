package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the MAJOR version every document in this package carries.
// It moves only for a breaking change.
const SchemaVersion = 1

// Published schema names. A consumer pins on name + SchemaVersion.
const (
	SchemaCase      = "mora.pilotreplay.case"
	SchemaCondition = "mora.pilotreplay.condition"
	SchemaAttempt   = "mora.pilotreplay.attempt"
	SchemaOutcome   = "mora.pilotreplay.outcome"
	SchemaReport    = "mora.pilotreplay.report"
	SchemaReceipt   = "mora.pilotreplay.receipt"
	SchemaRunGate   = "mora.pilotreplay.run_gate"
)

// Fidelity marks how well stored artifacts reconstruct historical exposure.
// Reconstruction must never silently upgrade to faithful_historical.
const (
	FidelityFaithfulHistorical = "faithful_historical"
	FidelityReconstruction     = "reconstruction"
	FidelityPartial            = "partial"
	FidelityUnknown            = "unknown"
)

// ExposureAvailability marks whether exact delivered bytes are present.
const (
	ExposurePresent   = "present"
	ExposureMissing   = "missing"
	ExposurePartial   = "partial"
	ExposureSynthetic = "synthetic"
)

// ProviderVersionStatus marks whether the exact provider/model version is known.
const (
	ProviderVersionKnown   = "known"
	ProviderVersionMissing = "missing"
	ProviderVersionApprox  = "approximate"
)

// ConditionKind enumerates the three initial pilot conditions. Changing
// retrieval or harness requires a separate condition or a declared confound.
const (
	ConditionOriginalMemory     = "original_memory"
	ConditionNoMemory           = "no_memory"
	ConditionReviewedMemoryEdit = "reviewed_memory_edit"
)

// CaseRole identifies the failure case and the two required controls.
const (
	CaseRoleFailure              = "failure"
	CaseRoleValidMemoryControl   = "valid_memory_control"
	CaseRoleNoRelevantMemoryCtrl = "no_relevant_memory_control"
)

// AttemptStatus values. Success is only "succeeded"; other terminals are not
// interchangeable with a successful outcome.
const (
	AttemptPending     = "pending"
	AttemptRunning     = "running"
	AttemptSucceeded   = "succeeded"
	AttemptFailed      = "failed"
	AttemptTimedOut    = "timed_out"
	AttemptSkipped     = "skipped"
	AttemptUnavailable = "unavailable"
)

// OutcomeKind is the oracle-facing result classification.
const (
	OutcomePass         = "pass"
	OutcomeFail         = "fail"
	OutcomeError        = "error"
	OutcomeSkipped      = "skipped"
	OutcomeUnavailable  = "unavailable"
	OutcomeTimedOut     = "timed_out"
	OutcomeInconclusive = "inconclusive"
)

// Access principals for the isolation access table.
const (
	PrincipalContender = "contender"
	PrincipalRepair    = "repair"
	PrincipalOracle    = "oracle"
	PrincipalRunner    = "runner"
	PrincipalOperator  = "operator"
)

// Access levels.
const (
	AccessDeny    = "deny"
	AccessAllow   = "allow"
	AccessRedact  = "redact"
	AccessHashRef = "hash_ref_only"
)

// DefaultDenyAction names the production-side actions that remain denied
// unless an explicit, reviewed exception is recorded outside this contract.
const (
	DenyProductionWrites = "production_writes"
	DenyExternalActions  = "external_actions"
	DenyLiveVaultAccess  = "live_vault_access"
)

// PlanningMatrixDefault is the suggested initial matrix. It is a planning
// default only — not approved spend and not a statistical-power claim.
const (
	PlanningRepsPerCell    = 5
	PlanningFailureCases   = 1
	PlanningControlCases   = 2
	PlanningConditions     = 3
	PlanningMaxPlannedRuns = PlanningRepsPerCell * (PlanningFailureCases + PlanningControlCases) * PlanningConditions // 45
)

// Byte / count bounds. Enforced by Validate; never left to the caller.
const (
	MaxIDBytes            = 128
	MaxLabelBytes         = 256
	MaxPathBytes          = 512
	MaxHashHexBytes       = 64
	MaxNoteBytes          = 2 << 10
	MaxURIBytes           = 1024
	MaxDirtyFiles         = 256
	MaxSnapshotRefs       = 64
	MaxDeliveredParts     = 128
	MaxAttemptsPerReport  = 64
	MaxOutcomesPerReport  = 64
	MaxConditions         = 16
	MaxAccessRows         = 64
	MaxConfoundNotes      = 16
	MaxSyntheticBytesNote = 64
)

// Error codes. Stable vocabulary for schema rejection.
const (
	CodeMissingField      = "missing_field"
	CodeInvalidEnum       = "invalid_enum"
	CodeInvalidValue      = "invalid_value"
	CodeSchemaMismatch    = "schema_mismatch"
	CodeIsolationBreach   = "isolation_breach"
	CodeRunGateDenied     = "run_gate_denied"
	CodeFidelityUpgrade   = "fidelity_upgrade_forbidden"
	CodeExposureMismatch  = "exposure_snapshot_mismatch"
	CodeHiddenInRunner    = "hidden_material_in_runner"
	CodeSpendCeiling      = "spend_ceiling_required"
	CodePermissionMissing = "run_permission_missing"
	CodeTooLarge          = "too_large"
	CodeTooManyItems      = "too_many_items"
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

// Header is carried by every top-level document.
type Header struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
}

func newHeader(schema string) Header {
	return Header{Schema: schema, SchemaVersion: SchemaVersion}
}

func (h Header) validate(wantSchema string) error {
	if h.Schema == "" {
		return errf(CodeMissingField, "schema", "schema name is required")
	}
	if h.Schema != wantSchema {
		return errf(CodeSchemaMismatch, "schema", "want %q, got %q", wantSchema, h.Schema)
	}
	if h.SchemaVersion != SchemaVersion {
		return errf(CodeSchemaMismatch, "schema_version", "want %d, got %d", SchemaVersion, h.SchemaVersion)
	}
	return nil
}

// DigestSHA256 returns the lowercase hex SHA-256 of b.
func DigestSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CanonicalJSONDigest marshals v with encoding/json then hashes the bytes.
// Useful for receipt example hashes; not a content-addressed store.
func CanonicalJSONDigest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return DigestSHA256(b), nil
}

func validateID(field, id string) error {
	if strings.TrimSpace(id) == "" {
		return errf(CodeMissingField, field, "id is required")
	}
	if len(id) > MaxIDBytes {
		return errf(CodeTooLarge, field, "exceeds %d bytes", MaxIDBytes)
	}
	return nil
}

func validateLabel(field, s string, required bool) error {
	if s == "" {
		if required {
			return errf(CodeMissingField, field, "required")
		}
		return nil
	}
	if len(s) > MaxLabelBytes {
		return errf(CodeTooLarge, field, "exceeds %d bytes", MaxLabelBytes)
	}
	return nil
}

func validateCommitSHA(field, sha string) error {
	if sha == "" {
		return errf(CodeMissingField, field, "commit sha is required")
	}
	if !isHex(sha) || (len(sha) != 40 && len(sha) != 64) {
		return errf(CodeInvalidValue, field, "want 40- or 64-char lowercase hex")
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}

func validateHash(field, h string, required bool) error {
	if h == "" {
		if required {
			return errf(CodeMissingField, field, "hash is required")
		}
		return nil
	}
	if len(h) != MaxHashHexBytes {
		return errf(CodeInvalidValue, field, "want %d hex chars, got %d", MaxHashHexBytes, len(h))
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errf(CodeInvalidValue, field, "hash must be lowercase hex")
		}
	}
	return nil
}

func validateTimestamp(field, ts string, required bool) error {
	if ts == "" {
		if required {
			return errf(CodeMissingField, field, "timestamp is required")
		}
		return nil
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		return errf(CodeInvalidValue, field, "must be RFC3339: %v", err)
	}
	return nil
}

func inSet(field, got string, allowed ...string) error {
	if got == "" {
		return errf(CodeMissingField, field, "required")
	}
	for _, a := range allowed {
		if got == a {
			return nil
		}
	}
	return errf(CodeInvalidEnum, field, "got %q, allowed %v", got, allowed)
}

func validateNote(field, s string) error {
	if len(s) > MaxNoteBytes {
		return errf(CodeTooLarge, field, "exceeds %d bytes", MaxNoteBytes)
	}
	return nil
}

func validatePath(field, p string, required bool) error {
	if p == "" {
		if required {
			return errf(CodeMissingField, field, "path is required")
		}
		return nil
	}
	if len(p) > MaxPathBytes {
		return errf(CodeTooLarge, field, "exceeds %d bytes", MaxPathBytes)
	}
	// Public artifacts must not embed absolute private home/vault paths.
	if strings.HasPrefix(p, "/Users/") || strings.HasPrefix(p, "/home/") || strings.Contains(p, "Library/") {
		return errf(CodeInvalidValue, field, "absolute private host paths are forbidden in public contract artifacts; use synthetic relative paths")
	}
	return nil
}
