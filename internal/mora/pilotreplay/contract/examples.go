package contract

// Synthetic digests — deterministic placeholders, not real vault bytes.
const (
	synthCommitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	synthHashA     = "1111111111111111111111111111111111111111111111111111111111111111"
	synthHashB     = "2222222222222222222222222222222222222222222222222222222222222222"
	synthHashC     = "3333333333333333333333333333333333333333333333333333333333333333"
	synthHashD     = "4444444444444444444444444444444444444444444444444444444444444444"
	synthHashE     = "5555555555555555555555555555555555555555555555555555555555555555"
)

func synthHarness() HarnessConfig {
	return HarnessConfig{
		ModelID:               "synthetic-model-r1",
		Provider:              "synthetic-provider",
		ProviderVersionStatus: ProviderVersionMissing,
		ToolsetID:             "synthetic-toolset-v1",
		HarnessID:             "synthetic-harness",
		HarnessVersion:        "0.0.0-synthetic",
		Notes:                 "provider version intentionally missing for fixture",
	}
}

func synthControls() ControlSpec {
	return ControlSpec{
		ValidMemoryConstraint:       "API clients must send Idempotency-Key on mutating POST; do not remove this constraint.",
		ValidMemoryMustSurvive:      true,
		NoRelevantMemoryTask:        true,
		NoRelevantMemorySuccessRule: "formatting-only task success must not require the proposed memory edit.",
	}
}

func synthOracle() OraclePackage {
	return OraclePackage{
		PackageID:               "oracle-synth-001",
		HiddenTestRefs:          []string{"oracle/tests/hidden_suite.sha256:" + synthHashC},
		ExpectedAnswerRefs:      []string{"oracle/expected/answer.sha256:" + synthHashD},
		ReferencePatchRefs:      []string{},
		PostCutoffEvidenceRefs:  []string{},
		ReferenceMemoryCeiling:  false,
		InaccessibleToContender: true,
		InaccessibleToRepair:    true,
	}
}

func synthRunner() RunnerPackage {
	return RunnerPackage{
		PackageID:               "runner-synth-001",
		ContenderVisiblePaths:   []string{"task/README.md", "task/src/main.go", "task/go.mod"},
		ContainsHiddenTests:     false,
		ContainsExpectedAnswers: false,
		ContainsReferencePatch:  false,
		ContainsPostCutoff:      false,
		ContentMinimized:        true,
	}
}

// ExamplePositiveFailureCase is a synthetic eligible failure-case skeleton.
func ExamplePositiveFailureCase() CaseDocument {
	c := NewCaseDocument()
	c.CaseID = "case-synth-failure-001"
	c.Role = CaseRoleFailure
	c.Eligibility = Eligibility{
		EligibleForHistoricalAttribution: false, // synthetic: plumbing only
		PermissionRecorded:               true,
		PrivatePackageOutsidePublicRepo:  true,
		MissingInputs:                    []string{},
		Decision:                         "eligible",
		Rationale:                        "Synthetic fixture for schema tests; not a real historical case.",
	}
	c.Cutoff = TimeCutoff{
		CutoffAt:            "2026-01-15T18:00:00Z",
		TimezoneNote:        "UTC",
		PostCutoffForbidden: true,
	}
	c.Repository = RepositoryState{
		CommitSHA: synthCommitSHA,
		DirtyFiles: []DirtyFile{
			{Path: "task/src/main.go", ContentSHA: synthHashA, Status: "modified"},
		},
		RelevantDirtyOnly: true,
	}
	c.Harness = synthHarness()
	c.MemorySnapshot = MemorySnapshot{
		SnapshotID:    "snap-synth-001",
		Format:        "synthetic_v1",
		BytesSHA256:   synthHashB,
		ByteLength:    128,
		MemoryVersion: "synth-mem-v1",
		RecordIDs:     []string{"mem_synth_idempotency"},
		Fidelity:      FidelityReconstruction,
		Synthetic:     true,
		Notes:         "Stored memory bytes/version — not delivered prompt bytes.",
	}
	c.DeliveredContext = DeliveredContext{
		ManifestID:           "exposure-synth-001",
		ExposureAvailability: ExposureSynthetic,
		Fidelity:             FidelityReconstruction,
		Parts: []DeliveredPart{
			{PartID: "part-system", Kind: "system", BytesSHA256: synthHashC, ByteLength: 64},
			{PartID: "part-memory", Kind: "memory_block", BytesSHA256: synthHashD, ByteLength: 32, MemoryRefs: []string{"mem_synth_idempotency"}},
			{PartID: "part-user", Kind: "user", BytesSHA256: synthHashE, ByteLength: 48},
		},
		Synthetic: true,
	}
	c.RunnerPackage = synthRunner()
	c.OraclePackage = synthOracle()
	c.Controls = synthControls()
	c.IsolationAssumptions = DefaultIsolationAssumptions()
	c.Notes = "Synthetic positive example. Snapshot hash != exposure part hashes by design."
	return c
}

