package mora

// Packet-H r7 witnesses for legacy migration (45a-d) and the durable attempt
// lifecycle (51a-d). Each named test observes one production call site so the
// corresponding matrix mutation makes that test red.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func packetHSeedLegacyFlat(t *testing.T, cfg Config, name string) (stale, torn Memory) {
	t.Helper()
	registerSub(t, cfg, name)
	if err := os.MkdirAll(shareCorpusDir(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	stale = fixtureMemory("mem_20260701_000000_aaaaaaaa", "Revoked legacy row", "revoked legacy secret")
	torn = fixtureMemory("mem_20260701_000001_bbbbbbbb", "Torn legacy corpus", "torn composite secret")
	body, err := renderMemory(torn)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareCorpusDir(cfg, name), torn.ID+".md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// The stale index intentionally contains a different memory than the torn
	// flat corpus. Neither artifact has provenance tying it to a completed pull.
	stale.Path = filepath.Join(shareCorpusDir(cfg, name), stale.ID+".md")
	if err := buildShareGenIndex(context.Background(), shareIndexPath(cfg, name), []Memory{stale}); err != nil {
		t.Fatal(err)
	}
	return stale, torn
}

func packetHFixtureDir(t *testing.T, cfg Config, mems []Memory) string {
	t.Helper()
	id := writeTestIdentity(t, cfg)
	dir := t.TempDir()
	buildShareRepoFixture(t, dir, id.Recipient(), mems, true)
	return dir
}

func packetHAssertNoOwnerEvidence(t *testing.T, res ThinkResult, owner string) {
	t.Helper()
	for _, ev := range res.Evidence {
		if ev.Owner == owner {
			t.Fatalf("think served legacy evidence for %q: %+v", owner, ev)
		}
	}
}

// row 45a: neither a stale flat index nor a torn flat corpus is served or
// re-cut. Only a fresh, pinned pull can mint the first generation.
func TestLegacyFlatShareIsFailClosedUntilPull(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	name := "neil"
	stale, torn := packetHSeedLegacyFlat(t, cfg, name)

	h := shareHealthOne(cfg, name, time.Now())
	if h.State != healthFailed || !strings.Contains(h.LastError, "legacy share not yet migrated") {
		t.Fatalf("legacy health = %+v; want failed with pull guidance", h)
	}
	if _, ok := findSharedMemory(cfg, torn.ID); ok {
		t.Fatal("read served the torn legacy corpus")
	}
	if _, ok := findSharedMemory(cfg, stale.ID); ok {
		t.Fatal("read served the stale legacy index row")
	}
	shared, err := searchSharedCorpora(context.Background(), cfg, "legacy secret", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 0 {
		t.Fatalf("search served untrusted legacy state: %+v", shared)
	}
	thought, err := buildThink(context.Background(), cfg, "legacy secret", "", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	packetHAssertNoOwnerEvidence(t, thought, name)

	clean := fixtureMemory("mem_20260701_000002_cccccccc", "Pinned clean row", "clean pinned snapshot")
	id := writeTestIdentity(t, cfg)
	remote := realGitShareRemote(t, id.Recipient(), []Memory{clean})
	if _, err := realExec(context.Background(), "", "git", "clone", remote, shareRepoDir(cfg, name)); err != nil {
		t.Fatalf("clone trusted source: %v", err)
	}
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sf.Subscriptions {
		if sf.Subscriptions[i].Name == name {
			sf.Subscriptions[i].Remote = remote
		}
	}
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := sharePull(context.Background(), cfg, []string{name}, &out, realExec); err != nil {
		t.Fatalf("first pinned pull: %v\n%s", err, out.String())
	}
	if _, ok := findSharedMemory(cfg, clean.ID); !ok {
		t.Fatal("first pinned pull did not publish the clean generation")
	}
	if _, ok := findSharedMemory(cfg, stale.ID); ok {
		t.Fatal("first pull blessed the stale legacy index")
	}
	if _, ok := findSharedMemory(cfg, torn.ID); ok {
		t.Fatal("first pull blessed the torn legacy corpus")
	}
}

// row 45b: a latch failure aborts migration before legacy retirement; a retry
// retires legacy only after the one-way latch exists.
func TestMigratedLatchExistsBeforeLegacyRetirement(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	name := "neil"
	packetHSeedLegacyFlat(t, cfg, name)
	dir := packetHFixtureDir(t, cfg, []Memory{fixtureMemory("mem_20260702_000000_cccccccc", "Clean", "trusted")})

	origSync := atomicio.SyncDirFn
	atomicio.SyncDirFn = func(dir string) error {
		if filepath.Clean(dir) == filepath.Clean(shareSubRoot(cfg, name)) {
			return errors.New("injected latch directory-sync failure")
		}
		return origSync(dir)
	}
	_, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: name, Remote: "r"}, dir)
	atomicio.SyncDirFn = origSync
	if err == nil || !strings.Contains(err.Error(), "migrated latch") {
		t.Fatalf("latch directory-sync failure = %v; want surfaced migration error", err)
	}
	if !hasLegacyFlat(cfg, name) {
		t.Fatal("legacy state retired before the migrated latch became durable")
	}
	if _, err := os.Stat(shareMigratedLatchPath(cfg, name)); err != nil {
		t.Fatalf("renamed latch missing after injected post-rename barrier failure: %v", err)
	}

	if _, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: name, Remote: "r"}, dir); err != nil {
		t.Fatalf("migration retry: %v", err)
	}
	if _, err := os.Stat(shareMigratedLatchPath(cfg, name)); err != nil {
		t.Fatalf("durable migrated latch missing: %v", err)
	}
	if hasLegacyFlat(cfg, name) {
		t.Fatal("legacy state survived after the migrated latch became durable")
	}
}

