package mora

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReproduceIngestCompletedOperationWithProducerStampFailure reproduces the exact issue #329
// scenario: a completed `ingest run --all` whose operation receipt advances to completed while
// the producer stamp fails. It asserts that the command surfaces a non-zero exit and preserves
// the stamp failure in actionable health output.
func TestReproduceIngestCompletedOperationWithProducerStampFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// Set up a filesystem source with one file to ingest.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "note.md"), []byte("# Note\nContent"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "connect", "filesystem", srcDir)

	t0 := time.Date(2026, 8, 13, 6, 15, 0, 0, time.UTC)
	setProducerClock(t, t0)

	// Adopt the producer expectation.
	if err := saveExpectedProducers(cfg, map[string]expectedProducer{
		"ingest-hourly": {Name: "ingest-hourly", IntervalSeconds: 3600, Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}

	// Inject a stamp failure into mutateProducersFn to simulate lock failure/disk error during stamping.
	oldMutate := mutateProducersFn
	t.Cleanup(func() { mutateProducersFn = oldMutate })
	mutateProducersFn = func(cfg Config, mutate func(map[string]producerStatus) error) error {
		return errors.New("simulated producer status disk I/O error")
	}

	var out bytes.Buffer
	runErr := Run(context.Background(), []string{"ingest", "run", "--all"}, &out, &out, nil)
	if runErr == nil {
		t.Fatal("ingest run --all must return non-zero error when producer stamping fails")
	}
	if !strings.Contains(runErr.Error(), "producer stamp for ingest-hourly failed") {
		t.Fatalf("expected producer stamp failure in error, got: %v", runErr)
	}

	// Verify that the operation activity receipt completed.
	ops := operationActivities(cfg, t0.Add(time.Second), func(int) bool { return true })
	var ingestOp *operationActivity
	for i := range ops {
		if ops[i].Kind == operationKindIngest {
			ingestOp = &ops[i]
			break
		}
	}
	if ingestOp == nil {
		t.Fatal("expected ingest operation activity record")
	}
	if ingestOp.State != operationCompleted {
		t.Fatalf("expected ingest operation to be completed, got state %q", ingestOp.State)
	}

	// Verify that the stamp failure is preserved in actionable health output.
	health := producerHealthAll(cfg, t0.Add(5*time.Minute))
	var foundHealth bool
	for _, ph := range health {
		if ph.Name == "ingest-hourly" {
			foundHealth = true
			if ph.State != prodFailed {
				t.Fatalf("expected ingest-hourly state to be %q, got %q", prodFailed, ph.State)
			}
			if !strings.Contains(ph.LastError, "simulated producer status disk I/O error") {
				t.Fatalf("expected actionable stamp error in health, got %q", ph.LastError)
			}
		}
	}
	if !foundHealth {
		t.Fatal("expected ingest-hourly in producerHealthAll output")
	}
}

// TestSuccessfulCompletedIngestAdvancesProducerRecord tests that a normal successful ingest
// advances the producer record and produces fresh health.
func TestSuccessfulCompletedIngestAdvancesProducerRecord(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "note.md"), []byte("# Note\nContent"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "connect", "filesystem", srcDir)

	t0 := time.Date(2026, 8, 13, 6, 15, 0, 0, time.UTC)
	setProducerClock(t, t0)

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"ingest", "run", "--all"}, &out, &out, nil); err != nil {
		t.Fatalf("ingest run --all failed: %v", err)
	}

	st, err := loadProducerStatus(cfg)
	if err != nil {
		t.Fatalf("loadProducerStatus: %v", err)
	}
	ps, ok := st["ingest-hourly"]
	if !ok {
		t.Fatal("ingest-hourly producer status missing")
	}
	if ps.LastAttemptAt != t0.Format(time.RFC3339) || ps.LastSuccessAt != t0.Format(time.RFC3339) {
		t.Fatalf("unexpected timestamps: attempt=%q success=%q want=%q", ps.LastAttemptAt, ps.LastSuccessAt, t0.Format(time.RFC3339))
	}
	if ps.LastError != "" {
		t.Fatalf("unexpected LastError: %q", ps.LastError)
	}

	health := producerHealthAll(cfg, t0.Add(10*time.Minute))
	for _, ph := range health {
		if ph.Name == "ingest-hourly" && ph.State != prodFresh {
			t.Fatalf("expected ingest-hourly to be fresh, got %q", ph.State)
		}
	}
}

