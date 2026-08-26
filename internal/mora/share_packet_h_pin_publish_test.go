package mora

// Exact r7 Packet-H witnesses for the pin/publish/read lane. These tests keep
// one production mechanism per name so the Gate 2 driver can fail closed when
// a required witness is renamed or omitted.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func TestUncommittedOrHalfBuiltGenerationNeverServed(t *testing.T) {
	subRun(t, "premature_link", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		id := writeTestIdentity(t, cfg)
		registerSub(t, cfg, "neil")
		old := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Old", "last good")
		publishGen(t, cfg, "neil", id, []Memory{old})
		before, _, err := resolvePublishedCommit(cfg, "neil")
		if err != nil {
			t.Fatal(err)
		}

		fresh := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Fresh", "half built")
		buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), []Memory{fresh}, true)
		orig := rebuildShareIndexFn
		rebuildShareIndexFn = func(context.Context, string, []Memory) error { return errInjectedRebuild }
		t.Cleanup(func() { rebuildShareIndexFn = orig })
		if _, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: "neil", Remote: "r"}, shareRepoDir(cfg, "neil")); err == nil {
			t.Fatal("half-built generation import succeeded")
		}
		after, ok, err := resolvePublishedCommit(cfg, "neil")
		if err != nil || !ok || after.Seq != before.Seq {
			t.Fatalf("half-built generation became resolvable: before=%+v after=%+v ok=%v err=%v", before, after, ok, err)
		}
		if _, ok := findSharedMemory(cfg, fresh.ID); ok {
			t.Fatal("half-built generation was served")
		}
	})
}

func TestUncommittedGenerationDirIsNeverResolved(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	publishGen(t, cfg, "neil", id, []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "committed")})

	ghostID := "mem_20260601_000009_cccccccc"
	ghost := "gen-ghost"
	dir := shareGenCorpusDir(cfg, "neil", ghost)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := renderMemory(fixtureMemory(ghostID, "Ghost", "uncommitted"))
	if err := os.WriteFile(filepath.Join(dir, ghostID+".md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := findSharedMemory(cfg, ghostID); ok {
		t.Fatal("resolver enumerated gens/ and served an uncommitted directory")
	}
	commit, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil || !ok || commit.Gen == ghost {
		t.Fatalf("uncommitted gen resolved: %+v ok=%v err=%v", commit, ok, err)
	}
}

func TestGitGenerationReadsOnlyPinnedObjects(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	a := fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "first")
	remote := realGitShareRemote(t, id.Recipient(), []Memory{a})
	var out strings.Builder
	if err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &out, realExec); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, out.String())
	}

	b := fixtureMemory("mem_20260601_000001_bbbbbbbb", "B", "pinned only")
	body, _ := renderMemory(b)
	ct, err := encryptShareBytes([]age.Recipient{id.Recipient()}, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "memories", b.ID+".md.age"), ct, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "add pinned B")

	pin, err := acquireGitPin(context.Background(), cfg, shareSubscription{Name: "neil", Remote: remote}, "run-pin-read", realExec)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.cleanup()

	// Simulate another actor replacing the shared worktree after this run pins.
	repo := shareRepoDir(cfg, "neil")
	_ = os.RemoveAll(filepath.Join(repo, "memories"))
	if err := os.WriteFile(filepath.Join(repo, "share.json"), []byte("not pinned input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	if err := pin.blobs(context.Background())(func(stem string, _ []byte) error {
		seen[stem] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Fatalf("pinned object reader followed mutable worktree: seen=%v", seen)
	}
	if _, err := pin.manifest(context.Background()); err != nil {
		t.Fatalf("manifest was read from mutable worktree instead of pin: %v", err)
	}
}

func TestConcurrentBucketPullsDoNotShareStagingDir(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	a := shareFetchDir(cfg, "neil", "run-a")
	b := shareFetchDir(cfg, "neil", "run-b")
	if a == b {
		t.Fatal("concurrent bucket pulls share one fixed staging directory")
	}
	for path, body := range map[string]string{a: "a", b: "b"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "owner"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gotA, _ := os.ReadFile(filepath.Join(a, "owner"))
	gotB, _ := os.ReadFile(filepath.Join(b, "owner"))
	if string(gotA) != "a" || string(gotB) != "b" {
		t.Fatalf("bucket staging mixed: A=%q B=%q", gotA, gotB)
	}
}

func TestReadServesTheBytesItHashed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	original := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Original", "hashed bytes")
	publishGen(t, cfg, "neil", id, []Memory{original})
	replacement := fixtureMemory(original.ID, "Replacement", "changed after hash")
	replacementBody, _ := renderMemory(replacement)

	origHook := testHookSharedReadAfterHash
	testHookSharedReadAfterHash = func(path string) {
		if err := os.WriteFile(path, replacementBody, 0o644); err != nil {
			t.Errorf("replace after hash: %v", err)
		}
	}
	t.Cleanup(func() { testHookSharedReadAfterHash = origHook })
	got, ok := findSharedMemory(cfg, original.ID)
	if !ok {
		t.Fatal("read failed after post-hash replacement")
	}
	if got.Text != original.Text || got.Title != original.Title {
		t.Fatalf("read re-opened the path after verification: got title=%q text=%q", got.Title, got.Text)
	}
}

