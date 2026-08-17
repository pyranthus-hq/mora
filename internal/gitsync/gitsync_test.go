package gitsync

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type Config struct{ VaultDir, DataDir, StateDir, ConfigDir string }

func syncGit(ctx context.Context, cfg Config, args []string, _ io.Writer, run Runner) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	doInit := fs.Bool("init", false, "")
	remote := fs.String("remote", "", "")
	github := fs.Bool("github", false, "")
	name := fs.String("name", "mora-vault", "")
	message := fs.String("m", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return Sync(ctx, Options{Vault: cfg.VaultDir, Init: *doInit, Remote: *remote, GitHub: *github, RepoName: *name, CommitMessage: *message, Now: time.Now()}, run)
}
func redactCredentials(s string) string { return RedactCredentials(s) }
func realExec(ctx context.Context, dir, name string, args ...string) (string, error) {
	return RealExec(ctx, dir, name, args...)
}
func vaultRepoState(path string) (bool, error) { return VaultRepoState(path) }
func skipOnWindows(t *testing.T, _ ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission semantics differ on Windows")
	}
}

// fakeExec records every (name + args) invocation and replays canned output /
// errors keyed by "<name> <args[0]>" (e.g. "git push", "git status"). It is the
// single seam that stands in for the real git/gh subprocesses so gitsync logic
// is exercised with zero shelling out. It models `origin` realistically: a fresh
// repo has none, so `git remote get-url origin` errors until `git remote add`,
// `git remote set-url`, or `gh repo create` wires one — mirroring real git's
// non-zero exit, which is the signal configureRemote branches on.
type fakeExec struct {
	calls     [][]string
	out       map[string]string
	errOn     map[string]error
	hasOrigin bool
}

func (f *fakeExec) run(_ context.Context, _ string, name string, args ...string) (string, error) {
	rec := append([]string{name}, args...)
	f.calls = append(f.calls, rec)

	// Model the origin remote's lifecycle so get-url's exit code is realistic.
	if name == "git" && len(args) >= 2 && args[0] == "remote" {
		switch args[1] {
		case "get-url":
			if !f.hasOrigin {
				return "", os.ErrNotExist
			}
			return "git@example.test:me/vault.git", nil
		case "add", "set-url":
			f.hasOrigin = true
			return "", nil
		}
	}
	if name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "create" {
		f.hasOrigin = true // gh wires origin via --source/--remote
		if e, ok := f.errOn["gh repo"]; ok {
			f.hasOrigin = false
			return "", e
		}
		return "", nil
	}

	key := name
	if len(args) > 0 {
		key = name + " " + args[0]
	}
	if e, ok := f.errOn[key]; ok {
		return f.out[key], e
	}
	return f.out[key], nil
}

