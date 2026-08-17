package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
func TestLoadRunRecord_AbsentIsColdStart(t *testing.T) {
	cfg := loopTestCfg(t)
	if rec, ok := loadRunRecord(cfg, "daily-brief"); ok {
		t.Fatalf("absent record: got ok=true rec=%+v, want (zero,false)", rec)
	}
}

// TestLoadRunRecord_CorruptSelfHealsToAbsent: a garbage latest.json reads as
// (zero,false) with no panic and the file left intact (brief.go:108 corruption model).
func TestLoadRunRecord_CorruptSelfHealsToAbsent(t *testing.T) {
	cfg := loopTestCfg(t)
	writeRawFile(t, loopLatestPath(cfg, "daily-brief"), []byte("{ this is not json"))
	rec, ok := loadRunRecord(cfg, "daily-brief")
	if ok {
		t.Fatalf("corrupt record: got ok=true rec=%+v, want (zero,false)", rec)
	}
	if _, err := os.Stat(loopLatestPath(cfg, "daily-brief")); err != nil {
		t.Fatalf("corrupt file should be left intact: %v", err)
	}
}

// TestLoadRunRecord_SchemaMismatchIsAbsent: a record stamped with a future schema
// version can't be trusted -> (zero,false) (brief.go hash_schema_version reset model).
func TestLoadRunRecord_SchemaMismatchIsAbsent(t *testing.T) {
	cfg := loopTestCfg(t)
	rec := loopRunRecord{
		SchemaVersion: 999, LoopID: "daily-brief", RunID: "run_x",
		Period: "2026-06-24", Status: loopRunSucceeded, Attempt: 1,
	}
	body, _ := json.MarshalIndent(rec, "", "  ")
	writeRawFile(t, loopLatestPath(cfg, "daily-brief"), body)
	if got, ok := loadRunRecord(cfg, "daily-brief"); ok {
		t.Fatalf("schema mismatch: got ok=true rec=%+v, want (zero,false)", got)
	}
}

