package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loop_test.go drives the `mora loop` durable-run spine (the Trigger gap):
// run-identity + same-period idempotency + a crash-safe lease + a queryable
// health status. Written RED-first, tier by tier (store -> lock -> begin ->
// done -> status), mirroring brief.go's load/save/lock + self-heal conventions.
//
// Injected now everywhere (never time.Now() in a helper) so date/TTL logic is
// deterministic — the same discipline brief.go enforces. fixedNow is already
// declared in brief_test.go (2026-06-08); this file uses its own instant.
var loopNow = time.Date(2026, 6, 24, 8, 0, 0, 0, time.UTC)

// loopTestCfg returns a Config rooted in per-test temp dirs. StateDir holds the
// run journal + lease (<StateDir>/loops/); VaultDir holds the brief artifacts
// the daily-brief loop produces (only read by status in later tiers).
func loopTestCfg(t *testing.T) Config {
	t.Helper()
	return Config{StateDir: t.TempDir(), VaultDir: t.TempDir()}
}

// writeRawFile plants exact bytes at path (creating parents) — used to seed
// corrupt/partial files the self-heal path must tolerate.
func writeRawFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// TIER A — run-record store self-heal + path safety
// ---------------------------------------------------------------------------

// TestLoadRunRecord_AbsentIsColdStart: a missing record reads as (zero, false),
// the cold-start gate the whole begin flow keys on (mirrors LoadStatus/ErrNotExist).

// TestLoadRunRecord_CorruptSelfHealsToAbsent: a garbage latest.json reads as
// (zero,false) with no panic and the file left intact (brief.go:108 corruption model).

// TestLoadRunRecord_SchemaMismatchIsAbsent: a record stamped with a future schema
// version can't be trusted -> (zero,false) (brief.go hash_schema_version reset model).

// TestLoadRunRecord_PathIdentityMismatchIsAbsent: a record whose body loop_id
// disagrees with the directory it sits in is misfiled and untrustworthy -> absent.

func TestLoopBeginInvalidExistingRecordFailsClosed(t *testing.T) {
	cases := map[string][]byte{
		"corrupt": []byte("{not-json"),
		"future-schema": func() []byte {
			rec := loopRunRecord{SchemaVersion: loopRunSchemaVersion + 1, LoopID: "daily-brief", RunID: "run_done", Period: periodFor("daily", loopNow), Status: loopRunSucceeded, Attempt: 1}
			body, _ := json.Marshal(rec)
			return body
		}(),
		"wrong-identity": func() []byte {
			rec := loopRunRecord{SchemaVersion: loopRunSchemaVersion, LoopID: "other", RunID: "run_done", Period: periodFor("daily", loopNow), Status: loopRunSucceeded, Attempt: 1}
			body, _ := json.Marshal(rec)
			return body
		}(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := loopTestCfg(t)
			writeRawFile(t, loopLatestPath(cfg, "daily-brief"), body)
			var out bytes.Buffer
			err := loopBegin(cfg, "daily-brief", true, loopNow, &out)
			if err == nil || !strings.Contains(err.Error(), "refusing to overwrite idempotency evidence") {
				t.Fatalf("begin over invalid existing record = %v, want fail-closed refusal", err)
			}
			after, readErr := os.ReadFile(loopLatestPath(cfg, "daily-brief"))
			if readErr != nil || !bytes.Equal(after, body) {
				t.Fatalf("begin overwrote invalid evidence: body=%q err=%v", after, readErr)
			}
		})
	}
}

// TestValidLoopID guards the filepath.Join traversal boundary: only
// [A-Za-z0-9._-]+, and never "", ".", "..", or anything with a separator.

// TestSaveRunRecord_StampsAndRoundTrips: save then load returns the same logical
// record, with schema_version stamped to current and updated_at = now (UTC RFC3339),
// and the file written 0600 (carries sensitive cursor/paths).

// ---------------------------------------------------------------------------
// TIER D — crash-safe lease (acquire / reap / break)
// ---------------------------------------------------------------------------

