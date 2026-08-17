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

	"github.com/pyranthus-hq/mora/internal/memory"
)

// mora_pulse_test.go — Plan 13-03 integration wiring:
//   - pulse --sync (D13-4: sync-first refresh BEFORE buildDigest; honest,
//     NON-aborting errors that surface via the existing three-state)
//   - pulse --brief-file (D13-5: persist the dated vault artifact, off by default)
//   - pulse --notify (D13-5: post the gated toast, off by default)
//   - scheduleCommands["pulse-daily"] durable wrapper routing
//
// The two backfills (backfillGoogleFn / backfillIMessageFn) are package-level func
// vars so a test can swap them with t.Cleanup-restore — NO real network, NO
// t.Parallel (the seam is a shared global; per the cross-model guidance these
// tests must never run in parallel while they mutate the package var).

// errBackfillStub is the sentinel a stubbed backfill returns to prove cmdPulse
// swallows a sync error (never aborts the brief).
var errBackfillStub = errors.New("backfill stub failure")

// TestPulseSyncFirstRunsRefreshBeforeBuild (D13-4): with --sync (delta mode), the
// enabled-source refresh runs BEFORE the digest build, so the brief reflects
// post-sync data. We assert ordering: google then imessage (cmdSync's order) and
// that the build still produces a brief afterwards.
func TestPulseSyncFirstRunsRefreshBeforeBuild(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Quarterly review", 2*time.Hour, now)

	var calls []string
	origG, origI := backfillGoogleFn, backfillIMessageFn
	t.Cleanup(func() { backfillGoogleFn, backfillIMessageFn = origG, origI })
	backfillGoogleFn = func(ctx context.Context, c Config, w io.Writer) (int, error) {
		calls = append(calls, "google")
		return 0, nil
	}
	backfillIMessageFn = func(ctx context.Context, c Config, w io.Writer) (int, error) {
		calls = append(calls, "imessage")
		return 0, nil
	}

	out := runPulse(t, "--digest", "--sync")
	if len(calls) != 2 || calls[0] != "google" || calls[1] != "imessage" {
		t.Fatalf("--sync must refresh google then imessage before the build; got call order %v", calls)
	}
	if !strings.Contains(out, "Mora digest") {
		t.Fatalf("--sync must still render the digest; got:\n%s", out)
	}
}

// TestPulseSyncFirstNoSyncSkipsRefresh: without --sync, ad-hoc pulse does NOT
// refresh any source (the backfills are never called).
func TestPulseSyncFirstNoSyncSkipsRefresh(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Weekly sync", 1*time.Hour, now)

	called := false
	origG, origI := backfillGoogleFn, backfillIMessageFn
	t.Cleanup(func() { backfillGoogleFn, backfillIMessageFn = origG, origI })
	backfillGoogleFn = func(ctx context.Context, c Config, w io.Writer) (int, error) { called = true; return 0, nil }
	backfillIMessageFn = func(ctx context.Context, c Config, w io.Writer) (int, error) { called = true; return 0, nil }

	runPulse(t, "--digest")
	if called {
		t.Fatalf("pulse --digest without --sync must NOT refresh any source")
	}
}

// TestPulseHonestErrorDoesNotAbortAndSurfacesUnavailable (D13-4 / T-13-09): a
// backfill error must NOT abort the brief — cmdPulse returns nil and still renders
// — and the failed source surfaces as "unavailable (sync error)" via the EXISTING
// three-state (the backfill recorded the error into SyncStatus). No parallel error
// channel.
func TestPulseHonestErrorDoesNotAbortAndSurfacesUnavailable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	// Start healthy so the ONLY way it reads unavailable is the recorded error.
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Board deck", 1*time.Hour, now)

	origG, origI := backfillGoogleFn, backfillIMessageFn
	t.Cleanup(func() { backfillGoogleFn, backfillIMessageFn = origG, origI })
	// The failing backfill records the error into SyncStatus (mirroring the real
	// ingest path) and returns an error — cmdPulse must swallow it.
	backfillGoogleFn = func(ctx context.Context, c Config, w io.Writer) (int, error) {
		seedSyncStatusFull(t, c, "gmail", &memory.SyncStatus{
			Source:        "gmail",
			LastSynced:    now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
			LastAttemptAt: now.UTC().Format(time.RFC3339),
			LastError:     "boom: transient network failure",
			ErrorCount:    1,
		})
		return 0, errBackfillStub
	}
	backfillIMessageFn = func(ctx context.Context, c Config, w io.Writer) (int, error) { return 0, nil }

	// Must NOT abort: runPulse fails the test if Run returns a non-nil error.
	out := runPulse(t, "--digest", "--sync")
	if !strings.Contains(out, "Mora digest") {
		t.Fatalf("a sync error must NOT abort the brief; got:\n%s", out)
	}
	if !strings.Contains(out, "unavailable (sync error)") {
		t.Fatalf("a failed source must surface as unavailable via the existing three-state; got:\n%s", out)
	}
}