// row 45c: the latch uses atomicWriteDurable, including both the temp-file sync
// and the parent-directory sync, in that order.
func TestMigratedLatchWriteIsCrashDurable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	name := "neil"
	packetHSeedLegacyFlat(t, cfg, name)
	dir := packetHFixtureDir(t, cfg, []Memory{fixtureMemory("mem_20260703_000000_cccccccc", "Clean", "trusted")})

	var trace []string
	origFileSync, origDirSync := atomicio.MarkerSyncFn, atomicio.SyncDirFn
	atomicio.MarkerSyncFn = func(f *os.File) error {
		if strings.HasPrefix(filepath.Base(f.Name()), ".migrated-") {
			trace = append(trace, "latch-file-sync")
		}
		return origFileSync(f)
	}
	atomicio.SyncDirFn = func(dir string) error {
		if filepath.Clean(dir) == filepath.Clean(shareSubRoot(cfg, name)) {
			trace = append(trace, "latch-dir-sync")
		}
		return origDirSync(dir)
	}
	t.Cleanup(func() { atomicio.MarkerSyncFn, atomicio.SyncDirFn = origFileSync, origDirSync })

	if _, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: name, Remote: "r"}, dir); err != nil {
		t.Fatal(err)
	}
	fileAt, dirAt := -1, -1
	for i, event := range trace {
		switch event {
		case "latch-file-sync":
			if fileAt == -1 {
				fileAt = i
			}
		case "latch-dir-sync":
			if dirAt == -1 {
				dirAt = i
			}
		}
	}
	if fileAt < 0 || dirAt < 0 || fileAt >= dirAt {
		t.Fatalf("migrated latch durability trace = %v; want file sync before dir sync", trace)
	}
}