// plantLock writes a lock body verbatim — used to seed expired/fresh leases.
func plantLock(t *testing.T, cfg Config, id, runID string, pid int, acquiredAt time.Time) {
	t.Helper()
	body, _ := json.Marshal(loopLockBody{RunID: runID, PID: pid, AcquiredAt: acquiredAt.UTC().Format(time.RFC3339)})
	writeRawFile(t, loopLockPath(cfg, id), body)
}

// spawnDeadPid starts then reaps a process so its pid is reliably dead (ESRCH).

// TestReapStaleLock_ByTTL: a lock whose acquired_at is older than the TTL is
// reaped — the TTL is the sole abandonment bound (the run exceeded its budget).

// TestReapStaleLock_FreshDeadPidNotReaped pins the cross-process model fix: a
// FRESH lease whose begin-process pid is already dead must NOT be reaped — that
// pid is dead by design the instant `begin` exits, and reaping it would let a
// second begin start a concurrent run mid-flight. Only the TTL bounds abandonment.

// TestReapStaleLock_FreshNotReaped: a fresh lease is protected — never reaped.

// TestReapStaleLock_CorruptIsStale: an empty/garbage lock (writer crashed in the
// O_EXCL-create -> body-Write window) self-heals as stale.

// TestBreakLock_ContentChangedPreserves: if the lock no longer holds the bytes
// we judged stale, breakLock must leave the newer holder untouched.

// TestBreakLockSerializesRemoveBeforeFreshPublish is the issue-58 race witness:
// a publisher arriving at breakLock's remove boundary must wait until the stale
// inode is removed, then publish successfully. The old rename-away/restore
// sequence exposed a free path and could drop this fresh inode on restore EEXIST.

