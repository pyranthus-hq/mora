package cases

import "github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"

// Synthetic digests — deterministic placeholders, not real vault or repo bytes.
const (
	synthCommitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	synthHashSnap  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	synthHashDirty = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	synthHashPartS = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	synthHashPartM = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	synthHashPartU = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	synthHashPartT = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

func synthHarness() contract.HarnessConfig {
	return contract.HarnessConfig{
		ModelID:               "synthetic-model-eligibility-r1",
		Provider:              "synthetic-provider",
		ProviderVersionStatus: contract.ProviderVersionMissing,
		ToolsetID:             "synthetic-toolset-eligibility-v1",
		HarnessID:             "synthetic-harness-eligibility",
		HarnessVersion:        "0.0.0-synthetic",
		Notes:                 "Synthetic eligibility fixture; provider version intentionally missing.",
	}
}

func synthRunner(packageID string) contract.RunnerPackage {
	return contract.RunnerPackage{
		PackageID:               packageID,
		ContenderVisiblePaths:   []string{"task/README.md", "task/src/widget.go", "task/go.mod"},
		ContainsHiddenTests:     false,
		ContainsExpectedAnswers: false,
		ContainsReferencePatch:  false,
		ContainsPostCutoff:      false,
		ContentMinimized:        true,
	}
}

func synthOracle(packageID string) contract.OraclePackage {
	return contract.OraclePackage{
		PackageID:               packageID,
		HiddenTestRefs:          []string{"oracle/tests/hidden_suite.sha256:" + synthHashPartT},
		ExpectedAnswerRefs:      []string{"oracle/expected/answer.sha256:" + synthHashPartU},
		ReferencePatchRefs:      []string{},
		PostCutoffEvidenceRefs:  []string{},
		ReferenceMemoryCeiling:  false,
		InaccessibleToContender: true,
		InaccessibleToRepair:    true,
	}
}

func baseEligibilityCase(caseID, role string) contract.CaseDocument {
	c := contract.NewCaseDocument()
	c.CaseID = caseID
	c.Role = role
	c.Eligibility = contract.Eligibility{
		EligibleForHistoricalAttribution: false, // synthetic plumbing only
		PermissionRecorded:               true,
		PrivatePackageOutsidePublicRepo:  true,
		MissingInputs:                    []string{},
		Decision:                         "eligible",
		Rationale:                        "Synthetic eligibility fixture for #542 public half; not a real historical case.",
	}
	c.Cutoff = contract.TimeCutoff{
		CutoffAt:            "2026-03-01T17:00:00Z",
		TimezoneNote:        "UTC",
		PostCutoffForbidden: true,
	}
	c.Repository = contract.RepositoryState{
		CommitSHA: synthCommitSHA,
		DirtyFiles: []contract.DirtyFile{
			{Path: "task/src/widget.go", ContentSHA: synthHashDirty, Status: "modified"},
		},
		RelevantDirtyOnly:  true,
		ReconstructionNote: "Synthetic workspace reconstruction at frozen commit + one dirty file; not a real incident.",
	}
	c.Harness = synthHarness()
	c.MemorySnapshot = contract.MemorySnapshot{
		SnapshotID:    "snap-elig-" + caseID,
		Format:        "synthetic_v1",
		BytesSHA256:   synthHashSnap,
		ByteLength:    256,
		MemoryVersion: "synth-elig-mem-v1",
		RecordIDs:     []string{"mem_synth_retry_budget"},
		Fidelity:      contract.FidelityReconstruction,
		Synthetic:     true,
		Notes:         "Stored memory snapshot bytes/version — distinct from delivered prompt parts.",
	}
	c.DeliveredContext = contract.DeliveredContext{
		ManifestID:           "exposure-elig-" + caseID,
		ExposureAvailability: contract.ExposureSynthetic,
		Fidelity:             contract.FidelityReconstruction,
		Parts: []contract.DeliveredPart{
			{PartID: "part-system", Kind: "system", BytesSHA256: synthHashPartS, ByteLength: 80},
			{PartID: "part-memory", Kind: "memory_block", BytesSHA256: synthHashPartM, ByteLength: 40, MemoryRefs: []string{"mem_synth_retry_budget"}},
			{PartID: "part-user", Kind: "user", BytesSHA256: synthHashPartU, ByteLength: 60},
		},
		Synthetic: true,
	}
	c.RunnerPackage = synthRunner("runner-elig-" + caseID)
	c.OraclePackage = synthOracle("oracle-elig-" + caseID)
	c.IsolationAssumptions = contract.DefaultIsolationAssumptions()
	return c
}

// ExamplePositiveFailureManifest is a synthetic failure-case eligibility shape.
func ExamplePositiveFailureManifest() contract.CaseDocument {
	c := baseEligibilityCase("case-elig-synth-failure-001", contract.CaseRoleFailure)
	c.Controls = contract.ControlSpec{
		ValidMemoryConstraint:       "Retrying a mutating API call must reuse the same Idempotency-Key; do not discard this constraint in the edit condition.",
		ValidMemoryMustSurvive:      true,
		NoRelevantMemoryTask:        true,
		NoRelevantMemorySuccessRule: "A docs-typo-only task must succeed without the proposed memory edit.",
	}
	c.Notes = "Synthetic positive failure eligibility shape. Snapshot hash ≠ any delivered part hash. Not a real incident."
	return c
}

// ExamplePositiveValidMemoryControlManifest is the valid-memory control shape.
func ExamplePositiveValidMemoryControlManifest() contract.CaseDocument {
	c := baseEligibilityCase("case-elig-synth-valid-memory-001", contract.CaseRoleValidMemoryControl)
	c.Controls = contract.ControlSpec{
		ValidMemoryConstraint:       "Retrying a mutating API call must reuse the same Idempotency-Key; do not discard this constraint in the edit condition.",
		ValidMemoryMustSurvive:      true,
		NoRelevantMemoryTask:        false,
		NoRelevantMemorySuccessRule: "",
	}
	c.Eligibility.Rationale = "Synthetic valid-memory control: the Idempotency-Key constraint must survive the reviewed edit."
	c.Notes = "Synthetic valid-memory control eligibility shape."
	return c
}

// ExamplePositiveNoRelevantMemoryControlManifest is the no-relevant-memory control.
func ExamplePositiveNoRelevantMemoryControlManifest() contract.CaseDocument {
	c := baseEligibilityCase("case-elig-synth-no-relevant-memory-001", contract.CaseRoleNoRelevantMemoryCtrl)
	c.MemorySnapshot.RecordIDs = []string{}
	c.DeliveredContext.Parts = []contract.DeliveredPart{
		{PartID: "part-system", Kind: "system", BytesSHA256: synthHashPartS, ByteLength: 80},
		{PartID: "part-user", Kind: "user", BytesSHA256: synthHashPartU, ByteLength: 60},
	}
	c.Controls = contract.ControlSpec{
		ValidMemoryConstraint:       "",
		ValidMemoryMustSurvive:      false,
		NoRelevantMemoryTask:        true,
		NoRelevantMemorySuccessRule: "A docs-typo-only task must succeed without the proposed memory edit.",
	}
	c.Eligibility.Rationale = "Synthetic no-relevant-memory control: success must not require the proposed edit."
	c.Notes = "Synthetic no-relevant-memory control eligibility shape."
	return c
}

// AllPositiveEligibilityManifests returns the three required synthetic shapes.
func AllPositiveEligibilityManifests() []contract.CaseDocument {
	return []contract.CaseDocument{
		ExamplePositiveFailureManifest(),
		ExamplePositiveValidMemoryControlManifest(),
		ExamplePositiveNoRelevantMemoryControlManifest(),
	}
}

// ExampleRejectMissingDeliveredContext has exposure marked missing.
func ExampleRejectMissingDeliveredContext() contract.CaseDocument {
	c := ExamplePositiveFailureManifest()
	c.CaseID = "case-elig-synth-reject-missing-delivered"
	c.DeliveredContext.ExposureAvailability = contract.ExposureMissing
	c.DeliveredContext.Parts = []contract.DeliveredPart{}
	c.DeliveredContext.Fidelity = contract.FidelityUnknown
	c.DeliveredContext.MissingReason = "synthetic: delivered prompt bytes were never captured"
	c.DeliveredContext.Synthetic = true
	c.Eligibility.Decision = "prospective_capture"
	c.Eligibility.EligibleForHistoricalAttribution = false
	c.Eligibility.Rationale = "Rejection fixture: missing delivered context."
	c.Notes = "Rejection fixture — missing delivered context."
	return c
}

// ExampleRejectMissingDirtyFiles claims relevant dirty-only with an empty list.
func ExampleRejectMissingDirtyFiles() contract.CaseDocument {
	c := ExamplePositiveFailureManifest()
	c.CaseID = "case-elig-synth-reject-missing-dirty"
	c.Repository.DirtyFiles = []contract.DirtyFile{}
	c.Repository.RelevantDirtyOnly = true
	c.Eligibility.Rationale = "Rejection fixture: missing dirty files under relevant_dirty_only."
	c.Notes = "Rejection fixture — missing dirty files."
	return c
}

// ExampleRejectUnknownPermission claims eligible without a permission record.
// Contract Validate rejects first; eligibility packaging re-codes the same gate.
func ExampleRejectUnknownPermission() contract.CaseDocument {
	c := ExamplePositiveFailureManifest()
	c.CaseID = "case-elig-synth-reject-unknown-permission"
	c.Eligibility.PermissionRecorded = false
	c.Eligibility.Decision = "eligible"
	c.Eligibility.Rationale = "Rejection fixture: unknown / unrecorded permission."
	c.Notes = "Rejection fixture — unknown permission."
	return c
}

// ExampleRejectPostCutoffRepairEvidence puts post-cutoff material in the runner.
func ExampleRejectPostCutoffRepairEvidence() contract.CaseDocument {
	c := ExamplePositiveFailureManifest()
	c.CaseID = "case-elig-synth-reject-post-cutoff-repair"
	c.RunnerPackage.ContainsPostCutoff = true
	c.RunnerPackage.ContenderVisiblePaths = append(
		c.RunnerPackage.ContenderVisiblePaths,
		"post_cutoff/repair_hint.md",
	)
	c.OraclePackage.PostCutoffEvidenceRefs = []string{"oracle/post_cutoff/chat.sha256:" + synthHashPartT}
	c.Eligibility.Rationale = "Rejection fixture: post-cutoff repair evidence leaked into contender/repair inputs."
	c.Notes = "Rejection fixture — post-cutoff repair evidence."
	return c
}

// AllRejectionFixtures returns the four required rejection shapes.
func AllRejectionFixtures() []struct {
	Name string
	Case contract.CaseDocument
	Code string
} {
	return []struct {
		Name string
		Case contract.CaseDocument
		Code string
	}{
		{"missing_delivered_context", ExampleRejectMissingDeliveredContext(), CodeMissingDeliveredContext},
		{"missing_dirty_files", ExampleRejectMissingDirtyFiles(), CodeMissingDirtyFiles},
		{"unknown_permission", ExampleRejectUnknownPermission(), CodeUnknownPermission},
		{"post_cutoff_repair_evidence", ExampleRejectPostCutoffRepairEvidence(), CodePostCutoffRepair},
	}
}