// TestLoadRunRecord_PathIdentityMismatchIsAbsent: a record whose body loop_id
// disagrees with the directory it sits in is misfiled and untrustworthy -> absent.
func TestLoadRunRecord_PathIdentityMismatchIsAbsent(t *testing.T) {
	cfg := loopTestCfg(t)
	rec := loopRunRecord{
		SchemaVersion: loopRunSchemaVersion, LoopID: "B", RunID: "run_x",
		Period: "2026-06-24", Status: loopRunSucceeded, Attempt: 1,
	}
	body, _ := json.MarshalIndent(rec, "", "  ")
	writeRawFile(t, loopLatestPath(cfg, "A"), body) // filed under A, body says B
	if got, ok := loadRunRecord(cfg, "A"); ok {
		t.Fatalf("path/identity mismatch: got ok=true rec=%+v, want (zero,false)", got)
	}
}

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
func TestValidLoopID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"daily-brief", true}, {"a.b_c-1", true}, {"A", true},
		{"", false}, {".", false}, {"..", false},
		{"a/b", false}, {"a b", false}, {"../escape", false}, {"a\\b", false},
	}
	for _, c := range cases {
		if got := validLoopID(c.id); got != c.want {
			t.Errorf("validLoopID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestSaveRunRecord_StampsAndRoundTrips: save then load returns the same logical
// record, with schema_version stamped to current and updated_at = now (UTC RFC3339),
// and the file written 0600 (carries sensitive cursor/paths).
func TestSaveRunRecord_StampsAndRoundTrips(t *testing.T) {
	cfg := loopTestCfg(t)
	in := loopRunRecord{
		LoopID: "daily-brief", RunID: "run_a", Period: "2026-06-24",
		Status: loopRunRunning, Attempt: 1, StartedAt: loopNow.UTC().Format(time.RFC3339),
		IdempotencyKey: "daily-brief@2026-06-24",
	}
	if err := saveRunRecord(cfg, in, loopNow); err != nil {
		t.Fatalf("saveRunRecord: %v", err)
	}
	got, ok := loadRunRecord(cfg, "daily-brief")
	if !ok {
		t.Fatal("round-trip: load returned ok=false")
	}
	if got.SchemaVersion != loopRunSchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, loopRunSchemaVersion)
	}
	if got.UpdatedAt != loopNow.UTC().Format(time.RFC3339) {
		t.Errorf("updated_at = %q, want %q", got.UpdatedAt, loopNow.UTC().Format(time.RFC3339))
	}
	if got.LoopID != "daily-brief" || got.RunID != "run_a" || got.Status != loopRunRunning {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	info, err := os.Stat(loopLatestPath(cfg, "daily-brief"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	assertPermUnix(t, info.Mode(), 0o600)
}

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
func spawnDeadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a process to get a dead pid: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // reap; pid is now dead (not yet reused)
	return pid
}

// TestReapStaleLock_ByTTL: a lock whose acquired_at is older than the TTL is
// reaped — the TTL is the sole abandonment bound (the run exceeded its budget).
func TestReapStaleLock_ByTTL(t *testing.T) {
	cfg := loopTestCfg(t)
	plantLock(t, cfg, "L", "run_x", os.Getpid(), loopNow.Add(-16*time.Minute)) // EXPIRED
	reaped, err := reapStaleLock(loopLockPath(cfg, "L"), loopNow)
	if err != nil || !reaped {
		t.Fatalf("reapStaleLock(expired) = (%v,%v), want (true,nil)", reaped, err)
	}
	if _, err := os.Stat(loopLockPath(cfg, "L")); !os.IsNotExist(err) {
		t.Fatalf("expired lock should be removed; stat err=%v", err)
	}
}

// TestReapStaleLock_FreshDeadPidNotReaped pins the cross-process model fix: a
// FRESH lease whose begin-process pid is already dead must NOT be reaped — that
// pid is dead by design the instant `begin` exits, and reaping it would let a
// second begin start a concurrent run mid-flight. Only the TTL bounds abandonment.
func TestReapStaleLock_FreshDeadPidNotReaped(t *testing.T) {
	dp := spawnDeadPid(t)
	cfg := loopTestCfg(t)
	plantLock(t, cfg, "L", "run_x", dp, loopNow) // FRESH acquired_at, dead begin-pid
	reaped, err := reapStaleLock(loopLockPath(cfg, "L"), loopNow)
	if err != nil || reaped {
		t.Fatalf("reapStaleLock(fresh, dead pid) = (%v,%v), want (false,nil) — TTL is the only bound", reaped, err)
	}
	if _, err := os.Stat(loopLockPath(cfg, "L")); err != nil {
		t.Fatalf("a fresh lease must NOT be removed regardless of pid: %v", err)
	}
}

// TestReapStaleLock_FreshNotReaped: a fresh lease is protected — never reaped.
func TestReapStaleLock_FreshNotReaped(t *testing.T) {
	cfg := loopTestCfg(t)
	plantLock(t, cfg, "L", "run_x", os.Getpid(), loopNow) // fresh
	reaped, err := reapStaleLock(loopLockPath(cfg, "L"), loopNow)
	if err != nil || reaped {
		t.Fatalf("reapStaleLock(fresh) = (%v,%v), want (false,nil)", reaped, err)
	}
	if _, err := os.Stat(loopLockPath(cfg, "L")); err != nil {
		t.Fatalf("fresh lock must NOT be removed: %v", err)
	}
}

// TestReapStaleLock_CorruptIsStale: an empty/garbage lock (writer crashed in the
// O_EXCL-create -> body-Write window) self-heals as stale.
func TestReapStaleLock_CorruptIsStale(t *testing.T) {
	for _, body := range [][]byte{[]byte(""), []byte("garbage"), []byte(`{"pid":0}`)} {
		cfg := loopTestCfg(t)
		writeRawFile(t, loopLockPath(cfg, "L"), body)
		reaped, err := reapStaleLock(loopLockPath(cfg, "L"), loopNow)
		if err != nil || !reaped {
			t.Fatalf("reapStaleLock(corrupt %q) = (%v,%v), want (true,nil)", body, reaped, err)
		}
	}
}

// TestBreakLock_ContentChangedPreserves: if the lock no longer holds the bytes
// we judged stale, breakLock must leave the newer holder untouched.
func TestBreakLock_ContentChangedPreserves(t *testing.T) {
	cfg := loopTestCfg(t)
	writeRawFile(t, loopLockPath(cfg, "L"), []byte("AAA-fresh-holder"))
	reaped, err := breakLock(loopLockPath(cfg, "L"), []byte("BBB-what-we-observed"))
	if err != nil || reaped {
		t.Fatalf("breakLock(content changed) = (%v,%v), want (false,nil)", reaped, err)
	}
	got, rerr := os.ReadFile(loopLockPath(cfg, "L"))
	if rerr != nil || string(got) != "AAA-fresh-holder" {
		t.Fatalf("a fresh holder's lock must remain intact; got %q err=%v", got, rerr)
	}
}

// TestBreakLockSerializesRemoveBeforeFreshPublish is the issue-58 race witness:
// a publisher arriving at breakLock's remove boundary must wait until the stale
// inode is removed, then publish successfully. The old rename-away/restore
// sequence exposed a free path and could drop this fresh inode on restore EEXIST.
func TestBreakLockSerializesRemoveBeforeFreshPublish(t *testing.T) {
	cfg := loopTestCfg(t)
	path := loopLockPath(cfg, "L")
	stale := []byte("stale-holder")
	fresh := []byte("fresh-holder")
	writeRawFile(t, path, stale)

	type publishResult struct {
		published bool
		err       error
	}
	started := make(chan struct{})
	result := make(chan publishResult, 1)
	testHookBreakLockBeforeRemove = func() {
		go func() {
			close(started)
			published, err := publishLockFile(path, fresh)
			result <- publishResult{published: published, err: err}
		}()
		<-started
		select {
		case got := <-result:
			t.Fatalf("fresh publisher completed inside breakLock's guarded remove: %+v", got)
		case <-time.After(50 * time.Millisecond):
			// Expected: publisher is blocked on the persistent OS guard.
		}
	}
	t.Cleanup(func() { testHookBreakLockBeforeRemove = nil })

	reaped, err := breakLock(path, stale)
	if err != nil || !reaped {
		t.Fatalf("breakLock(stale) = (%v,%v), want (true,nil)", reaped, err)
	}
	got := <-result
	if got.err != nil || !got.published {
		t.Fatalf("fresh publisher after serialized remove = %+v, want published", got)
	}
	body, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(body, fresh) {
		t.Fatalf("fresh lock was dropped after break: body=%q err=%v", body, err)
	}
}

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
func TestAcquireLoopLock_ReapsThenAcquires(t *testing.T) {
	cfg := loopTestCfg(t)
	plantLock(t, cfg, "L", "run_old", os.Getpid(), loopNow.Add(-16*time.Minute)) // expired
	release, err := acquireLoopLock(cfg, "L", "run_new", loopNow)
	if err != nil || release == nil {
		t.Fatalf("acquireLoopLock over expired: err=%v release==nil=%v, want a release", err, release == nil)
	}
	data, _ := os.ReadFile(loopLockPath(cfg, "L"))
	var b loopLockBody
	if json.Unmarshal(data, &b) != nil || b.RunID != "run_new" || b.PID != os.Getpid() {
		t.Fatalf("new lock body = %+v, want run_new + pid %d (body=%s)", b, os.Getpid(), data)
	}
	release()
	if _, err := os.Stat(loopLockPath(cfg, "L")); !os.IsNotExist(err) {
		t.Fatalf("release should remove the lock; stat err=%v", err)
	}
}

// TestLoopBegin_ConcurrentExactlyOneProceeds: N simultaneous acquireLoopLock
// calls -> exactly one winner, the rest errLoopLockHeld (mutual exclusion).
func TestLoopBegin_ConcurrentExactlyOneProceeds(t *testing.T) {
	cfg := loopTestCfg(t)
	if err := os.MkdirAll(loopDir(cfg, "L"), 0o700); err != nil {
		t.Fatal(err)
	}
	const N = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var releases []func()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := acquireLoopLock(cfg, "L", "run_c", loopNow)
			if err == nil {
				mu.Lock()
				releases = append(releases, rel)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(releases) != 1 {
		t.Fatalf("concurrent acquire: %d winners, want exactly 1", len(releases))
	}
	for _, r := range releases {
		r()
	}
}

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
func TestLoopBegin_FreshDayProceeds(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("fresh begin: %v", err)
	}
	rec, ok := loadRunRecord(cfg, "daily-brief")
	if !ok {
		t.Fatal("no run record written")
	}
	if rec.Status != loopRunRunning || rec.Attempt != 1 || rec.RunID == "" || rec.Period != "2026-06-24" {
		t.Fatalf("unexpected fresh record: %+v", rec)
	}
	if _, err := os.Stat(loopLockPath(cfg, "daily-brief")); err != nil {
		t.Fatalf(".lock not created: %v", err)
	}
	if strings.Contains(out.String(), "skip") {
		t.Fatalf("fresh begin should not skip; out=%q", out.String())
	}
}

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
func TestLoopBegin_CrashedRunReclaimsAttempt2(t *testing.T) {
	cfg := loopTestCfg(t)
	seedRecord(t, cfg, loopRunRecord{
		LoopID: "daily-brief", RunID: "run_crashed", Period: "2026-06-24",
		Status: loopRunRunning, Attempt: 1, IdempotencyKey: "daily-brief@2026-06-24",
		CursorToken: "cur-mid", HeartbeatAt: loopNow.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})
	plantLock(t, cfg, "daily-brief", "run_crashed", os.Getpid(), loopNow.Add(-20*time.Minute)) // leaked + expired
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("reclaim begin: %v", err)
	}
	rec, ok := loadRunRecord(cfg, "daily-brief")
	if !ok || rec.Attempt != 2 || rec.Status != loopRunRunning {
		t.Fatalf("want a fresh attempt-2 running record, got %+v", rec)
	}
	if rec.RunID == "run_crashed" {
		t.Fatal("reclaim must mint a NEW run id")
	}
	var recovered bool
	for _, l := range readJournal(t, cfg, "daily-brief") {
		if strings.Contains(l, "run_crashed") && strings.Contains(l, "failed") && strings.Contains(l, "recovered") {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("want a recovered-failed journal line for run_crashed, got %v", readJournal(t, cfg, "daily-brief"))
	}
}

// TestLoopBegin_ResumesCursorToken: a failed record (today) with a committed
// cursor -> the attempt-2 record carries that cursor forward (resume, not cold).
func TestLoopBegin_ResumesCursorToken(t *testing.T) {
	cfg := loopTestCfg(t)
	seedRecord(t, cfg, loopRunRecord{
		LoopID: "daily-brief", RunID: "run_failed", Period: "2026-06-24",
		Status: loopRunFailed, Attempt: 1, IdempotencyKey: "daily-brief@2026-06-24",
		CursorToken: "cur-A", LastError: "sync timeout",
	})
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("reclaim begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	if rec.Attempt != 2 {
		t.Fatalf("want attempt 2, got %d", rec.Attempt)
	}
	if rec.CursorToken != "cur-A" {
		t.Fatalf("want resumed cursor cur-A, got %q", rec.CursorToken)
	}
}

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
	if err := loopDone(cfg, "daily-brief", "", true, "", false, loopNow, &out); err != nil {
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
	if err := loopDone(cfg, "daily-brief", "", false, "sync: token expired", false, loopNow, &out); err != nil {
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
func TestLoopDoneRejectsDuplicateTerminalTransition(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	if err := loopDone(cfg, "daily-brief", rec.RunID, true, "", false, loopNow, &out); err != nil {
		t.Fatalf("first done: %v", err)
	}
	journalBefore := readJournal(t, cfg, "daily-brief")

	err := loopDone(cfg, "daily-brief", rec.RunID, false, "late failure", false, loopNow.Add(time.Minute), &out)
	if err == nil || !strings.Contains(err.Error(), "already terminal") {
		t.Fatalf("duplicate done error = %v, want already-terminal refusal", err)
	}
	after, _ := loadRunRecord(cfg, "daily-brief")
	if after.Status != loopRunSucceeded || after.LastError != "" {
		t.Fatalf("duplicate done rewrote terminal record: %+v", after)
	}
	if journalAfter := readJournal(t, cfg, "daily-brief"); len(journalAfter) != len(journalBefore) {
		t.Fatalf("duplicate done appended journal: before=%v after=%v", journalBefore, journalAfter)
	}
	out.Reset()
	err = loopBegin(cfg, "daily-brief", true, loopNow.Add(2*time.Minute), &out)
	if code, ok := ExitCodeFor(err); !ok || code != loopSkipExitCode {
		t.Fatalf("same-day begin after duplicate done = %v, want exit %d", err, loopSkipExitCode)
	}
}

// TestLoopHeartbeatKeepsLongRunUnreapable proves an active run can cross the
// original 15-minute lease age without a second begin reclaiming it. Heartbeat
// refreshes both lock acquired_at and latest.json heartbeat_at owner-CAS.
func TestLoopHeartbeatKeepsLongRunUnreapable(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	heartbeatAt := loopNow.Add(14 * time.Minute)
	if err := heartbeatLoopRun(cfg, "daily-brief", rec.RunID, heartbeatAt); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	refreshed, _ := loadRunRecord(cfg, "daily-brief")
	if refreshed.HeartbeatAt != heartbeatAt.Format(time.RFC3339) {
		t.Fatalf("record heartbeat_at = %q, want %q", refreshed.HeartbeatAt, heartbeatAt.Format(time.RFC3339))
	}
	data, err := os.ReadFile(loopLockPath(cfg, "daily-brief"))
	if err != nil {
		t.Fatal(err)
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil || body.RunID != rec.RunID || body.AcquiredAt != heartbeatAt.Format(time.RFC3339) {
		t.Fatalf("heartbeated lease = %+v; body=%s", body, data)
	}

	out.Reset()
	err = loopBegin(cfg, "daily-brief", true, loopNow.Add(20*time.Minute), &out)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second begin after active heartbeat = %v, want held-lease refusal", err)
	}
	current, _ := loadRunRecord(cfg, "daily-brief")
	if current.RunID != rec.RunID || current.Attempt != 1 {
		t.Fatalf("active run was reclaimed despite heartbeat: %+v", current)
	}
}

// TestLoopEffectGuardBlocksTTLReclaim forces the issue-58 post-fence stall:
// run A passes its owner fence and then pauses inside the non-idempotent effect
// past the old TTL while run B attempts reclaim. B must remain blocked until A
// finishes, then observe A's post-effect heartbeat and refuse to start.
func TestLoopEffectGuardBlocksTTLReclaim(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin A: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	later := loopNow.Add(20 * time.Minute)
	origClock := loopClock
	t.Cleanup(func() { loopClock = origClock })
	loopClock = func() time.Time { return later }

	entered := make(chan struct{})
	releaseEffect := make(chan struct{})
	effectDone := make(chan error, 1)
	effects := 0
	go func() {
		effectDone <- withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error {
			effects++
			close(entered)
			<-releaseEffect
			return nil
		})
	}()
	<-entered

	beginDone := make(chan error, 1)
	go func() {
		var beginOut bytes.Buffer
		beginDone <- loopBegin(cfg, "daily-brief", true, later, &beginOut)
	}()
	premature := false
	var beginErr error
	select {
	case beginErr = <-beginDone:
		premature = true
	case <-time.After(50 * time.Millisecond):
		// Expected: reclaim is blocked on the same OS guard as the effect.
	}
	close(releaseEffect)
	if err := <-effectDone; err != nil {
		t.Fatalf("guarded effect: %v", err)
	}
	if premature {
		t.Fatal("run B reclaimed while run A was inside its fenced effect")
	}
	if !premature {
		beginErr = <-beginDone
	}
	if beginErr == nil || !strings.Contains(beginErr.Error(), "already running") {
		t.Fatalf("begin B after A post-heartbeat = %v, want held-lease refusal", beginErr)
	}
	current, _ := loadRunRecord(cfg, "daily-brief")
	if effects != 1 || current.RunID != rec.RunID || current.Attempt != 1 {
		t.Fatalf("effect/reclaim state = effects:%d record:%+v, want one effect and original attempt", effects, current)
	}
}

// TestLoopEffectCheckpointStopsPostEffectTTLReclaim covers the other side of
// the effect boundary: if A commits and is then suspended before loopDone, its
// durable effect marker must make a >TTL same-period begin skip, not run a
// second advance. The same run also cannot invoke the effect twice directly.
func TestLoopEffectCheckpointStopsPostEffectTTLReclaim(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin A: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	effectAt := loopNow.Add(time.Minute)
	origClock := loopClock
	t.Cleanup(func() { loopClock = origClock })
	loopClock = func() time.Time { return effectAt }

	effects := 0
	if err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error {
		effects++
		return nil
	}); err != nil {
		t.Fatalf("first effect: %v", err)
	}
	committed, _ := loadRunRecord(cfg, "daily-brief")
	if committed.EffectStartedAt != effectAt.Format(time.RFC3339) {
		t.Fatalf("effect_started_at = %q, want %q", committed.EffectStartedAt, effectAt.Format(time.RFC3339))
	}
	if committed.EffectCommittedAt != effectAt.Format(time.RFC3339) {
		t.Fatalf("effect_committed_at = %q, want %q", committed.EffectCommittedAt, effectAt.Format(time.RFC3339))
	}
	if err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error {
		effects++
		return nil
	}); err == nil || !strings.Contains(err.Error(), "already committed") {
		t.Fatalf("duplicate same-run effect = %v, want committed refusal", err)
	}
	if effects != 1 {
		t.Fatalf("effect executions = %d, want exactly one", effects)
	}

	out.Reset()
	err := loopBegin(cfg, "daily-brief", true, loopNow.Add(20*time.Minute), &out)
	if code, ok := ExitCodeFor(err); !ok || code != loopSkipExitCode {
		t.Fatalf("post-effect >TTL begin = %v, want exit %d skip", err, loopSkipExitCode)
	}
	if !strings.Contains(out.String(), "effect-already-committed") {
		t.Fatalf("post-effect skip did not expose committed reconciliation: %q", out.String())
	}
	current, _ := loadRunRecord(cfg, "daily-brief")
	if current.RunID != rec.RunID || current.Attempt != 1 {
		t.Fatalf("post-effect begin opened another attempt: %+v", current)
	}
}

// TestLoopEffectIntentClosesPostEffectCrashWindow plants the exact kill point
// after the non-idempotent function returns but before effect_committed_at can
// be saved. The durable pre-effect intent must make both a leaked-running lease
// and a wrapper-recorded failure refuse automatic same-period retry.
func TestLoopEffectIntentClosesPostEffectCrashWindow(t *testing.T) {
	effectAt := loopNow.Add(time.Minute)
	origClock, origHook := loopClock, testHookLoopEffectAfterRun
	t.Cleanup(func() {
		loopClock = origClock
		testHookLoopEffectAfterRun = origHook
	})
	loopClock = func() time.Time { return effectAt }
	testHookLoopEffectAfterRun = func() error { return errors.New("simulated death before checkpoint") }

	t.Run("hard crash leaves running intent", func(t *testing.T) {
		cfg := loopTestCfg(t)
		var out bytes.Buffer
		if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
			t.Fatalf("begin: %v", err)
		}
		rec, _ := loadRunRecord(cfg, "daily-brief")
		effects := 0
		err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error {
			effects++
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "automatic retry is blocked") {
			t.Fatalf("post-effect checkpoint interruption = %v, want fail-closed error", err)
		}
		uncertain, _ := loadRunRecord(cfg, "daily-brief")
		if uncertain.EffectStartedAt != effectAt.Format(time.RFC3339) || uncertain.EffectCommittedAt != "" {
			t.Fatalf("interrupted effect record = %+v, want started without committed", uncertain)
		}
		out.Reset()
		err = loopBegin(cfg, "daily-brief", true, loopNow.Add(20*time.Minute), &out)
		if err == nil || !strings.Contains(err.Error(), "outcome is uncertain") {
			t.Fatalf("same-period begin after simulated crash = %v, want uncertainty refusal", err)
		}
		if _, ok := ExitCodeFor(err); ok {
			t.Fatalf("uncertain effect must be an explicit error, not a successful skip: %v", err)
		}
		if current, _ := loadRunRecord(cfg, "daily-brief"); current.RunID != rec.RunID || current.Attempt != 1 || effects != 1 {
			t.Fatalf("crash recovery replayed or replaced effect: effects=%d record=%+v", effects, current)
		}
	})

	t.Run("wrapper records failure but intent remains", func(t *testing.T) {
		cfg := loopTestCfg(t)
		var out bytes.Buffer
		if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
			t.Fatalf("begin: %v", err)
		}
		rec, _ := loadRunRecord(cfg, "daily-brief")
		if err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error { return nil }); err == nil {
			t.Fatal("checkpoint interruption unexpectedly succeeded")
		}
		if err := loopDone(cfg, "daily-brief", rec.RunID, false, "checkpoint interrupted", false, effectAt, &out); err != nil {
			t.Fatalf("record failed wrapper outcome: %v", err)
		}
		out.Reset()
		err := loopBegin(cfg, "daily-brief", true, loopNow.Add(2*time.Minute), &out)
		if err == nil || !strings.Contains(err.Error(), "outcome is uncertain") {
			t.Fatalf("same-period begin after failed close = %v, want uncertainty refusal", err)
		}
		failedUncertain, _ := loadRunRecord(cfg, "daily-brief")
		h := classifyLoopHealth(loopRegistration{LoopID: "daily-brief", Cadence: "daily"}, failedUncertain, true, loopNow.Add(2*time.Minute))
		if h.State != "uncertain" {
			t.Fatalf("health state = %q, want uncertain", h.State)
		}
	})
}