// TestSignedAppScheduledIngestRejectsStaleProducerReceipt tests that a receipt from prior to
// invocation launch time is rejected even if last_attempt_at == last_success_at.
func TestSignedAppScheduledIngestRejectsStaleProducerReceipt(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}

	// Producer status has a 2-hour-old receipt where last_attempt_at == last_success_at.
	staleTime := "2026-08-13T04:15:40Z"
	if err := saveProducerStatus(cfg, map[string]producerStatus{
		"ingest-hourly": {
			Name:          "ingest-hourly",
			LastAttemptAt: staleTime,
			LastSuccessAt: staleTime,
		},
	}); err != nil {
		t.Fatal(err)
	}

	launchTime, err := time.Parse(time.RFC3339, "2026-08-13T06:14:21Z")
	if err != nil {
		t.Fatal(err)
	}

	// Validating against launchTime must reject the stale receipt.
	verr := validateProducerReceipt(cfg, "ingest-hourly", launchTime)
	if verr == nil {
		t.Fatal("expected validateProducerReceipt to reject stale receipt")
	}
	if !strings.Contains(verr.Error(), "is older than invocation launch time") {
		t.Fatalf("expected error mentioning older than launch time, got: %v", verr)
	}
}

// TestSignedAppScheduledIngestRejectsUnrelatedProducerChange tests that if status.json changes
// during the run because of an unrelated producer, but ingest-hourly remains stale, it is rejected.
func TestSignedAppScheduledIngestRejectsUnrelatedProducerChange(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}

	staleTime := "2026-08-13T04:15:40Z"
	unrelatedTime := "2026-08-13T06:14:30Z"
	if err := saveProducerStatus(cfg, map[string]producerStatus{
		"ingest-hourly": {
			Name:          "ingest-hourly",
			LastAttemptAt: staleTime,
			LastSuccessAt: staleTime,
		},
		"index-hourly": {
			Name:          "index-hourly",
			LastAttemptAt: unrelatedTime,
			LastSuccessAt: unrelatedTime,
		},
	}); err != nil {
		t.Fatal(err)
	}

	launchTime, err := time.Parse(time.RFC3339, "2026-08-13T06:14:21Z")
	if err != nil {
		t.Fatal(err)
	}

	verr := validateProducerReceipt(cfg, "ingest-hourly", launchTime)
	if verr == nil {
		t.Fatal("expected validateProducerReceipt to reject stale ingest-hourly receipt even when index-hourly updated")
	}
	if !strings.Contains(verr.Error(), "is older than invocation launch time") {
		t.Fatalf("expected older than launch time error, got: %v", verr)
	}
}

