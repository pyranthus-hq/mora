package mora

// share_primitives_test.go — Packet H fence-primitive, git-pin, bucket-floor, and
// storage witnesses (in-process). The three-actor lease replays and the byte-
// accounting certificates each redden exactly their production mutation.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

// row 40a: concurrent seq claims are atomic — exactly one os.Link wins.
func TestConcurrentCommitClaimsAreAtomic(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "0000000001")
	mk := func(tag string) string {
		p := filepath.Join(dir, ".t-"+tag)
		if err := os.WriteFile(p, []byte(tag), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := claimExclusiveDurable(mk("a"), dest); err != nil {
		t.Fatalf("first claim failed: %v", err)
	}
	if err := claimExclusiveDurable(mk("b"), dest); err == nil {
		t.Fatal("second claim to the same seq succeeded; want EEXIST")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "a" {
		t.Fatalf("winner overwritten: %q", got)
	}
}

// row 40c: a reaped holder's blind release can never steal a live successor's
// lease (three-actor A reaped → B holds → A's late release must be a no-op).
func TestBlindReleaseCannotStealLiveLease(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	past := time.Now().Add(-2 * shareImportTTL)
	relA, err := acquireImportLease(cfg, name, "runA", past) // A acquired long ago (reapable)
	if err != nil {
		t.Fatal(err)
	}
	_ = relA
	relB, err := acquireImportLease(cfg, name, "runB", time.Now()) // B reaps A and holds
	if err != nil {
		t.Fatalf("B could not reap+acquire: %v", err)
	}
	defer relB()
	// A's late release (owner-CAS on runA) must NOT delete B's lease.
	releaseLockFileFor(shareImportLockPath(cfg, name), "runA")
	if err := verifyImportLeaseOwner(cfg, name, "runB", time.Now()); err != nil {
		t.Fatalf("B's live lease was stolen by A's blind release: %v", err)
	}
}

// row 40d: a reaped lease cannot be resurrected by the old holder's heartbeat.
func TestReapedLeaseCannotBeResurrectedByHeartbeat(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	past := time.Now().Add(-2 * shareImportTTL)
	if _, err := acquireImportLease(cfg, name, "runA", past); err != nil {
		t.Fatal(err)
	}
	relB, err := acquireImportLease(cfg, name, "runB", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer relB()
	// A's CAS heartbeat must fail (ownership lost) and not re-stamp A's lease live.
	if heartbeatLockFileFor(shareImportLockPath(cfg, name), "runA", time.Now()) {
		t.Fatal("reaped holder A resurrected its lease via heartbeat")
	}
	if err := verifyImportLeaseOwner(cfg, name, "runB", time.Now()); err != nil {
		t.Fatalf("B lost ownership to A's heartbeat: %v", err)
	}
	if verifyImportLeaseOwner(cfg, name, "runA", time.Now()) == nil {
		t.Fatal("A's commit fence would pass falsely after reap")
	}
}

// row 51e: a stale completer cannot mask a successor's active attempt record.
func TestStaleCompleterCannotMaskSuccessorAttempt(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	if err := os.MkdirAll(shareSubRoot(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := startShareAttempt(cfg, name, "runA", time.Now()); err != nil {
		t.Fatal(err)
	}
	// B reaps and publishes its own active record.
	if err := startShareAttempt(cfg, name, "runB", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A's stale completion must be discarded, never clobbering B.
	err := finishShareAttempt(cfg, name, "runA", shareAttempt{State: "succeeded", Seq: 1})
	if err == nil {
		t.Fatal("stale completer A succeeded in writing over B")
	}
	got, ok, loadErr := loadShareAttempt(cfg, name)
	if loadErr != nil || !ok || got.RunID != "runB" || got.State != "active" {
		t.Fatalf("successor B's active record was masked: %+v", got)
	}
}

// ---- git pin (real git) ----

func readFileMaybe(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// row 46a: the pin fetch writes ONLY the run-private ref; FETCH_HEAD, HEAD, and
// refs/remotes/origin/* stay byte-for-byte unchanged.
func TestGitFetchWritesOnlyRunPrivatePin(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	remote := realGitShareRemote(t, id.Recipient(), []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")})
	var buf bytes.Buffer
	if err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &buf, realExec); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	repo := shareRepoDir(cfg, "neil")

	// Advance the remote.
	body, _ := renderMemory(fixtureMemory("mem_20260601_000001_bbbbbbbb", "B", "two"))
	ct, _ := encryptShareBytes([]age.Recipient{id.Recipient()}, body)
	if err := os.WriteFile(filepath.Join(remote, "memories", "mem_20260601_000001_bbbbbbbb.md.age"), ct, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "b")
	newTip := strings.TrimSpace(mustGit(t, remote, "rev-parse", "HEAD"))

	fhBefore := readFileMaybe(filepath.Join(repo, ".git", "FETCH_HEAD"))
	headBefore := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
	originBefore := mustGit(t, repo, "for-each-ref", "refs/remotes/origin/")

	pin, err := acquireGitPin(context.Background(), cfg, shareSubscription{Name: "neil", Remote: remote}, "runpin", realExec)
	if err != nil {
		t.Fatalf("acquireGitPin: %v", err)
	}
	defer pin.cleanup()
	if pin.sha != newTip {
		t.Fatalf("pin sha %s != new tip %s", pin.sha, newTip)
	}
	if got := readFileMaybe(filepath.Join(repo, ".git", "FETCH_HEAD")); got != fhBefore {
		t.Fatal("pin fetch mutated FETCH_HEAD")
	}
	if got := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD")); got != headBefore {
		t.Fatal("pin fetch moved HEAD")
	}
	if got := mustGit(t, repo, "for-each-ref", "refs/remotes/origin/"); got != originBefore {
		t.Fatalf("pin fetch mutated refs/remotes/origin/*:\nbefore %s\nafter  %s", originBefore, got)
	}
	// Exactly the private ref advanced.
	if got := strings.TrimSpace(mustGit(t, repo, "rev-parse", "--verify", pin.ref)); got != newTip {
		t.Fatalf("private pin ref = %s; want %s", got, newTip)
	}
}

// row 46b: a non-fast-forward pin (publisher history rewrite) is refused.
func TestGitNonFastForwardPinIsRefused(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	remote := realGitShareRemote(t, id.Recipient(), []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")})
	var buf bytes.Buffer
	if err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &buf, realExec); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	// Rewrite remote history (amend ⇒ the published base is no longer an ancestor).
	mustGit(t, remote, "commit", "--amend", "--allow-empty", "-q", "-m", "rewritten history")
	pin, err := acquireGitPin(context.Background(), cfg, shareSubscription{Name: "neil", Remote: remote}, "runpin", realExec)
	if pin != nil {
		pin.cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "rotated") {
		t.Fatalf("non-ff pin = %v; want rotated-history refusal", err)
	}
}

// ---- bucket anti-rollback floor ----

// row 50a: a replayed older bucket version is rejected by the committed floor
// even after a crash lost LastVersion.
func TestReplayedOlderBucketEnvelopeRejected(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	// Simulate a durable commit at version 5 (floor 5) whose LastVersion update was
	// lost to a crash.
	if err := os.MkdirAll(shareCommitsDir(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := shareCommit{Seq: 1, Gen: "gen-old", RunID: "r0", BucketFloor: 5, BuiltAt: "2026-07-01T00:00:00Z"}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(shareCommitPath(cfg, name, 1), b, 0o644); err != nil {
		t.Fatal(err)
	}
	rel := acquirePublishLeases(t, cfg, name, "runX")
	defer rel()
	// A replayed envelope at version 4 (< committed floor 5) must be rejected.
	_, err := publishShareGeneration(cfg, shareCommitParams{
		name: name, runID: "runX", gen: "gen-replay", isBucket: true, fetched: 4,
		subVersion: 0, parentFloor: -1, builtAt: time.Now(), corpusDigest: "d", indexDigest: "d", count: 1,
	})
	if err != errRollback {
		t.Fatalf("replayed older bucket version = %v; want errRollback", err)
	}
}

// row 50c (heal inheritance): a repair commit inherits its parent's floor.
func TestHealInheritsBucketFloor(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	if err := os.MkdirAll(shareSubRoot(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := acquirePublishLeases(t, cfg, name, "runX")
	defer rel()
	seq, err := publishShareGeneration(cfg, shareCommitParams{
		name: name, runID: "runX", gen: "gen-heal", parentFloor: 7,
		builtAt: time.Now(), corpusDigest: "d", indexDigest: "d", count: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := commitAtSeq(t, cfg, name, seq)
	if c.BucketFloor != 7 {
		t.Fatalf("heal commit floor = %d; want inherited 7", c.BucketFloor)
	}
}

// row 44e / floor safety: GC refuses to lower the replay floor.
func TestGCRefusesToLowerBucketFloor(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	if err := os.MkdirAll(shareCommitsDir(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	// seq 1 carries a HIGHER floor than the published seq 2 — a pathological legacy
	// state GC must refuse to reduce.
	write := func(seq, floor int, gen string) {
		rec := shareCommit{Seq: seq, Gen: gen, RunID: "r", BucketFloor: floor, BuiltAt: "2026-07-01T00:00:00Z"}
		b, _ := json.MarshalIndent(rec, "", "  ")
		if err := os.WriteFile(shareCommitPath(cfg, name, seq), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// seq 1 carries the high floor; several newer records (floor 3) push seq 1
	// past the K-retain window so GC would delete it — and thereby lower the floor.
	write(1, 9, "gen-a")
	write(2, 3, "gen-b")
	write(3, 3, "gen-c")
	write(4, 3, "gen-d")
	write(5, 3, "gen-e")
	err := shareGCSweep(cfg, name, time.Now())
	if err == nil {
		t.Fatal("GC lowered the replay floor; want a loud abort")
	}
	if _, statErr := os.Stat(shareCommitPath(cfg, name, 1)); os.IsNotExist(statErr) {
		t.Fatal("GC deleted the higher-floor record it should have refused to touch")
	}
}

// ---- whole-product storage accountant ----

// row 53h: storage accounting fails CLOSED on an unreadable path.
func TestShareStorageAccountingFailsClosedOnUnreadablePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX chmod semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod permission bits")
	}
	withTempHome(t)
	cfg := mustConfig(t)
	bad := filepath.Join(cfg.DataDir, "share", "unreadable")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o700) })
	if _, err := productStorageBytes(cfg); err == nil {
		t.Fatal("storage accounting returned no error on an unreadable path (undercount)")
	}
}

// row 53g: doctor's storage_status uses the whole-product accountant.
func TestDoctorStorageUsesWholeProductAccountant(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	// A sparse file larger than the 15 GiB ceiling under DataDir (no real disk use).
	big := filepath.Join(cfg.DataDir, "share", "huge.bin")
	if err := os.MkdirAll(filepath.Dir(big), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if terr := f.Truncate(16 << 30); terr != nil {
		f.Close()
		t.Skipf("cannot create sparse file: %v", terr)
	}
	f.Close()
	used, err := productStorageBytes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if storageStatus(used) != "over" {
		t.Fatalf("storage_status = %q at %d bytes; want over", storageStatus(used), used)
	}
}

// row 54b: a legal larger share has an explicit oversubscription decision path.
func TestLegalLargeShareHasExplicitOversubscriptionPath(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	// Set a tiny limit so even a small share is refused.
	var buf bytes.Buffer
	if err := cmdShareStorageLimit(cfg, []string{"16"}, &buf, testStderr, time.Now()); err != nil {
		t.Fatal(err)
	}
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(),
		[]Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "some content here")}, true)
	_, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: "neil", Remote: "r"}, shareRepoDir(cfg, "neil"))
	if err == nil || !strings.Contains(err.Error(), "storage-limit") {
		t.Fatalf("oversubscription refusal = %v; want a storage-limit decision path", err)
	}
	// The exact printed decision must admit the same share. Jumping to an
	// unrelated large value would not prove that <required> is actionable.
	const marker = "storage-limit "
	i := strings.Index(err.Error(), marker)
	if i < 0 {
		t.Fatalf("refusal did not print required storage-limit bytes: %v", err)
	}
	fields := strings.Fields(err.Error()[i+len(marker):])
	if len(fields) == 0 {
		t.Fatalf("refusal did not print required storage-limit bytes: %v", err)
	}
	required := strings.Trim(fields[0], "'\"")
	if err := cmdShareStorageLimit(cfg, []string{required}, &buf, testStderr, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: "neil", Remote: "r"}, shareRepoDir(cfg, "neil")); err != nil {
		t.Fatalf("printed storage-limit %s did not admit the same input: %v", required, err)
	}
}
