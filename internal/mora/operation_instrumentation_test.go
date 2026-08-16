package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeIngestFixture(t *testing.T, name string) (Source, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte("# "+name+"\n\noperation activity fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Source{Type: "filesystem", Name: name, Path: dir, Scope: "personal"}, path
}

func activityWithKind(activities []operationActivity, kind operationKind, state operationState) *operationActivity {
	for i := range activities {
		if activities[i].Kind == kind && activities[i].State == state {
			return &activities[i]
		}
	}
	return nil
}

func TestOperationProgressPeriodicHeartbeatUsesInjectedTicker(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	h, err := beginOperation(cfg, operationKindIngest, "ingesting", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	origTicker, origClock := newOperationHeartbeatTicker, operationClock
	t.Cleanup(func() { newOperationHeartbeatTicker, operationClock = origTicker, origClock })
	newOperationHeartbeatTicker = func(time.Duration) operationHeartbeatTicker {
		return operationHeartbeatTicker{C: ticks, stop: func() {}}
	}
	heartbeatAt := operationTestNow.Add(5 * time.Minute)
	operationClock = func() time.Time { return heartbeatAt }
	progress := startOperationProgress(cfg, h, "ingesting")
	ticks <- heartbeatAt
	deadline := time.Now().Add(time.Second)
	for {
		rec, err := loadOperationRecord(operationPath(cfg, h.kind, h.runID))
		if err == nil && rec.HeartbeatAt == heartbeatAt.Format(time.RFC3339Nano) {
			break
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("periodic heartbeat receipt stayed unreadable: %v", err)
			}
			t.Fatalf("periodic heartbeat did not advance: %+v", rec)
		}
		time.Sleep(time.Millisecond)
	}
	if err := progress.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestIngestActivityMarkedBeforeVisibleAndCompletesAfterCoveredRebuild(t *testing.T) {
	cfg := gate2Vault(t)
	src, _ := writeIngestFixture(t, "alpha")

	origHook := testHookFSPreWrite
	t.Cleanup(func() { testHookFSPreWrite = origHook })
	var sawRunID string
	testHookFSPreWrite = func(string) {
		acts := operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
		a := activityWithKind(acts, operationKindIngest, operationRunning)
		if a == nil || a.Phase != "ingesting" {
			t.Fatalf("pre-visible activities = %+v", acts)
		}
		sawRunID = a.RunID
		journal, err := os.ReadFile(ingestJournalPath(cfg, ingestOperationSourceKey(src)))
		if err != nil || !bytes.Contains(journal, []byte("run "+a.RunID+" ")) {
			t.Fatalf("journal was not durably bound before publish: err=%v body=%q activity=%+v", err, journal, a)
		}
	}

	if _, err := ingestSource(cfg, src, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if sawRunID == "" {
		t.Fatal("pre-visible hook did not observe ingest activity")
	}
	acts := operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
	if a := activityWithKind(acts, operationKindIngest, operationRunning); a == nil || a.Phase != "awaiting_rebuild" {
		t.Fatalf("post-ingest activities = %+v", acts)
	}
	if _, ok := activeOperationProgress.Load(sawRunID); !ok {
		t.Fatal("awaiting-rebuild ingest stopped its bounded heartbeat early")
	}
	if h := healthOf(cfg, operationClock().Add(time.Second)); h.Index.State != idxDirty || h.State != healthUnhealthy {
		t.Fatalf("active ingest weakened dirty health: %+v", h)
	}

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	acts = operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
	a := activityWithKind(acts, operationKindIngest, operationCompleted)
	if a == nil || a.RunID != sawRunID || a.Phase != "journal_retired" {
		t.Fatalf("covered ingest did not complete after journal retirement: %+v", acts)
	}
	if _, ok := activeOperationProgress.Load(sawRunID); ok {
		t.Fatal("covered ingest heartbeat was not stopped")
	}
	if _, err := os.Stat(ingestJournalPath(cfg, ingestOperationSourceKey(src))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal still exists after completed receipt: %v", err)
	}
}

func TestIngestFailureWritesTerminalReceipt(t *testing.T) {
	cfg := gate2Vault(t)
	_, err := ingestSource(cfg, Source{Type: "bogus", Name: "private-source-name"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown source type") {
		t.Fatalf("ingest error = %v", err)
	}
	acts := operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
	a := activityWithKind(acts, operationKindIngest, operationFailed)
	if a == nil || a.FailureCode != "ingest_failed" || a.Phase != "failed" {
		t.Fatalf("failed ingest activity = %+v", acts)
	}
	body, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("private-source-name")) {
		t.Fatalf("failed activity leaked source identity: %s", body)
	}
}

func TestRebuildActivityExistsBeforeListingAndFailureIsTerminal(t *testing.T) {
	cfg := gate2Vault(t)
	orig := listRebuildFiles
	t.Cleanup(func() { listRebuildFiles = orig })
	listRebuildFiles = func(c Config) ([]string, error) {
		acts := operationActivities(c, operationClock().Add(time.Second), func(int) bool { return true })
		a := activityWithKind(acts, operationKindIndexRebuild, operationRunning)
		if a == nil || a.Phase != "listing" {
			t.Fatalf("listing observed no running rebuild receipt: %+v", acts)
		}
		ops, err := listPendingOps(c)
		if err != nil || len(ops) == 0 {
			t.Fatalf("listing observed no pending rebuild marker: ops=%+v err=%v", ops, err)
		}
		return nil, errors.New("injected listing failure")
	}
	if _, err := rebuildIndex(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "injected listing failure") {
		t.Fatalf("rebuild error = %v", err)
	}
	acts := operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
	if a := activityWithKind(acts, operationKindIndexRebuild, operationFailed); a == nil || a.FailureCode != "rebuild_failed" {
		t.Fatalf("failed rebuild activity = %+v", acts)
	}
}

func TestCommittedRebuildCleanupFailureIsPartialNotCompleted(t *testing.T) {
	cfg := gate2Vault(t)
	origRemove, origIndexClock := removePendingOpFile, indexClock
	t.Cleanup(func() { removePendingOpFile, indexClock = origRemove, origIndexClock })
	removePendingOpFile = func(string) error { return errors.New("injected pending cleanup failure") }
	commitAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	indexClock = func() time.Time { return commitAt }

	if _, err := rebuildIndex(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "index committed but pending-marker retirement failed") {
		t.Fatalf("rebuild error = %v", err)
	}
	acts := operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
	a := activityWithKind(acts, operationKindIndexRebuild, operationFailed)
	if a == nil || a.FailureCode != "post_commit_cleanup_failed" {
		t.Fatalf("post-commit activity = %+v", acts)
	}
	if activityWithKind(acts, operationKindIndexRebuild, operationCompleted) != nil {
		t.Fatalf("cleanup failure falsely completed: %+v", acts)
	}
	h := indexHealthOf(cfg, commitAt.Add(time.Second))
	if h.State != idxDirty {
		t.Fatal("failed marker retirement did not preserve dirty health")
	}
	if h.IndexedAt != commitAt.Format(time.RFC3339) {
		t.Fatalf("database commit was not preserved: indexed_at=%q want %q", h.IndexedAt, commitAt.Format(time.RFC3339))
	}
}

func TestJournalRetirementFailureLeavesIngestUncompleted(t *testing.T) {
	cfg := gate2Vault(t)
	src, _ := writeIngestFixture(t, "journal-fail")
	if _, err := ingestSource(cfg, src, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	origRemove := removeIngestJournalFile
	t.Cleanup(func() { removeIngestJournalFile = origRemove })
	removeIngestJournalFile = func(string) error { return errors.New("injected journal cleanup failure") }

	if _, err := rebuildIndex(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "index committed but ingest-journal retirement failed") {
		t.Fatalf("rebuild error = %v", err)
	}
	acts := operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
	if activityWithKind(acts, operationKindIngest, operationCompleted) != nil {
		t.Fatalf("ingest falsely completed before journal retirement: %+v", acts)
	}
	if activityWithKind(acts, operationKindIngest, operationRunning) == nil {
		t.Fatalf("ingest did not remain awaiting recovery: %+v", acts)
	}
	if a := activityWithKind(acts, operationKindIndexRebuild, operationFailed); a == nil || a.FailureCode != "post_commit_cleanup_failed" {
		t.Fatalf("rebuild did not report partial failure: %+v", acts)
	}
	removeIngestJournalFile = origRemove
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("cleanup rebuild: %v", err)
	}
}

func TestConcurrentIngestActivitiesAreAnonymous(t *testing.T) {
	cfg := gate2Vault(t)
	srcA, pathA := writeIngestFixture(t, "private-alpha")
	srcB, pathB := writeIngestFixture(t, "private-beta")

	origHook := testHookIngestActivityStarted
	t.Cleanup(func() { testHookIngestActivityStarted = origHook })
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	testHookIngestActivityStarted = func(Config, Source, operationHandle) {
		entered <- struct{}{}
		<-release
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, src := range []Source{srcA, srcB} {
		wg.Add(1)
		go func(s Source) {
			defer wg.Done()
			_, err := ingestSource(cfg, s, &bytes.Buffer{})
			errCh <- err
		}(src)
	}
	<-entered
	<-entered
	acts := operationActivities(cfg, operationClock().Add(time.Second), func(int) bool { return true })
	running := 0
	for _, a := range acts {
		if a.Kind == operationKindIngest && a.State == operationRunning {
			running++
		}
	}
	if running != 2 {
		t.Fatalf("running concurrent ingests = %d, activities=%+v", running, acts)
	}
	body, err := json.Marshal(acts)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{srcA.Name, srcB.Name, pathA, pathB, srcA.Path, srcB.Path} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("activity projection leaked source identity %q: %s", secret, body)
		}
	}
	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("cleanup rebuild: %v", err)
	}
}

func TestDoctorStrictFailsDuringActiveIngest(t *testing.T) {
	withTempHome(t)
	origDoctorClock := doctorClock
	doctorClock = func() time.Time { return time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC) }
	t.Cleanup(func() { doctorClock = origDoctorClock })
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src, _ := writeIngestFixture(t, "strict-active")
	if _, err := ingestSource(cfg, src, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"doctor", "--json", "--strict"}, &out, &out, strings.NewReader("")); err == nil {
		t.Fatalf("strict doctor succeeded during active dirty ingest: %s", out.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("doctor JSON: %v\n%s", err, out.String())
	}
	if rep.Healthy || activityWithKind(rep.Activities, operationKindIngest, operationRunning) == nil || rep.Index.State != idxDirty {
		t.Fatalf("doctor report = %+v", rep)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("cleanup rebuild: %v", err)
	}
}

func TestUnmarkIndexDirtyRetriesTransientRemoval(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	opID := "op_retry"
	path := pendingOpPath(cfg, opID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	transient := errors.New("transient sharing violation")
	origRemove, origRetryable := removePendingOpFile, leaseRemovalRetryableFn
	t.Cleanup(func() { removePendingOpFile, leaseRemovalRetryableFn = origRemove, origRetryable })
	calls := 0
	removePendingOpFile = func(got string) error {
		calls++
		if calls == 1 {
			return transient
		}
		return os.Remove(got)
	}
	leaseRemovalRetryableFn = func(err error) bool { return errors.Is(err, transient) }
	if err := unmarkIndexDirty(cfg, opID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("remove calls = %d, want 2", calls)
	}
}
