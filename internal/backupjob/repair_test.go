package backupjob

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFirstUseClockRollbackAndInvalidFinish(t *testing.T) {
	root := t.TempDir()
	config, state := filepath.Join(root, "new-config"), filepath.Join(root, "new-state")
	if _, found, err := LoadTarget(config); err != nil || found {
		t.Fatalf("initial target: %v %v", found, err)
	}
	if err := SaveSelectedTarget(config, testTarget()); err != nil {
		t.Fatal(err)
	}
	run, err := Begin(state, testSpec("first"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(run, Result{State: "success"}, testNow.Add(time.Minute)); err == nil {
		t.Fatal("invalid result accepted")
	}
	if live, err := LeaseLive(state, "vault-git"); err != nil || !live {
		t.Fatalf("invalid finish lost lease: %v %v", live, err)
	}
	if _, err := Finish(run, Result{State: "succeeded"}, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(receiptPath(state, "vault-git"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(state, testSpec("backwards"), testNow.Add(-time.Minute)); err == nil {
		t.Fatal("clock rollback began work")
	}
	after, err := os.ReadFile(receiptPath(state, "vault-git"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rollback changed receipt", err)
	}
}

func TestJobOperationBindingAndIndependentPull(t *testing.T) {
	state := t.TempDir()
	wrong := testSpec("wrong")
	wrong.Operation = "local_archive"
	if _, err := Begin(state, wrong, testNow); err == nil {
		t.Fatal("wrong operation accepted")
	}
	wrong.Operation = "isolated_pull"
	if _, err := Begin(state, wrong, testNow); err == nil {
		t.Fatal("pull overwrites push")
	}
	push, err := Begin(state, testSpec("push"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Finish(push, Result{State: "succeeded"}, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pull := testSpec("pull")
	pull.JobID = "vault-git-pull"
	pull.Operation = "isolated_pull"
	run, err := Begin(state, pull, testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	stored, found, err := LoadReceipt(state, "vault-git")
	if err != nil || !found || stored.RunID != "push" || stored.State != "succeeded" {
		t.Fatalf("push lost: %+v %v", stored, err)
	}
}

func TestClassifyAllBackupStates(t *testing.T) {
	state, target := t.TempDir(), testTarget()
	expected := testSpec("")
	expected.TargetID = ""
	if h := Classify(state, expected, nil, nil, nil, testNow); h.State != "unconfigured" || h.RetryEligible {
		t.Fatalf("unconfigured: %+v", h)
	}
	run, err := Begin(state, testSpec("one"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	running := run.Receipt
	if h := Classify(state, testSpec(""), &target, &running, freshProducer(), testNow); h.State != "running" || h.RetryEligible {
		t.Fatalf("running: %+v", h)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if h := Classify(state, testSpec(""), &target, &running, freshProducer(), testNow); h.FailureCode != "interrupted" || !h.RetryEligible {
		t.Fatalf("interrupted: %+v", h)
	}
	spec := testSpec("two")
	spec.RecoverRunID, spec.RecoverStartedAt = running.RunID, running.StartedAt
	next, err := Begin(state, spec, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(run, Result{State: "succeeded"}, testNow.Add(2*time.Minute)); !errors.Is(err, ErrOwnershipLost) {
		t.Fatal("old completer accepted", err)
	}
	failed, err := Finish(next, Result{State: "failed", FailureCode: "offline"}, testNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if h := Classify(state, testSpec(""), &target, &failed, freshProducer(), testNow.Add(3*time.Minute)); h.State != "offline" || !h.RetryEligible {
		t.Fatalf("offline: %+v", h)
	}
	third, err := Begin(state, testSpec("three"), testNow.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	success, err := Finish(third, Result{State: "succeeded"}, testNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if h := Classify(state, testSpec(""), &target, &success, freshProducer(), testNow.Add(3*time.Hour)); h.State != "stale" {
		t.Fatalf("aging: %+v", h)
	}
}

func TestAlertRecoveryPreservesEveryCorruptFileAndRejectsLateAck(t *testing.T) {
	state := t.TempDir()
	h := healthFor(testSpec(""), "failed", "auth_required", false, "", "")
	h.LastAttemptAt = testNow.Format(time.RFC3339)
	old, notify, err := AdvanceAlert(state, h, testNow)
	if err != nil || !notify {
		t.Fatal(err)
	}
	bad := []byte("broken")
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(alertPath(state, h.JobID), bad, 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := AdvanceAlert(state, h, testNow)
		var observed *UnreadableError
		if !errors.As(err, &observed) || observed.Digest == "" {
			t.Fatal("missing recovery evidence", err)
		}
		if err := RecoverAlert(state, h.JobID, observed.Digest); err != nil {
			t.Fatal(err)
		}
		current, _, err := AdvanceAlert(state, h, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if current.EpisodeID == old.EpisodeID {
			t.Fatal("reused episode ID")
		}
		if err := RecordAlertDelivery(state, h.JobID, old.EpisodeID, testNow); !errors.Is(err, ErrOwnershipLost) {
			t.Fatal("late ack accepted", err)
		}
	}
	matches, err := filepath.Glob(alertPath(state, h.JobID) + ".corrupt-*")
	if err != nil || len(matches) != 2 {
		t.Fatalf("lost corrupt records: %v %v", matches, err)
	}
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, bad) {
			t.Fatal("corrupt evidence changed", err)
		}
	}
}

func TestAlertDifferentFailureAndConfirmedRecoveryReset(t *testing.T) {
	state := t.TempDir()
	h := healthFor(testSpec(""), "offline", "offline", true, "", "")
	h.LastAttemptAt = testNow.Format(time.RFC3339)
	first, _, err := AdvanceAlert(state, h, testNow)
	if err != nil {
		t.Fatal(err)
	}
	wrong := h
	wrong.State = "healthy"
	wrong.TargetID = "other"
	wrong.LastSuccessAt = testNow.Add(time.Minute).Format(time.RFC3339)
	same, notify, err := AdvanceAlert(state, wrong, testNow.Add(time.Minute))
	if err != nil || notify || same.RecoveredAt != "" || same.EpisodeID != first.EpisodeID {
		t.Fatalf("wrong success closed episode: %+v %v", same, err)
	}
	h.State, h.FailureCode = "failed", "auth_required"
	next, notify, err := AdvanceAlert(state, h, testNow.Add(2*time.Minute))
	if err != nil || !notify || next.EpisodeID == first.EpisodeID {
		t.Fatalf("new failure: %+v %v", next, err)
	}
	if err := RecordAlertDelivery(state, h.JobID, first.EpisodeID, testNow.Add(2*time.Minute)); !errors.Is(err, ErrOwnershipLost) {
		t.Fatal("stale delivery accepted", err)
	}
	h.State = "healthy"
	h.LastSuccessAt = testNow.Add(3 * time.Minute).Format(time.RFC3339)
	closed, notify, err := AdvanceAlert(state, h, testNow.Add(4*time.Minute))
	if err != nil || notify || closed.RecoveredAt == "" {
		t.Fatalf("confirmed recovery: %+v %v", closed, err)
	}

}

func TestBoundedReadersAndHonestCorruptHealth(t *testing.T) {
	state, target := t.TempDir(), testTarget()
	run, err := Begin(state, testSpec("one"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath(state, "vault-git"), []byte(strings.Repeat("x", maxRecordBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := Inspect(state, testSpec(""), &target, freshProducer(), testNow)
	var observed *UnreadableError
	if h.FailureCode != "receipt_unreadable" || !errors.As(err, &observed) || observed.Digest != "" {
		t.Fatalf("oversize health: %+v %v", h, err)
	}
	if _, err := checkedPath(state, filepath.Join(state, "..", "escape")); err == nil {
		t.Fatal("path escape accepted")
	}
	target.AuthorizedReaderIDs = make([]string, maxReaders+1)
	if err := SaveSelectedTarget(t.TempDir(), target); err == nil {
		t.Fatal("unbounded readers accepted")
	}
}

func TestConcurrentCloseFinishDoesNotRace(t *testing.T) {
	run, err := Begin(t.TempDir(), testSpec("one"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = run.Close() }()
	go func() { defer wg.Done(); _, _ = Finish(run, Result{State: "succeeded"}, testNow.Add(time.Minute)) }()
	wg.Wait()
}

func TestAlertOnePerFailureEpisodeAcrossRestart(t *testing.T) {
	state := t.TempDir()
	h := healthFor(testSpec(""), "offline", "offline", true, "", "")
	h.LastAttemptAt = testNow.Format(time.RFC3339)
	first, _, err := AdvanceAlert(state, h, testNow)
	if err != nil {
		t.Fatal(err)
	}
	h.LastAttemptAt = testNow.Add(2 * time.Minute).Format(time.RFC3339)
	retry, _, err := AdvanceAlert(state, h, testNow.Add(3*time.Minute))
	if err != nil || retry.EpisodeID != first.EpisodeID {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	h.State = "healthy"
	h.LastSuccessAt = testNow.Add(time.Minute).Format(time.RFC3339)
	old, _, err := AdvanceAlert(state, h, testNow.Add(4*time.Minute))
	if err != nil || old.RecoveredAt != "" {
		t.Fatalf("old success closed retry: %+v %v", old, err)
	}
	h.LastSuccessAt = testNow.Add(5 * time.Minute).Format(time.RFC3339)
	fresh, _, err := AdvanceAlert(state, h, testNow.Add(6*time.Minute))
	if err != nil || fresh.RecoveredAt == "" {
		t.Fatalf("new success failed recovery: %+v %v", fresh, err)
	}
}
