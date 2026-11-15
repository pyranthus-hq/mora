package runner

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(Options{
		ParentDir: t.TempDir(),
		Budget: Budget{
			CeilingUSDMicros: 1,
			MaxRuns:          100,
			MaxWallSeconds:   3600,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestSyntheticCasesValidate(t *testing.T) {
	for _, c := range AllSynthCases() {
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", c.CaseID, err)
		}
		for _, cond := range ConditionsFor(c.CaseID) {
			if err := cond.Validate(); err != nil {
				t.Fatalf("%s/%s: %v", c.CaseID, cond.ConditionID, err)
			}
		}
	}
}

func TestResetNoBleedBetweenConditions(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	conds := ConditionsFor(c.CaseID)
	gate := SyntheticPlumbingGate()

	var vaultDigests []string
	var contenderDigests []string

	for i, cond := range conds {
		rec, err := r.RunTrial(context.Background(), TrialRequest{
			Case:      c,
			Condition: cond,
			Gate:      gate,
			RepIndex:  i,
			Contender: &SyntheticContender{},
		})
		if err != nil {
			t.Fatalf("trial %s: %v", cond.Kind, err)
		}
		if !rec.Isolation.ResetObserved || !rec.Isolation.InitialHashesMatched {
			t.Fatalf("reset/hash check failed for %s: %+v", cond.Kind, rec.Isolation)
		}
		// After trial, workspace is dirty; next RunTrial resets.
		h, err := r.ws.ComputeHashes()
		if err != nil {
			t.Fatal(err)
		}
		vaultDigests = append(vaultDigests, h.Vault)
		contenderDigests = append(contenderDigests, h.Contender)

		// Force a distinctive bleed marker, then ensure next reset clears it.
		bleed := filepath.Join(r.ws.Layout.Vault, "BLEED_MARKER")
		if err := os.WriteFile(bleed, []byte(cond.Kind), 0o644); err != nil {
			t.Fatal(err)
		}
		contBleed := filepath.Join(r.ws.Layout.Contender, "BLEED.txt")
		if err := os.WriteFile(contBleed, []byte(cond.Kind), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Final reset must restore baseline (no bleed).
	got, err := r.ws.Reset()
	if err != nil {
		t.Fatalf("final reset: %v", err)
	}
	if got != r.ws.Baseline() {
		t.Fatalf("final reset hashes != baseline")
	}
	if _, err := os.Stat(filepath.Join(r.ws.Layout.Vault, "BLEED_MARKER")); !os.IsNotExist(err) {
		t.Fatal("bleed marker survived reset")
	}
	if _, err := os.Stat(filepath.Join(r.ws.Layout.Contender, "BLEED.txt")); !os.IsNotExist(err) {
		t.Fatal("contender bleed survived reset")
	}

	// Distinct condition injections should have produced distinct vault states
	// before reset (proves conditions actually differed).
	if vaultDigests[0] == vaultDigests[1] && vaultDigests[1] == vaultDigests[2] {
		// After each trial the vault has condition_memory.json — kinds differ so digests should differ.
		t.Fatal("expected distinct post-trial vault digests across conditions")
	}
	_ = contenderDigests
}

func TestAttemptAndExposureReceipts(t *testing.T) {
	r := newTestRunner(t)
	gate := SyntheticPlumbingGate()

	cases := AllSynthCases()
	var receipts []AttemptReceipt
	for _, c := range cases {
		cond := ConditionsFor(c.CaseID)[0] // original_memory
		rec, err := r.RunTrial(context.Background(), TrialRequest{
			Case: c, Condition: cond, Gate: gate, RepIndex: 0,
			Contender: &SyntheticContender{},
		})
		if err != nil {
			t.Fatalf("%s: %v", c.CaseID, err)
		}
		receipts = append(receipts, rec)

		if rec.ClaimsEfficacy {
			t.Fatal("receipt must not claim efficacy")
		}
		if rec.Exposure.SnapshotBytesSHA256 == "" || rec.Exposure.DeliveredDigest == "" {
			t.Fatal("missing snapshot or delivered digests")
		}
		if rec.Exposure.SnapshotBytesSHA256 == rec.Exposure.DeliveredDigest {
			t.Fatal("snapshot digest must differ from delivered digest")
		}
		if rec.Digests.HarnessID == "" || rec.Digests.ModelID == "" {
			t.Fatal("missing harness/model identity")
		}
		if rec.Attempt.StartedAt == "" || rec.Attempt.FinishedAt == "" {
			t.Fatal("missing start/end timestamps")
		}
		if len(rec.OutputRefs) == 0 {
			t.Fatal("expected output refs")
		}
		// Tool results: failure case includes tool_result part → captured;
		// no-relevant-memory has no tool_result → unavailable.
		switch c.Role {
		case contract.CaseRoleFailure, contract.CaseRoleValidMemoryControl:
			if rec.Exposure.ToolResultStatus != ToolResultCaptured {
				t.Fatalf("%s: want tool results captured, got %s", c.CaseID, rec.Exposure.ToolResultStatus)
			}
		case contract.CaseRoleNoRelevantMemoryCtrl:
			if rec.Exposure.ToolResultStatus != ToolResultUnavailable {
				t.Fatalf("%s: want tool results unavailable, got %s", c.CaseID, rec.Exposure.ToolResultStatus)
			}
		}
	}

	// Distinct I/O across the three roles.
	if receipts[0].ExitClass != "task_failure" {
		t.Fatalf("failure case exit_class=%s want task_failure", receipts[0].ExitClass)
	}
	if receipts[1].ExitClass != "ok" || receipts[2].ExitClass != "ok" {
		t.Fatalf("controls exit_class = %s, %s", receipts[1].ExitClass, receipts[2].ExitClass)
	}
	// Stdout payloads must be distinct by role.
	bodies := make([]string, 3)
	for i, rec := range receipts {
		for _, ref := range rec.OutputRefs {
			if strings.HasSuffix(ref, "stdout.json") {
				b, err := os.ReadFile(ref)
				if err != nil {
					t.Fatal(err)
				}
				bodies[i] = string(b)
			}
		}
		if bodies[i] == "" {
			t.Fatalf("missing stdout for %s", rec.Attempt.CaseID)
		}
	}
	if bodies[0] == bodies[1] || bodies[1] == bodies[2] || bodies[0] == bodies[2] {
		t.Fatal("expected distinct stdout across three synthetic roles")
	}
}

func TestFailureRetentionNoSilentRetry(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	cond := ConditionsFor(c.CaseID)[0]
	gate := SyntheticPlumbingGate()

	rec, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: gate, RepIndex: 0,
		Contender: &SyntheticContender{BehaviorOverride: "malformed"},
	})
	if err != nil {
		// malformed may or may not return error; receipt must still be preserved
		t.Logf("run err (preserved): %v", err)
	}
	if rec.Attempt.Status != contract.AttemptFailed {
		t.Fatalf("status=%s want failed", rec.Attempt.Status)
	}
	if rec.Attempt.ErrorCode != CodeMalformedOutput && rec.ExitClass != "malformed" {
		t.Fatalf("expected malformed retention, got status=%s exit=%s code=%s",
			rec.Attempt.Status, rec.ExitClass, rec.Attempt.ErrorCode)
	}
	if len(r.Receipts()) != 1 {
		t.Fatal("expected exactly one preserved receipt (no silent retry)")
	}

	// Timeout retention.
	rec2, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: gate, RepIndex: 1,
		TimeoutSeconds: 1,
		Contender:      &SyntheticContender{Hang: true},
	})
	if rec2.Attempt.Status != contract.AttemptTimedOut {
		t.Fatalf("timeout status=%s err=%v", rec2.Attempt.Status, err)
	}
	if len(r.Receipts()) != 2 {
		t.Fatalf("want 2 receipts, got %d", len(r.Receipts()))
	}
}