// TestSignedAppIngestTokenReceiptRelay tests the token-bound receipt mechanism for ingest.
func TestSignedAppIngestTokenReceiptRelay(t *testing.T) {
	oldExe, oldOpen, oldGOOS, oldNow := protectedSyncExecutable, protectedSyncRunOpen, runtimeGOOS, protectedSyncNow
	t.Cleanup(func() {
		protectedSyncExecutable, protectedSyncRunOpen, runtimeGOOS, protectedSyncNow = oldExe, oldOpen, oldGOOS, oldNow
	})
	runtimeGOOS = func() string { return "darwin" }
	app := filepath.Join(t.TempDir(), "Mora.app")
	exe := filepath.Join(app, "Contents", "MacOS", "mora")
	protectedSyncExecutable = func() (string, error) { return exe, nil }
	launchTime := time.Date(2026, 8, 13, 6, 15, 0, 0, time.UTC)
	protectedSyncNow = func() time.Time { return launchTime }

	cfg := Config{StateDir: t.TempDir()}

	// Case 1: Clean run with fresh token receipt.
	protectedSyncRunOpen = func(_ context.Context, args ...string) error {
		token := args[len(args)-1]
		return writeProtectedSyncReceipt(cfg, protectedSyncReceipt{
			Token:       token,
			Source:      "ingest-hourly",
			CompletedAt: launchTime.UTC().Format(time.RFC3339),
		})
	}
	if err := relayProtectedIngest(context.Background(), cfg, true, ""); err != nil {
		t.Fatalf("relayProtectedIngest failed: %v", err)
	}

	// Case 2: Ingest failed in child process and left an error receipt.
	protectedSyncRunOpen = func(_ context.Context, args ...string) error {
		token := args[len(args)-1]
		return writeProtectedSyncReceipt(cfg, protectedSyncReceipt{
			Token:       token,
			Source:      "ingest-hourly",
			CompletedAt: launchTime.UTC().Format(time.RFC3339),
			Error:       "producer stamp for ingest-hourly failed",
		})
	}
	if err := relayProtectedIngest(context.Background(), cfg, true, ""); err == nil || !strings.Contains(err.Error(), "producer stamp for ingest-hourly failed") {
		t.Fatalf("expected error from failed receipt, got: %v", err)
	}

	// Case 3: Stale receipt returned from before launch time.
	protectedSyncRunOpen = func(_ context.Context, args ...string) error {
		token := args[len(args)-1]
		return writeProtectedSyncReceipt(cfg, protectedSyncReceipt{
			Token:       token,
			Source:      "ingest-hourly",
			CompletedAt: "2026-08-13T04:15:40Z",
		})
	}
	if err := relayProtectedIngest(context.Background(), cfg, true, ""); err == nil || !strings.Contains(err.Error(), "older than invocation launch time") {
		t.Fatalf("expected stale receipt error, got: %v", err)
	}

	// Case 4: cmdIngest with --mora-app-receipt flag writes token receipt.
	withTempHome(t)
	run(t, "init")
	cfgCmd := mustConfig(t)
	setProducerClock(t, launchTime)
	token, _ := newProtectedSyncToken()
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"ingest", "run", "--all", protectedSyncReceiptFlag, token}, &out, &out, nil); err != nil {
		t.Fatalf("ingest run with receipt flag failed: %v", err)
	}
	r, err := readProtectedSyncReceipt(cfgCmd, token, "ingest-hourly", launchTime)
	if err != nil {
		t.Fatalf("readProtectedSyncReceipt: %v", err)
	}
	if r.Token != token || r.Source != "ingest-hourly" || r.Error != "" {
		t.Fatalf("unexpected receipt: %+v", r)
	}
}

// TestTokenlessScheduledIngestRelaysThroughSignedApp proves the host command
// starts an app child exactly once and accepts only that child receipt. The child
// carries the token, so it executes directly instead of recursively relaying.
func TestTokenlessScheduledIngestRelaysThroughSignedApp(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	oldExe, oldOpen, oldGOOS, oldNow := protectedSyncExecutable, protectedSyncRunOpen, runtimeGOOS, protectedSyncNow
	t.Cleanup(func() {
		protectedSyncExecutable, protectedSyncRunOpen, runtimeGOOS, protectedSyncNow = oldExe, oldOpen, oldGOOS, oldNow
	})
	runtimeGOOS = func() string { return "darwin" }
	app := filepath.Join(t.TempDir(), "Mora.app")
	exe := filepath.Join(app, "Contents", "MacOS", "mora")
	protectedSyncExecutable = func() (string, error) { return exe, nil }
	launchTime := time.Date(2026, 8, 13, 6, 15, 0, 0, time.UTC)
	protectedSyncNow = func() time.Time { return launchTime }
	setProducerClock(t, launchTime)

	launches := 0
	protectedSyncRunOpen = func(ctx context.Context, args ...string) error {
		launches++
		childStart := -1
		for i, arg := range args {
			if arg == "--args" {
				childStart = i + 1
				break
			}
		}
		if childStart < 0 {
			return errors.New("missing app child arguments")
		}
		return Run(ctx, args[childStart:], io.Discard, io.Discard, nil)
	}

	if err := Run(context.Background(), []string{"ingest", "run", "--all"}, io.Discard, io.Discard, nil); err != nil {
		t.Fatalf("tokenless scheduled ingest relay failed: %v", err)
	}
	if launches != 1 {
		t.Fatalf("signed-app launches = %d, want exactly one (the token child must not relay)", launches)
	}
	status, err := loadProducerStatus(cfg)
	if err != nil {
		t.Fatalf("load producer status: %v", err)
	}
	if got := status["ingest-hourly"]; got.LastAttemptAt != launchTime.Format(time.RFC3339) || got.LastSuccessAt != launchTime.Format(time.RFC3339) {
		t.Fatalf("child producer receipt = %+v, want current successful invocation", got)
	}
}