// briefsDir is the dated-artifact directory writeBriefArtifact targets.
func briefsDir(cfg Config) string { return filepath.Join(cfg.VaultDir, "briefs") }

// countBriefFiles returns how many *-brief.md files exist under briefs/.
func countBriefFiles(t *testing.T, cfg Config) int {
	t.Helper()
	entries, err := os.ReadDir(briefsDir(cfg))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read briefs dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-brief.md") {
			n++
		}
	}
	return n
}

// TestPulseBriefFileWritesExactlyOneArtifact (D13-5 / SC#2): --brief-file with
// --digest persists exactly one briefs/<date>-brief.md.
func TestPulseBriefFileWritesExactlyOneArtifact(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Quarterly review", 1*time.Hour, now)

	runPulse(t, "--digest", "--brief-file")
	if got := countBriefFiles(t, cfg); got != 1 {
		t.Fatalf("--brief-file must write exactly one briefs/<date>-brief.md; got %d", got)
	}
	// The artifact body must equal the human brief render (one source of truth).
	want := filepath.Join(briefsDir(cfg), now.UTC().Format("2006-01-02")+"-brief.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected dated artifact at %s: %v", want, err)
	}
}

// TestPulseNotifyRoutesThroughInjectedNotifier (D13-5 / SC#3): --notify (with a
// persisted brief) routes through the notify seam, called with the brief path.
// The seam is injected so NO real osascript spawns.
func TestPulseNotifyRoutesThroughInjectedNotifier(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Board deck", 1*time.Hour, now)

	var gotPath string
	called := false
	orig := notifyBriefFn
	t.Cleanup(func() { notifyBriefFn = orig })
	notifyBriefFn = func(path string, _ *urgentNote) error { called = true; gotPath = path; return nil }

	runPulse(t, "--digest", "--brief-file", "--notify")
	if !called {
		t.Fatalf("--notify with a persisted brief must call the notifier")
	}
	wantPath := filepath.Join(briefsDir(cfg), now.UTC().Format("2006-01-02")+"-brief.md")
	if gotPath != wantPath {
		t.Fatalf("notifier must be called with the brief path; got %q want %q", gotPath, wantPath)
	}
}

// TestPulseNotifySuppressedWithoutFlag (D13-5): --notify NOT set ⇒ the notifier is
// never called, even when --brief-file persisted a brief.
func TestPulseNotifySuppressedWithoutFlag(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Standup", 1*time.Hour, now)

	called := false
	orig := notifyBriefFn
	t.Cleanup(func() { notifyBriefFn = orig })
	notifyBriefFn = func(path string, _ *urgentNote) error { called = true; return nil }

	runPulse(t, "--digest", "--brief-file")
	if called {
		t.Fatalf("--brief-file without --notify must NOT call the notifier")
	}
}

// TestPulseBriefFilePersistErrorIsNonFatal (D13-5 / T-13-12): a persist error must
// not abort the brief — cmdPulse still renders and returns nil. We force the error
// by making the brief path un-writable (briefs/ is a file, not a dir).
func TestPulseBriefFilePersistErrorIsNonFatal(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Retro", 1*time.Hour, now)

	// Make briefs/ a FILE so MkdirAll(briefs) inside writeBriefArtifact fails.
	if err := os.WriteFile(briefsDir(cfg), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}
	// A notifier must NOT be called when persist failed (no path to point at).
	called := false
	orig := notifyBriefFn
	t.Cleanup(func() { notifyBriefFn = orig })
	notifyBriefFn = func(path string, _ *urgentNote) error { called = true; return nil }

	// runPulse fails the test if Run returns non-nil — proves non-fatal.
	out := runPulse(t, "--digest", "--brief-file", "--notify")
	if !strings.Contains(out, "Mora digest") {
		t.Fatalf("a persist error must not abort the brief; got:\n%s", out)
	}
	if called {
		t.Fatalf("notify must be suppressed when persist failed (no brief path)")
	}
}