func TestDenialWithoutBudgetsNoProviderStart(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	cond := ConditionsFor(c.CaseID)[0]
	stub := &ExternalProviderStub{}

	rec, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: DeniedGate(), RepIndex: 0,
		Contender: stub,
	})
	if err == nil {
		t.Fatal("expected gate denial error")
	}
	if stub.StartCountValue() != 0 {
		t.Fatalf("provider started %d times; want 0", stub.StartCountValue())
	}
	if rec.ProviderStarted {
		t.Fatal("receipt ProviderStarted must be false")
	}
	if rec.Attempt.ProviderInvoked {
		t.Fatal("attempt must not mark provider invoked")
	}
	if rec.Attempt.Status != contract.AttemptSkipped {
		t.Fatalf("status=%s want skipped", rec.Attempt.Status)
	}

	// Exhausted monetary ceiling with authorized-shape gate still blocks stub.
	r2 := newTestRunner(t)
	r2.budget = Budget{CeilingUSDMicros: 1, SpentUSDMicros: 1, MaxRuns: 10}
	stub2 := &ExternalProviderStub{}
	rec2, err := r2.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: SyntheticPlumbingGate(), RepIndex: 0,
		Contender: stub2,
	})
	if err == nil {
		t.Fatal("expected denial")
	}
	if stub2.StartCountValue() != 0 {
		t.Fatal("provider must not start when ceiling exhausted")
	}
	if rec2.ProviderStarted {
		t.Fatal("ProviderStarted true on exhausted ceiling")
	}

	// Absent approval: empty gate.
	r3 := newTestRunner(t)
	stub3 := &ExternalProviderStub{}
	empty := contract.NewRunGate()
	empty.GateID = "gate-empty"
	empty.Policy = DeniedGate().Policy
	_, err = r3.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: empty, RepIndex: 0, Contender: stub3,
	})
	if err == nil {
		t.Fatal("expected denial on empty gate")
	}
	if stub3.StartCountValue() != 0 {
		t.Fatal("provider started without approval")
	}
}