// ExamplePositiveValidMemoryControl is the valid-memory control skeleton.
func ExamplePositiveValidMemoryControl() CaseDocument {
	c := ExamplePositiveFailureCase()
	c.CaseID = "case-synth-valid-memory-control-001"
	c.Role = CaseRoleValidMemoryControl
	c.Eligibility.Rationale = "Synthetic control: valid memory constraint must survive the edit condition."
	c.Notes = "Control: Idempotency-Key constraint must remain after reviewed memory edit."
	return c
}

// ExamplePositiveNoRelevantMemoryControl is the no-relevant-memory control.
func ExamplePositiveNoRelevantMemoryControl() CaseDocument {
	c := ExamplePositiveFailureCase()
	c.CaseID = "case-synth-no-relevant-memory-001"
	c.Role = CaseRoleNoRelevantMemoryCtrl
	c.MemorySnapshot.RecordIDs = []string{}
	c.DeliveredContext.Parts = []DeliveredPart{
		{PartID: "part-system", Kind: "system", BytesSHA256: synthHashC, ByteLength: 64},
		{PartID: "part-user", Kind: "user", BytesSHA256: synthHashE, ByteLength: 48},
	}
	c.Eligibility.Rationale = "Synthetic control: task has no relevant memory; success must not require the edit."
	c.Notes = "Control: formatting-only task without relevant memory."
	return c
}

// ExamplePositiveConditions returns the three initial conditions for a case.
func ExamplePositiveConditions(caseID string) []ConditionDocument {
	orig := NewConditionDocument()
	orig.ConditionID = "cond-original-" + caseID
	orig.CaseID = caseID
	orig.Kind = ConditionOriginalMemory
	orig.HarnessUnchanged = true
	orig.RetrievalUnchanged = true

	none := NewConditionDocument()
	none.ConditionID = "cond-none-" + caseID
	none.CaseID = caseID
	none.Kind = ConditionNoMemory
	none.HarnessUnchanged = true
	none.RetrievalUnchanged = true

	edit := NewConditionDocument()
	edit.ConditionID = "cond-edit-" + caseID
	edit.CaseID = caseID
	edit.Kind = ConditionReviewedMemoryEdit
	edit.MemoryEditRef = "edit-synth-001"
	edit.EditReviewedBy = "synth-reviewer"
	edit.HarnessUnchanged = true
	edit.RetrievalUnchanged = true

	return []ConditionDocument{orig, none, edit}
}

// ExamplePositiveAttemptSucceeded is a successful attempt skeleton.
func ExamplePositiveAttemptSucceeded() AttemptDocument {
	a := NewAttemptDocument()
	a.AttemptID = "attempt-synth-ok-001"
	a.ConditionID = "cond-original-case-synth-failure-001"
	a.CaseID = "case-synth-failure-001"
	a.RepIndex = 0
	a.Status = AttemptSucceeded
	a.StartedAt = "2026-01-16T12:00:00Z"
	a.FinishedAt = "2026-01-16T12:05:00Z"
	a.TimeoutSeconds = 600
	a.CostUSDMicros = 0
	a.ProviderInvoked = true
	a.ResetObserved = true
	a.IsolationHeld = true
	return a
}

// ExamplePositiveOutcomePass pairs with a succeeded attempt.
func ExamplePositiveOutcomePass() OutcomeDocument {
	o := NewOutcomeDocument()
	o.OutcomeID = "outcome-synth-pass-001"
	o.AttemptID = "attempt-synth-ok-001"
	o.Kind = OutcomePass
	o.CheckerID = "oracle-checker-synth-v1"
	return o
}

// ExampleDeniedRunGate lacks permission / ceiling freeze — must reject before provider.
func ExampleDeniedRunGate() RunGate {
	g := NewRunGate()
	g.GateID = "gate-synth-denied-001"
	g.RunPermissionGranted = false
	g.MatrixFrozen = false
	g.MonetaryCeilingFrozen = false
	g.Policy = ExecutionPolicy{
		ResetRequired:         true,
		IsolationRequired:     true,
		ContentMinimization:   true,
		DefaultTimeoutSeconds: 600,
		Retry: RetryPolicy{
			MaxRetriesPerAttempt: 0,
			MissingRunPolicy:     "fail_closed",
		},
		Cost: CostAccounting{
			Currency:         "USD",
			SpentUSDMicros:   0,
			CeilingUSDMicros: 0, // invalid — no ceiling
			FailClosed:       true,
		},
		DefaultDeny: []string{DenyProductionWrites, DenyExternalActions, DenyLiveVaultAccess},
	}
	return g
}

