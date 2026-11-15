package report

import (
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

func TestPlanRejectsMissingCeilings(t *testing.T) {
	p := NewComparisonPlan("plan-missing")
	// No ceilings set.
	if err := p.Validate(); err == nil {
		t.Fatal("expected missing spend ceiling rejection")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeMissingCeiling {
		t.Fatalf("want %s, got %v", CodeMissingCeiling, err)
	}

	p.SpendCeilingUSDMicros = 100
	if err := p.Validate(); err == nil {
		t.Fatal("expected missing run ceiling")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeMissingCeiling {
		t.Fatalf("want %s for run, got %v", CodeMissingCeiling, err)
	}

	p.RunCeiling = 45
	if err := p.Validate(); err == nil {
		t.Fatal("expected missing time ceiling")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeMissingCeiling {
		t.Fatalf("want %s for time, got %v", CodeMissingCeiling, err)
	}
}

func TestPlanResolveAgainstSpendCeiling(t *testing.T) {
	p := SyntheticComparisonPlan()
	if err := p.ResolveAgainstCeilings(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.ResolvedMaxRuns != contract.PlanningMaxPlannedRuns {
		t.Fatalf("ResolvedMaxRuns=%d want %d", p.ResolvedMaxRuns, contract.PlanningMaxPlannedRuns)
	}
	if p.IsApprovedSpend {
		t.Fatal("planning default must not be approved spend")
	}

	// Over-budget estimate must reject.
	p.EstimatedCostPerRunUSDMicros = 1
	p.SpendCeilingUSDMicros = 10 // 45*1 > 10
	if err := p.Validate(); err == nil {
		t.Fatal("expected spend ceiling rejection when estimate exceeds ceiling")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeSpendCeiling {
		t.Fatalf("want %s, got %v", CodeSpendCeiling, err)
	}
}

func TestPlanRunCeilingCapsMatrix(t *testing.T) {
	p := SyntheticComparisonPlan()
	p.RunCeiling = 10 // less than 45
	if err := p.Validate(); err == nil {
		t.Fatal("matrix 45 must exceed run ceiling 10")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeRunCeiling {
		t.Fatalf("want %s, got %v", CodeRunCeiling, err)
	}
}

func TestAuthorizeRealComparisonFailsClosed(t *testing.T) {
	p := SyntheticComparisonPlan()
	if err := p.AuthorizeRealComparison(); err == nil {
		t.Fatal("public synthetic plan must not authorize real comparison")
	} else if e, ok := err.(*Error); !ok || e.Code != CodePrivateEligibility {
		t.Fatalf("want %s, got %v", CodePrivateEligibility, err)
	}

	p.PrivateEligibilityReady = true
	if err := p.AuthorizeRealComparison(); err == nil {
		t.Fatal("must still reject without spend authorization ref")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeUnauthorizedCompare {
		t.Fatalf("want %s, got %v", CodeUnauthorizedCompare, err)
	}
}

func TestAssembleKeepsFailedAndMissingVisible(t *testing.T) {
	plan := SyntheticComparisonPlan()
	scorer := SyntheticScorer()
	trials := SyntheticControlBundle()

	rep, err := Assemble("report-synth-545-001", plan, scorer, trials)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if rep.ClaimsEfficacy {
		t.Fatal("claims_efficacy must be false")
	}
	if rep.Counts.MissingTrials < 1 {
		t.Fatalf("expected missing trials counted, got %+v", rep.Counts)
	}
	if rep.Counts.NegativeTrials < 1 {
		t.Fatalf("expected negatives preserved, got %+v", rep.Counts)
	}
	for _, tr := range rep.Trials {
		if isFailedOrMissing(tr) || isNegative(tr) {
			if !tr.Visible {
				t.Fatalf("trial %s must remain visible", tr.TrialID)
			}
		}
	}

	// Hidden failed trial must reject.
	hidden := FixtureFailureMissing()
	hidden.Visible = false
	if _, err := Assemble("report-hidden", plan, scorer, []TrialRecord{hidden}); err == nil {
		t.Fatal("hidden missing trial must be rejected")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeMissingTrial {
		t.Fatalf("want %s, got %v", CodeMissingTrial, err)
	}
}

func TestAssembleDistinguishesCaseCountFromRepetition(t *testing.T) {
	plan := SyntheticComparisonPlan()
	scorer := SyntheticScorer()
	// Same case, two reps.
	a := FixtureFailureOriginal()
	b := FixtureFailureOriginal()
	b.TrialID = "trial-synth-failure-orig-002"
	b.RepIndex = 1
	rep, err := Assemble("report-reps", plan, scorer, []TrialRecord{a, b})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if rep.Counts.CaseCount != 1 {
		t.Fatalf("case_count=%d want 1", rep.Counts.CaseCount)
	}
	if rep.Counts.RepetitionCount != 2 {
		t.Fatalf("repetition_count=%d want 2", rep.Counts.RepetitionCount)
	}
	if rep.Counts.CaseCount == rep.Counts.RepetitionCount {
		t.Fatal("case count must not equal repetition count for multi-rep single case")
	}
	ids := DistinctCaseIDs(rep.Trials)
	if len(ids) != 1 || ids[0] != a.CaseID {
		t.Fatalf("distinct cases=%v", ids)
	}
}

func TestAssembleRejectsScorerMismatch(t *testing.T) {
	plan := SyntheticComparisonPlan()
	scorer := SyntheticScorer()
	bad := FixtureScorerMismatch()
	if _, err := Assemble("report-mismatch", plan, scorer, []TrialRecord{bad}); err == nil {
		t.Fatal("scorer digest mismatch must reject")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeHashMismatch {
		t.Fatalf("want %s, got %v", CodeHashMismatch, err)
	}

	ver := FixtureFailureOriginal()
	ver.CheckerVersion = "9.9.9-wrong"
	if _, err := Assemble("report-ver", plan, scorer, []TrialRecord{ver}); err == nil {
		t.Fatal("scorer version mismatch must reject")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeScorerMismatch {
		t.Fatalf("want %s, got %v", CodeScorerMismatch, err)
	}
}

func TestDamageGateOnConstraintDrop(t *testing.T) {
	drop := FixtureEditDamageDrop()
	res := EvaluateDamageGate(drop)
	if !res.Failed {
		t.Fatal("constraint drop must fail damage gate")
	}

	ok := FixtureValidMemoryPreserved()
	resOK := EvaluateDamageGate(ok)
	if resOK.Failed {
		t.Fatalf("preserved constraint must pass damage gate: %s", resOK.Reason)
	}

	// Apply mutates trial.
	t2 := FixtureFailureOriginal()
	t2.ConditionKind = contract.ConditionReviewedMemoryEdit
	t2.ConstraintPreserved = boolPtr(false)
	t2.TaskSuccess = boolPtr(true)
	t2.OutcomeKind = contract.OutcomePass
	applied := ApplyDamageGate(&t2)
	if !applied.Failed || !t2.DamageGateFailed || !t2.Visible {
		t.Fatalf("ApplyDamageGate did not stamp failure/visibility: %+v %+v", applied, t2)
	}
	if t2.OutcomeKind != contract.OutcomeFail {
		t.Fatalf("outcome=%s want fail", t2.OutcomeKind)
	}
}

func TestBothControlsPresentInBundle(t *testing.T) {
	bundle := SyntheticControlBundle()
	var havePreserve, haveNRM bool
	for _, tr := range bundle {
		if tr.CaseRole == contract.CaseRoleValidMemoryControl && tr.ConstraintPreserved != nil && *tr.ConstraintPreserved {
			havePreserve = true
		}
		if tr.CaseRole == contract.CaseRoleNoRelevantMemoryCtrl && tr.NoRelevantMemoryOK != nil && *tr.NoRelevantMemoryOK {
			haveNRM = true
		}
	}
	if !havePreserve || !haveNRM {
		t.Fatalf("bundle must include both controls: preserve=%v nrm=%v", havePreserve, haveNRM)
	}
}

func TestPublicMethodSummaryClaimsEfficacyFalse(t *testing.T) {
	s := NewPublicMethodSummary(time.Date(2026, 9, 29, 7, 0, 0, 0, time.UTC))
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if s.ClaimsEfficacy || s.PlanningIsApprovedSpend || s.RealRunsExecuted || s.SpendAuthorized {
		t.Fatalf("public flags must stay false: %+v", s)
	}
	md := s.RenderMarkdown()
	for _, needle := range []string{"#545", "Claims efficacy:** false", "Adit", "≤45", "Private eligibility:** `pending`"} {
		if !strings.Contains(md, needle) {
			t.Errorf("markdown must disclose %q", needle)
		}
	}

	s.ClaimsEfficacy = true
	if err := s.Validate(); err == nil {
		t.Fatal("claims_efficacy=true must reject")
	}
}

func TestToContractReportNoEfficacy(t *testing.T) {
	plan := SyntheticComparisonPlan()
	scorer := SyntheticScorer()
	rep, err := Assemble("report-synth-contract-001", plan, scorer, SyntheticControlBundle())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	doc := rep.ToContractReport()
	if err := doc.Validate(); err != nil {
		t.Fatalf("contract report validate: %v", err)
	}
	if doc.ClaimsEfficacy {
		t.Fatal("contract projection must not claim efficacy")
	}
	if doc.Cost.CeilingUSDMicros != plan.SpendCeilingUSDMicros {
		t.Fatalf("cost ceiling=%d want %d", doc.Cost.CeilingUSDMicros, plan.SpendCeilingUSDMicros)
	}
}

func TestNegativesPreservedInCounts(t *testing.T) {
	plan := SyntheticComparisonPlan()
	scorer := SyntheticScorer()
	rep, err := Assemble("report-neg", plan, scorer, []TrialRecord{
		FixtureValidMemoryDropped(),
		FixtureNoRelevantMemoryNegative(),
		FixtureEditDamageDrop(),
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if rep.Counts.NegativeTrials < 3 {
		t.Fatalf("expected ≥3 negatives, got %+v", rep.Counts)
	}
	if rep.Counts.DamageGateFails < 2 {
		t.Fatalf("expected ≥2 damage-gate fails, got %+v", rep.Counts)
	}
}

func TestApprovedSpendWithoutAuthRefRejected(t *testing.T) {
	p := SyntheticComparisonPlan()
	p.IsApprovedSpend = true
	p.SpendAuthorizationRef = ""
	if err := p.Validate(); err == nil {
		t.Fatal("is_approved_spend without auth ref must reject")
	} else if e, ok := err.(*Error); !ok || e.Code != CodeUnauthorizedCompare {
		t.Fatalf("want %s, got %v", CodeUnauthorizedCompare, err)
	}
}
