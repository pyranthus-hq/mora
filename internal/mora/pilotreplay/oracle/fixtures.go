package oracle

import "github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"

// Synthetic stand-in tokens — NOT real private gold. Public fixtures only.
const (
	synthConstraintToken = "Idempotency-Key"
	synthConstraintFile  = "task/constraints.md"
	synthRegressionFile  = "task/src/widget.go"
	synthRegressionToken = "func WidgetHealthy()"
	synthTaskPassKey     = "task_result"
	synthTaskPassVal     = "correct_implementation"
	synthTaskFailKey     = "task_result"
	synthTaskFailVal     = "known_fault"
	synthNRMPassKey      = "task_result"
	synthNRMPassVal      = "docs_typo_fixed"
	synthNRMForbidKey    = "required_memory_edit"
	synthNRMForbidVal    = "true"
)

// SyntheticHiddenSpec is the public synthetic evaluator stand-in.
// Real private hidden tests remain Adit/evaluator-only outside this repo.
func SyntheticHiddenSpec() HiddenSpec {
	h := NewHiddenSpec()
	h.SpecID = "hidden-synth-oracle-v1"
	h.Synthetic = true
	h.TaskPassMarkers = []MarkerCheck{
		{Key: synthTaskPassKey, RequiredValue: synthTaskPassVal},
	}
	h.TaskFailMarkers = []MarkerCheck{
		{Key: synthTaskFailKey, RequiredValue: synthTaskFailVal},
	}
	h.ConstraintChecks = []FileTokenCheck{
		{
			Path:        synthConstraintFile,
			Token:       synthConstraintToken,
			MustContain: true,
			Description: "valid-memory constraint: mutating POST retries must reuse Idempotency-Key",
		},
	}
	h.RegressionGuards = []FileTokenCheck{
		{
			Path:        synthRegressionFile,
			Token:       synthRegressionToken,
			MustContain: true,
			Description: "unrelated healthy helper must survive edits",
		},
	}
	h.NoRelevantMemory = NoRelevantMemorySpec{
		SuccessMarkers: []MarkerCheck{
			{Key: synthNRMPassKey, RequiredValue: synthNRMPassVal},
		},
		ForbiddenMarkers: []MarkerCheck{
			{Key: synthNRMForbidKey, RequiredValue: synthNRMForbidVal},
		},
		SuccessRule: "A docs-typo-only task must succeed without the proposed memory edit.",
	}
	h.Notes = "Synthetic stand-in for #544 public suite. Real private gold stays outside the public repository (Adit handoff)."
	return h
}

func baseArtifact(id, role string) OutputArtifact {
	a := NewOutputArtifact()
	a.ArtifactID = id
	a.CaseRole = role
	a.Meta.ProducedBy = "synthetic_fixture"
	a.Meta.Notes = "Synthetic output artifact for offline oracle sensitivity tests."
	// Default preserved constraint + regression bodies.
	a.FileContents[synthConstraintFile] = "# Constraints\n\nRetrying a mutating API call must reuse the same Idempotency-Key.\n"
	a.FileContents[synthRegressionFile] = "package task\n\nfunc WidgetHealthy() bool { return true }\n"
	return a
}

// FixtureKnownBad is a deliberately faulty output (known coding failure).
func FixtureKnownBad() OutputArtifact {
	a := baseArtifact("artifact-synth-known-bad-001", contract.CaseRoleFailure)
	a.TaskMarkers[synthTaskFailKey] = synthTaskFailVal
	a.Meta.Notes = "Known-bad synthetic: task_result=known_fault"
	return a
}

// FixtureCorrect is an independently justified expected-behavior fixture.
// Justification: markers match SyntheticHiddenSpec pass requirements; constraint
// and regression tokens are present. Not derived from observing a comparison.
func FixtureCorrect() OutputArtifact {
	a := baseArtifact("artifact-synth-correct-001", contract.CaseRoleFailure)
	a.TaskMarkers[synthTaskPassKey] = synthTaskPassVal
	a.Meta.Notes = "Correct synthetic: independently justified expected markers + preserved constraints"
	return a
}