// ExampleAuthorizedRunGate is structurally complete but still not real spend approval.
// Authorization here means schema-level gates pass; monetary approval is external.
func ExampleAuthorizedRunGate() RunGate {
	g := NewRunGate()
	g.GateID = "gate-synth-authorized-shape-001"
	g.RunPermissionGranted = true
	g.PermissionRecordRef = "permission/synth-record-001"
	g.MatrixFrozen = true
	g.MonetaryCeilingFrozen = true
	g.ApprovedBy = "synth-operator"
	g.Policy = ExecutionPolicy{
		ResetRequired:         true,
		IsolationRequired:     true,
		ContentMinimization:   true,
		DefaultTimeoutSeconds: 600,
		Retry: RetryPolicy{
			MaxRetriesPerAttempt: 1,
			RetryOnTimeout:       false,
			RetryOnUnavailable:   true,
			MissingRunPolicy:     "record_unavailable",
		},
		Cost: CostAccounting{
			Currency:           "USD",
			SpentUSDMicros:     0,
			CeilingUSDMicros:   1, // 1 micro-USD synthetic ceiling for shape tests
			FailClosed:         true,
			ProviderCalls:      0,
			AccountingComplete: true,
		},
		DefaultDeny: []string{DenyProductionWrites, DenyExternalActions, DenyLiveVaultAccess},
	}
	g.Notes = "Shape-only authorized gate for schema tests. Does not authorize real paid runs."
	return g
}

// ExampleNegativeHiddenInRunner embeds oracle material in the runner package.
func ExampleNegativeHiddenInRunner() CaseDocument {
	c := ExamplePositiveFailureCase()
	c.CaseID = "case-synth-neg-hidden-runner"
	c.RunnerPackage.ContainsHiddenTests = true
	c.RunnerPackage.ContainsExpectedAnswers = true
	return c
}

// ExampleNegativeFidelityUpgrade claims faithful exposure from reconstruction snapshot.
func ExampleNegativeFidelityUpgrade() CaseDocument {
	c := ExamplePositiveFailureCase()
	c.CaseID = "case-synth-neg-fidelity-upgrade"
	c.MemorySnapshot.Fidelity = FidelityReconstruction
	c.DeliveredContext.ExposureAvailability = ExposurePresent
	c.DeliveredContext.Fidelity = FidelityFaithfulHistorical
	c.DeliveredContext.Synthetic = false
	c.DeliveredContext.MissingReason = ""
	return c
}

// ExampleNegativeMissingExposureClaimsFaithful marks missing exposure as faithful.
func ExampleNegativeMissingExposureClaimsFaithful() DeliveredContext {
	return DeliveredContext{
		ManifestID:           "exposure-synth-neg-missing",
		ExposureAvailability: ExposureMissing,
		Fidelity:             FidelityFaithfulHistorical,
		Parts:                []DeliveredPart{},
		MissingReason:        "bytes not captured",
		Synthetic:            true,
	}
}

// ExampleNegativeAttemptStatusConfusion labels a skip as succeeded.
func ExampleNegativeAttemptStatusConfusion() AttemptDocument {
	a := ExamplePositiveAttemptSucceeded()
	a.AttemptID = "attempt-synth-neg-status"
	a.Status = AttemptSucceeded
	a.SkipReason = "should not succeed with skip reason"
	a.ProviderInvoked = false
	return a
}

// ExampleSkippedAttempt is a properly skipped attempt (no provider invoke).
func ExampleSkippedAttempt() AttemptDocument {
	a := NewAttemptDocument()
	a.AttemptID = "attempt-synth-skipped-001"
	a.ConditionID = "cond-original-case-synth-failure-001"
	a.CaseID = "case-synth-failure-001"
	a.RepIndex = 1
	a.Status = AttemptSkipped
	a.FinishedAt = "2026-01-16T12:00:00Z"
	a.TimeoutSeconds = 600
	a.ProviderInvoked = false
	a.SkipReason = "run gate denied"
	a.ResetObserved = false
	a.IsolationHeld = true
	return a
}

// ExampleTimedOutAttempt is a properly timed-out attempt.
func ExampleTimedOutAttempt() AttemptDocument {
	a := NewAttemptDocument()
	a.AttemptID = "attempt-synth-timeout-001"
	a.ConditionID = "cond-original-case-synth-failure-001"
	a.CaseID = "case-synth-failure-001"
	a.RepIndex = 2
	a.Status = AttemptTimedOut
	a.StartedAt = "2026-01-16T13:00:00Z"
	a.FinishedAt = "2026-01-16T13:10:00Z"
	a.TimeoutSeconds = 600
	a.ProviderInvoked = true
	a.ErrorCode = "timeout"
	a.ResetObserved = true
	a.IsolationHeld = true
	return a
}