// row 45d: once the latch exists, losing commits can never resurrect either
// legacy artifact.
func TestEmptyCommitsWithLatchFailsClosed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	name := "neil"
	stale, torn := packetHSeedLegacyFlat(t, cfg, name)
	if err := atomicio.WriteDurable(shareMigratedLatchPath(cfg, name), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(shareCommitsDir(cfg, name))

	h := shareHealthOne(cfg, name, time.Now())
	if h.State != healthFailed || !strings.Contains(h.LastError, "committed generation lost") {
		t.Fatalf("empty commits with latch health = %+v", h)
	}
	for _, id := range []string{stale.ID, torn.ID} {
		if _, ok := findSharedMemory(cfg, id); ok {
			t.Fatalf("read resurrected legacy id %s", id)
		}
	}
	shared, err := searchSharedCorpora(context.Background(), cfg, "legacy secret", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 0 {
		t.Fatalf("search resurrected legacy state: %+v", shared)
	}
	thought, err := buildThink(context.Background(), cfg, "legacy secret", "", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	packetHAssertNoOwnerEvidence(t, thought, name)
}

// row 51a: every transport's first write must observe the durable active record.
// The initial-clone subtest drives the real subscribe dispatcher because clone
// used to sit outside shareBuildAndPublish while helper-only tests stayed green.
func TestShareAttemptStartPrecedesFetch(t *testing.T) {
	t.Run("chokepoint callback", func(t *testing.T) {
		withTempHome(t)
		cfg := mustConfig(t)
		name := "neil"
		sawActive := false
		fetchErr := errors.New("injected fetch failure")
		err := shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(runID string) (int, error) {
			a, ok, err := loadShareAttempt(cfg, name)
			sawActive = err == nil && ok && a.RunID == runID && a.State == "active"
			return 0, fetchErr
		})
		if !sawActive {
			t.Fatal("fetch began before the active attempt record was published")
		}
		if !errors.Is(err, fetchErr) {
			t.Fatalf("fetch failure = %v; want original error", err)
		}
	})

	t.Run("initial git clone", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		writeTestIdentity(t, cfg)
		name := "neil"
		cloneErr := errors.New("injected clone stop")
		sawActive := false
		exec := func(ctx context.Context, dir, command string, args ...string) (string, error) {
			if command == "git" && len(args) > 0 && args[0] == "clone" {
				a, ok, err := loadShareAttempt(cfg, name)
				sawActive = err == nil && ok && a.State == "active" && a.RunID != ""
				return "", cloneErr
			}
			return realExec(ctx, dir, command, args...)
		}
		var out bytes.Buffer
		err := shareSubscribe(context.Background(), cfg,
			[]string{name, "--remote", "https://example.invalid/share.git"}, &out, exec)
		if !sawActive {
			t.Fatal("initial git clone began before the durable active attempt record")
		}
		if !errors.Is(err, cloneErr) {
			t.Fatalf("subscribe clone failure = %v; want injected error", err)
		}
	})

	t.Run("initial bucket probe", func(t *testing.T) {
		f := newBucketFixture(t)
		if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
			t.Fatal(err)
		}
		sawActive := false
		observed := &getObservingStore{
			objectStore: f.store,
			beforeFirstGet: func() {
				a, ok, err := loadShareAttempt(f.cfg, "acme")
				sawActive = err == nil && ok && a.State == "active" && a.RunID != ""
			},
		}
		confirm := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
		var out bytes.Buffer
		if err := shareSubscribeBucketWithStore(f.ctx, f.cfg, "acme", f.bc, confirm, &out, observed); err != nil {
			t.Fatalf("bucket subscribe: %v\n%s", err, out.String())
		}
		if !sawActive {
			t.Fatal("initial bucket probe wrote before the durable active attempt record")
		}
	})
}

// row 51b: attempt start's file data reaches its sync barrier before fetch.
func TestShareAttemptStartFsyncsBeforeRename(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	var trace []string
	orig := shareAttemptStartFileSyncFn
	shareAttemptStartFileSyncFn = func(f *os.File) error {
		trace = append(trace, "file-sync")
		return orig(f)
	}
	t.Cleanup(func() { shareAttemptStartFileSyncFn = orig })
	fetchErr := errors.New("stop after fetch boundary")
	_ = shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(string) (int, error) {
		trace = append(trace, "fetch")
		return 0, fetchErr
	})
	if len(trace) < 2 || trace[0] != "file-sync" || trace[len(trace)-1] != "fetch" {
		t.Fatalf("attempt start trace = %v; want file-sync before fetch", trace)
	}
}

