package mora

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestSyncGitInit_BootstrapsRepoAndPushes(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	f := dirtyFake()
	var out strings.Builder

	err := syncGit(context.Background(), cfg, []string{"--init", "--remote", "git@example.test:me/vault.git"}, &out, f.run)
	if err != nil {
		t.Fatalf("syncGit --init: %v", err)
	}

	// .gitignore must be written with the rebuildable/sensitive exclusions.
	gi, rerr := os.ReadFile(filepath.Join(cfg.VaultDir, ".gitignore"))
	if rerr != nil {
		t.Fatalf("reading .gitignore: %v", rerr)
	}
	for _, want := range []string{"index.db", ".DS_Store", "tokens/"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf(".gitignore missing %q\n%s", want, gi)
		}
	}

	// Bootstrap sequence: init, remote add, add -A, commit, push -u.
	if !f.sawSubcommand("git", "init") {
		t.Error("expected `git init`")
	}
	if !f.sawSubcommand("git", "remote", "add", "origin", "git@example.test:me/vault.git") {
		t.Errorf("expected `git remote add origin <url>`, calls=%v", f.calls)
	}
	if !f.sawSubcommand("git", "add", "-A") {
		t.Error("expected `git add -A`")
	}
	if !f.sawSubcommand("git", "commit", "-m") {
		t.Error("expected `git commit -m`")
	}
	if !f.sawSubcommand("git", "push", "-u", "origin", "HEAD") {
		t.Errorf("expected `git push -u origin HEAD`, calls=%v", f.calls)
	}
	// Loud privacy disclosure must be printed.
	if !strings.Contains(strings.ToLower(out.String()), "leaves this device") {
		t.Errorf("expected privacy disclosure in output, got:\n%s", out.String())
	}
	// Never force-push (would silently destroy remote history).
	for _, c := range f.calls {
		for _, a := range c {
			if a == "--force" || a == "-f" || strings.HasPrefix(a, "--force-with-lease") {
				t.Errorf("must not force-push, calls=%v", f.calls)
			}
		}
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

func TestSyncGit_FailLoudOnPushError(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	f.errOn = map[string]error{"git push": os.ErrPermission}
	var out strings.Builder

	if err := syncGit(context.Background(), cfg, nil, &out, f.run); err == nil {
		t.Fatal("push failure must surface (never swallowed)")
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

func TestGitDailyScheduleWired(t *testing.T) {
	if scheduleCommands["git-daily"] != "sync git" {
		t.Errorf("git-daily must map to `sync git`, got %q", scheduleCommands["git-daily"])
	}
	if launchdSchedule("git-daily") == "" {
		t.Error("git-daily must have a launchd schedule")
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

func TestSyncGit_DestinationFlagsRequireInit(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Without --init, a destination flag must be rejected — NOT silently ignored
	// while the vault pushes to whatever origin happens to be configured.
	for _, args := range [][]string{
		{"--remote", "git@example.test:elsewhere/x.git"},
		{"--github"},
		{"--name", "other"},
	} {
		f := dirtyFake()
		f.hasOrigin = true
		var out strings.Builder
		err := syncGit(context.Background(), cfg, args, &out, f.run)
		if err == nil || !strings.Contains(err.Error(), "--init") {
			t.Errorf("args %v without --init must fail mentioning --init, got: %v", args, err)
		}
		if f.sawSubcommand("git", "push") || f.sawSubcommand("gh", "repo") {
			t.Errorf("args %v must not reach push/create, calls=%v", args, f.calls)
		}
	}
}

func TestSyncGit_RejectsPositionalAndConflictingFlags(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	for _, args := range [][]string{
		{"--init", "--github", "mora-backup"},                        // positional repo name: would silently create default "mora-vault"
		{"--init", "--github", "--remote", "git@example.test:x.git"}, // two destinations: refuse, don't resolve by precedence
		{"--init", "--name", "x"},                                    // --name without --github
	} {
		f := dirtyFake()
		var out strings.Builder
		if err := syncGit(context.Background(), cfg, args, &out, f.run); err == nil {
			t.Errorf("args %v must be rejected", args)
		}
		if f.sawSubcommand("gh", "repo", "create") {
			t.Errorf("args %v must not create any repo, calls=%v", args, f.calls)
		}
	}
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

func TestSyncGit_BlocksTrackedSensitiveFiles(t *testing.T) {
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	// Tracked despite .gitignore (pre-existing repo / user-edited ignore list):
	// the guard must hard-stop, because ignore rules don't apply to tracked files.
	f.out["git ls-files"] = "tokens/google.json\nindex.db\n"
	var out strings.Builder
	err := syncGit(context.Background(), cfg, nil, &out, f.run)
	if err == nil || !strings.Contains(err.Error(), "tokens/google.json") {
		t.Fatalf("tracked sensitive files must hard-stop the sync, got: %v", err)
	}
	if f.sawSubcommand("git", "commit") || f.sawSubcommand("git", "push") {
		t.Errorf("must not commit/push tracked sensitive files, calls=%v", f.calls)
	}
}