// TestPulseDefaultsOffNoPersistNoNotify (D13-5 off-by-default contract): plain
// `pulse --digest` (no new flags) persists NOTHING and notifies NOTHING.
func TestPulseDefaultsOffNoPersistNoNotify(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Sprint", 1*time.Hour, now)

	called := false
	orig := notifyBriefFn
	t.Cleanup(func() { notifyBriefFn = orig })
	notifyBriefFn = func(path string, _ *urgentNote) error { called = true; return nil }

	runPulse(t, "--digest")
	if got := countBriefFiles(t, cfg); got != 0 {
		t.Fatalf("pulse --digest (off by default) must persist NO brief; got %d", got)
	}
	if called {
		t.Fatalf("pulse --digest (off by default) must notify NOTHING")
	}
}

// TestScheduleCommandPulseDailyUsesDurableWrapper (issue #58): the OS scheduler
// invokes the loop-aware wrapper, never pulse --advance directly. The wrapper
// owns the exact advancing flags in-process after it has a durable run id.
func TestScheduleCommandPulseDailyUsesDurableWrapper(t *testing.T) {
	const want = "schedule run pulse-daily"
	if got := scheduleCommands["pulse-daily"]; got != want {
		t.Fatalf("pulse-daily command string\n got: %q\nwant: %q", got, want)
	}
	// Each wrapper token renders as its own <string> arg in the plist.
	args := plistArgs(want)
	for _, token := range []string{"schedule", "run", "pulse-daily"} {
		if !strings.Contains(args, "<string>"+token+"</string>") {
			t.Fatalf("plistArgs must render %q as its own <string>; got:\n%s", token, args)
		}
	}
	if strings.Contains(args, "--advance") {
		t.Fatalf("OS schedule must not bypass the durable wrapper with direct --advance: %s", args)
	}
	legacy := []string{"--write", "--digest", "--advance", "--sync", "--brief-file", "--notify"}
	if !legacyPulseDailyInvocation(legacy) {
		t.Fatal("the exact pre-wrapper pulse-daily argv must route through the compatibility bridge")
	}
	if legacyPulseDailyInvocation(append(append([]string{}, legacy...), "--envelope")) {
		t.Fatal("an ad-hoc pulse with extra flags must not be mistaken for an installed legacy schedule")
	}
	reordered := append([]string{}, legacy...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if legacyPulseDailyInvocation(reordered) {
		t.Fatal("only the exact historical scheduler argv should activate compatibility routing")
	}
	// No OTHER scheduled job string changed (pin them).
	others := map[string]string{
		"index-hourly":  "index rebuild",
		"backup-daily":  "backup",
		"lint-weekly":   "lint",
		"ingest-hourly": "ingest run --all",
	}
	for job, exp := range others {
		if got := scheduleCommands[job]; got != exp {
			t.Fatalf("scheduled job %q must be unchanged; got %q want %q", job, got, exp)
		}
	}
}

// TestInstalledLegacyPulseDailyRoutesThroughDurableGate proves an existing
// pre-upgrade scheduler entry is repaired by the upgraded binary itself. Its
// exact old pulse argv enters the wrapper, succeeds once, and a same-day second
// fire skips before consuming a newly-arrived item or rewriting the artifact.
func TestInstalledLegacyPulseDailyRoutesThroughDurableGate(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now().UTC().Truncate(time.Second)
	seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
	digestSeed(t, cfg, "gmail", "Legacy scheduled first item", time.Hour, now)

	origLoopClock, origBriefClock := loopClock, briefClock
	origG, origI, origNotify := backfillGoogleFn, backfillIMessageFn, notifyBriefFn
	t.Cleanup(func() {
		loopClock, briefClock = origLoopClock, origBriefClock
		backfillGoogleFn, backfillIMessageFn, notifyBriefFn = origG, origI, origNotify
	})
	loopClock = func() time.Time { return now }
	briefClock = func() time.Time { return now }
	backfillGoogleFn = func(context.Context, Config, io.Writer) (int, error) { return 0, nil }
	backfillIMessageFn = func(context.Context, Config, io.Writer) (int, error) { return 0, nil }
	notifyBriefFn = func(string, *urgentNote) error { return nil }

	legacyArgs := []string{"pulse", "--write", "--digest", "--advance", "--sync", "--brief-file", "--notify"}
	firstOut, err := runErr(t, legacyArgs...)
	if err != nil {
		t.Fatalf("first installed legacy fire: %v\n%s", err, firstOut)
	}
	rec, ok := loadRunRecord(cfg, "daily-brief")
	if !ok || rec.Status != loopRunSucceeded || rec.Attempt != 1 {
		t.Fatalf("legacy fire record = %+v, want first-attempt success", rec)
	}
	artifact := filepath.Join(briefsDir(cfg), now.Format("2006-01-02")+"-brief.md")
	firstArtifact, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("legacy fire did not persist artifact: %v", err)
	}
	journalBefore := readJournal(t, cfg, "daily-brief")
	if len(journalBefore) != 1 {
		t.Fatalf("legacy fire journal = %v, want one terminal", journalBefore)
	}

	// If the second invocation bypassed the wrapper, this fresh delta would be
	// consumed and today's artifact would change. The durable gate must stop it.
	digestSeed(t, cfg, "gmail", "Must remain unread after duplicate fire", 30*time.Minute, now)
	secondOut, err := runErr(t, legacyArgs...)
	if err != nil {
		t.Fatalf("duplicate installed legacy fire: %v\n%s", err, secondOut)
	}
	secondArtifact, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondArtifact, firstArtifact) {
		t.Fatal("duplicate legacy scheduler fire bypassed the gate and rewrote today's artifact")
	}
	if got := readJournal(t, cfg, "daily-brief"); len(got) != len(journalBefore) {
		t.Fatalf("duplicate legacy fire appended a terminal: before=%v after=%v", journalBefore, got)
	}
	current, _ := loadRunRecord(cfg, "daily-brief")
	if current.RunID != rec.RunID || current.Attempt != 1 {
		t.Fatalf("duplicate legacy fire opened another attempt: before=%+v after=%+v", rec, current)
	}
	if !strings.Contains(secondOut, "already succeeded") {
		t.Fatalf("duplicate legacy fire did not report durable skip: %q", secondOut)
	}
}

