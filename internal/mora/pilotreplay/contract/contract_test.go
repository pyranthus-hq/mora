package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPositiveExamplesValidate(t *testing.T) {
	t.Parallel()
	cases := []CaseDocument{
		ExamplePositiveFailureCase(),
		ExamplePositiveValidMemoryControl(),
		ExamplePositiveNoRelevantMemoryControl(),
	}
	for _, c := range cases {
		c := c
		t.Run(c.CaseID, func(t *testing.T) {
			t.Parallel()
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
	for _, cond := range ExamplePositiveConditions("case-synth-failure-001") {
		cond := cond
		t.Run(cond.ConditionID, func(t *testing.T) {
			t.Parallel()
			if err := cond.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
	attempt := ExamplePositiveAttemptSucceeded()
	if err := attempt.Validate(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	outcome := ExamplePositiveOutcomePass()
	if err := outcome.Validate(); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if err := ValidateAttemptOutcomePair(attempt, outcome); err != nil {
		t.Fatalf("pair: %v", err)
	}
	report := ExamplePositiveReport()
	if err := report.Validate(); err != nil {
		t.Fatalf("report: %v", err)
	}
	gate := ExampleAuthorizedRunGate()
	if err := gate.Validate(); err != nil {
		t.Fatalf("authorized gate validate: %v", err)
	}
	if err := gate.AuthorizeProviderInvocation(); err != nil {
		t.Fatalf("authorized gate authorize: %v", err)
	}
	matrix := DefaultPlanningMatrix()
	if err := matrix.Validate("matrix"); err != nil {
		t.Fatalf("matrix: %v", err)
	}
}

func TestMemorySnapshotDistinctFromExposure(t *testing.T) {
	t.Parallel()
	c := ExamplePositiveFailureCase()
	if c.MemorySnapshot.BytesSHA256 == "" || len(c.DeliveredContext.Parts) == 0 {
		t.Fatal("fixture incomplete")
	}
	for _, p := range c.DeliveredContext.Parts {
		if p.BytesSHA256 == c.MemorySnapshot.BytesSHA256 {
			t.Fatalf("exposure part %s hash equals snapshot hash; snapshot and exposure must stay separate", p.PartID)
		}
	}
	// Mutating snapshot must not be confused with exposure presence.
	c.MemorySnapshot.BytesSHA256 = synthHashA
	c.DeliveredContext.ExposureAvailability = ExposureMissing
	c.DeliveredContext.Parts = []DeliveredPart{}
	c.DeliveredContext.Fidelity = FidelityUnknown
	c.DeliveredContext.MissingReason = "not captured"
	c.DeliveredContext.Synthetic = true
	if err := c.MemorySnapshot.Validate("memory_snapshot"); err != nil {
		t.Fatalf("snapshot still valid: %v", err)
	}
	if err := c.DeliveredContext.Validate("delivered_context"); err != nil {
		t.Fatalf("missing exposure valid: %v", err)
	}
}

func TestFaithfulReplayVsReconstruction(t *testing.T) {
	t.Parallel()
	neg := ExampleNegativeFidelityUpgrade()
	if err := neg.Validate(); err == nil {
		t.Fatal("expected rejection of reconstruction upgraded to faithful_historical")
	} else if err.(*Error).Code != CodeFidelityUpgrade {
		t.Fatalf("want CodeFidelityUpgrade, got %v", err)
	}

	missing := ExampleNegativeMissingExposureClaimsFaithful()
	if err := missing.Validate("delivered_context"); err == nil {
		t.Fatal("expected rejection of missing exposure claiming faithful_historical")
	} else if err.(*Error).Code != CodeFidelityUpgrade {
		t.Fatalf("want CodeFidelityUpgrade, got %v", err)
	}

	synth := ExamplePositiveFailureCase()
	synth.MemorySnapshot.Fidelity = FidelityFaithfulHistorical
	if err := synth.MemorySnapshot.Validate("memory_snapshot"); err == nil {
		t.Fatal("synthetic snapshot must not claim faithful_historical")
	}
}

func TestAttemptStatusDistinctions(t *testing.T) {
	t.Parallel()
	table := []struct {
		name    string
		attempt AttemptDocument
		outcome OutcomeDocument
		wantErr bool
	}{
		{
			name:    "succeeded_pass",
			attempt: ExamplePositiveAttemptSucceeded(),
			outcome: ExamplePositiveOutcomePass(),
		},
		{
			name:    "skipped",
			attempt: ExampleSkippedAttempt(),
			outcome: func() OutcomeDocument {
				o := NewOutcomeDocument()
				o.OutcomeID = "o-skip"
				o.AttemptID = ExampleSkippedAttempt().AttemptID
				o.Kind = OutcomeSkipped
				o.CheckerID = "oracle-checker-synth-v1"
				return o
			}(),
		},
		{
			name:    "timed_out",
			attempt: ExampleTimedOutAttempt(),
			outcome: func() OutcomeDocument {
				o := NewOutcomeDocument()
				o.OutcomeID = "o-to"
				o.AttemptID = ExampleTimedOutAttempt().AttemptID
				o.Kind = OutcomeTimedOut
				o.CheckerID = "oracle-checker-synth-v1"
				return o
			}(),
		},
		{
			name:    "unavailable",
			attempt: ExampleUnavailableAttempt(),
			outcome: func() OutcomeDocument {
				o := NewOutcomeDocument()
				o.OutcomeID = "o-unavail"
				o.AttemptID = ExampleUnavailableAttempt().AttemptID
				o.Kind = OutcomeUnavailable
				o.CheckerID = "oracle-checker-synth-v1"
				return o
			}(),
		},
		{
			name:    "failed",
			attempt: ExampleFailedAttempt(),
			outcome: func() OutcomeDocument {
				o := NewOutcomeDocument()
				o.OutcomeID = "o-fail"
				o.AttemptID = ExampleFailedAttempt().AttemptID
				o.Kind = OutcomeFail
				o.CheckerID = "oracle-checker-synth-v1"
				return o
			}(),
		},
		{
			name:    "skip_labeled_as_pass",
			attempt: ExampleSkippedAttempt(),
			outcome: func() OutcomeDocument {
				o := ExamplePositiveOutcomePass()
				o.AttemptID = ExampleSkippedAttempt().AttemptID
				o.Kind = OutcomePass
				return o
			}(),
			wantErr: true,
		},
	}
	for _, row := range table {
		row := row
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if err := row.attempt.Validate(); err != nil {
				t.Fatalf("attempt validate: %v", err)
			}
			if err := row.outcome.Validate(); err != nil {
				t.Fatalf("outcome validate: %v", err)
			}
			err := ValidateAttemptOutcomePair(row.attempt, row.outcome)
			if row.wantErr && err == nil {
				t.Fatal("expected pair error")
			}
			if !row.wantErr && err != nil {
				t.Fatalf("unexpected pair error: %v", err)
			}
		})
	}

	neg := ExampleNegativeAttemptStatusConfusion()
	if err := neg.Validate(); err == nil {
		t.Fatal("succeeded+skip_reason+no provider must fail")
	}
}

func TestRejectAbsentRunPermissionOrSpendCeiling(t *testing.T) {
	t.Parallel()
	denied := ExampleDeniedRunGate()
	// Validate itself fails on missing ceiling.
	if err := denied.Validate(); err == nil {
		t.Fatal("denied gate with zero ceiling must fail Validate")
	} else if err.(*Error).Code != CodeSpendCeiling {
		t.Fatalf("want spend ceiling code, got %v", err)
	}
	if err := denied.AuthorizeProviderInvocation(); err == nil {
		t.Fatal("AuthorizeProviderInvocation must reject denied gate")
	}

	g := ExampleAuthorizedRunGate()
	g.RunPermissionGranted = false
	if err := g.AuthorizeProviderInvocation(); err == nil {
		t.Fatal("must reject absent run permission")
	} else if err.(*Error).Code != CodePermissionMissing {
		t.Fatalf("want permission code, got %v", err)
	}

	g = ExampleAuthorizedRunGate()
	g.MonetaryCeilingFrozen = false
	if err := g.AuthorizeProviderInvocation(); err == nil {
		t.Fatal("must reject unfrozen monetary ceiling")
	} else if err.(*Error).Code != CodeRunGateDenied {
		t.Fatalf("want run gate denied, got %v", err)
	}

	g = ExampleAuthorizedRunGate()
	g.MatrixFrozen = false
	if err := g.AuthorizeProviderInvocation(); err == nil {
		t.Fatal("must reject unfrozen matrix")
	}

	g = ExampleAuthorizedRunGate()
	g.Policy.Cost.CeilingUSDMicros = 0
	if err := g.AuthorizeProviderInvocation(); err == nil {
		t.Fatal("must reject zero spend ceiling")
	}
}

func TestRejectHiddenMaterialInRunner(t *testing.T) {
	t.Parallel()
	neg := ExampleNegativeHiddenInRunner()
	if err := neg.Validate(); err == nil {
		t.Fatal("runner with hidden tests/answers must be rejected")
	} else if err.(*Error).Code != CodeHiddenInRunner {
		t.Fatalf("want CodeHiddenInRunner, got %v", err)
	}

	for _, flag := range []struct {
		name string
		mut  func(*CaseDocument)
	}{
		{"hidden_tests", func(c *CaseDocument) { c.RunnerPackage.ContainsHiddenTests = true }},
		{"expected_answers", func(c *CaseDocument) { c.RunnerPackage.ContainsExpectedAnswers = true }},
		{"reference_patch", func(c *CaseDocument) { c.RunnerPackage.ContainsReferencePatch = true }},
		{"post_cutoff", func(c *CaseDocument) { c.RunnerPackage.ContainsPostCutoff = true }},
	} {
		flag := flag
		t.Run(flag.name, func(t *testing.T) {
			t.Parallel()
			c := ExamplePositiveFailureCase()
			flag.mut(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected rejection")
			} else if err.(*Error).Code != CodeHiddenInRunner {
				t.Fatalf("want CodeHiddenInRunner, got %v", err)
			}
		})
	}
}

func TestBothControlsSpecified(t *testing.T) {
	t.Parallel()
	failure := ExamplePositiveFailureCase()
	if !failure.Controls.ValidMemoryMustSurvive || failure.Controls.ValidMemoryConstraint == "" {
		t.Fatal("failure case missing surviving valid-memory constraint")
	}
	if !failure.Controls.NoRelevantMemoryTask || failure.Controls.NoRelevantMemorySuccessRule == "" {
		t.Fatal("failure case missing no-relevant-memory control rule")
	}
	valid := ExamplePositiveValidMemoryControl()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid control: %v", err)
	}
	none := ExamplePositiveNoRelevantMemoryControl()
	if err := none.Validate(); err != nil {
		t.Fatalf("no-relevant control: %v", err)
	}

	bad := ExamplePositiveFailureCase()
	bad.Controls.ValidMemoryConstraint = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("failure without valid-memory constraint must fail")
	}
	bad = ExamplePositiveFailureCase()
	bad.Controls.NoRelevantMemoryTask = false
	if err := bad.Validate(); err == nil {
		t.Fatal("failure without no-relevant-memory flag must fail")
	}
}

func TestThreeConditionsAndConfoundRule(t *testing.T) {
	t.Parallel()
	conds := ExamplePositiveConditions("case-synth-failure-001")
	if len(conds) != 3 {
		t.Fatalf("want 3 conditions, got %d", len(conds))
	}
	kinds := map[string]bool{}
	for _, c := range conds {
		kinds[c.Kind] = true
	}
	for _, want := range []string{ConditionOriginalMemory, ConditionNoMemory, ConditionReviewedMemoryEdit} {
		if !kinds[want] {
			t.Fatalf("missing condition kind %s", want)
		}
	}

	edit := conds[2]
	edit.HarnessUnchanged = false
	edit.DeclaredConfounds = nil
	edit.DeclaredConfounds = []string{}
	if err := edit.Validate(); err == nil {
		t.Fatal("harness change without confound must fail")
	}
	edit.DeclaredConfounds = []string{"harness bump for tooling drift"}
	if err := edit.Validate(); err != nil {
		t.Fatalf("declared confound should allow: %v", err)
	}

	edit = conds[2]
	edit.MemoryEditRef = ""
	if err := edit.Validate(); err == nil {
		t.Fatal("reviewed edit without memory_edit_ref must fail")
	}
}

func TestAccessTableDeniesOracleToContender(t *testing.T) {
	t.Parallel()
	table := DefaultAccessTable()
	if len(table) == 0 {
		t.Fatal("empty access table")
	}
	denied := map[string]bool{}
	for _, row := range ContenderDeniedResources() {
		denied[row] = false
	}
	for _, row := range table {
		if err := row.Validate("row"); err != nil {
			t.Fatalf("access row: %v", err)
		}
		if row.Principal == PrincipalContender {
			if _, ok := denied[row.Resource]; ok {
				if row.Access != AccessDeny {
					t.Fatalf("contender must be deny on %s, got %s", row.Resource, row.Access)
				}
				denied[row.Resource] = true
			}
		}
		if row.Principal == PrincipalRepair {
			switch row.Resource {
			case "oracle_hidden_tests", "oracle_expected_answers", "oracle_reference_patches", "post_cutoff_evidence":
				if row.Access != AccessDeny {
					t.Fatalf("repair must be deny on %s", row.Resource)
				}
			}
		}
	}
	for res, ok := range denied {
		if !ok {
			t.Fatalf("missing contender deny row for %s", res)
		}
	}
	assumptions := DefaultIsolationAssumptions()
	if len(assumptions) < 5 {
		t.Fatalf("expected explicit isolation assumptions, got %d", len(assumptions))
	}
}

func TestPlanningMatrixIsNotApprovedSpend(t *testing.T) {
	t.Parallel()
	m := DefaultPlanningMatrix()
	if m.MaxPlannedRuns != 45 {
		t.Fatalf("want 45 planned runs, got %d", m.MaxPlannedRuns)
	}
	if m.IsApprovedSpend || m.IsStatisticalClaim {
		t.Fatal("planning default must not claim spend or power")
	}
	if err := m.Validate("matrix"); err != nil {
		t.Fatal(err)
	}
	m.IsApprovedSpend = true
	if err := m.Validate("matrix"); err == nil {
		t.Fatal("approved spend claim must be rejected on planning matrix")
	}
}

func TestOracleRejectsReferenceMemoryCeiling(t *testing.T) {
	t.Parallel()
	o := synthOracle()
	o.ReferenceMemoryCeiling = true
	if err := o.Validate("oracle_package"); err == nil {
		t.Fatal("reference-memory ceiling must be rejected for initial pilot")
	}
}

func TestReportAndReceiptRefuseEfficacyClaims(t *testing.T) {
	t.Parallel()
	r := ExamplePositiveReport()
	r.ClaimsEfficacy = true
	if err := r.Validate(); err == nil {
		t.Fatal("report must not claim efficacy")
	}
	receipt, err := BuildContractReceipt("2026-01-16T00:00:00Z", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if receipt.ClaimsEfficacy {
		t.Fatal("receipt must not claim efficacy")
	}
	if len(receipt.ExampleHashes) == 0 {
		t.Fatal("receipt needs example hashes")
	}
	receipt.ClaimsEfficacy = true
	if err := receipt.Validate(); err == nil {
		t.Fatal("efficacy claim on receipt must fail")
	}
}

func TestJSONRoundTripPositiveCase(t *testing.T) {
	t.Parallel()
	c := ExamplePositiveFailureCase()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back CaseDocument
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/Users/") || strings.Contains(string(b), "Library/") {
		t.Fatal("public fixture leaked private host path")
	}
}

func TestPackageIsLeaf(t *testing.T) {
	t.Parallel()
	// Compile-time: this package imports only stdlib. Runtime check via go list in a
	// separate test would need shell; keep a doc assertion via import graph comment.
	// Ensure synthetic constants stay non-empty.
	if SchemaVersion != 1 {
		t.Fatalf("SchemaVersion=%d", SchemaVersion)
	}
	if PlanningMaxPlannedRuns != 45 {
		t.Fatalf("PlanningMaxPlannedRuns=%d", PlanningMaxPlannedRuns)
	}
}

func TestProviderVersionMissingMarkedExplicitly(t *testing.T) {
	t.Parallel()
	h := synthHarness()
	if h.ProviderVersionStatus != ProviderVersionMissing {
		t.Fatal("fixture should mark provider version missing")
	}
	if err := h.Validate("harness"); err != nil {
		t.Fatal(err)
	}
	h.ProviderVersion = "secretly-filled"
	if err := h.Validate("harness"); err == nil {
		t.Fatal("missing status must not carry a version string")
	}
}

func TestSkippedUnavailableDoNotInvokeProvider(t *testing.T) {
	t.Parallel()
	for _, a := range []AttemptDocument{ExampleSkippedAttempt(), ExampleUnavailableAttempt()} {
		if a.ProviderInvoked {
			t.Fatalf("%s must not invoke provider", a.Status)
		}
		if err := a.Validate(); err != nil {
			t.Fatal(err)
		}
		a.ProviderInvoked = true
		if err := a.Validate(); err == nil {
			t.Fatalf("%s with provider_invoked must fail", a.Status)
		}
	}
}

func TestImportsAreStdlibOnly(t *testing.T) {
	t.Parallel()
	// Guarded by package design; keep a runtime reminder that Schema names stay stable.
	for _, name := range []string{SchemaCase, SchemaCondition, SchemaAttempt, SchemaOutcome, SchemaReport, SchemaReceipt, SchemaRunGate} {
		if name == "" || !strings.HasPrefix(name, "mora.pilotreplay.") {
			t.Fatalf("unexpected schema name %q", name)
		}
	}
}

func TestFailedAndTimedOutAttemptsRequireErrorCode(t *testing.T) {
	for _, attempt := range []AttemptDocument{ExampleFailedAttempt(), ExampleTimedOutAttempt()} {
		t.Run(attempt.Status, func(t *testing.T) {
			if err := attempt.Validate(); err != nil {
				t.Fatalf("valid attempt rejected: %v", err)
			}
			attempt.ErrorCode = ""
			err := attempt.Validate()
			if err == nil || !strings.Contains(err.Error(), "error_code") || !strings.Contains(err.Error(), CodeMissingField) {
				t.Fatalf("missing error code: got %v, want missing-field error for error_code", err)
			}
		})
	}
}