// ExampleUnavailableAttempt is a properly unavailable attempt.
func ExampleUnavailableAttempt() AttemptDocument {
	a := NewAttemptDocument()
	a.AttemptID = "attempt-synth-unavailable-001"
	a.ConditionID = "cond-original-case-synth-failure-001"
	a.CaseID = "case-synth-failure-001"
	a.RepIndex = 3
	a.Status = AttemptUnavailable
	a.FinishedAt = "2026-01-16T14:00:00Z"
	a.TimeoutSeconds = 600
	a.ProviderInvoked = false
	a.SkipReason = "provider capacity unavailable"
	a.IsolationHeld = true
	return a
}

// ExampleFailedAttempt is a provider-invoked failure (not a skip).
func ExampleFailedAttempt() AttemptDocument {
	a := NewAttemptDocument()
	a.AttemptID = "attempt-synth-failed-001"
	a.ConditionID = "cond-original-case-synth-failure-001"
	a.CaseID = "case-synth-failure-001"
	a.RepIndex = 4
	a.Status = AttemptFailed
	a.StartedAt = "2026-01-16T14:00:00Z"
	a.FinishedAt = "2026-01-16T14:03:00Z"
	a.TimeoutSeconds = 600
	a.ProviderInvoked = true
	a.ErrorCode = "harness_error"
	a.ResetObserved = true
	a.IsolationHeld = true
	return a
}

// ExamplePositiveReport is a contract-level report that does not claim efficacy.
func ExamplePositiveReport() ReportDocument {
	r := NewReportDocument()
	r.ReportID = "report-synth-001"
	r.CaseIDs = []string{"case-synth-failure-001", "case-synth-valid-memory-control-001", "case-synth-no-relevant-memory-001"}
	r.ConditionIDs = []string{"cond-original", "cond-none", "cond-edit"}
	r.AttemptIDs = []string{}
	r.OutcomeIDs = []string{}
	r.Matrix = DefaultPlanningMatrix()
	r.Cost = CostAccounting{
		Currency:           "USD",
		SpentUSDMicros:     0,
		CeilingUSDMicros:   1,
		FailClosed:         true,
		AccountingComplete: true,
	}
	r.AccessTable = DefaultAccessTable()
	r.Assumptions = DefaultIsolationAssumptions()
	r.Limitations = []string{
		"Schema validation does not qualify a real case.",
		"Schema validation does not authorize paid model execution.",
		"Synthetic fixtures cannot support historical attribution.",
	}
	r.ClaimsEfficacy = false
	return r
}

// BuildContractReceipt hashes the synthetic positive examples into a receipt.
func BuildContractReceipt(generatedAt string, testsPassed bool) (ContractReceipt, error) {
	r := NewContractReceipt()
	r.ReceiptID = "receipt-synth-contract-v1"
	r.GeneratedAt = generatedAt
	r.TestsPassed = testsPassed
	r.ClaimsEfficacy = false
	r.UnresolvedLimits = []string{
		"Real case qualification is owned by #542 and is out of scope for this contract package.",
		"Runner/oracle implementations are owned by later issues (#543+).",
		"Actual matrix and monetary ceiling must freeze before any provider invocation.",
		"Reference-memory ceiling remains future optional work.",
		"Provider version was missing in synthetic harness fixtures by design.",
	}
	r.Notes = "Offline contract receipt. Does not claim downstream efficacy."

	examples := map[string]any{
		"positive_failure_case":         ExamplePositiveFailureCase(),
		"positive_valid_memory_control": ExamplePositiveValidMemoryControl(),
		"positive_no_relevant_memory":   ExamplePositiveNoRelevantMemoryControl(),
		"positive_conditions":           ExamplePositiveConditions("case-synth-failure-001"),
		"positive_attempt_succeeded":    ExamplePositiveAttemptSucceeded(),
		"positive_outcome_pass":         ExamplePositiveOutcomePass(),
		"positive_report":               ExamplePositiveReport(),
		"authorized_run_gate_shape":     ExampleAuthorizedRunGate(),
		"denied_run_gate":               ExampleDeniedRunGate(),
		"default_access_table":          DefaultAccessTable(),
		"default_planning_matrix":       DefaultPlanningMatrix(),
	}
	for name, ex := range examples {
		h, err := CanonicalJSONDigest(ex)
		if err != nil {
			return ContractReceipt{}, err
		}
		r.ExampleHashes[name] = h
	}
	return r, nil
}