// TestScheduledPulseDailyDurableLifecycle is the end-to-end issue-58 witness:
// the first scheduler fire begins, advances, persists, and closes succeeded;
// a duplicate same-day fire exits successfully at the loop gate without a
// second pulse, artifact rewrite, watermark consumption, or journal terminal.
func TestScheduledPulseDailyDurableLifecycle(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now().UTC().Truncate(time.Second)
	seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
	digestSeed(t, cfg, "gmail", "Durable scheduler item", time.Hour, now)

	origLoopClock, origBriefClock := loopClock, briefClock
	origG, origI, origNotify := backfillGoogleFn, backfillIMessageFn, notifyBriefFn
	origMarkerSync, origDirSync := markerSyncFn, syncDirFn
	t.Cleanup(func() {
		loopClock, briefClock = origLoopClock, origBriefClock
		backfillGoogleFn, backfillIMessageFn, notifyBriefFn = origG, origI, origNotify
		markerSyncFn, syncDirFn = origMarkerSync, origDirSync
	})
	loopClock = func() time.Time { return now }
	briefClock = func() time.Time { return now }
	backfillGoogleFn = func(context.Context, Config, io.Writer) (int, error) { return 0, nil }
	backfillIMessageFn = func(context.Context, Config, io.Writer) (int, error) { return 0, nil }
	notifyBriefFn = func(string, *urgentNote) error { return nil }
	var durabilityTrace []string
	classifyDurableDir := func(dir string) string {
		dir = filepath.Clean(dir)
		switch {
		case strings.HasPrefix(dir, filepath.Join(cfg.StateDir, "loops")+string(filepath.Separator)):
			return "loop"
		case dir == filepath.Join(cfg.VaultDir, "briefs"):
			return "artifact"
		case dir == filepath.Join(cfg.StateDir, "brief"):
			return "watermark"
		default:
			return "other"
		}
	}
	markerSyncFn = func(f *os.File) error {
		durabilityTrace = append(durabilityTrace, classifyDurableDir(filepath.Dir(f.Name()))+":fsync")
		return origMarkerSync(f)
	}
	syncDirFn = func(dir string) error {
		durabilityTrace = append(durabilityTrace, classifyDurableDir(dir)+":dirsync")
		return origDirSync(dir)
	}

	var first bytes.Buffer
	if err := runScheduledPulseDaily(context.Background(), cfg, &first, testStderr); err != nil {
		t.Fatalf("first scheduled run: %v\n%s", err, first.String())
	}
	wantDurability := "loop:fsync,loop:dirsync,artifact:fsync,artifact:dirsync,watermark:fsync,watermark:dirsync,loop:fsync,loop:dirsync,loop:fsync,loop:dirsync"
	if got := strings.Join(durabilityTrace, ","); got != wantDurability {
		t.Fatalf("scheduled durability trace = %q\nwant %q (intent -> artifact -> watermark -> commit -> terminal)", got, wantDurability)
	}
	rec, ok := loadRunRecord(cfg, "daily-brief")
	if !ok || rec.Status != loopRunSucceeded {
		t.Fatalf("scheduled run record = %+v, want succeeded", rec)
	}
	if !briefSnapshotExists(cfg, "gmail") {
		t.Fatal("scheduled wrapper did not advance the watermark")
	}
	artifact := filepath.Join(briefsDir(cfg), now.Format("2006-01-02")+"-brief.md")
	firstArtifact, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("scheduled wrapper did not persist artifact: %v", err)
	}
	journalBefore := readJournal(t, cfg, "daily-brief")
	if len(journalBefore) != 1 {
		t.Fatalf("first scheduled run journal = %v, want one terminal", journalBefore)
	}

	var second bytes.Buffer
	if err := runScheduledPulseDaily(context.Background(), cfg, &second, testStderr); err != nil {
		t.Fatalf("duplicate scheduled run: %v\n%s", err, second.String())
	}
	secondArtifact, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondArtifact, firstArtifact) {
		t.Fatal("duplicate scheduled fire rewrote today's artifact")
	}
	if got := readJournal(t, cfg, "daily-brief"); len(got) != len(journalBefore) {
		t.Fatalf("duplicate scheduled fire added a terminal: before=%v after=%v", journalBefore, got)
	}
	if !strings.Contains(second.String(), "already succeeded") {
		t.Fatalf("duplicate scheduled fire did not report durable skip: %q", second.String())
	}
}