// TestLeaseGuardPlacementSafety pins both guard-path safety properties: stable
// guards remain within Mora-controlled writable roots, while a share-import
// guard stays outside the removable subscription root it serializes.
func TestLeaseGuardPlacementSafety(t *testing.T) {
	base := t.TempDir()
	cfg := Config{
		VaultDir: filepath.Join(base, "vault"),
		DataDir:  filepath.Join(base, "data"),
	}
	inside := func(root, path string) bool {
		root = resolveRealDeep(root)
		path = resolveRealDeep(path)
		rel, err := filepath.Rel(root, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}

	govGuard := leaseGuardPath(governanceLockPath(cfg))
	if !inside(cfg.VaultDir, govGuard) {
		t.Fatalf("governance guard %q escaped writable vault root %q", govGuard, cfg.VaultDir)
	}
	if !strings.HasSuffix(govGuard, ".lock") {
		t.Fatalf("guard %q must retain a .lock ignore suffix", govGuard)
	}

	shareRoot := shareSubRoot(cfg, "neil")
	shareGuard := leaseGuardPath(shareImportLockPath(cfg, "neil"))
	if inside(shareRoot, shareGuard) {
		t.Fatalf("share guard %q must live outside removable root %q", shareGuard, shareRoot)
	}
}

// TestLeaseGuardCanonicalizesSymlinkAliases pins guard identity to the real
// filesystem path, not its spelling. This is the same shape as macOS
// /tmp -> /private/tmp and a symlinked MORA_CONFIG_DIR captured differently by
// a scheduler and an interactive shell.
func TestLeaseGuardCanonicalizesSymlinkAliases(t *testing.T) {
	realRoot := t.TempDir()
	aliasParent := t.TempDir()
	aliasRoot := filepath.Join(aliasParent, "config-alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	realLock := filepath.Join(realRoot, "nested", "sources.json.lock")
	aliasLock := filepath.Join(aliasRoot, "nested", "sources.json.lock")
	if got, want := leaseGuardPath(aliasLock), leaseGuardPath(realLock); got != want {
		t.Fatalf("one lease through symlink aliases produced different guards:\n alias: %s\n  real: %s", got, want)
	}
}

// TestLeaseGuardSurvivesShareRootRemoval is the inode-split regression: deleting
// and recreating subs/<name> while one process holds its import guard must not
// let another process open a different guard inode and enter concurrently.
func TestLeaseGuardSurvivesShareRootRemoval(t *testing.T) {
	cfg := Config{DataDir: t.TempDir()}
	root := shareSubRoot(cfg, "neil")
	lockPath := shareImportLockPath(cfg, "neil")

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withLeaseFileGuard(lockPath, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	if err := os.RemoveAll(root); err != nil {
		close(releaseFirst)
		<-firstDone
		t.Fatalf("remove share root: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		close(releaseFirst)
		<-firstDone
		t.Fatalf("recreate share root: %v", err)
	}

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- withLeaseFileGuard(lockPath, func() error {
			close(secondEntered)
			return nil
		})
	}()
	premature := false
	select {
	case <-secondEntered:
		premature = true
	case <-time.After(50 * time.Millisecond):
		// Expected: both opens target the persistent guard outside shareRoot.
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first guard: %v", err)
	}
	if !premature {
		select {
		case <-secondEntered:
		case <-time.After(time.Second):
			t.Fatal("second guard did not enter after first released")
		}
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second guard: %v", err)
	}
	if premature {
		t.Fatal("share-root removal split one logical guard into two concurrently held inodes")
	}
}

// TestAcquireLoopLock_ReapsThenAcquires: a single acquire over an expired lock
// reaps it and takes the lease, stamping this run's id + the current pid.

// TestLoopBegin_ConcurrentExactlyOneProceeds: N simultaneous acquireLoopLock
// calls -> exactly one winner, the rest errLoopLockHeld (mutual exclusion).

// ---------------------------------------------------------------------------
// TIER B — the begin gate (idempotency + reclaim + cursor resume)
// ---------------------------------------------------------------------------

func seedRecord(t *testing.T, cfg Config, rec loopRunRecord) {
	t.Helper()
	if err := saveRunRecord(cfg, rec, loopNow); err != nil {
		t.Fatalf("seed record: %v", err)
	}
}

func readJournal(t *testing.T, cfg Config, id string) []string {
	t.Helper()
	b, err := os.ReadFile(loopJournalPath(cfg, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read journal: %v", err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestLoopBegin_FreshDayProceeds: empty state -> exit 0, a running record
// (attempt 1, today's period, a run id) and a .lock.

// TestLoopBegin_DoubleBeginSameDaySucceededSkips: a succeeded record for today
// -> exit-10 sentinel + {"skip":"already-succeeded"}, no new run, no lock.
func TestLoopBegin_DoubleBeginSameDaySucceededSkips(t *testing.T) {
	cfg := loopTestCfg(t)
	seedRecord(t, cfg, loopRunRecord{
		LoopID: "daily-brief", RunID: "run_old", Period: "2026-06-24",
		Status: loopRunSucceeded, Attempt: 1, IdempotencyKey: "daily-brief@2026-06-24",
	})
	var out bytes.Buffer
	err := loopBegin(cfg, "daily-brief", true, loopNow, &out)
	var ece exitCodeError
	if !errors.As(err, &ece) || ece.ExitCode() != loopSkipExitCode {
		t.Fatalf("want exit-%d skip, got err=%v", loopSkipExitCode, err)
	}
	if !strings.Contains(out.String(), "already-succeeded") {
		t.Fatalf("want already-succeeded body, got %q", out.String())
	}
	if rec, _ := loadRunRecord(cfg, "daily-brief"); rec.RunID != "run_old" {
		t.Fatalf("skip must not replace the record; got run id %q", rec.RunID)
	}
	if _, err := os.Stat(loopLockPath(cfg, "daily-brief")); !os.IsNotExist(err) {
		t.Fatalf("skip must not create a lock; stat err=%v", err)
	}
}

// TestExitCodeFor_MatchesOnlyMoraSentinel guards the main() exit-code path: the
// loop skip sentinel must be honored, but a %w-wrapped *exec.ExitError from a
// failed subprocess (git, schtasks, ...) must NOT hijack mora's exit status just
// because it also implements ExitCode() int. Re-execs itself as a child that
// exits 3 to obtain a genuine *exec.ExitError, portably on any OS.
func TestExitCodeFor_MatchesOnlyMoraSentinel(t *testing.T) {
	if os.Getenv("MORA_EXITCODE_HELPER") == "1" {
		os.Exit(3) // child arm: give the parent a real non-zero *exec.ExitError
	}

	if code, ok := ExitCodeFor(exitCodeError{code: loopSkipExitCode}); !ok || code != loopSkipExitCode {
		t.Fatalf("ExitCodeFor(sentinel) = (%d, %v), want (%d, true)", code, ok, loopSkipExitCode)
	}
	if code, ok := ExitCodeFor(fmt.Errorf("wrapped: %w", exitCodeError{code: 7, msg: "boom"})); !ok || code != 7 {
		t.Fatalf("ExitCodeFor(wrapped sentinel) = (%d, %v), want (7, true)", code, ok)
	}

	// A real *exec.ExitError (child exits 3) implements ExitCode() int but is NOT
	// our sentinel — must report no structured code so main() falls through to the
	// generic exit 1 instead of exiting 3.
	cmd := exec.Command(os.Args[0], "-test.run=^TestExitCodeFor_MatchesOnlyMoraSentinel$")
	cmd.Env = append(os.Environ(), "MORA_EXITCODE_HELPER=1")
	exitErr := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(exitErr, &ee) {
		t.Fatalf("setup: want a *exec.ExitError from the child, got %T (%v)", exitErr, exitErr)
	}
	if code, ok := ExitCodeFor(fmt.Errorf("subprocess failed: %w", exitErr)); ok {
		t.Fatalf("ExitCodeFor(wrapped *exec.ExitError) = (%d, true); a subprocess error must not hijack the exit code", code)
	}
	if code, ok := ExitCodeFor(errors.New("plain")); ok {
		t.Fatalf("ExitCodeFor(plain) = (%d, true), want (_, false)", code)
	}
}

// TestLoopBegin_CrashedRunReclaimsAttempt2: a stale running record + leaked
// expired .lock -> reap, ONE synthetic failed journal line for the abandoned run,
// and a NEW running record at attempt 2 (same period).

// TestLoopBegin_ResumesCursorToken: a failed record (today) with a committed
// cursor -> the attempt-2 record carries that cursor forward (resume, not cold).

// ---------------------------------------------------------------------------
// TIER C — the done transitions
// ---------------------------------------------------------------------------

// TestLoopDone_OkWritesJournalAndFlipsStatus: done --ok flips to succeeded, sets
// finished_at, clears heartbeat, appends a succeeded journal line, removes .lock.
func TestLoopDone_OkWritesJournalAndFlipsStatus(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	out.Reset()
	if err := loopDone(cfg, "daily-brief", "", true, "", loopNow, &out); err != nil {
		t.Fatalf("done ok: %v", err)
	}
	rec, ok := loadRunRecord(cfg, "daily-brief")
	if !ok || rec.Status != loopRunSucceeded {
		t.Fatalf("want succeeded, got %+v", rec)
	}
	if rec.FinishedAt == "" {
		t.Error("finished_at not set on terminal")
	}
	if rec.HeartbeatAt != "" {
		t.Error("heartbeat_at must be cleared on terminal")
	}
	var sawSucceeded bool
	for _, l := range readJournal(t, cfg, "daily-brief") {
		if strings.Contains(l, "succeeded") && strings.Contains(l, rec.RunID) {
			sawSucceeded = true
		}
	}
	if !sawSucceeded {
		t.Fatalf("want a succeeded journal line for %s, got %v", rec.RunID, readJournal(t, cfg, "daily-brief"))
	}
	if _, err := os.Stat(loopLockPath(cfg, "daily-brief")); !os.IsNotExist(err) {
		t.Fatalf("done must remove the lock; stat err=%v", err)
	}
}

// TestLoopDone_FailKeepsRetryable: done --fail flips to failed, sets last_error,
// removes the lock, and leaves the period reclaimable (a same-day begin returns
// exit 0 attempt 2, NOT the exit-10 already-succeeded skip).
func TestLoopDone_FailKeepsRetryable(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	out.Reset()
	if err := loopDone(cfg, "daily-brief", "", false, "sync: token expired", loopNow, &out); err != nil {
		t.Fatalf("done fail: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	if rec.Status != loopRunFailed || rec.LastError == "" {
		t.Fatalf("want failed+last_error, got %+v", rec)
	}
	if _, err := os.Stat(loopLockPath(cfg, "daily-brief")); !os.IsNotExist(err) {
		t.Fatalf("fail must remove the lock; stat err=%v", err)
	}
	out.Reset()
	err := loopBegin(cfg, "daily-brief", true, loopNow, &out)
	var ece exitCodeError
	if errors.As(err, &ece) {
		t.Fatal("a failed period must NOT be treated as already-succeeded")
	}
	if err != nil {
		t.Fatalf("reclaim after fail: %v", err)
	}
	if rec2, _ := loadRunRecord(cfg, "daily-brief"); rec2.Attempt != 2 {
		t.Fatalf("want attempt 2 after a failed period, got %d", rec2.Attempt)
	}
}

// TestLoopDoneRejectsDuplicateTerminalTransition pins issue #58's terminal
// guard: a duplicate done cannot rewrite succeeded -> failed, append a second
// journal line, or reopen the same-day begin gate.

// TestLoopHeartbeatKeepsLongRunUnreapable proves an active run can cross the
// original 15-minute lease age without a second begin reclaiming it. Heartbeat
// refreshes both lock acquired_at and latest.json heartbeat_at owner-CAS.

// TestLoopEffectGuardBlocksTTLReclaim forces the issue-58 post-fence stall:
// run A passes its owner fence and then pauses inside the non-idempotent effect
// past the old TTL while run B attempts reclaim. B must remain blocked until A
// finishes, then observe A's post-effect heartbeat and refuse to start.

// TestLoopEffectCheckpointStopsPostEffectTTLReclaim covers the other side of
// the effect boundary: if A commits and is then suspended before loopDone, its
// durable effect marker must make a >TTL same-period begin skip, not run a
// second advance. The same run also cannot invoke the effect twice directly.

// TestLoopEffectIntentClosesPostEffectCrashWindow plants the exact kill point
// after the non-idempotent function returns but before effect_committed_at can
// be saved. The durable pre-effect intent must make both a leaked-running lease
// and a wrapper-recorded failure refuse automatic same-period retry.

// TestLoopEffectErrorKeepsIntentFailClosed covers advanceBrief's partial-write
// shape: an error after an artifact or earlier source snapshot may already have
// persisted cannot be treated as proof that no effect happened.

// TestLoopEffectKillHelper is subprocess-only. It durably enters the effect,
// writes one observable side effect, and then pauses at the post-effect /
// pre-commit boundary until the parent kills it.

// TestLoopHeartbeatRefusesSupersededOwner pins the owner-CAS half: an old run
// cannot restamp a successor's lease or latest.json heartbeat.

// TestLoopDone_OkArchivesImmutableRunCopy: done --ok writes an immutable
// runs/<period>_<run_id>.json audit copy.

// TestLoopDone_SupersededRunRefused: a late done from a run that a newer attempt
// has replaced must refuse — never clobber the current run's record (Codex #3).

// TestLoopDone_RefusesWhenLeaseMovedToNewerRun: even if the record still names
// our run, a newer run already holding the LEASE means we lost — done must refuse
// (the lock reflects the newer run before latest.json catches up).
func TestLoopDone_RefusesWhenLeaseMovedToNewerRun(t *testing.T) {
	cfg := loopTestCfg(t)
	// Record still shows run_R1 (a newer run hasn't saved its record yet)...
	seedRecord(t, cfg, loopRunRecord{
		LoopID: "daily-brief", RunID: "run_R1", Period: "2026-06-24",
		Status: loopRunRunning, Attempt: 1, IdempotencyKey: "daily-brief@2026-06-24",
	})
	// ...but the LEASE is already held by run_R2.
	plantLock(t, cfg, "daily-brief", "run_R2", os.Getpid(), loopNow)
	var out bytes.Buffer
	err := loopDone(cfg, "daily-brief", "run_R1", true, "", loopNow, &out)
	if err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("done while lease moved to a newer run: err=%v, want a 'lease' refusal", err)
	}
	if rec, _ := loadRunRecord(cfg, "daily-brief"); rec.Status != loopRunRunning {
		t.Fatalf("refused done must NOT flip the record; got %+v", rec)
	}
}

// TestReleaseLoopLockForOnlyOwnRun: releasing a lease never deletes a lock a
// DIFFERENT run owns (Codex #3 — the lock-deletion half).

// TestReleaseSerializesAgainstSameOwnerHeartbeat closes the release leak: once
// release has read its owner, a heartbeat must wait. Release removes the lease;
// the delayed heartbeat then observes absence and cannot silently leave a fresh
// same-owner lock behind.

// ---------------------------------------------------------------------------
// TIER E — status health ladder (GENERIC: run-record + registry only)
// ---------------------------------------------------------------------------
// The ladder is pure over an injected now. HOME is pinned to a temp dir so the
// scheduler annotation is deterministic (no stray com.mora.*.plist). The
// brief-coupled refinements (ran-uncommitted, source-stale) are deliberately
// out of scope to keep `mora loop` brief-agnostic.

// TestLoopStatus_NeverRun: no run record -> "never-run", scheduler annotated.

// TestLoopStatus_RunningFresh: a running record with a recent heartbeat -> "running".

// TestLoopStatus_RunningAbandonedIsStale: a running record whose heartbeat is far
// in the past is an abandoned/leaked run -> "stale" (stale beats a dead 'running').

// TestLoopStatus_Failed: a failed terminal -> "failed", surfacing the error.

// TestLoopStatus_Ok: a recent success -> "ok".

// TestLoopStatus_SucceededTooOldIsStale: a success older than the cadence allows
// -> "stale" (success is liveness, not freshness).

// TestLoopAllowedLag: the cadence staleness windows (a dead hourly loop must not
// read fresh for 48h).

// TestLoopScheduledAnnotation: the secondary scheduler annotation reflects the
// plist's presence (darwin-only; the non-darwin floor reports false).

// TestLoopStatus_Integration: the loopStatus command emits the classified state
// as JSON over the real file store.

// TestLoopHeartbeatCommandDispatch pins the public CLI route used by the daily
// brief skill; it must parse --run, emit JSON, and update the durable heartbeat.
func TestLoopHeartbeatCommandDispatch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	origClock := loopClock
	t.Cleanup(func() { loopClock = origClock })
	loopClock = func() time.Time { return loopNow }

	beginOut, err := runErr(t, "loop", "begin", "daily-brief", "--json")
	if err != nil {
		t.Fatalf("loop begin: %v\n%s", err, beginOut)
	}
	var begun map[string]any
	if err := json.Unmarshal([]byte(beginOut), &begun); err != nil {
		t.Fatalf("begin JSON: %v\n%s", err, beginOut)
	}
	runID, _ := begun["run_id"].(string)
	if runID == "" {
		t.Fatalf("begin JSON missing run_id: %s", beginOut)
	}

	heartbeatAt := loopNow.Add(time.Minute)
	loopClock = func() time.Time { return heartbeatAt }
	heartbeatOut, err := runErr(t, "loop", "heartbeat", "daily-brief", "--run", runID, "--json")
	if err != nil {
		t.Fatalf("loop heartbeat: %v\n%s", err, heartbeatOut)
	}
	var heartbeat map[string]string
	if err := json.Unmarshal([]byte(heartbeatOut), &heartbeat); err != nil {
		t.Fatalf("heartbeat JSON: %v\n%s", err, heartbeatOut)
	}
	if heartbeat["run_id"] != runID || heartbeat["heartbeat_at"] != heartbeatAt.Format(time.RFC3339) {
		t.Fatalf("heartbeat JSON = %+v", heartbeat)
	}

	cfg := mustConfig(t)
	rec, ok := loadRunRecord(cfg, "daily-brief")
	if !ok || rec.HeartbeatAt != heartbeatAt.Format(time.RFC3339) {
		t.Fatalf("durable heartbeat record = %+v", rec)
	}
}
