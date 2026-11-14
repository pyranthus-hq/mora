package backupjob

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/health"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func testTarget() Target {
	return Target{Schema: schema, ID: "target1", Provider: "github", Repository: "owner/repo", RemoteDigest: "digest1", Visibility: "private", SelectedAt: testNow.Add(-time.Hour).Format(time.RFC3339), PrivateVerifiedAt: testNow.Add(-time.Hour).Format(time.RFC3339), AuthorizedReaderIDs: []string{"owner", "reader"}}
}
func testSpec(run string) JobSpec {
	return JobSpec{JobID: "vault-git", TargetID: "target1", PolicyDigest: "policy1", RunID: run, Operation: "push"}
}
func freshProducer() *health.Producer {
	return &health.Producer{State: health.ProducerFresh, IntervalSeconds: 3600}
}

func TestBeginDurableBeforeWork(t *testing.T) {
	state := t.TempDir()
	run, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	receipt, found, err := LoadReceipt(state, "vault-git")
	if err != nil || !found || receipt.State != "running" || receipt.RunID != run.Receipt.RunID {
		t.Fatalf("receipt=%#v found=%v err=%v", receipt, found, err)
	}
	if live, err := LeaseLive(state, "vault-git"); err != nil || !live {
		t.Fatalf("lease live=%v err=%v", live, err)
	}
}