// row 51c: the active record's parent directory is synced before fetch.
func TestShareAttemptStartSyncsDirBeforeFetch(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	var trace []string
	orig := shareAttemptStartDirSyncFn
	shareAttemptStartDirSyncFn = func(dir string) error {
		trace = append(trace, "dir-sync")
		return orig(dir)
	}
	t.Cleanup(func() { shareAttemptStartDirSyncFn = orig })
	fetchErr := errors.New("stop after fetch boundary")
	_ = shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(string) (int, error) {
		trace = append(trace, "fetch")
		return 0, fetchErr
	})
	if len(trace) < 2 || trace[0] != "dir-sync" || trace[len(trace)-1] != "fetch" {
		t.Fatalf("attempt start trace = %v; want dir-sync before fetch", trace)
	}
}

// row 51d: while the winning commit link's directory barrier is running, the
// attempt is still active. Only after that barrier returns may it become
// succeeded with the matching sequence.
func TestShareAttemptSuccessFollowsDurableCommit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	name := "neil"
	registerSub(t, cfg, name)
	dir := packetHFixtureDir(t, cfg, []Memory{fixtureMemory("mem_20260704_000000_aaaaaaaa", "Committed", "durable")})

	sawBarrier := false
	orig := shareCommitSyncDirFn
	shareCommitSyncDirFn = func(commitsDir string) error {
		sawBarrier = true
		a, ok, err := loadShareAttempt(cfg, name)
		if err != nil || !ok || a.State != "active" {
			t.Fatalf("attempt at commit durability barrier = %+v, ok=%v, err=%v; want active", a, ok, err)
		}
		return orig(commitsDir)
	}
	t.Cleanup(func() { shareCommitSyncDirFn = orig })

	if _, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: name, Remote: "r"}, dir); err != nil {
		t.Fatal(err)
	}
	if !sawBarrier {
		t.Fatal("winning commit skipped its commits-directory durability barrier")
	}
	commit, ok, err := resolvePublishedCommit(cfg, name)
	if err != nil || !ok {
		t.Fatalf("published commit = %+v, ok=%v, err=%v", commit, ok, err)
	}
	a, ok, err := loadShareAttempt(cfg, name)
	if err != nil || !ok || a.State != "succeeded" || a.Seq != commit.Seq || a.RunID != commit.RunID {
		t.Fatalf("terminal attempt = %+v, ok=%v, err=%v; commit=%+v", a, ok, err, commit)
	}
}

// Regression: owner-CAS crash debris is repaired before a successor fetches.
func TestShareAttemptPreflightRecoversClaimDebris(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	if err := os.MkdirAll(shareSubRoot(cfg, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := startShareAttempt(cfg, name, "dead-run", time.Now()); err != nil {
		t.Fatal(err)
	}
	claim := shareAttemptPath(cfg, name) + ".claim-dead-run"
	if err := atomicio.RenameReplaceWithRetry(shareAttemptPath(cfg, name), claim); err != nil {
		t.Fatal(err)
	}

	sawSuccessor := false
	stopErr := errors.New("stop after recovered preflight")
	err := shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(runID string) (int, error) {
		a, ok, err := loadShareAttempt(cfg, name)
		sawSuccessor = err == nil && ok && a.RunID == runID && a.State == "active"
		return 0, stopErr
	})
	if !errors.Is(err, stopErr) || !sawSuccessor {
		t.Fatalf("successor after claim recovery: saw=%v err=%v", sawSuccessor, err)
	}
	claims, err := shareAttemptClaimPaths(cfg, name)
	if err != nil || len(claims) != 0 {
		t.Fatalf("claim debris after recovery = %v, err=%v", claims, err)
	}
}

// Regression: a command cannot report success after its durable terminal
// attempt transition failed.
func TestShareAttemptTerminalTransitionFailureIsReturned(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	name := "neil"
	err := shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(string) (int, error) {
		if err := os.Remove(shareAttemptPath(cfg, name)); err != nil {
			return 0, err
		}
		return 1, nil
	})
	if err == nil || !strings.Contains(err.Error(), "durable attempt transition failed") {
		t.Fatalf("terminal transition error was swallowed: %v", err)
	}
}
