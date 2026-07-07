package mora

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// gitsync.go — realExec
// ---------------------------------------------------------------------------

// TestHk_RealExecSuccess runs a real subprocess (the test binary itself with a
// no-match filter) and asserts the combined output is captured with no error.
func TestHk_RealExecSuccess(t *testing.T) {
	out, err := realExec(context.Background(), "", os.Args[0],
		"-test.run=^TestHkRealExecNeverMatchesXYZ$", "-test.count=1")
	if err != nil {
		t.Fatalf("realExec on the test binary should succeed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "PASS") && !strings.Contains(out, "no tests to run") {
		t.Fatalf("expected go-test output from the subprocess, got: %q", out)
	}
}

// TestHk_RealExecRedactsCredentialOnError asserts a spawn failure returns a
// redacted error — an embedded PAT in the args must never survive into the
// message git/callers may log.
func TestHk_RealExecRedactsCredentialOnError(t *testing.T) {
	_, err := realExec(context.Background(), t.TempDir(),
		"mora-hk-nonexistent-binary-xyz", "push",
		"https://ghp_SECRET123@github.com/me/vault.git")
	if err == nil {
		t.Fatal("realExec on a missing binary must error")
	}
	if strings.Contains(err.Error(), "ghp_SECRET123") {
		t.Fatalf("credential leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("expected the redaction marker in the error, got: %v", err)
	}
}

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
func TestHk_SyncGitMissingVaultDir(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	cfg.VaultDir = filepath.Join(t.TempDir(), "does-not-exist")
	f := dirtyFake()
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "vault dir") {
		t.Fatalf("missing vault dir must fail loud, got: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("must not shell out when the vault dir is missing, calls=%v", f.calls)
	}
}

// TestHk_SyncGitGitNotFound asserts the `git --version` preflight failure is
// surfaced with actionable text.
func TestHk_SyncGitGitNotFound(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	f.errOn = map[string]error{"git --version": os.ErrNotExist}
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git is required") {
		t.Fatalf("missing git must fail loud, got: %v", err)
	}
}

// TestHk_SyncGitInitError asserts a failing `git init` aborts the bootstrap.
func TestHk_SyncGitInitError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	f.errOn = map[string]error{"git init": os.ErrPermission}
	err := syncGit(context.Background(), cfg, []string{"--init", "--remote", "git@x:me/v.git"}, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git init") {
		t.Fatalf("git init failure must abort, got: %v", err)
	}
}

// TestHk_SyncGitNoOriginNonInit asserts a repo with no origin (and no --init)
// fails loud rather than silently no-op'ing.
func TestHk_SyncGitNoOriginNonInit(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake() // hasOrigin defaults false
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "no usable `origin`") {
		t.Fatalf("missing origin must fail loud, got: %v", err)
	}
}

// TestHk_SyncGitAddError asserts a failing `git add -A` aborts before commit.
func TestHk_SyncGitAddError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	f.errOn = map[string]error{"git add": os.ErrPermission}
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git add") {
		t.Fatalf("git add failure must abort, got: %v", err)
	}
	if f.sawSubcommand("git", "commit") {
		t.Errorf("must not commit after a failed add, calls=%v", f.calls)
	}
}

// TestHk_SyncGitLsFilesError asserts a failing `git ls-files` (the tracked-secret
// guard) aborts the sync.
func TestHk_SyncGitLsFilesError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	f.errOn = map[string]error{"git ls-files": os.ErrPermission}
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git ls-files") {
		t.Fatalf("git ls-files failure must abort, got: %v", err)
	}
}

// TestHk_SyncGitStatusError asserts a failing `git status` aborts the sync.
func TestHk_SyncGitStatusError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	f.errOn = map[string]error{"git status": os.ErrPermission}
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git status") {
		t.Fatalf("git status failure must abort, got: %v", err)
	}
}

// TestHk_SyncGitCommitError asserts a failing `git commit` on a dirty tree aborts
// before any push.
func TestHk_SyncGitCommitError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	f.errOn = map[string]error{"git commit": os.ErrPermission}
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git commit") {
		t.Fatalf("git commit failure must abort, got: %v", err)
	}
	if f.sawSubcommand("git", "push") {
		t.Errorf("must not push after a failed commit, calls=%v", f.calls)
	}
}