// FixtureConstraintDropping passes the task markers but drops the valid-memory
// constraint token from constraints.md.
func FixtureConstraintDropping() OutputArtifact {
	a := baseArtifact("artifact-synth-constraint-drop-001", contract.CaseRoleFailure)
	a.TaskMarkers[synthTaskPassKey] = synthTaskPassVal
	a.FileContents[synthConstraintFile] = "# Constraints\n\n(constraint removed by faulty edit)\n"
	a.Meta.Notes = "Constraint-dropping synthetic: task markers pass but Idempotency-Key token absent"
	return a
}

// FixtureConstraintDroppingControl is the valid-memory control role dropping
// the constraint (damage control detection).
func FixtureConstraintDroppingControl() OutputArtifact {
	a := baseArtifact("artifact-synth-constraint-drop-ctrl-001", contract.CaseRoleValidMemoryControl)
	a.FileContents[synthConstraintFile] = "# Constraints\n\n(constraint removed)\n"
	a.Meta.Notes = "Valid-memory control with dropped constraint token"
	return a
}

// FixtureValidMemoryPreserved is the valid-memory control with constraint intact.
func FixtureValidMemoryPreserved() OutputArtifact {
	a := baseArtifact("artifact-synth-valid-memory-ok-001", contract.CaseRoleValidMemoryControl)
	a.Meta.Notes = "Valid-memory control: constraint preserved"
	return a
}

// FixtureNoRelevantMemoryOK succeeds without requiring a memory edit.
func FixtureNoRelevantMemoryOK() OutputArtifact {
	a := baseArtifact("artifact-synth-nrm-ok-001", contract.CaseRoleNoRelevantMemoryCtrl)
	a.TaskMarkers[synthNRMPassKey] = synthNRMPassVal
	// required_memory_edit intentionally absent
	a.Meta.Notes = "No-relevant-memory control: docs typo fixed without memory edit"
	return a
}

// FixtureNoRelevantMemoryRequiresEdit fails the control by requiring the edit.
func FixtureNoRelevantMemoryRequiresEdit() OutputArtifact {
	a := baseArtifact("artifact-synth-nrm-requires-edit-001", contract.CaseRoleNoRelevantMemoryCtrl)
	a.TaskMarkers[synthNRMPassKey] = synthNRMPassVal
	a.TaskMarkers[synthNRMForbidKey] = synthNRMForbidVal
	a.Meta.Notes = "No-relevant-memory negative: claims success only via required_memory_edit"
	return a
}

// FixtureMalformed has the wrong schema (unsupported/malformed).
func FixtureMalformed() OutputArtifact {
	a := baseArtifact("artifact-synth-malformed-001", contract.CaseRoleFailure)
	a.Schema = "not.a.valid.oracle_artifact"
	a.TaskMarkers[synthTaskPassKey] = synthTaskPassVal
	a.Meta.Notes = "Malformed schema for inconclusive mapping"
	return a
}

// FixtureUnsupportedVersion has an unsupported schema_version.
func FixtureUnsupportedVersion() OutputArtifact {
	a := baseArtifact("artifact-synth-unsupported-ver-001", contract.CaseRoleFailure)
	a.SchemaVersion = 99
	a.TaskMarkers[synthTaskPassKey] = synthTaskPassVal
	a.Meta.Notes = "Unsupported schema_version for inconclusive mapping"
	return a
}

// FixtureRegressionBroken passes task + constraint but breaks unrelated guard.
func FixtureRegressionBroken() OutputArtifact {
	a := baseArtifact("artifact-synth-regression-001", contract.CaseRoleFailure)
	a.TaskMarkers[synthTaskPassKey] = synthTaskPassVal
	a.FileContents[synthRegressionFile] = "package task\n\n// WidgetHealthy removed\n"
	a.Meta.Notes = "Unrelated regression: WidgetHealthy missing"
	return a
}