// TestLoopEffectErrorKeepsIntentFailClosed covers advanceBrief's partial-write
// shape: an error after an artifact or earlier source snapshot may already have
// persisted cannot be treated as proof that no effect happened.
func TestLoopEffectErrorKeepsIntentFailClosed(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	origClock := loopClock
	t.Cleanup(func() { loopClock = origClock })
	loopClock = func() time.Time { return loopNow.Add(time.Minute) }

	err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error {
		return errors.New("later snapshot write failed")
	})
	if err == nil || !strings.Contains(err.Error(), "outcome may be partial") {
		t.Fatalf("partial effect error = %v, want uncertainty", err)
	}
	got, _ := loadRunRecord(cfg, "daily-brief")
	if got.EffectStartedAt == "" || got.EffectCommittedAt != "" {
		t.Fatalf("partial effect record = %+v, want started without committed", got)
	}
	out.Reset()
	if err := loopBegin(cfg, "daily-brief", true, loopNow.Add(20*time.Minute), &out); err == nil || !strings.Contains(err.Error(), "outcome is uncertain") {
		t.Fatalf("retry after partial effect = %v, want uncertainty refusal", err)
	}
}

func TestLoopEffectDurableIntentPrecedesFunction(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	origClock := loopClock
	t.Cleanup(func() { loopClock = origClock })
	loopClock = func() time.Time { return loopNow.Add(time.Minute) }
	trace := withMarkerTrace(t)

	if err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error {
		*trace = append(*trace, "effect")
		inside, _ := loadRunRecord(cfg, "daily-brief")
		if inside.EffectStartedAt == "" || inside.EffectCommittedAt != "" {
			t.Fatalf("record at first effect instruction = %+v, want durable started intent", inside)
		}
		return nil
	}); err != nil {
		t.Fatalf("guarded effect: %v", err)
	}
	if got := strings.Join(*trace, ","); got != "fsync,dirsync,effect,fsync,dirsync" {
		t.Fatalf("effect durability trace = %q, want intent barriers before effect and commit barriers after", got)
	}
}

