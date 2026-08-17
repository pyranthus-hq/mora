package mora

import (
	"context"
	"database/sql"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// gitsync.go — realExec
// ---------------------------------------------------------------------------

// TestHk_RealExecSuccess runs a real subprocess (the test binary itself with a
// no-match filter) and asserts the combined output is captured with no error.

// TestHk_RealExecRedactsCredentialOnError asserts a spawn failure returns a
// redacted error — an embedded PAT in the args must never survive into the
// message git/callers may log.

// ---------------------------------------------------------------------------
// gitsync.go — syncGit preflight + per-subcommand failure paths
// ---------------------------------------------------------------------------

// TestHk_SyncGitBadFlag asserts an unknown flag is a hard parse error.
func TestHk_SyncGitBadFlag(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	if err := syncGit(context.Background(), cfg, []string{"--bogus"}, io.Discard, f.run); err == nil {
		t.Fatal("an unknown flag must fail the parse")
	}
}

// TestHk_SyncGitMissingVaultDir asserts a nonexistent vault dir fails loud before
// any git command runs.

// TestHk_SyncGitGitNotFound asserts the `git --version` preflight failure is
// surfaced with actionable text.

// TestHk_SyncGitInitError asserts a failing `git init` aborts the bootstrap.

// TestHk_SyncGitNoOriginNonInit asserts a repo with no origin (and no --init)
// fails loud rather than silently no-op'ing.

// TestHk_SyncGitAddError asserts a failing `git add -A` aborts before commit.

// TestHk_SyncGitLsFilesError asserts a failing `git ls-files` (the tracked-secret
// guard) aborts the sync.

// TestHk_SyncGitStatusError asserts a failing `git status` aborts the sync.

// TestHk_SyncGitCommitError asserts a failing `git commit` on a dirty tree aborts
// before any push.

// TestHk_SyncGitGithubCreateError asserts a failing `gh repo create` is surfaced
// with the gh-auth hint.

// TestHk_SyncGitRepointExistingOrigin asserts `--init --remote <url>` on a repo
// that already has an origin uses `git remote set-url` (re-point), not `add`.

// TestHk_ConfigureRemoteAddError asserts a failing `git remote add` (setting a
// fresh origin) is surfaced.

// TestHk_SyncGitGitignoreWriteError asserts a failure to write the defensive
// .gitignore (unwritable vault dir) aborts the --init bootstrap. Unix-only: it
// relies on POSIX directory-write permission semantics.

// ---------------------------------------------------------------------------
// gitsync.go — vaultRepoState
// ---------------------------------------------------------------------------

// TestHk_VaultRepoStateLstatError asserts a non-NotExist Lstat error (a path
// component that is a file, yielding ENOTDIR) is surfaced, not misread as
// "no repo".

// ---------------------------------------------------------------------------
// vaultid.go — readVaultMarker / createVaultMarkerIfAbsent
// ---------------------------------------------------------------------------

// TestHk_ReadVaultMarkerReadError asserts a non-NotExist read error (the marker
// path is a directory) is surfaced verbatim (not wrapped as "corrupt", which is
// reserved for a JSON-decode failure).
func TestHk_ReadVaultMarkerReadError(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.Mkdir(markerPath(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	_, present, err := readVaultMarker(cfg)
	if err == nil {
		t.Fatal("a directory at the marker path must produce a read error")
	}
	if present {
		t.Fatal("a read error must not report the marker as present")
	}
	if strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("a plain read error must not be labeled corrupt: %v", err)
	}
}

// TestHk_CreateVaultMarkerReadErrorPropagates asserts createVaultMarkerIfAbsent
// forwards a corrupt-marker read error rather than overwriting the marker.
func TestHk_CreateVaultMarkerReadErrorPropagates(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.WriteFile(markerPath(cfg), []byte("{ not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_new"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("a corrupt existing marker must abort the write, got: %v", err)
	}
}

// TestHk_CreateVaultMarkerTempError asserts a failure to stage the temp file (an
// unwritable vault dir) surfaces as an error. Unix-only.
func TestHk_CreateVaultMarkerTempError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write-permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("runs as root — the 0500 write bit is bypassed, so the write error can't be provoked")
	}
	cfg := sandboxCfg(t)
	if err := os.Chmod(cfg.VaultDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.VaultDir, 0o700) })
	if _, err := createVaultMarkerIfAbsent(cfg, "v_new"); err == nil {
		t.Fatal("an unwritable vault dir must fail the atomic marker write")
	}
}

// ---------------------------------------------------------------------------
// vaultid.go — readBlockRecord
// ---------------------------------------------------------------------------

// TestHk_ReadBlockRecordReadError asserts a non-NotExist read error (the record
// path is a directory) is surfaced.
func TestHk_ReadBlockRecordReadError(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blockRecordPath(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBlockRecord(cfg); err == nil {
		t.Fatal("a directory at the block-record path must produce a read error")
	}
}

// TestHk_ReadBlockRecordCorruptDegrades asserts a garbage block record degrades
// to "absent" (it is advisory only and must never fail `mora doctor`).
func TestHk_ReadBlockRecordCorruptDegrades(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockRecordPath(cfg), []byte("{ corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, present, err := readBlockRecord(cfg)
	if err != nil {
		t.Fatalf("a corrupt advisory record must degrade quietly, got err: %v", err)
	}
	if present {
		t.Fatalf("a corrupt record must read as absent, got %+v", rec)
	}
}

// ---------------------------------------------------------------------------
// vaultid.go — readIndexVaultID
// ---------------------------------------------------------------------------

// TestHk_ReadIndexVaultIDNoRow asserts an index_meta table with no vault_id row
// reads as an empty id (ErrNoRows -> "", nil), not an error.
func TestHk_ReadIndexVaultIDNoRow(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE index_meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO index_meta(key,value) VALUES('schema_version','2')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	id, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a table without the vault_id row must not error, got: %v", err)
	}
	if id != "" {
		t.Fatalf("readIndexVaultID = %q, want empty", id)
	}
}

// TestHk_ReadIndexVaultIDCorruptDB asserts an unexpected query error (a corrupt,
// non-database file) is surfaced rather than swallowed as "no id".
func TestHk_ReadIndexVaultIDCorruptDB(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath(cfg), []byte("this is not a sqlite database file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readIndexVaultID(context.Background(), cfg); err == nil {
		t.Fatal("a corrupt index db must surface an error, not read as empty id")
	}
}