// SyntheticCaseForBoundary returns a contract case suitable for access-boundary
// tests (synthetic IDs/paths only).
func SyntheticCaseForBoundary() contract.CaseDocument {
	c := contract.NewCaseDocument()
	c.CaseID = "case-oracle-synth-boundary-001"
	c.Role = contract.CaseRoleFailure
	c.Eligibility = contract.Eligibility{
		EligibleForHistoricalAttribution: false,
		PermissionRecorded:               true,
		PrivatePackageOutsidePublicRepo:  true,
		MissingInputs:                    []string{},
		Decision:                         "eligible",
		Rationale:                        "Synthetic boundary fixture for #544; not a real historical case.",
	}
	c.Cutoff = contract.TimeCutoff{
		CutoffAt:            "2026-05-01T18:00:00Z",
		TimezoneNote:        "UTC",
		PostCutoffForbidden: true,
	}
	c.Repository = contract.RepositoryState{
		CommitSHA: "dddddddddddddddddddddddddddddddddddddddd",
		DirtyFiles: []contract.DirtyFile{
			{Path: "task/src/widget.go", ContentSHA: "7070707070707070707070707070707070707070707070707070707070707070", Status: "modified"},
		},
		RelevantDirtyOnly:  true,
		ReconstructionNote: "Synthetic oracle boundary reconstruction.",
	}
	c.Harness = contract.HarnessConfig{
		ModelID:               "synthetic-model-oracle-r1",
		Provider:              "synthetic-local",
		ProviderVersionStatus: contract.ProviderVersionMissing,
		ToolsetID:             "synthetic-toolset-oracle-v1",
		HarnessID:             "synthetic-harness-oracle",
		HarnessVersion:        "0.0.0-synthetic-oracle",
		Notes:                 "Oracle synthetic harness; no paid provider.",
	}
	c.MemorySnapshot = contract.MemorySnapshot{
		SnapshotID:    "snap-oracle-boundary",
		Format:        "synthetic_v1",
		BytesSHA256:   "8080808080808080808080808080808080808080808080808080808080808080",
		ByteLength:    64,
		MemoryVersion: "synth-oracle-mem-v1",
		RecordIDs:     []string{"mem_synth_oracle_retry"},
		Fidelity:      contract.FidelityReconstruction,
		Synthetic:     true,
		Notes:         "Snapshot ≠ delivered.",
	}
	c.DeliveredContext = contract.DeliveredContext{
		ManifestID:           "exposure-oracle-boundary",
		ExposureAvailability: contract.ExposureSynthetic,
		Fidelity:             contract.FidelityReconstruction,
		Parts: []contract.DeliveredPart{
			{PartID: "part-system", Kind: "system", BytesSHA256: "9090909090909090909090909090909090909090909090909090909090909090", ByteLength: 16},
			{PartID: "part-user", Kind: "user", BytesSHA256: "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0", ByteLength: 16},
		},
		Synthetic: true,
	}
	c.RunnerPackage = contract.RunnerPackage{
		PackageID:               "runner-oracle-boundary",
		ContenderVisiblePaths:   []string{"task/README.md", "task/src/widget.go", "task/constraints.md"},
		ContainsHiddenTests:     false,
		ContainsExpectedAnswers: false,
		ContainsReferencePatch:  false,
		ContainsPostCutoff:      false,
		ContentMinimized:        true,
	}
	c.OraclePackage = contract.OraclePackage{
		PackageID:               "oracle-pkg-boundary",
		HiddenTestRefs:          []string{"oracle/tests/hidden_suite.sha256:b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"},
		ExpectedAnswerRefs:      []string{"oracle/expected/answer.sha256:c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"},
		ReferencePatchRefs:      []string{},
		PostCutoffEvidenceRefs:  []string{},
		ReferenceMemoryCeiling:  false,
		InaccessibleToContender: true,
		InaccessibleToRepair:    true,
	}
	c.Controls = contract.ControlSpec{
		ValidMemoryConstraint:       "Retrying a mutating API call must reuse the same Idempotency-Key.",
		ValidMemoryMustSurvive:      true,
		NoRelevantMemoryTask:        true,
		NoRelevantMemorySuccessRule: "A docs-typo-only task must succeed without the proposed memory edit.",
	}
	c.IsolationAssumptions = contract.DefaultIsolationAssumptions()
	c.Notes = "Synthetic boundary case for #544 access tests."
	return c
}