// TestHk_SyncGitGithubCreateError asserts a failing `gh repo create` is surfaced
// with the gh-auth hint.
func TestHk_SyncGitGithubCreateError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	f.errOn = map[string]error{"gh repo": os.ErrPermission}
	err := syncGit(context.Background(), cfg, []string{"--init", "--github"}, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "gh repo create") {
		t.Fatalf("gh repo create failure must surface, got: %v", err)
	}
}

// TestHk_SyncGitRepointExistingOrigin asserts `--init --remote <url>` on a repo
// that already has an origin uses `git remote set-url` (re-point), not `add`.
func TestHk_SyncGitRepointExistingOrigin(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	newURL := "git@example.test:me/relocated.git"
	if err := syncGit(context.Background(), cfg, []string{"--init", "--remote", newURL}, io.Discard, f.run); err != nil {
		t.Fatalf("re-point must succeed: %v", err)
	}
	if !f.sawSubcommand("git", "remote", "set-url", "origin", newURL) {
		t.Fatalf("re-point must use `git remote set-url`, calls=%v", f.calls)
	}
	if f.sawSubcommand("git", "remote", "add", "origin", newURL) {
		t.Fatalf("must not `git remote add` when origin already exists, calls=%v", f.calls)
	}
}

// TestHk_ConfigureRemoteAddError asserts a failing `git remote add` (setting a
// fresh origin) is surfaced.
func TestHk_ConfigureRemoteAddError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	// Custom exec: no origin yet, and `git remote add` fails; everything else ok.
	run := func(_ context.Context, _ string, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 2 && args[0] == "remote" && args[1] == "get-url" {
			return "", os.ErrNotExist
		}
		if name == "git" && len(args) >= 2 && args[0] == "remote" && args[1] == "add" {
			return "", os.ErrPermission
		}
		return "", nil
	}
	err := syncGit(context.Background(), cfg, []string{"--init", "--remote", "git@x:me/v.git"}, io.Discard, run)
	if err == nil || !strings.Contains(err.Error(), "git remote add origin") {
		t.Fatalf("git remote add failure must surface, got: %v", err)
	}
}

// TestHk_SyncGitGitignoreWriteError asserts a failure to write the defensive
// .gitignore (unwritable vault dir) aborts the --init bootstrap. Unix-only: it
// relies on POSIX directory-write permission semantics.
func TestHk_SyncGitGitignoreWriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write-permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("runs as root — the 0500 write bit is bypassed, so the write error can't be provoked")
	}
	cfg := gitSyncTestConfig(t)
	if err := os.Chmod(cfg.VaultDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.VaultDir, 0o700) })
	f := dirtyFake()
	err := syncGit(context.Background(), cfg, []string{"--init", "--remote", "git@x:me/v.git"}, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), ".gitignore") {
		t.Fatalf("unwritable vault must fail the .gitignore write, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// gitsync.go — vaultRepoState
// ---------------------------------------------------------------------------

// TestHk_VaultRepoStateLstatError asserts a non-NotExist Lstat error (a path
// component that is a file, yielding ENOTDIR) is surfaced, not misread as
// "no repo".
func TestHk_VaultRepoStateLstatError(t *testing.T) {
	skipOnWindows(t, "a file path component yields ERROR_PATH_NOT_FOUND on Windows, which os.IsNotExist treats as not-exist, so vaultRepoState correctly returns (false,nil) instead of a POSIX ENOTDIR error")
	root := t.TempDir()
	afile := filepath.Join(root, "afile")
	if err := os.WriteFile(afile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	isRepo, err := vaultRepoState(filepath.Join(afile, ".git"))
	if err == nil || !strings.Contains(err.Error(), "inspecting") {
		t.Fatalf("a non-NotExist Lstat error must surface, got isRepo=%v err=%v", isRepo, err)
	}
	if isRepo {
		t.Fatal("a failed inspection must not report the vault as a repo")
	}
}

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
