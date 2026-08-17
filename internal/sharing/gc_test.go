package sharing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/gitsync"
)

type Config = config.Config
type shareCommit = Commit

var shareImportTTL = 10 * time.Minute
var gcOwner string
var shareGCGitExecFn = gitsync.RealExec
var shareGCRemoveFn = os.Remove
var shareGCRemoveAllFn = os.RemoveAll
var shareGCRemovalRetryableFn = atomicio.SharingViolationRetryable

func testGCGCOptions() GCOptions {
	return GCOptions{GitExec: shareGCGitExecFn, Remove: shareGCRemoveFn, RemoveAll: shareGCRemoveAllFn, RemovalRetryable: shareGCRemovalRetryableFn, TTL: shareImportTTL}
}
func shareGCSweep(cfg Config, name string, now time.Time) error {
	return Sweep(GenerationStore{DataDir: cfg.DataDir}, name, gcOwner, now, testGCGCOptions())
}
func shareGenDir(cfg Config, name, gen string) string {
	return GenerationStore{DataDir: cfg.DataDir}.GenDir(name, gen)
}
func shareGenCorpusDir(cfg Config, name, gen string) string {
	return GenerationStore{DataDir: cfg.DataDir}.CorpusDir(name, gen)
}
func shareCommitsDir(cfg Config, name string) string {
	return GenerationStore{DataDir: cfg.DataDir}.CommitsDir(name)
}
func shareCommitPath(cfg Config, name string, seq int) string {
	return GenerationStore{DataDir: cfg.DataDir}.CommitPath(name, seq)
}
func shareFetchDir(cfg Config, name, runID string) string {
	return GenerationStore{DataDir: cfg.DataDir}.FetchDir(name, runID)
}
func shareRepoDir(cfg Config, name string) string { return RepoDir(cfg.DataDir, name) }
func genRunID(gen string) string                  { return RunID(gen) }
func resolvePublishedCommit(cfg Config, name string) (Commit, bool, error) {
	return GenerationStore{DataDir: cfg.DataDir}.Resolve(name)
}
func realExec(ctx context.Context, dir, name string, args ...string) (string, error) {
	return gitsync.RealExec(ctx, dir, name, args...)
}

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

func packetHOld(path string) error {
	old := time.Now().Add(-2 * shareImportTTL)
	return os.Chtimes(path, old, old)
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := realExec(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

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
	gcOwner = "live"
	defer func() { gcOwner = "" }()

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

func TestSweepPreservesFreshAndLiveOwnedWork(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	fresh := shareGenDir(cfg, name, "gen-fresh")
	owned := shareGenDir(cfg, name, "gen-live")
	fetch := shareFetchDir(cfg, name, "live")
	for _, path := range []string{fresh, owned, fetch} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := packetHOld(owned); err != nil {
		t.Fatal(err)
	}
	if err := packetHOld(fetch); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(GenerationStore{DataDir: cfg.DataDir}, name, "live", time.Now(), GCOptions{TTL: shareImportTTL}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fresh, owned, fetch} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active path removed %s: %v", path, err)
		}
	}
}

func TestSweepFailsClosedOnCorruptHead(t *testing.T) {
	cfg := packetHConfig(t)
	store := GenerationStore{DataDir: cfg.DataDir}
	if err := os.MkdirAll(store.CommitsDir("bad"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.CommitPath("bad", 1), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(store, "bad", "", time.Now(), GCOptions{}); err == nil {
		t.Fatal("corrupt head accepted")
	}
	if err := os.MkdirAll(store.Root("blocked"), 0o700); err != nil {
		t.Fatal(err)
	}
	gensErr := errors.New("generation scan failed")
	blocked := GenerationStore{DataDir: cfg.DataDir, ReadDir: func(path string) ([]os.DirEntry, error) {
		if path == store.GensDir("blocked") {
			return nil, gensErr
		}
		return os.ReadDir(path)
	}}
	if err := Sweep(blocked, "blocked", "", time.Now(), GCOptions{}); !errors.Is(err, gensErr) {
		t.Fatalf("generation scan error = %v", err)
	}
	if err := os.MkdirAll(store.Root("records"), 0o700); err != nil {
		t.Fatal(err)
	}
	reads := 0
	readErr := errors.New("commit scan failed")
	failing := GenerationStore{DataDir: cfg.DataDir, ReadDir: func(string) ([]os.DirEntry, error) {
		reads++
		if reads == 2 {
			return nil, readErr
		}
		return nil, nil
	}}
	if err := Sweep(failing, "records", "", time.Now(), GCOptions{}); !errors.Is(err, readErr) {
		t.Fatalf("commit scan error = %v", err)
	}
}

func TestDeferrableRemovalPolicyAndDefaultGCOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deferrableRemove(path, GCOptions{}.defaults()); err != nil {
		t.Fatal(err)
	}
	if err := deferrableRemove(path, GCOptions{}.defaults()); err != nil {
		t.Fatal(err)
	}
	retry := errors.New("sharing")
	if err := deferrableRemove("x", GCOptions{Remove: func(string) error { return retry }, RemovalRetryable: func(err error) bool { return err == retry }}.defaults()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := DeferrableRemoveAll(dir, GCOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := DeferrableRemoveAll(dir, GCOptions{}); err != nil {
		t.Fatal(err)
	}
	loud := errors.New("io")
	if err := DeferrableRemoveAll("x", GCOptions{RemoveAll: func(string) error { return loud }, RemovalRetryable: func(error) bool { return false }}); !errors.Is(err, loud) {
		t.Fatalf("err=%v", err)
	}
}

func TestSweepGitPinCleanupIsBestEffortAndStrictlyScoped(t *testing.T) {
	cfg := packetHConfig(t)
	const name = "acme"
	repo := shareRepoDir(cfg, name)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	var updates int
	run := func(_ context.Context, _ string, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "for-each-ref" {
			return "malformed\nrefs/mora/import/live aaa\nrefs/mora/import/dead bbb\n", nil
		}
		if len(args) > 0 && args[0] == "update-ref" {
			updates++
			return "", nil
		}
		return "", nil
	}
	if err := Sweep(GenerationStore{DataDir: cfg.DataDir}, name, "live", time.Now(), GCOptions{GitExec: run}); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Fatalf("updates=%d", updates)
	}
	failed := func(context.Context, string, string, ...string) (string, error) { return "", errors.New("git failed") }
	if err := Sweep(GenerationStore{DataDir: cfg.DataDir}, name, "", time.Now(), GCOptions{GitExec: failed}); err != nil {
		t.Fatalf("best-effort git failure surfaced: %v", err)
	}
}
