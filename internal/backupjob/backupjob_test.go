package backupjob

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/health"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func testTarget() Target {
	return Target{Schema: schema, ID: "target1", Provider: "github", Repository: "owner/repo", RemoteDigest: "digest1", Visibility: "private", SelectedAt: testNow.Add(-time.Hour).Format(time.RFC3339), PrivateVerifiedAt: testNow.Add(-time.Hour).Format(time.RFC3339), AuthorizedReaderIDs: []string{"owner"}}
}

func testSpec(run string) JobSpec {
	return JobSpec{JobID: "vault-git", TargetID: "target1", PolicyDigest: "policy1", RunID: run, Operation: "push"}
}

func TestBeginDurableBeforeWork(t *testing.T) {
	state := t.TempDir()
	running, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := LoadReceipt(state, "vault-git")
	if err != nil || !found || got.State != "running" || got.RunID != running.RunID {
		t.Fatalf("durable running receipt = %#v, found=%v, err=%v", got, found, err)
	}
	if _, err := Begin(state, testSpec("run2"), testNow); !errors.Is(err, ErrRunning) {
		t.Fatalf("second begin error = %v, want ErrRunning", err)
	}
}

func TestFailureRetainsLastSuccess(t *testing.T) {
	state := t.TempDir()
	first, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	success, err := Finish(state, first, Result{State: "succeeded"}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Begin(state, testSpec("run2"), testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := Finish(state, second, Result{State: "failed", FailureCode: "offline"}, testNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if failed.LastSuccessAt != success.LastSuccessAt || failed.State != "failed" || failed.SafeDetail != safeMessage("offline") {
		t.Fatalf("failed receipt = %#v, success = %#v", failed, success)
	}
}

func TestStaleCompleterCannotReplaceSuccessor(t *testing.T) {
	state := t.TempDir()
	first, err := Begin(state, testSpec("run1"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	nextSpec := testSpec("run2")
	nextSpec.RecoverRunID, nextSpec.RecoverStartedAt = first.RunID, first.StartedAt
	second, err := Begin(state, nextSpec, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(state, first, Result{State: "succeeded"}, testNow.Add(2*time.Minute)); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale finish error = %v, want ownership loss", err)
	}
	got, found, err := LoadReceipt(state, "vault-git")
	if err != nil || !found || got.RunID != second.RunID || got.State != "running" {
		t.Fatalf("successor receipt = %#v, found=%v, err=%v", got, found, err)
	}
}

func TestClassifyAllBackupStates(t *testing.T) {
	target := testTarget()
	fresh := &health.Producer{State: health.ProducerFresh, IntervalSeconds: 3600}
	base := Receipt{Schema: schema, JobID: "vault-git", TargetID: target.ID, PolicyDigest: "policy1", RunID: "run1", Operation: "push", State: "running", StartedAt: testNow.Add(-time.Minute).Format(time.RFC3339)}
	cases := []struct {
		name, want string
		target     *Target
		receipt    *Receipt
		leaseLive  bool
	}{
		{"unconfigured", "unconfigured", nil, nil, false},
		{"running", "running", &target, &base, true},
		{"interrupted", "failed", &target, &base, false},
		{"offline", "offline", &target, receipt(base, "failed", "offline", ""), false},
		{"diverged", "diverged", &target, receipt(base, "diverged", "", ""), false},
		{"stale", "stale", &target, receipt(base, "succeeded", "", testNow.Add(-3*time.Hour).Format(time.RFC3339)), false},
		{"healthy", "healthy", &target, receipt(base, "succeeded", "", testNow.Add(-time.Minute).Format(time.RFC3339)), false},
		{"future", "failed", &target, receipt(base, "succeeded", "", testNow.Add(time.Hour).Format(time.RFC3339)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.target, tc.receipt, fresh, true, tc.leaseLive, testNow)
			if got.State != tc.want {
				t.Fatalf("state = %q, want %q (%#v)", got.State, tc.want, got)
			}
		})
	}
}

func TestAlertOnePerFailureEpisodeAcrossRestart(t *testing.T) {
	state := t.TempDir()
	failure := Health{JobID: "vault-git", TargetID: "target1", State: "offline", FailureCode: "offline", PolicyDigest: "policy1", LastAttemptAt: testNow.Add(-time.Minute).Format(time.RFC3339)}
	first, notify, err := AdvanceAlert(state, failure, testNow)
	if err != nil || !notify || first.DeliveryState != "unknown" {
		t.Fatalf("first alert = %#v, notify=%v, err=%v", first, notify, err)
	}
	second, notify, err := AdvanceAlert(state, failure, testNow.Add(time.Minute))
	if err != nil || notify || second.EpisodeID != first.EpisodeID {
		t.Fatalf("repeat alert = %#v, notify=%v, err=%v", second, notify, err)
	}
	third, notify, err := AdvanceAlert(state, failure, testNow.Add(2*time.Minute))
	if err != nil || notify || third.EpisodeID != first.EpisodeID {
		t.Fatalf("restart alert = %#v, notify=%v, err=%v", third, notify, err)
	}
}

func TestAlertDifferentFailureAndConfirmedRecoveryReset(t *testing.T) {
	state := t.TempDir()
	offline := Health{JobID: "vault-git", TargetID: "target1", State: "offline", FailureCode: "offline", PolicyDigest: "policy1", LastAttemptAt: testNow.Add(-time.Minute).Format(time.RFC3339)}
	first, notify, err := AdvanceAlert(state, offline, testNow)
	if err != nil || !notify {
		t.Fatalf("first alert err=%v notify=%v", err, notify)
	}
	timeout := offline
	timeout.FailureCode, timeout.State = "timeout", "offline"
	second, notify, err := AdvanceAlert(state, timeout, testNow.Add(time.Minute))
	if err != nil || !notify || second.EpisodeID == first.EpisodeID {
		t.Fatalf("different failure = %#v notify=%v err=%v", second, notify, err)
	}
	unrelated := Health{JobID: "vault-git", TargetID: "other", State: "healthy", PolicyDigest: "policy1", LastSuccessAt: testNow.Add(2 * time.Minute).Format(time.RFC3339)}
	if _, _, err := AdvanceAlert(state, unrelated, testNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	matching := Health{JobID: "vault-git", TargetID: "target1", State: "healthy", PolicyDigest: "policy1", LastSuccessAt: testNow.Add(2 * time.Minute).Format(time.RFC3339)}
	recovered, notify, err := AdvanceAlert(state, matching, testNow.Add(2*time.Minute))
	if err != nil || notify || recovered.RecoveredAt == "" {
		t.Fatalf("recovery = %#v notify=%v err=%v", recovered, notify, err)
	}
	after, notify, err := AdvanceAlert(state, timeout, testNow.Add(3*time.Minute))
	if err != nil || !notify || after.EpisodeID == second.EpisodeID {
		t.Fatalf("post-recovery failure = %#v notify=%v err=%v", after, notify, err)
	}
}

func TestRejectsPathEscapeAndLinkedOutputs(t *testing.T) {
	if _, err := Begin(t.TempDir(), JobSpec{JobID: "..", TargetID: "target1", PolicyDigest: "policy1", RunID: "run1", Operation: "push"}, testNow); err == nil {
		t.Fatal("path escape job id was accepted")
	}
	root := t.TempDir()
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(t.TempDir(), linked); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Begin(linked, testSpec("run1"), testNow); err == nil {
		t.Fatal("linked output root was accepted")
	}
}

func receipt(base Receipt, state, code, success string) *Receipt {
	r := base
	r.State, r.FailureCode, r.LastSuccessAt = state, code, success
	r.FinishedAt = testNow.Add(-30 * time.Second).Format(time.RFC3339)
	return &r
}
