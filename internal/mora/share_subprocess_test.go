package mora

// share_subprocess_test.go — Packet H two-process replays (REAL subprocesses,
// not goroutine fakes): the interrupted-import and reaped-zombie incidents. The
// child runs TestShareSubprocessWorker (gated on an env var) and parks at a
// deterministic hook so the parent can SIGKILL it or race a successor.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestShareSubprocessWorker is the child helper. It resolves the SAME config the
// parent set up (via HOME) and runs a fixture import, parking at the hook named
// by MORA_SHARE_HOOK so the parent can interrupt or reap it.
func TestShareSubprocessWorker(t *testing.T) {
	if os.Getenv("MORA_SHARE_WORKER") == "" {
		t.Skip("not the subprocess worker")
	}
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("child loadConfig: %v", err)
	}
	name := os.Getenv("MORA_SHARE_WORKER_SUB")
	dir := os.Getenv("MORA_SHARE_WORKER_REPO")
	parked := os.Getenv("MORA_SHARE_PARKED")
	resume := os.Getenv("MORA_SHARE_RESUME")

	// ro-search mode: a pure reader process. It opens the published generation
	// read-only and searches it in a tight loop; the test's whole point is that a
	// second live process never surfaces SQLITE_BUSY, so any busy error is fatal.
	if os.Getenv("MORA_SHARE_MODE") == "ro-search" {
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			c, ok, rerr := resolvePublishedCommit(cfg, name)
			if rerr != nil {
				t.Fatalf("child resolvePublishedCommit: %v", rerr)
			}
			if !ok {
				continue
			}
			db, oerr := openShareIndexRO(context.Background(), shareGenIndexPath(cfg, name, c.Gen), c.IndexDigest)
			if oerr != nil {
				if isSQLiteBusy(oerr) {
					t.Fatalf("child reader hit SQLITE_BUSY on open: %v", oerr)
				}
				t.Fatalf("child openShareIndexRO: %v", oerr)
			}
			var n int
			qerr := db.QueryRowContext(context.Background(), `SELECT count(*) FROM memories`).Scan(&n)
			_ = db.Close()
			if qerr != nil {
				if isSQLiteBusy(qerr) {
					t.Fatalf("child reader hit SQLITE_BUSY on query: %v", qerr)
				}
				t.Fatalf("child query: %v", qerr)
			}
		}
		return
	}

	switch os.Getenv("MORA_SHARE_HOOK") {
	case "post-first-corpus":
		// Interrupted-import: park after the first corpus file lands, before commit.
		testHookPostFirstGenCorpusWrite = func() {
			_ = os.WriteFile(parked, []byte("1"), 0o644)
			select {} // block forever; the parent SIGKILLs us here
		}
	case "pre-commit-age-lease":
		// Zombie: fully build gen, then park right before the claim loop with an
		// AGED lease so a successor can reap us; resume when told.
		testHookPreCommitClaim = func() {
			ageImportLease(cfg, name)
			ageLeaseFile(shareStorageLockPath(cfg)) // so a successor can reap BOTH leases
			_ = os.WriteFile(parked, []byte("1"), 0o644)
			for {
				if _, err := os.Stat(resume); err == nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	// The result is intentionally ignored — the parent asserts on-disk state.
	if _, ierr := importFixtureGeneration(context.Background(), cfg,
		shareSubscription{Name: name, Remote: "r"}, dir); ierr != nil {
		fmt.Fprintf(os.Stderr, "worker importFixtureGeneration: %v\n", ierr)
	}
}

// isSQLiteBusy reports whether err is a raw SQLITE_BUSY ("database is locked"),
// the failure the share-DSN busy_timeout + WAL fix exists to prevent.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// ageImportLease rewrites the import lease's acquired_at to well past the TTL so a
// successor can reap it — a deterministic stand-in for a slow/parked holder.
func ageImportLease(cfg Config, name string) { ageLeaseFile(shareImportLockPath(cfg, name)) }

// ageLeaseFile rewrites a lease file's acquired_at to well past the TTL.
func ageLeaseFile(lockPath string) {
	old := time.Now().Add(-3 * shareImportTTL)
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil {
		return
	}
	body.AcquiredAt = old.UTC().Format(time.RFC3339)
	next, _ := json.Marshal(body)
	_ = os.WriteFile(lockPath, next, 0o600)
}

func spawnShareWorker(t *testing.T, home, sub, repo, hook, parked, resume string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestShareSubprocessWorker", "-test.timeout=120s")
	cmd.Env = append(os.Environ(),
		"MORA_SHARE_WORKER=1",
		"MORA_TEST_SUBPROCESS=1",
		"HOME="+home, "USERPROFILE="+home, "MORA_CONFIG_DIR=",
		"MORA_SHARE_WORKER_SUB="+sub,
		"MORA_SHARE_WORKER_REPO="+repo,
		"MORA_SHARE_HOOK="+hook,
		"MORA_SHARE_PARKED="+parked,
		"MORA_SHARE_RESUME="+resume,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	return cmd
}

// spawnROSearchWorker starts a pure-reader child that opens the published
// generation read-only and searches it in a loop, failing on any SQLITE_BUSY.
func spawnROSearchWorker(t *testing.T, home, sub string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestShareSubprocessWorker", "-test.timeout=120s")
	cmd.Env = append(os.Environ(),
		"MORA_SHARE_WORKER=1",
		"MORA_TEST_SUBPROCESS=1",
		"MORA_SHARE_MODE=ro-search",
		"HOME="+home, "USERPROFILE="+home, "MORA_CONFIG_DIR=",
		"MORA_SHARE_WORKER_SUB="+sub,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn ro-search worker: %v", err)
	}
	return cmd
}

func waitForFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// T2: an interrupted (SIGKILLed) import serves the PREVIOUS committed generation;
// the half-built generation is invisible on every surface and is never read fresh.
func TestInterruptedShareImportServesLastGoodGeneration(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	home := cfg.HomeDir()
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")

	keep := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Keep", "durable content")
	old := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Old", "present in last-good")
	publishGen(t, cfg, "neil", id, []Memory{keep, old}) // last-good (seq 1)
	lastGood, _, _ := resolvePublishedCommit(cfg, "neil")

	// Stage a gen-2 fixture that revokes `old` and adds a memory only it would have.
	onlyGen2 := fixtureMemory("mem_20260601_000002_cccccccc", "OnlyGen2", "half-built only")
	_ = os.RemoveAll(filepath.Join(shareRepoDir(cfg, "neil"), "memories"))
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), []Memory{keep, onlyGen2}, true)

	parked := filepath.Join(t.TempDir(), "parked")
	cmd := spawnShareWorker(t, home, "neil", shareRepoDir(cfg, "neil"), "post-first-corpus", parked, "")
	waitForFile(t, parked, 60*time.Second)
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	// (a) search/read serve the previous committed generation (last-good).
	c, ok, _ := resolvePublishedCommit(cfg, "neil")
	if !ok || c.Seq != lastGood.Seq {
		t.Fatalf("resolver advanced past the last-good gen: seq %d want %d", c.Seq, lastGood.Seq)
	}
	if _, ok := findSharedMemory(cfg, old.ID); !ok {
		t.Fatal("last-good generation stopped serving after the interrupt")
	}
	// (b) a memory only in the half-built gen is not found (never committed).
	if _, ok := findSharedMemory(cfg, onlyGen2.ID); ok {
		t.Fatal("a half-built, uncommitted generation was served")
	}
	// (c) doctor never reads fresh: the durable attempt has no matching completion.
	ageImportLease(cfg, "neil") // simulate the dead holder's lease aging past TTL
	if h := shareHealthOne(cfg, "neil", time.Now()); h.State == healthFresh {
		t.Fatalf("a SIGKILLed import was read fresh; want failed/stale (state %q)", h.State)
	}
}

// TestShareIndexNoSQLITEBUSYAcrossProcesses: separate OS processes reading a
// subscribed share's published generation while THIS process keeps publishing
// fresh generations (each build opens WAL + checkpoints TRUNCATE) must never
// surface a raw SQLITE_BUSY. The share-DSN fix (WAL + busy_timeout on both the
// builder and the mode=ro reader) plus the generation-publish layout — readers
// only ever open frozen, checkpointed, run-private index.db files — guarantees it.
func TestShareIndexNoSQLITEBUSYAcrossProcesses(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	home := cfg.HomeDir()
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")

	keep := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Keep", "durable content")
	publishGen(t, cfg, "neil", id, []Memory{keep}) // seq 1: something to read

	readers := []*exec.Cmd{
		spawnROSearchWorker(t, home, "neil"),
		spawnROSearchWorker(t, home, "neil"),
		spawnROSearchWorker(t, home, "neil"),
	}

	// While three separate processes read, publish several more generations here.
	for i := 0; i < 4; i++ {
		extra := fixtureMemory(
			"mem_20260601_00000"+string(rune('1'+i))+"_bbbbbbbb",
			"Extra", "generation churn")
		publishGen(t, cfg, "neil", id, []Memory{keep, extra})
	}

	for i, r := range readers {
		if err := r.Wait(); err != nil {
			t.Fatalf("reader %d exited non-zero (SQLITE_BUSY or worse): %v", i, err)
		}
	}
}

// T6: a reaped-then-resumed zombie holder cannot publish over its successor.
func TestZombieImportCannotPublishOverSuccessor(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	home := cfg.HomeDir()
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")

	keep := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Keep", "kept")
	revoked := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Revoked", "to be revoked")
	publishGen(t, cfg, "neil", id, []Memory{keep, revoked}) // seq 1 (pre-revocation baseline)

	// A: import the PRE-revocation repo, park before the commit claim with an aged
	// lease (reapable).
	preDir := t.TempDir()
	buildShareRepoFixture(t, preDir, id.Recipient(), []Memory{keep, revoked}, true)
	parkedA := filepath.Join(t.TempDir(), "parkedA")
	resumeA := filepath.Join(t.TempDir(), "resumeA")
	cmdA := spawnShareWorker(t, home, "neil", preDir, "pre-commit-age-lease", parkedA, resumeA)
	waitForFile(t, parkedA, 60*time.Second)

	// B: reap A and import the POST-revocation repo (revoked absent), committing.
	postDir := t.TempDir()
	buildShareRepoFixture(t, postDir, id.Recipient(), []Memory{keep}, true)
	if _, err := importFixtureGeneration(context.Background(), cfg,
		shareSubscription{Name: "neil", Remote: "r"}, postDir); err != nil {
		t.Fatalf("successor B import: %v", err)
	}
	bCommit, _, _ := resolvePublishedCommit(cfg, "neil")

	// Un-park A; its ownership re-verify must fail (lease is B's) so it aborts.
	if err := os.WriteFile(resumeA, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = cmdA.Process.Wait()

	// The revoked memory is never served, and A did not supersede B.
	if _, ok := findSharedMemory(cfg, revoked.ID); ok {
		t.Fatal("zombie A published the revoked memory over successor B")
	}
	final, _, _ := resolvePublishedCommit(cfg, "neil")
	if final.Seq != bCommit.Seq {
		t.Fatalf("zombie A advanced the published seq to %d over B's %d", final.Seq, bCommit.Seq)
	}
}