func TestContainmentOfObservableWrites(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	cond := ConditionsFor(c.CaseID)[0]
	rec, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: SyntheticPlumbingGate(), RepIndex: 0,
		Contender: &SyntheticContender{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Isolation.OracleLeakDetected {
		t.Fatalf("oracle leak: %v", rec.Isolation.OracleLeakPaths)
	}
	for _, w := range rec.Isolation.ContenderWrites {
		lower := strings.ToLower(w)
		for _, bad := range []string{"oracle", "gold", "expected_answer", "reference_patch", "post_cutoff", "hidden_test"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("contender write %q looks like oracle/gold material", w)
			}
		}
	}
	// Oracle ref staging must exist outside contender.
	if _, err := os.Stat(filepath.Join(r.ws.Layout.OracleRef, "oracle.hashref.json")); err != nil {
		t.Fatal("missing oracle hash-ref staging")
	}
	if _, err := os.Stat(filepath.Join(r.ws.Layout.Contender, "oracle.hashref.json")); !os.IsNotExist(err) {
		t.Fatal("oracle hash-ref must not appear under contender")
	}
}

func TestRunnerReceiptNotEfficacyClaim(t *testing.T) {
	r := newTestRunner(t)
	c := SynthValidMemoryCase()
	cond := ConditionsFor(c.CaseID)[0]
	_, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: SyntheticPlumbingGate(), RepIndex: 0,
		Contender: &SyntheticContender{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := BuildSessionReceipt(r, true)
	if err != nil {
		t.Fatal(err)
	}
	if sr.ClaimsEfficacy {
		t.Fatal("session receipt must not claim efficacy")
	}
	if !strings.Contains(sr.Notes, "not a historical efficacy") {
		t.Fatalf("notes=%q", sr.Notes)
	}
	if !slices.Contains(sr.UnresolvedLimits, "filesystem writes outside the disposable workspace root are not test-verified") {
		t.Fatalf("session receipt missing filesystem isolation limit: %v", sr.UnresolvedLimits)
	}
}

func TestAdmitPrivatePackageWithoutPublicCopy(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()

	priv := t.TempDir()
	// Runtime-specific content avoids matching this test source in the public tree.
	sentinel := "private-package-payload-" + priv
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(filepath.Join(priv, "case.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// Extra private file that must not appear in public fixture trees.
	if err := os.WriteFile(filepath.Join(priv, "private_note.txt"), []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	adm, err := r.AdmitCasePackage(AdmitOptions{SourcePath: priv, AllowSynthetic: true})
	if err != nil {
		t.Fatal(err)
	}
	if adm.StagedDir == "" {
		t.Fatal("expected staged dir under PrivateAdm")
	}
	rel, err := filepath.Rel(r.ws.Layout.PrivateAdm, adm.StagedDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("staged=%s want under %s (rel=%q, err=%v)", adm.StagedDir, r.ws.Layout.PrivateAdm, rel, err)
	}
	// Confirm both payloads were staged intact, then inspect all runner output,
	// including artifacts and preserved reset inputs, for copies outside staging.
	for name, want := range map[string]string{
		"case.json":        string(b),
		"private_note.txt": sentinel,
	} {
		got, err := os.ReadFile(filepath.Join(adm.StagedDir, name))
		if err != nil || string(got) != want {
			t.Fatalf("staged %s differs from source: %v", name, err)
		}
	}
	// Resolve the module root explicitly, then inspect public trees as well as
	// disposable output. This is bounded leak coverage, not a filesystem sandbox.
	repoRoot := testRepoRoot(t)
	for _, root := range []string{
		filepath.Dir(r.ws.Layout.Root),
		filepath.Join(repoRoot, "docs", "experiments", "memory-replay", "runner"),
		filepath.Join(repoRoot, "internal", "mora", "pilotreplay"),
	} {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == r.ws.Layout.PrivateAdm {
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(body), sentinel) || strings.Contains(string(body), string(b)) {
				t.Errorf("private payload copied outside PrivateAdm: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

	}

	// Reject admitting from public fixture trees.
	_, err = r.AdmitCasePackage(AdmitOptions{
		SourcePath:     "docs/experiments/memory-replay/cases",
		AllowSynthetic: true,
	})
	if err == nil {
		t.Fatal("expected reject when admitting from public docs tree")
	}

	// Reject production-looking paths.
	_, err = New(Options{ParentDir: "/Users/someone/Library/Application Support/mora/vault"})
	if err == nil {
		t.Fatal("expected reject production-looking parent")
	}
}

func testRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot resolve repository root: go.mod not found")
		}
		dir = parent
	}
}

// Exercise foreign separator styles on every host, not only Windows CI.
func TestProductionPathGuard(t *testing.T) {
	for _, p := range []string{
		"/Users/someone/Library/Application Support/mora/vault",
		`C:\Users\x\Library\Application Support\mora\vault`,
		`C:\Users\x\.mora\`,
		`C:\Users\x\.mora`,
		`C:\Users\x\.mora\vault`,
		`C:/Users/x/.mora/`,
		".mora/vault",
		".mora",
		`.mora\vault`,
		"/home/x/.mora/",
		"/home/x/.mora",
		`C:\Users\x\mora\vault`,
		`C:\Users\x\credentials.json`,
		`C:\Users\x\client_secret.json`,
		`C:\USERS\X\.MORA\vault`,
	} {
		t.Run(p, func(t *testing.T) {
			if !looksLikeProductionPath(p) {
				t.Fatalf("expected production path rejection: %q", p)
			}
			// Both callers must reject before filesystem access.
			if _, err := PrepareWorkspace(p); err == nil {
				t.Fatal("workspace accepted production path")
			} else if e, ok := err.(*Error); !ok || e.Code != CodePathRejected {
				t.Fatalf("expected path rejection, got %v", err)
			}
			r := newTestRunner(t)
			if _, err := r.AdmitCasePackage(AdmitOptions{SourcePath: p}); err == nil {
				t.Fatal("admission accepted production path")
			} else if e, ok := err.(*Error); !ok || e.Code != CodePathRejected {
				t.Fatalf("expected path rejection, got %v", err)
			}
		})
	}
	for _, p := range []string{"/tmp/replay", `C:\Temp\replay`, "/home/x/.mora-backup", `C:\Users\x\.mora-backup`} {
		if looksLikeProductionPath(p) {
			t.Errorf("unexpected production path rejection: %q", p)
		}
	}
}

func TestRejectOracleInRunnerPackage(t *testing.T) {
	r := newTestRunner(t)
	c := contract.ExampleNegativeHiddenInRunner()
	_, err := r.AdmitCasePackage(AdmitOptions{Case: &c, AllowSynthetic: true})
	if err == nil {
		t.Fatal("expected oracle leak rejection")
	}
}

func TestMissingExposureDenied(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	c.DeliveredContext.ExposureAvailability = contract.ExposureMissing
	c.DeliveredContext.Parts = nil
	c.DeliveredContext.MissingReason = "synthetic missing"
	c.DeliveredContext.Fidelity = contract.FidelityUnknown
	cond := ConditionsFor(c.CaseID)[0]
	_, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: SyntheticPlumbingGate(), RepIndex: 0,
		Contender: &SyntheticContender{},
	})
	if err == nil {
		t.Fatal("expected missing exposure error")
	}
	if e, ok := err.(*Error); !ok || e.Code != CodeMissingExposure {
		// may be admit reject from contract validate first
		if !strings.Contains(err.Error(), "exposure") && !strings.Contains(err.Error(), "delivered") && !strings.Contains(err.Error(), "admit") {
			t.Fatalf("unexpected err: %v", err)
		}
	}
}

func TestRejectedPathAccess(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	c.RunnerPackage.ContenderVisiblePaths = []string{"../escape.txt"}
	_, err := r.AdmitCasePackage(AdmitOptions{Case: &c, AllowSynthetic: true})
	if err != nil {
		t.Fatalf("admit case before injection: %v", err)
	}
	cond := ConditionsFor(c.CaseID)[0]
	_, err = r.InjectCondition(c, cond)
	if e, ok := err.(*Error); !ok || e.Code != CodePathRejected {
		t.Fatalf("expected rejected_path_access, got %v", err)
	}
}

func TestUnavailableProviderPreserved(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	cond := ConditionsFor(c.CaseID)[0]
	rec, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: SyntheticPlumbingGate(), RepIndex: 0,
		Contender: &SyntheticContender{BehaviorOverride: "unavailable"},
	})
	_ = err
	if rec.Attempt.Status != contract.AttemptUnavailable {
		t.Fatalf("status=%s want unavailable", rec.Attempt.Status)
	}
	if rec.Attempt.ProviderInvoked {
		t.Fatal("unavailable must not invoke provider")
	}
}

func TestExternalProviderNeverStartsEvenWithShapeGate(t *testing.T) {
	// Even an authorized-shape gate cannot start a real external provider in
	// this package — paid calls remain impossible in ordinary tests.
	r := newTestRunner(t)
	c := SynthFailureCase()
	cond := ConditionsFor(c.CaseID)[0]
	stub := &ExternalProviderStub{}
	rec, err := r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: SyntheticPlumbingGate(), RepIndex: 0,
		Contender: stub,
	})
	if err == nil {
		t.Fatal("expected provider blocked")
	}
	if stub.StartCountValue() != 0 {
		t.Fatal("external stub must not start")
	}
	if rec.ProviderStarted {
		t.Fatal("ProviderStarted")
	}
}

func TestSessionReceiptJSONRoundTrip(t *testing.T) {
	r := newTestRunner(t)
	sr, err := BuildSessionReceipt(r, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(sr)
	if err != nil {
		t.Fatal(err)
	}
	var back SessionReceipt
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ClaimsEfficacy || !back.TestsPassed {
		t.Fatalf("%+v", back)
	}
}

func TestNoSilentRetryOnTimeout(t *testing.T) {
	r := newTestRunner(t)
	c := SynthFailureCase()
	cond := ConditionsFor(c.CaseID)[0]
	gate := SyntheticPlumbingGate()
	gate.Policy.Retry.MaxRetriesPerAttempt = 0
	gate.Policy.Retry.RetryOnTimeout = false

	start := time.Now()
	_, _ = r.RunTrial(context.Background(), TrialRequest{
		Case: c, Condition: cond, Gate: gate, RepIndex: 0,
		TimeoutSeconds: 1,
		Contender:      &SyntheticContender{Hang: true},
	})
	elapsed := time.Since(start)
	if len(r.Receipts()) != 1 {
		t.Fatalf("silent retries detected: %d receipts", len(r.Receipts()))
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took too long: %v (possible retries)", elapsed)
	}
}
