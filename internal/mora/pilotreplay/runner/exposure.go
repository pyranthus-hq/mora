package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// ToolResultCaptureStatus records whether tool-result reads were captured.
const (
	ToolResultCaptured    = "captured"
	ToolResultUnavailable = "unavailable"
)

// ExposureRecord is the exact delivered context for one attempt, kept
// separate from the underlying memory snapshot.
type ExposureRecord struct {
	ConditionID          string                   `json:"condition_id"`
	ConditionKind        string                   `json:"condition_kind"`
	SnapshotID           string                   `json:"snapshot_id"`
	SnapshotBytesSHA256  string                   `json:"snapshot_bytes_sha256"`
	SnapshotByteLength   int                      `json:"snapshot_byte_length"`
	DeliveredManifestID  string                   `json:"delivered_manifest_id"`
	ExposureAvailability string                   `json:"exposure_availability"`
	Parts                []contract.DeliveredPart `json:"parts"`
	ToolResultStatus     string                   `json:"tool_result_status"`
	ToolResultNote       string                   `json:"tool_result_note,omitempty"`
	// InjectedMemoryDigest is the condition-selected memory bytes digest
	// actually placed for the contender (may be empty-marker for no_memory).
	InjectedMemoryDigest string `json:"injected_memory_digest"`
	// DeliveredDigest is a digest over the exposure parts only (not snapshot).
	DeliveredDigest string `json:"delivered_digest"`
}

// InputDigests freezes attempt inputs for the receipt.
type InputDigests struct {
	CaseID            string        `json:"case_id"`
	ConditionID       string        `json:"condition_id"`
	RepoCommitSHA     string        `json:"repo_commit_sha"`
	WorkspaceBaseline InitialHashes `json:"workspace_baseline"`
	Exposure          string        `json:"exposure_digest"`
	Snapshot          string        `json:"snapshot_digest"`
	HarnessID         string        `json:"harness_id"`
	ModelID           string        `json:"model_id"`
}

