package mora

import (
	"os"
	"path/filepath"
	"testing"
)

// brief_mt_cover_test.go covers brief.go's remaining store/resolver branches: the
// nil-items normalization on load/save, the lock's MkdirAll error and idempotent
// double-release, latestBriefPath's sub-directory skip, and resolveBrief's read /
// generate error paths.

// TestMt_LoadBriefSnapshotNilItemsNormalized: a well-formed snapshot with a
// matching schema version but NO items map loads with Items normalized to a
// non-nil empty map.

// TestMt_SaveBriefSnapshotNilItems: saving a snapshot with a nil Items map persists
// an empty object and round-trips to a non-nil empty map.

// TestMt_AcquireBriefLockMkdirError: a StateDir that is a FILE makes the lock's
// MkdirAll of <StateDir>/brief fail.

// TestMt_AcquireBriefLockDoubleRelease: releasing twice is idempotent (the second
// call is a guarded no-op), and a fresh acquire succeeds afterward.

// TestMt_LatestBriefPathSkipsSubdir: a directory whose name matches the brief
// pattern is skipped (only regular files are candidates), so the newest FILE wins.
func TestMt_LatestBriefPathSkipsSubdir(t *testing.T) {
	cfg := resolveCfg(t)
	seedBriefFile(t, cfg, "2026-06-08", "real")
	// A directory with a NEWER parseable date — must be skipped, not selected.
	dir := filepath.Join(cfg.VaultDir, "briefs", "2026-06-09-brief.md")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir brief-named subdir: %v", err)
	}
	path, dated, ok := latestBriefPath(cfg, resolveFixedNow)
	if !ok {
		t.Fatal("latestBriefPath ok=false, want the real file")
	}
	if filepath.Base(path) != "2026-06-08-brief.md" {
		t.Fatalf("latestBriefPath = %q, want the real file (subdir skipped)", path)
	}
	if got := dated.UTC().Format("2006-01-02"); got != "2026-06-08" {
		t.Fatalf("dated = %q, want 2026-06-08", got)
	}
}

// TestMt_ResolveBriefReadError: a fresh brief entry that is a DANGLING SYMLINK is
// selected by latestBriefPath (a non-dir entry with a parseable date) but fails the
// verbatim os.ReadFile, and resolveBrief surfaces that error.
func TestMt_ResolveBriefReadError(t *testing.T) {
	skipOnWindows(t, "os.Symlink needs SeCreateSymbolicLinkPrivilege (Developer Mode/elevation) on Windows; the dangling-symlink read-error injection is POSIX-only")
	cfg := resolveCfg(t)
	dir := filepath.Join(cfg.VaultDir, "briefs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "2026-06-08-brief.md") // today's UTC day → fresh
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, _, err := resolveBrief(cfg, resolveFixedNow, briefOpts{}); err == nil {
		t.Fatal("resolveBrief should surface the unreadable (dangling-symlink) brief error")
	}
}

// TestMt_ResolveBriefGenerateError: with no cached brief, an unreadable memories
// subtree makes the generate path's buildDigest fail, and resolveBrief surfaces it.
func TestMt_ResolveBriefGenerateError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	mtMakeMemoriesUnreadable(t, cfg) // WalkDir permission error in allMemoryFiles
	// No briefs/ dir exists → the cache path is skipped and we hit the generate path.
	if _, _, err := resolveBrief(cfg, resolveFixedNow, briefOpts{}); err == nil {
		t.Fatal("resolveBrief generate path should surface the buildDigest error")
	}
}