func TestCorruptedPublishedCorpusIsSurfacedByDoctor(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "intact")
	publishGen(t, cfg, "neil", id, []Memory{m})
	commit, _, _ := resolvePublishedCommit(cfg, "neil")
	path := filepath.Join(shareGenCorpusDir(cfg, "neil", commit.Gen), m.ID+".md")
	if err := os.WriteFile(path, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := shareHealthOne(cfg, "neil", time.Now())
	if h.State != healthFailed || !strings.Contains(h.LastError, "corpus") {
		t.Fatalf("doctor health hid published corpus corruption: %+v", h)
	}
}

func TestReplayedOlderBucketEnvelopeRejectedAfterCommitCrash(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	if err := os.MkdirAll(shareCommitsDir(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := shareCommit{Seq: 1, Gen: "gen-v5", RunID: "old", BucketFloor: 5, BuiltAt: time.Now().UTC().Format(time.RFC3339)}
	body, _ := json.Marshal(rec)
	if err := os.WriteFile(shareCommitPath(cfg, name, 1), body, 0o644); err != nil {
		t.Fatal(err)
	}
	release := acquirePublishLeases(t, cfg, name, "replay")
	defer release()
	_, err := publishShareGeneration(cfg, shareCommitParams{name: name, runID: "replay", gen: "gen-v4", isBucket: true, fetched: 4, parentFloor: -1, builtAt: time.Now(), corpusDigest: "d", indexDigest: "d"})
	if !errors.Is(err, errRollback) {
		t.Fatalf("older envelope after registry crash = %v; want rollback refusal", err)
	}
}

func TestBucketFloorIsPublishedInCommitRecord(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	release := acquirePublishLeases(t, cfg, name, "v7")
	defer release()
	seq, err := publishShareGeneration(cfg, shareCommitParams{name: name, runID: "v7", gen: "gen-v7", isBucket: true, fetched: 7, parentFloor: -1, builtAt: time.Now(), corpusDigest: "d", indexDigest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if got := commitAtSeq(t, cfg, name, seq).BucketFloor; got != 7 {
		t.Fatalf("linked commit floor=%d; want 7", got)
	}
}

func TestReapedStorageOwnerCannotPublish(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	const (
		name = "neil"
		runA = "storage-owner-a"
		runB = "storage-owner-b"
	)
	storageReleaseA, err := acquireStorageLease(cfg, runA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	importReleaseA, err := acquireImportLease(cfg, name, runA, time.Now())
	if err != nil {
		storageReleaseA()
		t.Fatal(err)
	}
	defer importReleaseA()

	// A loses only the aggregate lease while retaining its live import lease.
	// A successor then owns storage.lock, so A's publish fence must reject it.
	storageReleaseA()
	storageReleaseB, err := acquireStorageLease(cfg, runB, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer storageReleaseB()
	_, err = publishShareGeneration(cfg, shareCommitParams{
		name: name, runID: runA, gen: "gen-a", parentFloor: -1,
		builtAt: time.Now(), corpusDigest: "d", indexDigest: "d",
	})
	if err == nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("reaped storage owner published with a live import lease: %v", err)
	}
	if commit, ok, rerr := resolvePublishedCommit(cfg, name); rerr != nil || ok {
		t.Fatalf("reaped storage owner linked a commit: %+v ok=%v err=%v", commit, ok, rerr)
	}
}

func TestBucketFloorSurvivesHealGCAndReplay(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	publishGen(t, cfg, "neil", id, []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")})
	first := commitAtSeq(t, cfg, "neil", 1)
	first.BucketFloor = 7
	body, _ := json.MarshalIndent(first, "", "  ")
	if err := os.WriteFile(shareCommitPath(cfg, "neil", 1), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < shareGenRetain+1; i++ {
		if err := healShareIndex(context.Background(), cfg, "neil"); err != nil {
			t.Fatalf("heal %d: %v", i, err)
		}
	}
	if err := shareGCSweep(cfg, "neil", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shareCommitPath(cfg, "neil", 1)); !os.IsNotExist(err) {
		t.Fatalf("old floor record was not reclaimed; test cannot prove heal inheritance: %v", err)
	}
	release := acquirePublishLeases(t, cfg, "neil", "replay-v6")
	defer release()
	_, err := publishShareGeneration(cfg, shareCommitParams{name: "neil", runID: "replay-v6", gen: "gen-v6", isBucket: true, fetched: 6, parentFloor: -1, builtAt: time.Now(), corpusDigest: "d", indexDigest: "d"})
	if !errors.Is(err, errRollback) {
		t.Fatalf("replay after heal+GC = %v; want rollback refusal", err)
	}
}

func TestShareGenerationBuilderNeverWritesPublishedIndex(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	publishGen(t, cfg, "neil", id, []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")})
	commit, _, _ := resolvePublishedCommit(cfg, "neil")
	published := shareGenIndexPath(cfg, "neil", commit.Gen)
	before, _ := fileDigestOf(published)

	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), []Memory{fixtureMemory("mem_20260601_000001_bbbbbbbb", "B", "two")}, true)
	orig := rebuildShareIndexFn
	var writerPath string
	rebuildShareIndexFn = func(_ context.Context, path string, _ []Memory) error {
		writerPath = path
		return errInjectedRebuild
	}
	t.Cleanup(func() { rebuildShareIndexFn = orig })
	if _, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: "neil", Remote: "r"}, shareRepoDir(cfg, "neil")); err == nil {
		t.Fatal("injected private generation build unexpectedly succeeded")
	}
	if writerPath == "" || writerPath == published {
		t.Fatalf("generation writer targeted published index: writer=%q published=%q", writerPath, published)
	}
	after, _ := fileDigestOf(published)
	if before != after {
		t.Fatal("failed generation build mutated the reader-held published index")
	}
}
