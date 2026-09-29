package oracle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

func newTestChecker(t *testing.T) *Checker {
	t.Helper()
	c, err := NewChecker(SyntheticHiddenSpec())
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	return c
}

func dimStatus(g GradeReport, dim string) string {
	for _, d := range g.Dimensions {
		if d.Dimension == dim {
			return d.Status
		}
	}
	return ""
}

func TestFrozenPolicyDigestStable(t *testing.T) {
	p := FrozenScoringPolicy()
	if p.ClaimsEfficacy {
		t.Fatal("policy must not claim efficacy")
	}
	if p.MissingOutputPolicy != PolicyInconclusiveNeverSuccess {
		t.Fatalf("missing policy: %s", p.MissingOutputPolicy)
	}
	d1, err := PolicyDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := PolicyDigest(FrozenScoringPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || d1 == "" {
		t.Fatalf("policy digest unstable: %q vs %q", d1, d2)
	}
	rec, err := FreezeChecker(SyntheticHiddenSpec())
	if err != nil {
		t.Fatal(err)
	}
	if rec.ClaimsEfficacy {
		t.Fatal("freeze must not claim efficacy")
	}
	cd := CheckerDigest(rec)
	if len(cd) != 64 {
		t.Fatalf("checker digest want 64 hex, got %d", len(cd))
	}
	t.Logf("frozen checker_digest=%s policy_digest=%s hidden_spec_digest=%s", cd, rec.PolicyDigest, rec.HiddenSpecDigest)
}

func TestFailsKnownFault(t *testing.T) {
	c := newTestChecker(t)
	g, err := c.Grade(GradeRequest{
		ReportID:  "grade-known-bad",
		AttemptID: "attempt-known-bad",
		Artifact:  ptr(FixtureKnownBad()),
		Hidden:    SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if g.OutcomeKind != contract.OutcomeFail {
		t.Fatalf("want fail, got %s dims=%v", g.OutcomeKind, g.Dimensions)
	}
	if dimStatus(g, DimFailedTask) != DimFail {
		t.Fatalf("failed_task_outcome want fail, got %s", dimStatus(g, DimFailedTask))
	}
	if g.ClaimsEfficacy {
		t.Fatal("must not claim efficacy")
	}
}

func TestPassesIndependentlyJustifiedExpected(t *testing.T) {
	c := newTestChecker(t)
	art := FixtureCorrect()
	g, err := c.Grade(GradeRequest{
		ReportID:  "grade-correct",
		AttemptID: "attempt-correct",
		Artifact:  &art,
		Hidden:    SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if g.OutcomeKind != contract.OutcomePass {
		t.Fatalf("want pass, got %s dims=%v", g.OutcomeKind, g.Dimensions)
	}
	for _, need := range []string{DimFailedTask, DimValidMemoryConstraint, DimUnrelatedRegression} {
		if dimStatus(g, need) != DimPass {
			t.Fatalf("%s want pass, got %s", need, dimStatus(g, need))
		}
	}
	if dimStatus(g, DimNoRelevantMemory) != DimNotApplicable {
		t.Fatalf("no-relevant-memory on failure role want N/A, got %s", dimStatus(g, DimNoRelevantMemory))
	}
}

func TestDetectsConstraintViolation(t *testing.T) {
	c := newTestChecker(t)
	art := FixtureConstraintDropping()
	g, err := c.Grade(GradeRequest{
		ReportID:  "grade-constraint-drop",
		AttemptID: "attempt-constraint-drop",
		Artifact:  &art,
		Hidden:    SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if g.OutcomeKind != contract.OutcomeFail {
		t.Fatalf("want fail, got %s", g.OutcomeKind)
	}
	if dimStatus(g, DimFailedTask) != DimPass {
		t.Fatalf("task should still pass markers; got %s", dimStatus(g, DimFailedTask))
	}
	if dimStatus(g, DimValidMemoryConstraint) != DimFail {
		t.Fatalf("constraint want fail, got %s", dimStatus(g, DimValidMemoryConstraint))
	}

	// Also on valid-memory control role.
	ctrl := FixtureConstraintDroppingControl()
	g2, err := c.Grade(GradeRequest{
		ReportID: "grade-constraint-drop-ctrl",
		Artifact: &ctrl,
		Hidden:   SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade ctrl: %v", err)
	}
	if dimStatus(g2, DimValidMemoryConstraint) != DimFail {
		t.Fatalf("control constraint want fail, got %s", dimStatus(g2, DimValidMemoryConstraint))
	}
}

func TestDetectsUnrelatedRegression(t *testing.T) {
	c := newTestChecker(t)
	art := FixtureRegressionBroken()
	g, err := c.Grade(GradeRequest{
		ReportID: "grade-regression",
		Artifact: &art,
		Hidden:   SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if g.OutcomeKind != contract.OutcomeFail {
		t.Fatalf("want fail, got %s", g.OutcomeKind)
	}
	if dimStatus(g, DimUnrelatedRegression) != DimFail {
		t.Fatalf("regression want fail, got %s", dimStatus(g, DimUnrelatedRegression))
	}
}

func TestNoRelevantMemoryControl(t *testing.T) {
	c := newTestChecker(t)
	ok := FixtureNoRelevantMemoryOK()
	g, err := c.Grade(GradeRequest{
		ReportID: "grade-nrm-ok",
		Artifact: &ok,
		Hidden:   SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if dimStatus(g, DimNoRelevantMemory) != DimPass {
		t.Fatalf("nrm want pass, got %s dims=%v", dimStatus(g, DimNoRelevantMemory), g.Dimensions)
	}
	if g.OutcomeKind != contract.OutcomePass {
		t.Fatalf("nrm overall want pass, got %s", g.OutcomeKind)
	}

	bad := FixtureNoRelevantMemoryRequiresEdit()
	g2, err := c.Grade(GradeRequest{
		ReportID: "grade-nrm-bad",
		Artifact: &bad,
		Hidden:   SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade bad: %v", err)
	}
	if dimStatus(g2, DimNoRelevantMemory) != DimFail {
		t.Fatalf("nrm-requires-edit want fail, got %s", dimStatus(g2, DimNoRelevantMemory))
	}
	if g2.OutcomeKind != contract.OutcomeFail {
		t.Fatalf("nrm-requires-edit overall want fail, got %s", g2.OutcomeKind)
	}
}

func TestAbsentArtifactNeverSuccess(t *testing.T) {
	c := newTestChecker(t)
	g, err := c.Grade(GradeRequest{
		ReportID: "grade-absent",
		Artifact: nil,
		Hidden:   SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if g.OutcomeKind != contract.OutcomeInconclusive {
		t.Fatalf("want inconclusive, got %s", g.OutcomeKind)
	}
	if g.Scored {
		t.Fatal("absent artifact must be unscored")
	}
	if g.OutcomeKind == contract.OutcomePass {
		t.Fatal("never success")
	}
	for _, d := range g.Dimensions {
		if d.Status == DimPass {
			t.Fatalf("dimension %s must not pass on absent artifact", d.Dimension)
		}
	}
}

func TestMalformedAndUnsupportedNeverSuccess(t *testing.T) {
	c := newTestChecker(t)
	for _, art := range []OutputArtifact{FixtureMalformed(), FixtureUnsupportedVersion()} {
		a := art
		g, err := c.Grade(GradeRequest{
			ReportID: "grade-malformed",
			Artifact: &a,
			Hidden:   SyntheticHiddenSpec(),
		})
		if err != nil {
			t.Fatalf("Grade: %v", err)
		}
		if g.OutcomeKind != contract.OutcomeInconclusive {
			t.Fatalf("%s: want inconclusive, got %s", a.ArtifactID, g.OutcomeKind)
		}
		if g.Scored {
			t.Fatalf("%s: must be unscored", a.ArtifactID)
		}
	}
}

func TestGraderErrorNeverSuccess(t *testing.T) {
	c := newTestChecker(t)
	art := FixtureCorrect()
	g, err := c.Grade(GradeRequest{
		ReportID:         "grade-grader-error",
		Artifact:         &art,
		Hidden:           SyntheticHiddenSpec(),
		ForceGraderError: true,
	})
	if err == nil {
		t.Fatal("expected grader error")
	}
	if g.OutcomeKind != contract.OutcomeError {
		t.Fatalf("want error, got %s", g.OutcomeKind)
	}
	if g.Scored {
		t.Fatal("grader error must be unscored")
	}
	if g.OutcomeKind == contract.OutcomePass {
		t.Fatal("never success")
	}
}

func TestEvaluatorEnvSeparateCopy(t *testing.T) {
	art := FixtureCorrect()
	env, err := PrepareEvaluatorEnv(t.TempDir(), art)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Close() }()
	if env.Root == "" || env.ArtifactPath == "" {
		t.Fatal("empty env paths")
	}
	b, err := os.ReadFile(env.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadArtifactJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ArtifactID != art.ArtifactID {
		t.Fatalf("id mismatch")
	}
	// Contender workspace simulation: ensure evaluator root is not under a
	// fake contender path we create beside it.
	contender := filepath.Join(t.TempDir(), "contender")
	if err := os.MkdirAll(contender, 0o755); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(env.Root, contender) {
		t.Fatal("evaluator env must not live under contender workspace")
	}
	body, err := os.ReadFile(filepath.Join(env.Root, "files", synthConstraintFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), synthConstraintToken) {
		t.Fatal("constraint file not materialized in evaluator env")
	}
}

func TestAccessBoundaryNoGoldLeak(t *testing.T) {
	caze := SyntheticCaseForBoundary()
	if err := caze.Validate(); err != nil {
		t.Fatalf("case validate: %v", err)
	}

	cv, err := BuildContenderView(caze)
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertNoGoldInJSON("contender", cv); err != nil {
		t.Fatal(err)
	}
	if len(cv.OracleHashRefs) != 0 {
		t.Fatal("contender view must omit oracle hash-ref bodies/lists")
	}
	for _, p := range cv.ContenderVisiblePaths {
		lp := strings.ToLower(p)
		for _, bad := range []string{"oracle", "gold", "hidden_test", "expected_answer", "reference_patch"} {
			if strings.Contains(lp, bad) {
				t.Fatalf("contender path %q looks like gold", p)
			}
		}
	}

	rv, err := BuildRepairView(caze)
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertNoGoldInJSON("repair", rv); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rv)
	if strings.Contains(string(raw), "hidden_spec") || strings.Contains(string(raw), "PRIVATE_GOLD") {
		t.Fatal("repair view leaked hidden_spec/gold")
	}

	stage, err := BuildRunnerOracleStaging(caze)
	if err != nil {
		t.Fatal(err)
	}
	if !stage.HashRefOnly || stage.ContentsMounted {
		t.Fatal("runner staging must be hash-ref only with contents_mounted=false")
	}

	// Grade and ensure AllowedFeedback is clean.
	checker := newTestChecker(t)
	art := FixtureCorrect()
	g, err := checker.Grade(GradeRequest{
		ReportID: "grade-boundary",
		Artifact: &art,
		Hidden:   SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fb := g.ToAllowedFeedback()
	if err := AssertAllowedFeedbackClean(fb); err != nil {
		t.Fatal(err)
	}
	fbJSON, _ := json.Marshal(fb)
	for _, bad := range []string{"Detail", "PRIVATE_GOLD", "hidden_spec", "known_fault marker"} {
		if strings.Contains(string(fbJSON), bad) {
			t.Fatalf("allowed feedback contains %q: %s", bad, fbJSON)
		}
	}

	// The oracle-specific deny list must retain every contract oracle/cutoff
	// resource. Live-vault, production-write and external-action restrictions
	// belong to the runner rather than this oracle projection.
	for _, r := range contract.ContenderDeniedResources() {
		if !strings.HasPrefix(r, "oracle_") && r != "post_cutoff_evidence" {
			continue
		}
		found := false
		for _, denied := range ContenderMustNotSee() {
			if r == denied {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("oracle contender deny list missing contract resource %q", r)
		}
	}
	if len(contract.ContenderDeniedResources()) == 0 {
		t.Fatal("contract contender deny list empty")
	}
}

func TestOutcomeDocumentPairing(t *testing.T) {
	checker := newTestChecker(t)
	art := FixtureCorrect()
	g, err := checker.Grade(GradeRequest{
		ReportID:  "grade-pair",
		AttemptID: "attempt-pair-001",
		Artifact:  &art,
		Hidden:    SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatal(err)
	}
	o := g.ToOutcomeDocument("outcome-pair-001", "attempt-pair-001")
	if err := o.Validate(); err != nil {
		t.Fatalf("outcome validate: %v", err)
	}
	attempt := contract.NewAttemptDocument()
	attempt.AttemptID = "attempt-pair-001"
	attempt.ConditionID = "cond-synth"
	attempt.CaseID = "case-synth"
	attempt.RepIndex = 0
	attempt.Status = contract.AttemptSucceeded
	attempt.StartedAt = "2026-09-01T12:00:00Z"
	attempt.FinishedAt = "2026-09-01T12:00:01Z"
	attempt.TimeoutSeconds = 60
	attempt.ResetObserved = true
	attempt.IsolationHeld = true
	if err := attempt.Validate(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if err := contract.ValidateAttemptOutcomePair(attempt, o); err != nil {
		t.Fatalf("pair: %v", err)
	}
}

func TestPublicHiddenSpecMustBeSynthetic(t *testing.T) {
	h := SyntheticHiddenSpec()
	h.Synthetic = false
	if err := h.Validate(); err == nil {
		t.Fatal("expected rejection of non-synthetic hidden spec in public package")
	}
}

func TestSanitizeStripsGoldKeys(t *testing.T) {
	a := FixtureCorrect()
	a.TaskMarkers["gold_secret"] = "PRIVATE_GOLD_MARKER"
	// ValidateShape should reject gold keys before sanitize path in Grade.
	if err := a.ValidateShape(); err == nil {
		t.Fatal("expected gold key rejection")
	}
	// Direct sanitize still strips.
	a2 := FixtureCorrect()
	a2.TaskMarkers["expected_answer_leak"] = "x"
	san := sanitizeArtifact(a2)
	if _, ok := san.TaskMarkers["expected_answer_leak"]; ok {
		t.Fatal("sanitize must strip gold-like keys")
	}
}

func TestValidMemoryPreservedPasses(t *testing.T) {
	c := newTestChecker(t)
	art := FixtureValidMemoryPreserved()
	g, err := c.Grade(GradeRequest{
		ReportID: "grade-vm-ok",
		Artifact: &art,
		Hidden:   SyntheticHiddenSpec(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if dimStatus(g, DimValidMemoryConstraint) != DimPass {
		t.Fatalf("want constraint pass, got %s dims=%v", dimStatus(g, DimValidMemoryConstraint), g.Dimensions)
	}
}

func ptr(a OutputArtifact) *OutputArtifact { return &a }
