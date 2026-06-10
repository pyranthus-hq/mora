package mora

import (
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
//   - scheduleCommands["pulse-daily"] string update (sync-first + persist + notify)
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
	notifyBriefFn = func(path string) error { called = true; gotPath = path; return nil }

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
	notifyBriefFn = func(path string) error { called = true; return nil }

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
	notifyBriefFn = func(path string) error { called = true; return nil }

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
	notifyBriefFn = func(path string) error { called = true; return nil }

	runPulse(t, "--digest")
	if got := countBriefFiles(t, cfg); got != 0 {
		t.Fatalf("pulse --digest (off by default) must persist NO brief; got %d", got)
	}
	if called {
		t.Fatalf("pulse --digest (off by default) must notify NOTHING")
	}
}

// TestScheduleCommandPulseDailyUpdatedString (D13-5 / T-13-10): the pulse-daily
// command string is exactly the sync-first+persist+notify value, plistArgs renders
// each flag as its own <string>, and NO other scheduled job string changed.
func TestScheduleCommandPulseDailyUpdatedString(t *testing.T) {
	const want = "pulse --write --digest --advance --sync --brief-file --notify"
	if got := scheduleCommands["pulse-daily"]; got != want {
		t.Fatalf("pulse-daily command string\n got: %q\nwant: %q", got, want)
	}
	// Each flag renders as its own <string> arg in the plist.
	args := plistArgs(want)
	for _, flag := range []string{"pulse", "--write", "--digest", "--advance", "--sync", "--brief-file", "--notify"} {
		if !strings.Contains(args, "<string>"+flag+"</string>") {
			t.Fatalf("plistArgs must render %q as its own <string>; got:\n%s", flag, args)
		}
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
