package runner

import "github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"

// Synthetic digests for runner-owned fixtures (distinct from contract/cases).
const (
	runnerCommitSHA = "cccccccccccccccccccccccccccccccccccccccc"
	runnerHashSnap  = "1010101010101010101010101010101010101010101010101010101010101010"
	runnerHashDirty = "2020202020202020202020202020202020202020202020202020202020202020"
	runnerHashPartS = "3030303030303030303030303030303030303030303030303030303030303030"
	runnerHashPartM = "4040404040404040404040404040404040404040404040404040404040404040"
	runnerHashPartU = "5050505050505050505050505050505050505050505050505050505050505050"
	runnerHashTool  = "6060606060606060606060606060606060606060606060606060606060606060"
)

func runnerHarness() contract.HarnessConfig {
	return contract.HarnessConfig{
		ModelID:               "synthetic-model-runner-r1",
		Provider:              "synthetic-local",
		ProviderVersionStatus: contract.ProviderVersionMissing,
		ToolsetID:             "synthetic-toolset-runner-v1",
		HarnessID:             "synthetic-harness-runner",
		HarnessVersion:        "0.0.0-synthetic-runner",
		Notes:                 "Runner synthetic harness; no paid provider.",
	}
}

func runnerPkg(id string) contract.RunnerPackage {
	return contract.RunnerPackage{
		PackageID:               id,
		ContenderVisiblePaths:   []string{"task/README.md", "task/src/widget.go", "task/go.mod"},
		ContainsHiddenTests:     false,
		ContainsExpectedAnswers: false,
		ContainsReferencePatch:  false,
		ContainsPostCutoff:      false,
		ContentMinimized:        true,
	}
}

func oraclePkg(id string) contract.OraclePackage {
	return contract.OraclePackage{
		PackageID:               id,
		HiddenTestRefs:          []string{"oracle/tests/hidden_suite.sha256:" + runnerHashTool},
		ExpectedAnswerRefs:      []string{"oracle/expected/answer.sha256:" + runnerHashPartU},
		ReferencePatchRefs:      []string{},
		PostCutoffEvidenceRefs:  []string{},
		ReferenceMemoryCeiling:  false,
		InaccessibleToContender: true,
		InaccessibleToRepair:    true,
	}
}

func baseRunnerCase(caseID, role string) contract.CaseDocument {
	c := contract.NewCaseDocument()
	c.CaseID = caseID
	c.Role = role
	c.Eligibility = contract.Eligibility{
		EligibleForHistoricalAttribution: false,
		PermissionRecorded:               true,
		PrivatePackageOutsidePublicRepo:  true,
		MissingInputs:                    []string{},
		Decision:                         "eligible",
		Rationale:                        "Synthetic runner fixture (#543); not a real historical case.",
	}
	c.Cutoff = contract.TimeCutoff{
		CutoffAt:            "2026-04-01T18:00:00Z",
		TimezoneNote:        "UTC",
		PostCutoffForbidden: true,
	}
	c.Repository = contract.RepositoryState{
		CommitSHA: runnerCommitSHA,
		DirtyFiles: []contract.DirtyFile{
			{Path: "task/src/widget.go", ContentSHA: runnerHashDirty, Status: "modified"},
		},
		RelevantDirtyOnly:  true,
		ReconstructionNote: "Synthetic runner workspace reconstruction.",
	}
	c.Harness = runnerHarness()
	c.MemorySnapshot = contract.MemorySnapshot{
		SnapshotID:    "snap-runner-" + caseID,
		Format:        "synthetic_v1",
		BytesSHA256:   runnerHashSnap,
		ByteLength:    192,
		MemoryVersion: "synth-runner-mem-v1",
		RecordIDs:     []string{"mem_synth_runner_retry"},
		Fidelity:      contract.FidelityReconstruction,
		Synthetic:     true,
		Notes:         "Snapshot bytes/version — distinct from delivered parts.",
	}
	c.DeliveredContext = contract.DeliveredContext{
		ManifestID:           "exposure-runner-" + caseID,
		ExposureAvailability: contract.ExposureSynthetic,
		Fidelity:             contract.FidelityReconstruction,
		Parts: []contract.DeliveredPart{
			{PartID: "part-system", Kind: "system", BytesSHA256: runnerHashPartS, ByteLength: 64},
			{PartID: "part-memory", Kind: "memory_block", BytesSHA256: runnerHashPartM, ByteLength: 32, MemoryRefs: []string{"mem_synth_runner_retry"}},
			{PartID: "part-user", Kind: "user", BytesSHA256: runnerHashPartU, ByteLength: 48},
			{PartID: "part-tool", Kind: "tool_result", BytesSHA256: runnerHashTool, ByteLength: 24},
		},
		Synthetic: true,
	}
	c.RunnerPackage = runnerPkg("runner-pkg-" + caseID)
	c.OraclePackage = oraclePkg("oracle-pkg-" + caseID)
	c.IsolationAssumptions = contract.DefaultIsolationAssumptions()
	return c
}

// SynthFailureCase is the known task-failure synthetic case.
func SynthFailureCase() contract.CaseDocument {
	c := baseRunnerCase("case-runner-synth-failure-001", contract.CaseRoleFailure)
	c.Controls = contract.ControlSpec{
		ValidMemoryConstraint:       "Retrying a mutating API call must reuse the same Idempotency-Key.",
		ValidMemoryMustSurvive:      true,
		NoRelevantMemoryTask:        true,
		NoRelevantMemorySuccessRule: "A docs-typo-only task must succeed without the proposed memory edit.",
	}
	c.Notes = "Synthetic known task failure for runner plumbing."
	return c
}

// SynthValidMemoryCase is the valid-memory constraint control.
func SynthValidMemoryCase() contract.CaseDocument {
	c := baseRunnerCase("case-runner-synth-valid-memory-001", contract.CaseRoleValidMemoryControl)
	c.Controls = contract.ControlSpec{
		ValidMemoryConstraint:  "Retrying a mutating API call must reuse the same Idempotency-Key.",
		ValidMemoryMustSurvive: true,
	}
	c.Notes = "Synthetic valid-memory control for runner plumbing."
	return c
}

// SynthNoRelevantMemoryCase is the no-relevant-memory control.
func SynthNoRelevantMemoryCase() contract.CaseDocument {
	c := baseRunnerCase("case-runner-synth-no-relevant-memory-001", contract.CaseRoleNoRelevantMemoryCtrl)
	c.MemorySnapshot.RecordIDs = []string{}
	c.DeliveredContext.Parts = []contract.DeliveredPart{
		{PartID: "part-system", Kind: "system", BytesSHA256: runnerHashPartS, ByteLength: 64},
		{PartID: "part-user", Kind: "user", BytesSHA256: runnerHashPartU, ByteLength: 48},
	}
	c.Controls = contract.ControlSpec{
		NoRelevantMemoryTask:        true,
		NoRelevantMemorySuccessRule: "A docs-typo-only task must succeed without the proposed memory edit.",
	}
	c.Notes = "Synthetic no-relevant-memory control for runner plumbing."
	return c
}

// AllSynthCases returns the three required synthetic cases.
func AllSynthCases() []contract.CaseDocument {
	return []contract.CaseDocument{
		SynthFailureCase(),
		SynthValidMemoryCase(),
		SynthNoRelevantMemoryCase(),
	}
}

// ConditionsFor returns the three initial conditions for a case.
func ConditionsFor(caseID string) []contract.ConditionDocument {
	return contract.ExamplePositiveConditions(caseID)
}