// sawSubcommand reports whether the recorded calls include one whose name and
// leading args match the given prefix (order-insensitive across the whole log).
func (f *fakeExec) sawSubcommand(prefix ...string) bool {
	for _, c := range f.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for i := range prefix {
			if c[i] != prefix[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// gitSyncTestConfig builds a Config rooted at a temp dir with a real vault dir.
func gitSyncTestConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	// A representative vault file so `git add -A` has something to track.
	if err := os.WriteFile(filepath.Join(vault, "index.md"), []byte("# index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{
		VaultDir:  vault,
		DataDir:   filepath.Join(root, "data"),
		StateDir:  filepath.Join(root, "state"),
		ConfigDir: filepath.Join(root, "config"),
	}
}

// dirty makes the fake report a non-empty working tree so a commit is issued.
func dirtyFake() *fakeExec {
	return &fakeExec{
		out: map[string]string{
			"git --version": "git version 2.43.0",
			"git status":    "?? new.md\n",
			"git config":    "configured", // user.name/user.email already set
		},
	}
}

func TestSyncGitInit_IdempotentSkipsInitWhenRepoExists(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true // origin already configured
	var out strings.Builder

	if err := syncGit(context.Background(), cfg, []string{"--init"}, &out, f.run); err != nil {
		t.Fatalf("syncGit --init (existing repo): %v", err)
	}
	if f.sawSubcommand("git", "init") {
		t.Errorf("must not re-run `git init` when .git exists, calls=%v", f.calls)
	}
}

func TestSyncGitInit_NoRemoteFailsLoud(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	var out strings.Builder
	// No --remote, no --github, no existing origin (hasOrigin defaults false).

	err := syncGit(context.Background(), cfg, []string{"--init"}, &out, f.run)
	if err == nil {
		t.Fatal("expected fail-loud error when no remote is configured")
	}
	if f.sawSubcommand("git", "push", "-u", "origin", "HEAD") {
		t.Error("must not push when no remote is configured")
	}
}

func TestSyncGit_BareRequiresInit(t *testing.T) {
	cfg := gitSyncTestConfig(t) // no .git
	f := dirtyFake()
	var out strings.Builder

	err := syncGit(context.Background(), cfg, nil, &out, f.run)
	if err == nil {
		t.Fatal("expected error when vault is not a git repo")
	}
	if !strings.Contains(err.Error(), "--init") {
		t.Errorf("error should advise --init, got: %v", err)
	}
}

func TestSyncGit_GithubConvenienceCreatesPrivateRepo(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake() // no origin yet (hasOrigin defaults false)
	var out strings.Builder

	// Bare --github uses the default repo name; --name overrides it.
	err := syncGit(context.Background(), cfg, []string{"--init", "--github"}, &out, f.run)
	if err != nil {
		t.Fatalf("syncGit --github: %v", err)
	}
	if !f.sawSubcommand("gh", "repo", "create", "mora-vault") {
		t.Errorf("expected `gh repo create mora-vault`, calls=%v", f.calls)
	}

	f2 := dirtyFake()
	var out2 strings.Builder
	if err := syncGit(context.Background(), cfg, []string{"--init", "--github", "--name", "my-vault"}, &out2, f2.run); err != nil {
		t.Fatalf("syncGit --github --name: %v", err)
	}
	if !f2.sawSubcommand("gh", "repo", "create", "my-vault") {
		t.Errorf("expected `gh repo create my-vault`, calls=%v", f2.calls)
	}
	// Must be private + non-interactive.
	var ghCall []string
	for _, c := range f.calls {
		if len(c) >= 2 && c[0] == "gh" && c[1] == "repo" {
			ghCall = c
		}
	}
	joined := strings.Join(ghCall, " ")
	if !strings.Contains(joined, "--private") {
		t.Errorf("gh repo create must pass --private, got: %v", ghCall)
	}
}

func TestRedactCredentials(t *testing.T) {
	cases := map[string]string{
		"remote: error\nhttps://ghp_SECRET123@github.com/me/vault.git denied": "https://<redacted>@github.com/me/vault.git",
		"http://user:pass@host/x.git":                                         "http://<redacted>@host/x.git",
		"git@github.com:me/vault.git no creds here":                           "git@github.com:me/vault.git",
	}
	for in, mustContain := range cases {
		got := redactCredentials(in)
		if strings.Contains(got, "ghp_SECRET123") || strings.Contains(got, "user:pass") {
			t.Errorf("secret survived redaction: %q -> %q", in, got)
		}
		if !strings.Contains(got, mustContain) {
			t.Errorf("redact(%q) = %q, want substring %q", in, got, mustContain)
		}
	}
}

func TestSyncGit_FallbackIdentityWhenUnset(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Fresh machine: dirty tree but NO configured git identity (no "git config"
	// canned output → gitConfigSet reports unset).
	f := &fakeExec{
		hasOrigin: true,
		out: map[string]string{
			"git --version": "git version 2.43.0",
			"git status":    "?? new.md\n",
		},
	}
	var out strings.Builder
	if err := syncGit(context.Background(), cfg, nil, &out, f.run); err != nil {
		t.Fatalf("syncGit (no identity): %v", err)
	}
	// The commit call must carry fallback identity so a fresh machine doesn't abort.
	var commit []string
	for _, c := range f.calls {
		for _, a := range c {
			if a == "commit" {
				commit = c
			}
		}
	}
	joined := strings.Join(commit, " ")
	if !strings.Contains(joined, "user.email=mora@localhost") || !strings.Contains(joined, "user.name=Mora") {
		t.Errorf("commit must inject fallback identity when unset, got: %v", commit)
	}
}

func TestSyncGit_PreservesConfiguredIdentity(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake() // "git config" -> "configured" (identity is set)
	f.hasOrigin = true
	var out strings.Builder
	if err := syncGit(context.Background(), cfg, nil, &out, f.run); err != nil {
		t.Fatalf("syncGit: %v", err)
	}
	if !f.sawSubcommand("git", "commit") {
		t.Errorf("expected a commit on a dirty tree, calls=%v", f.calls)
	}
	for _, c := range f.calls {
		for _, a := range c {
			if strings.HasPrefix(a, "user.name=") || strings.HasPrefix(a, "user.email=") {
				t.Errorf("must not override a configured identity, calls=%v", f.calls)
			}
		}
	}
}

func TestSyncGit_RefusesGitfileAndSymlinkIndirection(t *testing.T) {
	// A `.git` FILE (worktree/submodule-style `gitdir:` indirection) or symlink
	// must be refused outright: following it would make every git command operate
	// on a PARENT or unrelated repository — staging the vault into that repo and
	// pushing it to that repo's remote.
	t.Run("gitfile", func(t *testing.T) {
		cfg := gitSyncTestConfig(t)
		if err := os.WriteFile(filepath.Join(cfg.VaultDir, ".git"), []byte("gitdir: ../.git\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f := dirtyFake()
		f.hasOrigin = true
		var out strings.Builder
		err := syncGit(context.Background(), cfg, nil, &out, f.run)
		if err == nil || !strings.Contains(err.Error(), "not a plain directory") {
			t.Fatalf("gitfile .git must be refused, got: %v", err)
		}
		if f.sawSubcommand("git", "add") {
			t.Errorf("must not stage anything behind an indirected .git, calls=%v", f.calls)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		skipOnWindows(t, "os.Symlink needs SeCreateSymbolicLinkPrivilege on Windows; the symlinked-.git indirection cannot be created without elevation (the gitfile subtest still covers the refusal)")
		cfg := gitSyncTestConfig(t)
		if err := os.Symlink(t.TempDir(), filepath.Join(cfg.VaultDir, ".git")); err != nil {
			t.Fatal(err)
		}
		f := dirtyFake()
		f.hasOrigin = true
		var out strings.Builder
		err := syncGit(context.Background(), cfg, nil, &out, f.run)
		if err == nil || !strings.Contains(err.Error(), "not a plain directory") {
			t.Fatalf("symlinked .git must be refused, got: %v", err)
		}
	})
}

func TestSyncGitInit_GithubIdempotentWhenOriginExists(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true // an earlier `--init --github` already wired origin
	var out strings.Builder
	if err := syncGit(context.Background(), cfg, []string{"--init", "--github"}, &out, f.run); err != nil {
		t.Fatalf("re-running --init --github with an existing origin must succeed: %v", err)
	}
	if f.sawSubcommand("gh", "repo", "create") {
		t.Errorf("must not re-create the GitHub repo when origin exists, calls=%v", f.calls)
	}
	if !f.sawSubcommand("git", "push", "-u", "origin", "HEAD") {
		t.Errorf("re-init should still push, calls=%v", f.calls)
	}
}

func TestSyncGit_RefusesDetachedHead(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	f.errOn = map[string]error{"git symbolic-ref": os.ErrNotExist} // -q exits non-zero when detached
	var out strings.Builder
	err := syncGit(context.Background(), cfg, nil, &out, f.run)
	if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("detached HEAD must be refused, got: %v", err)
	}
	if f.sawSubcommand("git", "add") || f.sawSubcommand("git", "commit") {
		t.Errorf("must not stage or commit on detached HEAD, calls=%v", f.calls)
	}
}

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

func TestHk_SyncGitGitNotFound(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	f.errOn = map[string]error{"git --version": os.ErrNotExist}
	err := syncGit(context.Background(), cfg, nil, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git is required") {
		t.Fatalf("missing git must fail loud, got: %v", err)
	}
}

func TestHk_SyncGitInitError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	f.errOn = map[string]error{"git init": os.ErrPermission}
	err := syncGit(context.Background(), cfg, []string{"--init", "--remote", "git@x:me/v.git"}, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "git init") {
		t.Fatalf("git init failure must abort, got: %v", err)
	}
}

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

func TestHk_SyncGitGithubCreateError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	f.errOn = map[string]error{"gh repo": os.ErrPermission}
	err := syncGit(context.Background(), cfg, []string{"--init", "--github"}, io.Discard, f.run)
	if err == nil || !strings.Contains(err.Error(), "gh repo create") {
		t.Fatalf("gh repo create failure must surface, got: %v", err)
	}
}

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
