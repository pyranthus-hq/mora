package cases

import (
	"fmt"
	"strings"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// Cases-package error codes (stable vocabulary for eligibility rejection).
const (
	CodeMissingDeliveredContext = "missing_delivered_context"
	CodeMissingDirtyFiles       = "missing_dirty_files"
	CodeUnknownPermission       = "unknown_permission"
	CodePostCutoffRepair        = "post_cutoff_repair_evidence"
	CodeSnapshotEqualsExposure  = "snapshot_equals_delivered_context"
	CodeMissingRequiredHash     = "missing_required_hash"
	CodeMissingCutoff           = "missing_cutoff"
	CodeRepoReconstruction      = "repo_reconstruction_incomplete"
	CodeContractReject          = "contract_reject"
)

// Error is the eligibility-validation error type for this package.
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

// ValidateEligibilityManifest performs offline eligibility packaging checks
// on top of the frozen #541 contract:
//
//   - required bytes / hashes / temporal cutoff / repository reconstruction
//   - memory snapshot bytes are not the delivered-context exposure
//   - rejects missing delivered context, missing dirty files (when
//     relevant_dirty_only), unknown permissions, and post-cutoff material
//     reachable by repair/contender
//
// Explicit rejection detectors run before contract.Validate so public fixtures
// surface stable cases.* codes. Synthetic fixtures may pass with
// decision=eligible and eligible_for_historical_attribution=false (plumbing
// only). Real historical attribution remains a private-packet outcome.
func ValidateEligibilityManifest(c contract.CaseDocument) error {
	// Stable rejection codes first (fixtures + protocol checklist).
	if err := rejectMissingDeliveredContext(c); err != nil {
		return err
	}
	if err := rejectMissingDirtyFiles(c); err != nil {
		return err
	}
	if err := rejectUnknownPermission(c); err != nil {
		return err
	}
	if err := rejectPostCutoffRepairEvidence(c); err != nil {
		return err
	}

	if err := requireCutoff(c); err != nil {
		return err
	}
	if err := requireHashesAndBytes(c); err != nil {
		return err
	}
	if err := requireRepoReconstruction(c); err != nil {
		return err
	}
	if err := requireSnapshotDistinctFromExposure(c); err != nil {
		return err
	}

	if err := c.Validate(); err != nil {
		return errf(CodeContractReject, "", "%v", err)
	}
	return nil
}

func requireCutoff(c contract.CaseDocument) error {
	if strings.TrimSpace(c.Cutoff.CutoffAt) == "" {
		return errf(CodeMissingCutoff, "cutoff.cutoff_at", "temporal cutoff is required")
	}
	if !c.Cutoff.PostCutoffForbidden {
		return errf(CodePostCutoffRepair, "cutoff.post_cutoff_forbidden_to_contender", "post-cutoff evidence must be forbidden to contender/repair")
	}
	return nil
}

func requireHashesAndBytes(c contract.CaseDocument) error {
	if c.MemorySnapshot.BytesSHA256 == "" {
		return errf(CodeMissingRequiredHash, "memory_snapshot.bytes_sha256", "snapshot hash is required")
	}
	if c.MemorySnapshot.ByteLength < 0 {
		return errf(CodeMissingRequiredHash, "memory_snapshot.byte_length", "byte_length cannot be negative")
	}
	if c.Repository.CommitSHA == "" {
		return errf(CodeMissingRequiredHash, "repository.commit_sha", "commit sha is required for reconstruction")
	}
	for i, d := range c.Repository.DirtyFiles {
		if d.ContentSHA == "" {
			return errf(CodeMissingRequiredHash, fmt.Sprintf("repository.dirty_files[%d].content_sha256", i), "dirty file hash is required")
		}
	}
	for i, p := range c.DeliveredContext.Parts {
		if p.BytesSHA256 == "" {
			return errf(CodeMissingRequiredHash, fmt.Sprintf("delivered_context.parts[%d].bytes_sha256", i), "delivered part hash is required")
		}
	}
	return nil
}

func requireRepoReconstruction(c contract.CaseDocument) error {
	if c.Repository.CommitSHA == "" {
		return errf(CodeRepoReconstruction, "repository.commit_sha", "repository commit is required")
	}
	// Reconstruction fidelity must carry an explicit note so a reader cannot
	// confuse a rebuilt workspace with faithful historical bytes.
	if c.MemorySnapshot.Fidelity == contract.FidelityReconstruction ||
		c.DeliveredContext.Fidelity == contract.FidelityReconstruction {
		if strings.TrimSpace(c.Repository.ReconstructionNote) == "" &&
			strings.TrimSpace(c.Notes) == "" {
			return errf(CodeRepoReconstruction, "repository.reconstruction_note", "reconstruction fidelity requires an explicit reconstruction note on the repository or case notes")
		}
	}
	return nil
}

func requireSnapshotDistinctFromExposure(c contract.CaseDocument) error {
	if c.MemorySnapshot.SnapshotID != "" && c.MemorySnapshot.SnapshotID == c.DeliveredContext.ManifestID {
		return errf(CodeSnapshotEqualsExposure, "memory_snapshot.snapshot_id", "snapshot_id must differ from delivered_context.manifest_id")
	}
	snap := c.MemorySnapshot.BytesSHA256
	if snap == "" {
		return nil
	}
	for _, p := range c.DeliveredContext.Parts {
		if p.BytesSHA256 == snap {
			return errf(CodeSnapshotEqualsExposure, "delivered_context.parts",
				"part %s hash equals memory_snapshot.bytes_sha256; snapshot bytes are not delivered context", p.PartID)
		}
	}
	return nil
}

// rejectMissingDeliveredContext fails closed when exposure is missing or empty
// for a packaging claim that needs delivered context (present/synthetic with parts).
func rejectMissingDeliveredContext(c contract.CaseDocument) error {
	switch c.DeliveredContext.ExposureAvailability {
	case contract.ExposureMissing:
		return errf(CodeMissingDeliveredContext, "delivered_context.exposure_availability",
			"eligibility packaging requires delivered context; mark prospective_capture privately if bytes were never captured")
	case contract.ExposurePresent, contract.ExposureSynthetic, contract.ExposurePartial:
		if len(c.DeliveredContext.Parts) == 0 {
			return errf(CodeMissingDeliveredContext, "delivered_context.parts",
				"non-missing exposure must include at least one delivered part")
		}
	}
	return nil
}

// rejectMissingDirtyFiles requires dirty_files when relevant_dirty_only is set.
func rejectMissingDirtyFiles(c contract.CaseDocument) error {
	if c.Repository.DirtyFiles == nil {
		return errf(CodeMissingDirtyFiles, "repository.dirty_files", "dirty_files must be a non-null slice ([] when empty)")
	}
	if c.Repository.RelevantDirtyOnly && len(c.Repository.DirtyFiles) == 0 {
		return errf(CodeMissingDirtyFiles, "repository.dirty_files",
			"relevant_dirty_only=true requires at least one dirty file entry with content hash")
	}
	return nil
}

// rejectUnknownPermission fails when eligibility claims a go decision without
// a recorded permission.
func rejectUnknownPermission(c contract.CaseDocument) error {
	if c.Eligibility.Decision == "eligible" && !c.Eligibility.PermissionRecorded {
		return errf(CodeUnknownPermission, "eligibility.permission_recorded",
			"eligible decision requires a recorded permission; unknown permission is a no-go")
	}
	if c.Eligibility.Decision == "eligible" && !c.Eligibility.PrivatePackageOutsidePublicRepo {
		return errf(CodeUnknownPermission, "eligibility.private_package_outside_public_repo",
			"eligible decision requires the private package to stay outside the public repo")
	}
	return nil
}

// rejectPostCutoffRepairEvidence fails when post-cutoff material is marked
// contender-visible or when the runner package admits post-cutoff content.
func rejectPostCutoffRepairEvidence(c contract.CaseDocument) error {
	if c.RunnerPackage.ContainsPostCutoff {
		return errf(CodePostCutoffRepair, "runner_package.contains_post_cutoff_evidence",
			"post-cutoff evidence must not reach contender or repair inputs")
	}
	for _, p := range c.RunnerPackage.ContenderVisiblePaths {
		lower := strings.ToLower(p)
		if strings.Contains(lower, "post_cutoff") || strings.Contains(lower, "post-cutoff") {
			return errf(CodePostCutoffRepair, "runner_package.contender_visible_paths",
				"path %q looks like post-cutoff material in contender/repair inputs", p)
		}
	}
	// Oracle may hold post-cutoff refs; they must remain inaccessible flags.
	if len(c.OraclePackage.PostCutoffEvidenceRefs) > 0 {
		if !c.OraclePackage.InaccessibleToContender || !c.OraclePackage.InaccessibleToRepair {
			return errf(CodePostCutoffRepair, "oracle_package",
				"post-cutoff evidence refs require inaccessible_to_contender and inaccessible_to_repair")
		}
	}
	return nil
}