// TestAdvancingPulseRefusesWrongLoopOwner proves the pulse-level commit fence is
// load-bearing independently of scheduler wiring: a stale run id is rejected
// before any watermark snapshot can be written.
func TestAdvancingPulseRefusesWrongLoopOwner(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now().UTC().Truncate(time.Second)
	seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
	digestSeed(t, cfg, "gmail", "Must not advance", time.Hour, now)
	var begin bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, now, &begin); err != nil {
		t.Fatal(err)
	}

	_, err := runErr(t, "pulse", "--digest", "--advance", "--loop", "daily-brief", "--loop-run", "run_stale")
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("stale fenced pulse error = %v, want superseded refusal", err)
	}
	if briefSnapshotExists(cfg, "gmail") {
		t.Fatal("stale fenced pulse advanced the watermark")
	}
}

func TestAdvancingPulseRefusesRunFromPreviousPeriod(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	beforeMidnight := time.Date(2026, 7, 16, 23, 59, 59, 0, time.UTC)
	afterMidnight := beforeMidnight.Add(2 * time.Second)
	seedSyncStatus(t, cfg, "gmail", beforeMidnight.Add(-time.Hour))
	digestSeed(t, cfg, "gmail", "Must stay for the new period", time.Hour, beforeMidnight)
	var begin bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, beforeMidnight, &begin); err != nil {
		t.Fatal(err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	origBriefClock, origLoopClock := briefClock, loopClock
	t.Cleanup(func() { briefClock, loopClock = origBriefClock, origLoopClock })
	briefClock = func() time.Time { return afterMidnight }
	loopClock = func() time.Time { return afterMidnight }

	_, err := runErr(t, "pulse", "--digest", "--advance", "--brief-file", "--loop", "daily-brief", "--loop-run", rec.RunID)
	if err == nil || !strings.Contains(err.Error(), "refusing cross-period advance") {
		t.Fatalf("cross-period fenced pulse = %v, want refusal", err)
	}
	if briefSnapshotExists(cfg, "gmail") {
		t.Fatal("cross-period fenced pulse advanced the watermark")
	}
	if _, statErr := os.Stat(briefArtifactPath(cfg, afterMidnight)); !os.IsNotExist(statErr) {
		t.Fatalf("cross-period fenced pulse wrote new-period artifact: %v", statErr)
	}
	after, _ := loadRunRecord(cfg, "daily-brief")
	if after.EffectStartedAt != "" || after.EffectCommittedAt != "" {
		t.Fatalf("cross-period refusal wrote effect evidence: %+v", after)
	}
}