func TestLoopEffectEvidenceStaysDurableThroughHeartbeatAndDone(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	origClock := loopClock
	t.Cleanup(func() { loopClock = origClock })
	loopClock = func() time.Time { return loopNow.Add(time.Minute) }
	trace := withMarkerTrace(t)
	if err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error { return nil }); err != nil {
		t.Fatalf("effect: %v", err)
	}

	*trace = nil
	if err := heartbeatLoopRun(cfg, "daily-brief", rec.RunID, loopNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("post-effect heartbeat: %v", err)
	}
	if got := strings.Join(*trace, ","); got != "fsync,dirsync" {
		t.Fatalf("post-effect heartbeat durability trace = %q, want fsync,dirsync", got)
	}

	*trace = nil
	if err := loopDone(cfg, "daily-brief", rec.RunID, true, "", false, loopNow.Add(3*time.Minute), &out); err != nil {
		t.Fatalf("post-effect done: %v", err)
	}
	if got := strings.Join(*trace, ","); got != "fsync,dirsync" {
		t.Fatalf("post-effect done durability trace = %q, want fsync,dirsync", got)
	}
}

func TestLoopEffectIntentDurabilityFailurePreventsFunction(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	origSync, origClock := markerSyncFn, loopClock
	markerSyncFn = func(*os.File) error { return errors.New("fsync unavailable") }
	loopClock = func() time.Time { return loopNow.Add(time.Minute) }
	t.Cleanup(func() {
		markerSyncFn = origSync
		loopClock = origClock
	})

	effects := 0
	err := withLoopRunEffect(cfg, "daily-brief", rec.RunID, func() error {
		effects++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "persist pre-effect loop intent") {
		t.Fatalf("intent durability failure = %v, want pre-effect refusal", err)
	}
	if effects != 0 {
		t.Fatalf("effect ran %d times despite failed durable intent", effects)
	}
	got, _ := loadRunRecord(cfg, "daily-brief")
	if got.EffectStartedAt != "" || got.EffectCommittedAt != "" {
		t.Fatalf("failed durable intent changed authoritative record: %+v", got)
	}
}

func TestLoopEffectRefusesCrossPeriodAdvance(t *testing.T) {
	cfg := loopTestCfg(t)
	beforeMidnight := time.Date(2026, 6, 24, 23, 59, 59, 0, time.UTC)
	afterMidnight := beforeMidnight.Add(2 * time.Second)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, beforeMidnight, &out); err != nil {
		t.Fatalf("begin before boundary: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	effects := 0
	err := withLoopRunEffectAt(cfg, "daily-brief", rec.RunID, afterMidnight, func() error {
		effects++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "refusing cross-period advance") {
		t.Fatalf("cross-period effect = %v, want refusal", err)
	}
	if effects != 0 {
		t.Fatalf("cross-period effect ran %d times", effects)
	}
	after, _ := loadRunRecord(cfg, "daily-brief")
	if after.EffectStartedAt != "" || after.EffectCommittedAt != "" {
		t.Fatalf("cross-period refusal wrote effect intent: %+v", after)
	}
}

// TestLoopEffectKillHelper is subprocess-only. It durably enters the effect,
// writes one observable side effect, and then pauses at the post-effect /
// pre-commit boundary until the parent kills it.
func TestLoopEffectKillHelper(t *testing.T) {
	if os.Getenv("MORA_TEST_LOOP_EFFECT_HELPER") != "1" {
		return
	}
	cfg := Config{StateDir: os.Getenv("MORA_TEST_LOOP_EFFECT_STATE")}
	runID := os.Getenv("MORA_TEST_LOOP_EFFECT_RUN")
	ready := os.Getenv("MORA_TEST_LOOP_EFFECT_READY")
	sideEffect := os.Getenv("MORA_TEST_LOOP_EFFECT_SENTINEL")
	testHookLoopEffectAfterRun = func() error {
		if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
			return err
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if err := withLoopRunEffect(cfg, "daily-brief", runID, func() error {
		return os.WriteFile(sideEffect, []byte("effect\n"), 0o600)
	}); err != nil {
		t.Fatalf("helper effect: %v", err)
	}
}

func TestLoopEffectProcessKillCannotReplaySamePeriod(t *testing.T) {
	root := t.TempDir()
	cfg := Config{StateDir: filepath.Join(root, "state")}
	now := time.Now().UTC().Truncate(time.Second)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, now, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	ready := filepath.Join(root, "ready")
	sentinel := filepath.Join(root, "effect")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoopEffectKillHelper$")
	cmd.Env = append(os.Environ(),
		"MORA_TEST_LOOP_EFFECT_HELPER=1",
		"MORA_TEST_LOOP_EFFECT_STATE="+cfg.StateDir,
		"MORA_TEST_LOOP_EFFECT_RUN="+rec.RunID,
		"MORA_TEST_LOOP_EFFECT_READY="+ready,
		"MORA_TEST_LOOP_EFFECT_SENTINEL="+sentinel,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start loop-effect helper: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			killed = true
			t.Fatal("loop-effect helper did not reach post-effect boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill loop-effect helper: %v", err)
	}
	_ = cmd.Wait()
	killed = true

	uncertain, _ := loadRunRecord(cfg, "daily-brief")
	startedAt, err := time.Parse(time.RFC3339, uncertain.EffectStartedAt)
	if err != nil || uncertain.EffectCommittedAt != "" {
		t.Fatalf("killed effect record = %+v, started parse err=%v", uncertain, err)
	}
	out.Reset()
	err = loopBegin(cfg, "daily-brief", true, startedAt.Add(loopLockTTL+time.Minute), &out)
	if err == nil || !strings.Contains(err.Error(), "outcome is uncertain") {
		t.Fatalf("post-kill same-period begin = %v, want uncertainty refusal", err)
	}
	after, _ := loadRunRecord(cfg, "daily-brief")
	if after.RunID != rec.RunID || after.Attempt != 1 {
		t.Fatalf("post-kill begin replaced run: before=%+v after=%+v", rec, after)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "effect\n" {
		t.Fatalf("side effect sentinel = %q, %v; want exactly one effect", body, err)
	}
	out.Reset()
	nextPeriod := startedAt.Add(24 * time.Hour)
	if err := loopBegin(cfg, "daily-brief", true, nextPeriod, &out); err != nil {
		t.Fatalf("next-period begin remained blocked by prior uncertainty: %v", err)
	}
	next, _ := loadRunRecord(cfg, "daily-brief")
	if next.Period == uncertain.Period || next.RunID == uncertain.RunID {
		t.Fatalf("next-period begin did not open a fresh period: old=%+v new=%+v", uncertain, next)
	}
}

func TestLoopBeginStartedOnlyAlwaysBlocksSamePeriod(t *testing.T) {
	for _, status := range []loopRunStatus{loopRunRunning, loopRunFailed, loopRunSucceeded} {
		t.Run(string(status), func(t *testing.T) {
			cfg := loopTestCfg(t)
			rec := loopRunRecord{
				LoopID: "daily-brief", RunID: "run_uncertain", Period: periodFor("daily", loopNow),
				Status: status, Attempt: 1, StartedAt: loopNow.Format(time.RFC3339),
				HeartbeatAt: loopNow.Format(time.RFC3339), EffectStartedAt: loopNow.Format(time.RFC3339),
			}
			if err := saveRunRecord(cfg, rec, loopNow); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := loopBegin(cfg, "daily-brief", true, loopNow.Add(time.Minute), &out)
			if err == nil || !strings.Contains(err.Error(), "outcome is uncertain") {
				t.Fatalf("begin over %s started-only record = %v, want uncertainty", status, err)
			}
		})
	}
}

func TestLoopDoneOKRejectsUncommittedStartedEffect(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	rec.EffectStartedAt = loopNow.Add(time.Minute).Format(time.RFC3339)
	if err := saveRunRecord(cfg, rec, loopNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	err := loopDone(cfg, "daily-brief", rec.RunID, true, "", false, loopNow.Add(2*time.Minute), &out)
	if err == nil || !strings.Contains(err.Error(), "uncertain non-idempotent effect") {
		t.Fatalf("done --ok over started-only effect = %v, want refusal", err)
	}
}

// TestLoopHeartbeatRefusesSupersededOwner pins the owner-CAS half: an old run
// cannot restamp a successor's lease or latest.json heartbeat.
func TestLoopHeartbeatRefusesSupersededOwner(t *testing.T) {
	cfg := loopTestCfg(t)
	seedRecord(t, cfg, loopRunRecord{
		LoopID: "daily-brief", RunID: "run_B", Period: "2026-06-24",
		Status: loopRunRunning, Attempt: 2, HeartbeatAt: loopNow.Format(time.RFC3339),
		IdempotencyKey: "daily-brief@2026-06-24",
	})
	plantLock(t, cfg, "daily-brief", "run_B", os.Getpid(), loopNow)
	err := heartbeatLoopRun(cfg, "daily-brief", "run_A", loopNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("stale heartbeat error = %v, want superseded refusal", err)
	}
	data, _ := os.ReadFile(loopLockPath(cfg, "daily-brief"))
	var body loopLockBody
	_ = json.Unmarshal(data, &body)
	if body.RunID != "run_B" || body.AcquiredAt != loopNow.Format(time.RFC3339) {
		t.Fatalf("stale heartbeat mutated successor lease: %+v", body)
	}
}

// TestLoopDone_OkArchivesImmutableRunCopy: done --ok writes an immutable
// runs/<period>_<run_id>.json audit copy.
func TestLoopDone_OkArchivesImmutableRunCopy(t *testing.T) {
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	rec, _ := loadRunRecord(cfg, "daily-brief")
	if err := loopDone(cfg, "daily-brief", "", true, "", false, loopNow, &out); err != nil {
		t.Fatalf("done ok: %v", err)
	}
	if _, err := os.Stat(loopRunArchivePath(cfg, "daily-brief", rec.Period, rec.RunID)); err != nil {
		t.Fatalf("immutable run archive not written: %v", err)
	}
}

// TestLoopDone_SupersededRunRefused: a late done from a run that a newer attempt
// has replaced must refuse — never clobber the current run's record (Codex #3).
func TestLoopDone_SupersededRunRefused(t *testing.T) {
	cfg := loopTestCfg(t)
	// The CURRENT run on record is run_R2 (running).
	seedRecord(t, cfg, loopRunRecord{
		LoopID: "daily-brief", RunID: "run_R2", Period: "2026-06-24",
		Status: loopRunRunning, Attempt: 2, IdempotencyKey: "daily-brief@2026-06-24",
	})
	var out bytes.Buffer
	// An abandoned run_R1 tries to close late.
	err := loopDone(cfg, "daily-brief", "run_R1", true, "", false, loopNow, &out)
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("late done from superseded run: err=%v, want a 'superseded' refusal", err)
	}
	if rec, _ := loadRunRecord(cfg, "daily-brief"); rec.Status != loopRunRunning || rec.RunID != "run_R2" {
		t.Fatalf("superseded done must NOT touch the current run; got %+v", rec)
	}
}

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
	err := loopDone(cfg, "daily-brief", "run_R1", true, "", false, loopNow, &out)
	if err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("done while lease moved to a newer run: err=%v, want a 'lease' refusal", err)
	}
	if rec, _ := loadRunRecord(cfg, "daily-brief"); rec.Status != loopRunRunning {
		t.Fatalf("refused done must NOT flip the record; got %+v", rec)
	}
}

// TestReleaseLoopLockForOnlyOwnRun: releasing a lease never deletes a lock a
// DIFFERENT run owns (Codex #3 — the lock-deletion half).
func TestReleaseLoopLockForOnlyOwnRun(t *testing.T) {
	cfg := loopTestCfg(t)
	if err := os.MkdirAll(loopDir(cfg, "daily-brief"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A newer run owns the lock.
	plantLock(t, cfg, "daily-brief", "run_R2", os.Getpid(), loopNow)
	releaseLoopLockFor(cfg, "daily-brief", "run_R1") // an older run tries to release
	if _, err := os.Stat(loopLockPath(cfg, "daily-brief")); err != nil {
		t.Fatalf("a newer run's lock must NOT be deleted by an older run's release: %v", err)
	}
	// The owning run releases it.
	releaseLoopLockFor(cfg, "daily-brief", "run_R2")
	if _, err := os.Stat(loopLockPath(cfg, "daily-brief")); !os.IsNotExist(err) {
		t.Fatalf("the owning run's release should remove the lock; stat err=%v", err)
	}
}

// TestReleaseSerializesAgainstSameOwnerHeartbeat closes the release leak: once
// release has read its owner, a heartbeat must wait. Release removes the lease;
// the delayed heartbeat then observes absence and cannot silently leave a fresh
// same-owner lock behind.
func TestReleaseSerializesAgainstSameOwnerHeartbeat(t *testing.T) {
	cfg := loopTestCfg(t)
	path := loopLockPath(cfg, "daily-brief")
	plantLock(t, cfg, "daily-brief", "run_A", os.Getpid(), loopNow)

	heartbeatStarted := make(chan struct{})
	heartbeatDone := make(chan bool, 1)
	heartbeatFinishedEarly := false
	heartbeatOwned := false
	testHookReleaseLockAfterRead = func() {
		go func() {
			close(heartbeatStarted)
			heartbeatDone <- heartbeatLockFileFor(path, "run_A", loopNow.Add(time.Minute))
		}()
		<-heartbeatStarted
		select {
		case heartbeatOwned = <-heartbeatDone:
			heartbeatFinishedEarly = true
		case <-time.After(50 * time.Millisecond):
			// Expected: heartbeat waits for release's read/check/remove transition.
		}
	}
	t.Cleanup(func() { testHookReleaseLockAfterRead = nil })

	releaseLockFileFor(path, "run_A")
	if heartbeatFinishedEarly {
		t.Fatalf("heartbeat completed inside guarded release (owned=%v)", heartbeatOwned)
	}
	heartbeatOwned = <-heartbeatDone
	if heartbeatOwned {
		t.Fatal("heartbeat resurrected a lease after the same owner's release")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("release/heartbeat race leaked lease; stat err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// TIER E — status health ladder (GENERIC: run-record + registry only)
// ---------------------------------------------------------------------------
// The ladder is pure over an injected now. HOME is pinned to a temp dir so the
// scheduler annotation is deterministic (no stray com.mora.*.plist). The
// brief-coupled refinements (ran-uncommitted, source-stale) are deliberately
// out of scope to keep `mora loop` brief-agnostic.

func reg(cadence string) loopRegistration {
	return loopRegistration{SchemaVersion: loopRegistrySchemaVersion, LoopID: "daily-brief", Enabled: true, Cadence: cadence}
}

// TestLoopStatus_NeverRun: no run record -> "never-run", scheduler annotated.
func TestLoopStatus_NeverRun(t *testing.T) {
	setTestHome(t, t.TempDir())
	h := classifyLoopHealth(reg("daily"), loopRunRecord{}, false, loopNow)
	if h.State != "never-run" {
		t.Fatalf("state = %q, want never-run", h.State)
	}
	if h.Scheduled {
		t.Error("want scheduled=false (no plist in temp HOME)")
	}
}

// TestLoopStatus_RunningFresh: a running record with a recent heartbeat -> "running".
func TestLoopStatus_RunningFresh(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec := loopRunRecord{
		LoopID: "daily-brief", RunID: "run_x", Period: "2026-06-24", Status: loopRunRunning,
		Attempt: 1, StartedAt: loopNow.Format(time.RFC3339), HeartbeatAt: loopNow.Format(time.RFC3339),
	}
	if h := classifyLoopHealth(reg("daily"), rec, true, loopNow); h.State != "running" {
		t.Fatalf("state = %q, want running", h.State)
	}
}

// TestLoopStatus_RunningAbandonedIsStale: a running record whose heartbeat is far
// in the past is an abandoned/leaked run -> "stale" (stale beats a dead 'running').
func TestLoopStatus_RunningAbandonedIsStale(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec := loopRunRecord{
		LoopID: "daily-brief", RunID: "run_x", Period: "2026-06-24", Status: loopRunRunning,
		Attempt: 1, StartedAt: loopNow.Add(-72 * time.Hour).Format(time.RFC3339),
		HeartbeatAt: loopNow.Add(-72 * time.Hour).Format(time.RFC3339),
	}
	if h := classifyLoopHealth(reg("daily"), rec, true, loopNow); h.State != "stale" {
		t.Fatalf("state = %q, want stale (abandoned running)", h.State)
	}
}

// TestLoopStatus_Failed: a failed terminal -> "failed", surfacing the error.
func TestLoopStatus_Failed(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec := loopRunRecord{
		LoopID: "daily-brief", RunID: "run_x", Period: "2026-06-24", Status: loopRunFailed,
		Attempt: 1, FinishedAt: loopNow.Format(time.RFC3339), LastError: "sync: token expired",
	}
	h := classifyLoopHealth(reg("daily"), rec, true, loopNow)
	if h.State != "failed" {
		t.Fatalf("state = %q, want failed", h.State)
	}
	if !strings.Contains(h.Message, "token expired") {
		t.Errorf("message should surface the error, got %q", h.Message)
	}
}

// TestLoopStatus_Ok: a recent success -> "ok".
func TestLoopStatus_Ok(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec := loopRunRecord{
		LoopID: "daily-brief", RunID: "run_x", Period: "2026-06-24", Status: loopRunSucceeded,
		Attempt: 1, FinishedAt: loopNow.Format(time.RFC3339),
	}
	if h := classifyLoopHealth(reg("daily"), rec, true, loopNow); h.State != "ok" {
		t.Fatalf("state = %q, want ok", h.State)
	}
}

// TestLoopStatus_SucceededTooOldIsStale: a success older than the cadence allows
// -> "stale" (success is liveness, not freshness).
func TestLoopStatus_SucceededTooOldIsStale(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec := loopRunRecord{
		LoopID: "daily-brief", RunID: "run_x", Period: "2026-06-21", Status: loopRunSucceeded,
		Attempt: 1, FinishedAt: loopNow.Add(-72 * time.Hour).Format(time.RFC3339), // > 48h daily lag
	}
	if h := classifyLoopHealth(reg("daily"), rec, true, loopNow); h.State != "stale" {
		t.Fatalf("state = %q, want stale (success older than daily lag)", h.State)
	}
}

// TestLoopAllowedLag: the cadence staleness windows (a dead hourly loop must not
// read fresh for 48h).
func TestLoopAllowedLag(t *testing.T) {
	cases := []struct {
		cadence string
		want    time.Duration
	}{
		{"daily", 48 * time.Hour},
		{"hourly", 2 * time.Hour},
		{"weekly", 8 * 24 * time.Hour},
		{"", 48 * time.Hour}, // default daily
	}
	for _, c := range cases {
		if got := loopAllowedLag(c.cadence); got != c.want {
			t.Errorf("loopAllowedLag(%q) = %v, want %v", c.cadence, got, c.want)
		}
	}
}

// TestLoopScheduledAnnotation: the secondary scheduler annotation reflects the
// plist's presence (darwin-only; the non-darwin floor reports false).
func TestLoopScheduledAnnotation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd plist detection is darwin-only (TTL-only floor elsewhere)")
	}
	home := t.TempDir()
	setTestHome(t, home)
	rec := loopRunRecord{
		LoopID: "daily-brief", RunID: "run_x", Period: "2026-06-24", Status: loopRunSucceeded,
		Attempt: 1, FinishedAt: loopNow.Format(time.RFC3339),
	}
	// absent plist -> scheduled false + annotation
	h := classifyLoopHealth(reg("daily"), rec, true, loopNow)
	if h.Scheduled || !strings.Contains(h.Message, "scheduler") {
		t.Fatalf("absent plist: scheduled=%v msg=%q, want false + scheduler annotation", h.Scheduled, h.Message)
	}
	// present plist (linked job pulse-daily) -> scheduled true, no annotation
	la := filepath.Join(home, "Library", "LaunchAgents")
	writeRawFile(t, filepath.Join(la, "com.mora.pulse-daily.plist"), []byte("<plist/>"))
	r := reg("daily")
	r.ScheduleJob = "pulse-daily"
	h2 := classifyLoopHealth(r, rec, true, loopNow)
	if !h2.Scheduled || strings.Contains(h2.Message, "scheduler missing") {
		t.Fatalf("present plist: scheduled=%v msg=%q, want true + no missing annotation", h2.Scheduled, h2.Message)
	}
}

// TestLoopStatus_Integration: the loopStatus command emits the classified state
// as JSON over the real file store.
func TestLoopStatus_Integration(t *testing.T) {
	setTestHome(t, t.TempDir())
	cfg := loopTestCfg(t)
	var out bytes.Buffer
	if err := loopBegin(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("begin: %v", err)
	}
	out.Reset()
	if err := loopStatus(cfg, "daily-brief", true, loopNow, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	var h loopHealth
	if err := json.Unmarshal(out.Bytes(), &h); err != nil {
		t.Fatalf("status json: %v (out=%q)", err, out.String())
	}
	if h.State != "running" {
		t.Fatalf("status state = %q, want running", h.State)
	}
}

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
	// Plan 01-07: the receipt carries schema_version as a number, so the
	// document no longer decodes into map[string]string.
	var heartbeat map[string]any
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