func TestStaleCompleterCannotReplaceSuccessor(t *testing.T) {
	state := t.TempDir()
	first, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	recovery := testSpec("run2")
	recovery.RecoverRunID, recovery.RecoverStartedAt = first.Receipt.RunID, first.Receipt.StartedAt
	if _, err := Begin(state, recovery, testNow.Add(time.Minute)); !errors.Is(err, ErrRunning) {
		t.Fatalf("live recovery error=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Begin(state, recovery, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := Finish(first, Result{State: "succeeded"}, testNow.Add(2*time.Minute)); !errors.Is(err, ErrOwnershipLost) {
		t.Fatal("stale completer accepted", err)
	}
	if second.Receipt.RunID != "run2" {
		t.Fatalf("recovered run=%#v", second.Receipt)
	}
}

func TestFailureRetainsLastSuccess(t *testing.T) {
	state := t.TempDir()
	first, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	success, err := Finish(first, Result{State: "succeeded"}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Begin(state, testSpec("run2"), testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := Finish(second, Result{State: "failed", FailureCode: "offline"}, testNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if failed.LastSuccessAt != success.LastSuccessAt || failed.SafeDetail != safeMessage("offline") {
		t.Fatalf("failure did not retain success: %#v", failed)
	}
}

func TestComposedDivergenceAlertDeliveryAndRecovery(t *testing.T) {
	state, target := t.TempDir(), testTarget()
	run, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	diverged, err := Finish(run, Result{State: "diverged"}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h := Classify(state, testSpec("expected"), &target, &diverged, freshProducer(), testNow.Add(2*time.Minute))
	if h.State != "diverged" || h.LastAttemptAt != diverged.StartedAt {
		t.Fatalf("diverged health=%#v", h)
	}
	alert, notify, err := AdvanceAlert(state, h, testNow.Add(2*time.Minute))
	if err != nil || !notify {
		t.Fatalf("alert=%#v notify=%v err=%v", alert, notify, err)
	}
	if err := RecordAlertDelivery(state, h.JobID, alert.EpisodeID, testNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, notify, err := AdvanceAlert(state, h, testNow.Add(4*time.Minute)); err != nil || notify {
		t.Fatalf("delivered alert retried notify=%v err=%v", notify, err)
	}
	next, err := Begin(state, testSpec("run2"), testNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	succeeded, err := Finish(next, Result{State: "succeeded"}, testNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	healthy := Classify(state, testSpec("expected"), &target, &succeeded, freshProducer(), testNow.Add(7*time.Minute))
	if healthy.State != "healthy" {
		t.Fatalf("healthy=%#v", healthy)
	}
	closed, notify, err := AdvanceAlert(state, healthy, testNow.Add(7*time.Minute))
	if err != nil || notify || closed.RecoveredAt == "" {
		t.Fatalf("recovery=%#v notify=%v err=%v", closed, notify, err)
	}
}

func TestClassifyRejectsWrongPolicyAndMissingProducer(t *testing.T) {
	state, target := t.TempDir(), testTarget()
	run, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	success, err := Finish(run, Result{State: "succeeded"}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wrong := testSpec("expected")
	wrong.PolicyDigest = "other"
	if got := Classify(state, wrong, &target, &success, freshProducer(), testNow.Add(2*time.Minute)); got.State != "failed" || got.FailureCode != "destination_mismatch" {
		t.Fatalf("wrong policy=%#v", got)
	}
	if got := Classify(state, testSpec("expected"), &target, &success, nil, testNow.Add(2*time.Minute)); got.State != "stale" {
		t.Fatalf("missing producer=%#v", got)
	}
	if got := Classify(state, testSpec("expected"), nil, nil, nil, testNow); got.State != "unconfigured" || got.JobID == "" {
		t.Fatalf("unconfigured=%#v", got)
	}
	local := JobSpec{JobID: "local-archive", TargetID: "local1", PolicyDigest: "policy1", Operation: "local_archive"}
	localReceipt := Receipt{Schema: schema, JobID: local.JobID, TargetID: local.TargetID, PolicyDigest: local.PolicyDigest, RunID: "run1", Operation: local.Operation, State: "succeeded", StartedAt: testNow.Add(-time.Minute).Format(time.RFC3339), FinishedAt: testNow.Add(-30 * time.Second).Format(time.RFC3339), LastSuccessAt: testNow.Add(-30 * time.Second).Format(time.RFC3339)}
	if got := Classify(state, local, &target, &localReceipt, freshProducer(), testNow); got.State != "healthy" {
		t.Fatalf("unrelated git target gated local archive: %#v", got)
	}
}

func TestRepeatSetupCanonicalReadersRefreshesVerification(t *testing.T) {
	config, target := t.TempDir(), testTarget()
	if err := SaveSelectedTarget(config, target); err != nil {
		t.Fatal(err)
	}
	updated := target
	updated.AuthorizedReaderIDs = []string{"reader", "owner"}
	updated.PrivateVerifiedAt = testNow.Add(-time.Minute).Format(time.RFC3339)
	if err := SaveSelectedTarget(config, updated); err != nil {
		t.Fatal(err)
	}
	got, found, err := LoadTarget(config)
	if err != nil || !found || got.PrivateVerifiedAt != updated.PrivateVerifiedAt {
		t.Fatalf("target=%#v found=%v err=%v", got, found, err)
	}
}

func TestCorruptReceiptRequiresObservedDigestAndQuarantines(t *testing.T) {
	state := t.TempDir()
	path, err := preparePath(state, receiptPath(state, "vault-git"))
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte("not json\n")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadReceipt(state, "vault-git"); !errors.Is(err, ErrReceiptUnreadable) {
		t.Fatalf("load corrupt err=%v", err)
	}
	if _, err := Begin(state, testSpec("run1"), testNow); !errors.Is(err, ErrReceiptUnreadable) {
		t.Fatalf("begin corrupt err=%v", err)
	}
	spec := testSpec("run1")
	spec.RecoverReceiptDigest = digestBytes(bad)
	run, err := Begin(state, spec, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if matches, err := filepath.Glob(path + ".corrupt-*"); err != nil || len(matches) != 1 {
		t.Fatalf("quarantine=%v err=%v", matches, err)
	}
}

func TestRejectsForeignReceiptAndMapsShareJobFilename(t *testing.T) {
	state := t.TempDir()
	run, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	success, err := Finish(run, Result{State: "succeeded"}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	foreignPath, err := preparePath(state, receiptPath(state, "local-archive"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDurableJSON(foreignPath, success); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadReceipt(state, "local-archive"); !errors.Is(err, ErrReceiptUnreadable) {
		t.Fatalf("foreign receipt err=%v", err)
	}
	share := JobSpec{JobID: "share-pull:team_a", TargetID: "share1", PolicyDigest: "policy1", RunID: "run1", Operation: "share_pull"}
	shareRun, err := Begin(state, share, testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer shareRun.Close()
	if strings.Contains(filepath.Base(receiptPath(state, share.JobID)), ":") {
		t.Fatal("logical share id reached filename")
	}
}

func TestRejectsLinkedOutputsAndWindowsAliases(t *testing.T) {
	root := t.TempDir()
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(t.TempDir(), linked); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Begin(linked, testSpec("run1"), testNow); err == nil {
		t.Fatal("linked root accepted")
	}
	for _, id := range []string{"share-pull:CON", "share-pull:evil:name", "share-pull:COM1", "share-pull:" + strings.Repeat("a", 65)} {
		spec := testSpec("run1")
		spec.JobID = id
		if _, err := Begin(t.TempDir(), spec, testNow); err == nil {
			t.Fatalf("unsafe id accepted: %q", id)
		}
	}
}

func TestCorruptAlertRequiresEvidenceAndUnconfiguredIsAlertable(t *testing.T) {
	state := t.TempDir()
	path, err := preparePath(state, alertPath(state, "vault-git"))
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte("not json\n")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	failure := Health{JobID: "vault-git", TargetID: "target1", State: "offline", FailureCode: "offline", PolicyDigest: "policy1", LastAttemptAt: testNow.Format(time.RFC3339)}
	if _, _, err := AdvanceAlert(state, failure, testNow); !errors.Is(err, ErrAlertUnreadable) {
		t.Fatalf("advance corrupt alert err=%v", err)
	}
	if err := RecoverAlert(state, "vault-git", digestBytes(bad)); err != nil {
		t.Fatal(err)
	}
	if _, notify, err := AdvanceAlert(state, failure, testNow); err != nil || !notify {
		t.Fatalf("recovered alert notify=%v err=%v", notify, err)
	}
	unconfigured := Health{JobID: "vault-git", State: "unconfigured", PolicyDigest: "policy2"}
	if alert, notify, err := AdvanceAlert(state, unconfigured, testNow.Add(time.Minute)); err != nil || !notify || alert.JobID == "" {
		t.Fatalf("unconfigured alert=%#v notify=%v err=%v", alert, notify, err)
	}
}
