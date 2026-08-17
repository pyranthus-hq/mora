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

func TestGitDailyScheduleWired(t *testing.T) {
	if scheduleCommands["git-daily"] != "sync git" {
		t.Errorf("git-daily must map to `sync git`, got %q", scheduleCommands["git-daily"])
	}
	if launchdSchedule("git-daily") == "" {
		t.Error("git-daily must have a launchd schedule")
	}
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
