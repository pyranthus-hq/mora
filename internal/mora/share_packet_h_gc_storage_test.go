package mora

// Packet-H GC/storage mutation certificates. Each exact test name corresponds
// to one owned r7 matrix row; the fixtures stay sparse/small while exercising
// the production sweep, accountant, admission, global lease, and generation
// builder paths.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func packetHConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		VaultDir:  filepath.Join(root, "vault"),
		ConfigDir: filepath.Join(root, "config"),
		DataDir:   filepath.Join(root, "data"),
		StateDir:  filepath.Join(root, "state"),
	}
	for _, dir := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func packetHWriteCommit(t *testing.T, cfg Config, name string, seq, floor int, gen string, bodyBytes int64) {
	t.Helper()
	if err := os.MkdirAll(shareGenCorpusDir(cfg, name, gen), 0o700); err != nil {
		t.Fatal(err)
	}
	if bodyBytes > 0 {
		f, err := os.Create(filepath.Join(shareGenCorpusDir(cfg, name, gen), "body.md"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(bodyBytes); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	rec := shareCommit{
		Seq: seq, Gen: gen, RunID: genRunID(gen), BucketFloor: floor,
		BuiltAt: "2026-07-01T00:00:00Z", CorpusDigest: "test", IndexDigest: "test",
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shareCommitsDir(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shareCommitPath(cfg, name, seq), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func packetHSetLimitHeadroom(t *testing.T, cfg Config, headroom int64) int64 {
	t.Helper()
	limit := int64(1 << 40)
	for i := 0; i < 4; i++ {
		body, err := json.Marshal(shareStorageLimit{Bytes: limit, UpdatedAt: "2026-07-16T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		if err := atomicio.WriteDurable(shareStorageLimitPath(cfg), append(body, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		used, err := productStorageBytes(cfg)
		if err != nil {
			t.Fatal(err)
		}
		next := used + headroom
		if next == limit {
			return limit
		}
		limit = next
	}
	body, _ := json.Marshal(shareStorageLimit{Bytes: limit, UpdatedAt: "2026-07-16T00:00:00Z"})
	if err := atomicio.WriteDurable(shareStorageLimitPath(cfg), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return limit
}

func packetHOld(path string) error {
	old := time.Now().Add(-2 * shareImportTTL)
	return os.Chtimes(path, old, old)
}

// row 44a
func TestGCReclaimsCommittedLosers(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	for seq := 1; seq <= 6; seq++ {
		packetHWriteCommit(t, cfg, name, seq, 1, fmt.Sprintf("gen-r%d", seq), 1)
	}
	if err := shareGCSweep(cfg, name, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{1, 2} {
		if _, err := os.Stat(shareGenDir(cfg, name, fmt.Sprintf("gen-r%d", seq))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("committed loser seq %d survived GC: %v", seq, err)
		}
	}
	for _, seq := range []int{3, 4, 5, 6} {
		if _, err := os.Stat(shareGenDir(cfg, name, fmt.Sprintf("gen-r%d", seq))); err != nil {
			t.Fatalf("retained/published seq %d was removed: %v", seq, err)
		}
	}
}

// row 44b
func TestGCReclaimsCrashOrphans(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	orphan := shareGenDir(cfg, name, "gen-crashed")
	if err := os.MkdirAll(filepath.Join(orphan, "corpus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := packetHOld(orphan); err != nil {
		t.Fatal(err)
	}
	if err := shareGCSweep(cfg, name, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale uncommitted generation survived: %v", err)
	}
}

// row 44c
func TestGCReclaimsStaleBucketFetchDirs(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	fetch := shareFetchDir(cfg, name, "crashed")
	if err := os.MkdirAll(fetch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fetch, "part"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := packetHOld(fetch); err != nil {
		t.Fatal(err)
	}
	if err := shareGCSweep(cfg, name, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fetch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale fetch staging survived: %v", err)
	}
}

// row 44d
func TestGCReclaimsOrphanedGitImportRefs(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	repo := shareRepoDir(cfg, name)
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "packet-h@test")
	mustGit(t, repo, "config", "user.name", "Packet H")
	if err := os.WriteFile(filepath.Join(repo, "tracked"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "tracked")
	mustGit(t, repo, "commit", "-q", "-m", "seed")
	sha := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
	for _, runID := range []string{"dead", "live", "retained"} {
		mustGit(t, repo, "update-ref", "refs/mora/import/"+runID, sha)
	}
	packetHWriteCommit(t, cfg, name, 1, 1, "gen-retained", 1)
	release, err := acquireImportLease(cfg, name, "live", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	origExec := shareGCGitExecFn
	var updates [][]string
	shareGCGitExecFn = func(ctx context.Context, dir, command string, args ...string) (string, error) {
		if command == "git" && len(args) > 0 && args[0] == "update-ref" {
			updates = append(updates, append([]string(nil), args...))
		}
		return origExec(ctx, dir, command, args...)
	}
	t.Cleanup(func() { shareGCGitExecFn = origExec })
	if err := shareGCSweep(cfg, name, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || strings.Join(updates[0], " ") != "update-ref -d refs/mora/import/dead "+sha {
		t.Fatalf("GC git mutations = %v; want one exact observed-sha CAS delete", updates)
	}
	if _, err := realExec(context.Background(), repo, "git", "rev-parse", "--verify", "refs/mora/import/dead"); err == nil {
		t.Fatal("orphan import ref survived")
	}
	for _, ref := range []string{"refs/mora/import/live", "refs/mora/import/retained", "HEAD"} {
		if _, err := realExec(context.Background(), repo, "git", "rev-parse", "--verify", ref); err != nil {
			t.Fatalf("GC touched live/retained/control ref %s: %v", ref, err)
		}
	}
}

// row 44e
func TestGCReclaimsStaleCommitRecordsWithoutLoweringBucketFloor(t *testing.T) {
	t.Run("reclaims records when the head carries the floor", func(t *testing.T) {
		cfg := packetHConfig(t)
		const name = "acme"
		for seq := 1; seq <= 6; seq++ {
			packetHWriteCommit(t, cfg, name, seq, seq, fmt.Sprintf("gen-r%d", seq), 1)
		}
		if err := shareGCSweep(cfg, name, time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, seq := range []int{1, 2} {
			if _, err := os.Stat(shareCommitPath(cfg, name, seq)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale commit %d survived: %v", seq, err)
			}
		}
		head, ok, err := resolvePublishedCommit(cfg, name)
		if err != nil || !ok || head.Seq != 6 || head.BucketFloor != 6 {
			t.Fatalf("published floor changed after GC: head=%+v ok=%v err=%v", head, ok, err)
		}
	})

	t.Run("refuses a legacy record whose floor exceeds the head", func(t *testing.T) {
		cfg := packetHConfig(t)
		const name = "legacy"
		for seq := 1; seq <= 5; seq++ {
			floor := 3
			if seq == 1 {
				floor = 9
			}
			packetHWriteCommit(t, cfg, name, seq, floor, fmt.Sprintf("gen-r%d", seq), 1)
		}
		if err := shareGCSweep(cfg, name, time.Now()); err == nil {
			t.Fatal("GC deleted the only durable high floor instead of failing closed")
		}
		if _, err := os.Stat(shareCommitPath(cfg, name, 1)); err != nil {
			t.Fatalf("high-floor record was removed: %v", err)
		}
	})
}

// row 44f
func TestGCDefersWindowsOpenFileDeletion(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	for seq := 1; seq <= 5; seq++ {
		packetHWriteCommit(t, cfg, name, seq, 1, fmt.Sprintf("gen-r%d", seq), 1)
	}
	target := shareGenDir(cfg, name, "gen-r1")
	openErr := errors.New("injected Windows sharing violation")
	origRemoveAll, origRetryable := shareGCRemoveAllFn, shareGCRemovalRetryableFn
	calls := 0
	shareGCRemoveAllFn = func(path string) error {
		if path == target {
			calls++
			return openErr
		}
		return origRemoveAll(path)
	}
	shareGCRemovalRetryableFn = func(err error) bool { return err == openErr }
	t.Cleanup(func() {
		shareGCRemoveAllFn = origRemoveAll
		shareGCRemovalRetryableFn = origRetryable
	})
	if err := shareGCSweep(cfg, name, time.Now()); err != nil {
		t.Fatalf("open-file deletion aborted sweep: %v", err)
	}
	if calls != 1 {
		t.Fatalf("sharing violation retried %d times; want one deferred attempt", calls)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("open generation was forced away: %v", err)
	}
}

// row 52a
func TestGCPreflightUnblocksAfterReadersClose(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	for seq := 1; seq <= 5; seq++ {
		sz := int64(1)
		if seq == 1 {
			sz = 1 << 20
		}
		packetHWriteCommit(t, cfg, name, seq, 1, fmt.Sprintf("gen-r%d", seq), sz)
	}
	target := shareGenDir(cfg, name, "gen-r1")
	if err := packetHOld(target); err != nil {
		t.Fatal(err)
	}
	packetHSetLimitHeadroom(t, cfg, -(1 << 19)) // current footprint is over until target is reclaimed

	openErr := errors.New("reader still holds index")
	origRemoveAll, origRetryable := shareGCRemoveAllFn, shareGCRemovalRetryableFn
	shareGCRemoveAllFn = func(path string) error {
		if path == target {
			return openErr
		}
		return origRemoveAll(path)
	}
	shareGCRemovalRetryableFn = func(err error) bool { return err == openErr }
	t.Cleanup(func() {
		shareGCRemoveAllFn = origRemoveAll
		shareGCRemovalRetryableFn = origRetryable
	})
	check := func(string) (int, error) {
		a, err := newShareStorageAdmission(cfg, name)
		if err != nil {
			return 0, err
		}
		return 0, a.checkCurrent()
	}
	if err := shareBuildAndPublish(context.Background(), cfg, name, buildModeHeal, check); err == nil {
		t.Fatal("build admitted while reader-pinned bytes still exceeded the limit")
	}
	shareGCRemoveAllFn, shareGCRemovalRetryableFn = origRemoveAll, origRetryable
	if err := shareBuildAndPublish(context.Background(), cfg, name, buildModeHeal, check); err != nil {
		t.Fatalf("next build stayed wedged after reader close/preflight reclaim: %v", err)
	}
}

// row 52b
func TestManualShareGCDoesNotRequireSuccessfulPull(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	registerSub(t, cfg, "acme")
	orphan := shareGenDir(cfg, "acme", "gen-crashed")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := packetHOld(orphan); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdShare(context.Background(), []string{"gc", "acme"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("manual dispatcher required a successful pull: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manual GC did not reclaim crash orphan: %v", err)
	}
	if !strings.Contains(out.String(), "whole-product footprint") || !strings.Contains(out.String(), "reclaimed") {
		t.Fatalf("manual GC did not report before/after footprint: %q", out.String())
	}
}

// row 53a
func TestShareStorageLimitIsWholeProduct(t *testing.T) {
	cfg := packetHConfig(t)
	base, err := productStorageBytes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		p := filepath.Join(shareSubRoot(cfg, name), "repo", "pack")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, 70), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := &shareStorageAdmission{cfg: cfg, name: "gamma", limit: base + 120}
	if err := a.checkCurrent(); err == nil {
		t.Fatal("two subscriptions were admitted as independent footprints")
	}
	if err := os.RemoveAll(shareSubRoot(cfg, "beta")); err != nil {
		t.Fatal(err)
	}
	if err := a.checkCurrent(); err != nil {
		t.Fatalf("one 70-byte subscription should fit the 120-byte aggregate headroom: %v", err)
	}
}

// row 53b
func TestShareStorageLimitIncludesAllProductRoots(t *testing.T) {
	cfg := packetHConfig(t)
	sizes := []int{11, 12, 13, 14}
	for i, root := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir} {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("root-%d", i)), bytes.Repeat([]byte{'x'}, sizes[i]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := productStorageBytes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != 50 {
		t.Fatalf("whole-product roots counted %d bytes; want 50", got)
	}

	t.Run("canonical overlap and hard links are deduplicated", func(t *testing.T) {
		root := t.TempDir()
		overlap := Config{
			DataDir:   root,
			VaultDir:  filepath.Join(root, "vault"),
			ConfigDir: filepath.Join(root, "config"),
			StateDir:  filepath.Join(root, "state"),
		}
		for _, dir := range []string{overlap.VaultDir, overlap.ConfigDir, overlap.StateDir} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		src := filepath.Join(overlap.VaultDir, "body")
		if err := os.WriteFile(src, bytes.Repeat([]byte{'h'}, 31), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(src, filepath.Join(overlap.StateDir, "same-body")); err != nil {
			t.Skipf("hard links unavailable on test filesystem: %v", err)
		}
		got, err := productStorageBytes(overlap)
		if err != nil {
			t.Fatal(err)
		}
		if got != 31 {
			t.Fatalf("nested roots/hard link charged %d bytes; want one 31-byte identity", got)
		}
	})

	t.Run("case-distinct roots are never collapsed by GOOS", func(t *testing.T) {
		origGOOS := runtimeGOOS
		runtimeGOOS = func() string { return "darwin" }
		t.Cleanup(func() { runtimeGOOS = origGOOS })
		if storagePathWithin(filepath.Join("tmp", "A"), filepath.Join("tmp", "a")) ||
			storagePathWithin(filepath.Join("tmp", "a"), filepath.Join("tmp", "A")) {
			t.Fatal("case-distinct roots collapsed solely because GOOS=darwin")
		}
	})
}

// row 53c
func TestShareStorageLimitIncludesRepo(t *testing.T) {
	cfg := packetHConfig(t)
	base, _ := productStorageBytes(cfg)
	p := filepath.Join(shareRepoDir(cfg, "acme"), "objects", "pack", "pack-test")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{'p'}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &shareStorageAdmission{cfg: cfg, name: "acme", limit: base + 4095}
	if err := a.checkCurrent(); err == nil {
		t.Fatal("repo pack bytes were omitted from admission")
	}
}

// row 53d
func TestShareStorageLimitIncludesFetchStaging(t *testing.T) {
	cfg := packetHConfig(t)
	base, _ := productStorageBytes(cfg)
	p := filepath.Join(shareFetchDir(cfg, "acme", "run"), "object")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{'f'}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &shareStorageAdmission{cfg: cfg, name: "acme", limit: base + 4095}
	if err := a.checkCurrent(); err == nil {
		t.Fatal("fetch staging bytes were omitted from admission")
	}
}

// row 53e
func TestShareStorageLimitReservesInflightBuild(t *testing.T) {
	t.Run("corpus write is rejected before IO", func(t *testing.T) {
		cfg := packetHConfig(t)
		packetHSetLimitHeadroom(t, cfg, 512)
		entry := shareBlobEntry{
			mem:  fixtureMemory("mem_20260716_000000_aaaaaaaa", "Bounded", "body"),
			body: bytes.Repeat([]byte{'m'}, 1024),
		}
		_, _, err := buildShareGenerationFromEntries(context.Background(), cfg, "acme", "gen-corpus-cap", []shareBlobEntry{entry})
		if err == nil || !strings.Contains(err.Error(), "storage-limit") {
			t.Fatalf("over-budget corpus write = %v; want admission refusal", err)
		}
		dst := filepath.Join(shareGenCorpusDir(cfg, "acme", "gen-corpus-cap"), entry.mem.ID+".md")
		if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("corpus bytes landed before admission: %v", statErr)
		}
	})

	t.Run("SQLite max_page_count enforces the byte cap", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.db")
		ctx := withShareIndexBudget(context.Background(), 4096)
		mem := fixtureMemory("mem_20260716_000001_bbbbbbbb", "SQLite cap", "body")
		if err := buildShareGenIndex(ctx, path, []Memory{mem}); err == nil {
			t.Fatal("one-page SQLite cap allowed the multi-table share index to grow")
		}
		if info, err := os.Stat(path); err == nil && info.Size() > 4096 {
			t.Fatalf("SQLite main database grew to %d bytes past its 4096-byte page cap", info.Size())
		}
	})

	t.Run("closed index is re-accounted before publication", func(t *testing.T) {
		testShareStoragePostBuildReaccount(t)
	})
}

func testShareStoragePostBuildReaccount(t *testing.T) {
	t.Helper()
	cfg := packetHConfig(t)
	packetHSetLimitHeadroom(t, cfg, 64<<10)
	body := bytes.Repeat([]byte{'m'}, 1024)
	entry := shareBlobEntry{mem: fixtureMemory("mem_20260716_000000_aaaaaaaa", "Bounded", "body"), body: body}
	origRebuild := rebuildShareIndexFn
	var budget int64
	rebuildShareIndexFn = func(ctx context.Context, path string, _ []Memory) error {
		var ok bool
		budget, ok = shareIndexBudget(ctx)
		if !ok || budget <= 0 || budget > 64<<10 {
			return fmt.Errorf("missing/invalid SQLite byte cap: %d, ok=%v", budget, ok)
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if err := f.Truncate(budget + 1); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	t.Cleanup(func() { rebuildShareIndexFn = origRebuild })
	_, _, err := buildShareGenerationFromEntries(context.Background(), cfg, "acme", "gen-bounded", []shareBlobEntry{entry})
	if err == nil || !strings.Contains(err.Error(), "storage-limit") {
		t.Fatalf("post-build overshoot = %v; want fail-closed decision", err)
	}
	if budget == 0 {
		t.Fatal("in-flight index build received no remaining-byte cap")
	}
}

// row 53f
func TestConcurrentSubscriptionsShareOneStorageReservation(t *testing.T) {
	cfg := packetHConfig(t)
	origTTL := shareImportTTL
	shareImportTTL = 60 * time.Millisecond
	t.Cleanup(func() { shareImportTTL = origTTL })
	packetHSetLimitHeadroom(t, cfg, 8<<10)
	const reservation = int64(5 << 10)
	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	enteredB := make(chan struct{}, 1)
	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	var once sync.Once
	go func() {
		err := shareBuildAndPublish(context.Background(), cfg, "alpha", buildModeHeal, func(string) (int, error) {
			a, err := newShareStorageAdmission(cfg, "alpha")
			if err != nil {
				return 0, err
			}
			if err := a.checkAdditional(reservation); err != nil {
				return 0, err
			}
			once.Do(func() { close(enteredA) })
			<-releaseA
			p := filepath.Join(shareSubRoot(cfg, "alpha"), "reserved.bin")
			return 0, os.WriteFile(p, bytes.Repeat([]byte{'a'}, int(reservation)), 0o600)
		})
		results <- result{name: "alpha", err: err}
	}()
	<-enteredA
	// Start B only after A's original storage lease timestamp is past TTL. The
	// storage heartbeat must keep that lease live for the whole build; without
	// it, B reaps A and double-spends the aggregate reservation.
	time.Sleep(3 * shareImportTTL)
	go func() {
		err := shareBuildAndPublish(context.Background(), cfg, "beta", buildModeHeal, func(string) (int, error) {
			enteredB <- struct{}{}
			a, err := newShareStorageAdmission(cfg, "beta")
			if err != nil {
				return 0, err
			}
			return 0, a.checkAdditional(reservation)
		})
		results <- result{name: "beta", err: err}
	}()
	enteredEarly := false
	select {
	case <-enteredB:
		enteredEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseA)
	r1, r2 := <-results, <-results
	if enteredEarly {
		t.Fatal("second subscription entered admission while the first held storage.lock")
	}
	byName := map[string]error{r1.name: r1.err, r2.name: r2.err}
	if byName["alpha"] != nil {
		t.Fatalf("first reservation failed: %v", byName["alpha"])
	}
	if byName["beta"] == nil || !strings.Contains(byName["beta"].Error(), "storage-limit") {
		t.Fatalf("second reservation double-spent aggregate space: %v", byName["beta"])
	}
}

func TestManualGCKeepsStorageLeaseAlivePastTTL(t *testing.T) {
	cfg := packetHConfig(t)
	origTTL := shareImportTTL
	shareImportTTL = 60 * time.Millisecond
	t.Cleanup(func() { shareImportTTL = origTTL })
	entered := make(chan struct{})
	release := make(chan struct{})
	testHookShareGCAfterStorageLease = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { testHookShareGCAfterStorageLease = nil })
	gcDone := make(chan error, 1)
	go func() {
		gcDone <- cmdShareGC(cfg, nil, io.Discard, time.Now())
	}()
	<-entered
	time.Sleep(3 * shareImportTTL)

	contenderEntered := make(chan struct{}, 1)
	contenderDone := make(chan error, 1)
	go func() {
		rel, err := acquireStorageLease(cfg, "gc-contender", time.Now())
		if err == nil {
			contenderEntered <- struct{}{}
			rel()
		}
		contenderDone <- err
	}()
	select {
	case <-contenderEntered:
		close(release)
		t.Fatal("manual GC's live storage lease was reaped after TTL")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-gcDone; err != nil {
		t.Fatal(err)
	}
	if err := <-contenderDone; err != nil {
		t.Fatalf("contender did not acquire after GC released: %v", err)
	}
}

func TestManualGCReclaimsUnregisteredFirstSubscribeCrash(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "orphan"
	var fetchDir, genDir string
	injected := errors.New("first subscribe crashed after local writes")
	err := shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(runID string) (int, error) {
		fetchDir = shareFetchDir(cfg, name, runID)
		genDir = shareGenDir(cfg, name, "gen-"+runID)
		for _, dir := range []string{fetchDir, genDir, filepath.Join(shareRepoDir(cfg, name), ".git", "objects", "pack")} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return 0, err
			}
		}
		if err := os.WriteFile(filepath.Join(fetchDir, "blob"), []byte("fetch debris"), 0o600); err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(genDir, "index.db"), []byte("generation debris"), 0o600); err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(shareRepoDir(cfg, name), ".git", "objects", "pack", "orphan.pack"), []byte("repo pack debris"), 0o600); err != nil {
			return 0, err
		}
		return 0, injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("failed first subscribe = %v; want injected crash", err)
	}
	if sf, err := loadShares(cfg); err != nil || len(sf.Subscriptions) != 0 {
		t.Fatalf("crashed first subscribe unexpectedly registered: %+v %v", sf, err)
	}
	var out bytes.Buffer
	if err := cmdShareGC(cfg, []string{name}, &out, time.Now()); err != nil {
		t.Fatalf("manual GC could not reclaim unregistered state: %v", err)
	}
	if _, err := os.Stat(shareSubRoot(cfg, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered repo/fetch/generation root survived GC: %v", err)
	}
}

// row 54a
func TestLegalLargeShareIsNotHardCappedAt4GiB(t *testing.T) {
	cfg := packetHConfig(t)
	legalCorpusBytes := int64(shareMaxShareEntries) * int64(shareMaxMemoryBytes)
	if legalCorpusBytes <= 4<<30 {
		t.Fatalf("fixture is not larger than the retired 4 GiB cap: %d", legalCorpusBytes)
	}
	// The opt-in remains proportional to the protocol-legal corpus, not a fixed
	// product cap. Nine corpus-widths exceed the conservative 8x index reserve.
	limit := legalCorpusBytes * 9
	body, _ := json.Marshal(shareStorageLimit{Bytes: limit, UpdatedAt: "2026-07-16T00:00:00Z"})
	if err := atomicio.WriteDurable(shareStorageLimitPath(cfg), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := admitShareGenerationBytes(cfg, "legal", legalCorpusBytes, shareMaxShareEntries); err != nil {
		t.Fatalf("protocol-legal 50k x 4 MiB share was hard-capped: %v", err)
	}
}

// row 54c
func TestDiskFullLeavesGenerationUncommitted(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	entry := shareBlobEntry{
		mem:  fixtureMemory("mem_20260716_000000_bbbbbbbb", "Disk full", "body"),
		body: []byte("body"),
	}
	origRebuild := rebuildShareIndexFn
	rebuildShareIndexFn = func(context.Context, string, []Memory) error { return syscall.ENOSPC }
	t.Cleanup(func() { rebuildShareIndexFn = origRebuild })
	var abandoned string
	err := shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(runID string) (int, error) {
		abandoned = shareGenDir(cfg, name, "gen-"+runID)
		seq, _, err := buildAndCommitGeneration(context.Background(), cfg, shareSubscription{Name: name}, runID, "fixture", []shareBlobEntry{entry}, shareCommitParams{parentFloor: -1})
		return seq, err
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("disk-full build = %v; want ENOSPC", err)
	}
	if _, ok, err := resolvePublishedCommit(cfg, name); err != nil || ok {
		t.Fatalf("disk-full generation became committed: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(abandoned); err != nil {
		t.Fatalf("failed generation was not left GC-reapable: %v", err)
	}
	if err := packetHOld(abandoned); err != nil {
		t.Fatal(err)
	}
	if err := shareGCSweep(cfg, name, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted ENOSPC generation was not reaped: %v", err)
	}
}