// InjectCondition stages condition-specific memory into the disposable vault
// and builds an exposure record. Snapshot bytes are never substituted for
// delivered parts.
func (r *Runner) InjectCondition(c contract.CaseDocument, cond contract.ConditionDocument) (ExposureRecord, error) {
	if err := cond.Validate(); err != nil {
		return ExposureRecord{}, errf(CodeAdmitReject, "condition", "%v", err)
	}
	if cond.CaseID != c.CaseID {
		return ExposureRecord{}, errf(CodeAdmitReject, "condition.case_id", "condition case_id %q != case %q", cond.CaseID, c.CaseID)
	}

	exp := ExposureRecord{
		ConditionID:          cond.ConditionID,
		ConditionKind:        cond.Kind,
		SnapshotID:           c.MemorySnapshot.SnapshotID,
		SnapshotBytesSHA256:  c.MemorySnapshot.BytesSHA256,
		SnapshotByteLength:   c.MemorySnapshot.ByteLength,
		DeliveredManifestID:  c.DeliveredContext.ManifestID,
		ExposureAvailability: c.DeliveredContext.ExposureAvailability,
		Parts:                append([]contract.DeliveredPart(nil), c.DeliveredContext.Parts...),
	}

	hasTool := false
	for _, p := range exp.Parts {
		if p.Kind == "tool_result" {
			hasTool = true
			break
		}
	}
	if hasTool {
		exp.ToolResultStatus = ToolResultCaptured
	} else {
		exp.ToolResultStatus = ToolResultUnavailable
		exp.ToolResultNote = "no tool_result parts in delivered context; request logs alone are not proof of model exposure"
	}

	if exp.ExposureAvailability == contract.ExposureMissing && len(exp.Parts) == 0 {
		return exp, errf(CodeMissingExposure, "delivered_context", "missing exposure capture: %s", c.DeliveredContext.MissingReason)
	}

	vaultPath := filepath.Join(r.ws.Layout.Vault, "condition_memory.json")
	var injected any
	switch cond.Kind {
	case contract.ConditionNoMemory:
		injected = map[string]any{
			"condition":       cond.Kind,
			"records":         []any{},
			"snapshot_ref":    c.MemorySnapshot.SnapshotID,
			"note":            "no_memory condition: vault staged empty; snapshot retained for accounting only",
			"snapshot_sha256": c.MemorySnapshot.BytesSHA256,
		}
		exp.InjectedMemoryDigest = contract.DigestSHA256([]byte("{}"))
	case contract.ConditionOriginalMemory:
		injected = map[string]any{
			"condition":      cond.Kind,
			"snapshot_id":    c.MemorySnapshot.SnapshotID,
			"bytes_sha256":   c.MemorySnapshot.BytesSHA256,
			"byte_length":    c.MemorySnapshot.ByteLength,
			"memory_version": c.MemorySnapshot.MemoryVersion,
			"record_ids":     c.MemorySnapshot.RecordIDs,
			"note":           "original_memory: snapshot bytes staged; delivered parts remain separate",
		}
		exp.InjectedMemoryDigest = c.MemorySnapshot.BytesSHA256
	case contract.ConditionReviewedMemoryEdit:
		injected = map[string]any{
			"condition":        cond.Kind,
			"memory_edit_ref":  cond.MemoryEditRef,
			"edit_reviewed_by": cond.EditReviewedBy,
			"base_snapshot_id": c.MemorySnapshot.SnapshotID,
			"base_sha256":      c.MemorySnapshot.BytesSHA256,
			"note":             "reviewed_memory_edit: edited view staged; snapshot identity retained separately",
		}
		exp.InjectedMemoryDigest = contract.DigestSHA256([]byte(cond.MemoryEditRef + "|" + c.MemorySnapshot.BytesSHA256))
	default:
		return ExposureRecord{}, errf(CodeAdmitReject, "condition.kind", "unsupported %q", cond.Kind)
	}

	b, err := json.MarshalIndent(injected, "", "  ")
	if err != nil {
		return ExposureRecord{}, errf(CodeInternal, "inject", "%v", err)
	}
	if err := os.WriteFile(vaultPath, b, 0o644); err != nil {
		return ExposureRecord{}, errf(CodeWorkspace, "vault", "%v", err)
	}

	partPayload, err := json.Marshal(exp.Parts)
	if err != nil {
		return ExposureRecord{}, errf(CodeInternal, "exposure", "%v", err)
	}
	exp.DeliveredDigest = contract.DigestSHA256(partPayload)

	if len(exp.Parts) > 0 && exp.SnapshotBytesSHA256 == exp.DeliveredDigest {
		return ExposureRecord{}, errf(CodeMissingExposure, "snapshot_vs_delivered", "snapshot digest must not equal delivered-parts digest")
	}

	for _, rel := range c.RunnerPackage.ContenderVisiblePaths {
		if err := rejectUnsafeRel(rel); err != nil {
			return ExposureRecord{}, err
		}
		dst := filepath.Join(r.ws.Layout.Contender, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return ExposureRecord{}, errf(CodeWorkspace, "contender", "%v", err)
		}
		body := []byte("# synthetic contender input\npath: " + rel + "\ncase: " + c.CaseID + "\ncondition: " + cond.Kind + "\n")
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return ExposureRecord{}, errf(CodeWorkspace, "contender", "%v", err)
		}
	}

	oracleMeta := map[string]any{
		"package_id":    c.OraclePackage.PackageID,
		"hash_ref_only": true,
		"hidden_tests":  c.OraclePackage.HiddenTestRefs,
		"note":          "oracle contents are not mounted into contender workspace",
	}
	ob, err := json.MarshalIndent(oracleMeta, "", "  ")
	if err != nil {
		return ExposureRecord{}, errf(CodeInternal, "oracle_ref", "%v", err)
	}
	if err := os.WriteFile(filepath.Join(r.ws.Layout.OracleRef, "oracle.hashref.json"), ob, 0o644); err != nil {
		return ExposureRecord{}, errf(CodeWorkspace, "oracle_ref", "%v", err)
	}

	return exp, nil
}

func rejectUnsafeRel(rel string) error {
	if rel == "" || filepath.IsAbs(rel) {
		return errf(CodePathRejected, "path", "rejected contender path %q", rel)
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".." {
			return errf(CodePathRejected, "path", "rejected contender path %q", rel)
		}
	}
	lower := strings.ToLower(filepath.ToSlash(rel))
	for _, bad := range []string{"oracle", "gold", "expected_answer", "reference_patch", "post_cutoff", "hidden_test"} {
		if strings.Contains(lower, bad) {
			return errf(CodeOracleLeak, "path", "refusing to stage %q into contender", rel)
		}
	}
	return nil
}
