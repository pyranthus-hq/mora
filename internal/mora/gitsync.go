package mora

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// execFunc is the single seam through which gitsync shells out to `git` (and the
// optional `gh`). It returns combined stdout+stderr and an error. Tests inject a
// fake; production uses realExec. dir is the working directory for the command
// ("" = inherit the process cwd, used only for the `git --version` preflight).
type execFunc func(ctx context.Context, dir, name string, args ...string) (string, error)

// credentialURL matches a userinfo component in an HTTP(S) URL (e.g. a PAT
// embedded as `https://ghp_xxx@github.com/...` or `http://user:pass@host`). Git
// echoes the remote URL on push failures, so any such secret must be redacted
// before it reaches the terminal, a log file, or a returned error.
var credentialURL = regexp.MustCompile(`(https?://)[^/@\s]+@`)

// redactCredentials strips userinfo (tokens / user:pass) from HTTP(S) URLs so a
// fail-loud git error never leaks the secret it embedded. SSH/scp-style remotes
// (git@host:path) carry no secret and are left untouched.
func redactCredentials(s string) string {
	return credentialURL.ReplaceAllString(s, "${1}<redacted>@")
}

// realExec runs a subprocess, capturing combined output. On failure it returns a
// redacted error so an embedded credential in the args (e.g. `git remote add
// origin https://token@host`) or in git's output never escapes verbatim.
func realExec(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil {
		return s, fmt.Errorf("%s %s: %w\n%s",
			name, redactCredentials(strings.Join(args, " ")), err, redactCredentials(s))
	}
	return s, nil
}

// gitignoreBody is the defensive ignore list written on `--init`. index.db lives
// in DataDir (outside the vault) by default, but the vault and data dir CAN be
// co-located, so the rebuildable index and any stray secrets/tokens are excluded
// regardless. Restore path: `git clone` → `mora index rebuild`.
const gitignoreBody = `# Mora vault — off-device git sync (managed by ` + "`mora sync git`" + `)
# Excluded: rebuildable index + anything secret/state. Restore = clone + ` + "`mora index rebuild`" + `.
index.db
*.db
*.db-shm
*.db-wal
.DS_Store
tokens/
*.token
*.lock
identity*
share/
`

// gitSyncDisclosure is the loud, opt-in privacy notice. Pushing the vault breaks
// the unqualified zero-egress claim: the vault holds DECODED iMessages + Gmail in
// plaintext, so the remote must be private and user-controlled.
const gitSyncDisclosure = `
  ⚠ Your Mora vault now LEAVES THIS DEVICE on every sync.
    It contains decoded iMessages + Gmail threads in PLAINTEXT.
    The remote must be a PRIVATE repository you control — Mora runs no server.
    Want ciphertext at rest on the remote? Layer git-remote-gcrypt over your remote.
    Restore on a new machine: git clone <remote> ~/vault/mora && mora index rebuild`

// syncGit implements `mora sync git [--init] [--remote URL] [--github [name]]
// [-m msg]`: a one-way, push-only, fail-loud backup of the vault to a private git
// remote. v1 has no pull/rebase/conflict logic (the local vault is the single
// writer); the --remote/--github config + the `git` subverb are the seams a v2
// two-way sync would extend. run is the injectable exec seam.
func syncGit(ctx context.Context, cfg Config, args []string, stdout io.Writer, run execFunc) error {
	fs := flag.NewFlagSet("sync git", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	doInit := fs.Bool("init", false, "bootstrap the vault as a git repo and push the first commit")
	remote := fs.String("remote", "", "remote URL to configure as origin (git-generic: GitHub/GitLab/self-hosted/USB)")
	github := fs.Bool("github", false, "create a PRIVATE GitHub repo via gh and wire it as origin")
	repoName := fs.String("name", "mora-vault", "repo name to create with --github")
	msg := fs.String("m", "", "commit message (default: timestamped)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Flag discipline — silently mis-parsing a destination flag on a command that
	// pushes plaintext off-device is a security bug, not a UX nit, so every
	// unusable combination is rejected loudly instead of resolved by precedence.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — the repo name goes via `--github --name <repo>`", fs.Arg(0))
	}
	if !*doInit && (*github || *remote != "" || *repoName != "mora-vault") {
		return fmt.Errorf("--remote/--github/--name configure the destination and require --init (run `mora sync git --init …` to point or re-point the backup)")
	}
	if *github && *remote != "" {
		return fmt.Errorf("--github and --remote are mutually exclusive — pick one destination")
	}
	if *repoName != "mora-vault" && !*github {
		return fmt.Errorf("--name only applies with --github")
	}
	githubReq := *github
	githubName := *repoName

	vault := cfg.VaultDir
	if _, err := os.Stat(vault); err != nil {
		return fmt.Errorf("vault dir %s: %w", vault, err)
	}

	// Preflight: git must be on PATH. This is the only command run with dir="".
	if _, err := run(ctx, "", "git", "--version"); err != nil {
		return fmt.Errorf("git is required for vault sync but was not found: %w", err)
	}

	gitDir := filepath.Join(vault, ".git")
	isRepo, repoErr := vaultRepoState(gitDir)
	if repoErr != nil {
		return repoErr
	}

	if !*doInit && !isRepo {
		return fmt.Errorf("vault is not a git repo — run `mora sync git --init --remote <URL>` (or --init --github) first")
	}

	if *doInit {
		// Defensive .gitignore (idempotent: only written if absent, never clobbers
		// a user's edits).
		giPath := filepath.Join(vault, ".gitignore")
		if _, err := os.Stat(giPath); os.IsNotExist(err) {
			if werr := atomicWrite(giPath, []byte(gitignoreBody), 0o644); werr != nil {
				return fmt.Errorf("writing .gitignore: %w", werr)
			}
		}
		// `git init` only when there's no repo yet (detected via .git, NOT
		// rev-parse — the latter would walk up into a parent repo if the vault is
		// nested under one).
		if !isRepo {
			if _, err := run(ctx, vault, "git", "init"); err != nil {
				return fmt.Errorf("git init: %w", err)
			}
		}
		if err := configureRemote(ctx, vault, githubReq, githubName, *remote, run); err != nil {
			return err
		}
	}

	// origin must exist before we attempt a push (fail-loud, no silent no-op).
	// The probe error rides along so a corrupt config reads differently from a
	// genuinely-missing remote.
	if _, err := run(ctx, vault, "git", "remote", "get-url", "origin"); err != nil {
		return fmt.Errorf("no usable `origin` remote (%v) — run `mora sync git --init --remote <URL>` (or --init --github)", err)
	}

	// Refuse detached HEAD before staging anything: `git push origin HEAD` cannot
	// update a branch from a detached HEAD, and the commit made first would be
	// left dangling once HEAD moves. Fail before mutating, not after.
	if _, err := run(ctx, vault, "git", "symbolic-ref", "-q", "HEAD"); err != nil {
		return fmt.Errorf("vault repo is in detached HEAD — check out a branch (e.g. `git -C %s checkout main`) before syncing: %w", vault, err)
	}

	if _, err := run(ctx, vault, "git", "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// The .gitignore shields only files git is not already tracking. If index.db
	// or a token file was ever tracked (a pre-existing vault repo, a user-edited
	// ignore list), `git add -A` keeps shipping it — so this is a hard stop, not
	// a warning: the contract is that the index and secrets never leave.
	tracked, lsErr := run(ctx, vault, "git", "ls-files", "--",
		"index.db", "*.db", "*.db-shm", "*.db-wal", "*.token", "tokens", "identity*", "share")
	if lsErr != nil {
		return fmt.Errorf("git ls-files: %w", lsErr)
	}
	if t := strings.TrimSpace(tracked); t != "" {
		return fmt.Errorf("refusing to sync: sensitive/rebuildable files are git-TRACKED in the vault (the .gitignore only shields untracked files):\n%s\nuntrack them first (the working copy is kept): git -C %s rm -r --cached <path>  — then re-run `mora sync git`", t, vault)
	}
	// Commit only when the working tree is dirty. `git status --porcelain` is
	// empty on a clean tree AND works before the first commit exists (unlike
	// `git diff-index HEAD`, which errors with no HEAD).
	status, err := run(ctx, vault, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		commitMsg := *msg
		if commitMsg == "" {
			commitMsg = "mora vault sync " + time.Now().Format(time.RFC3339)
		}
		// On a fresh machine with no global git identity, `git commit` aborts. A
		// backup must not break out of the box, so inject a fallback identity ONLY
		// for the field(s) the user has not set (their real name/email is preserved
		// when present — `-c` is omitted for a configured field).
		args := append(commitIdentityArgs(ctx, vault, run), "commit", "-m", commitMsg)
		if _, err := run(ctx, vault, "git", args...); err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
	}

	// Push. On --init use `-u` to set upstream; thereafter a plain push to origin
	// HEAD. NO --force: a non-fast-forward rejection means the remote diverged and
	// is surfaced loudly rather than silently overwritten (the vault is a backup).
	pushArgs := []string{"push", "origin", "HEAD"}
	if *doInit {
		pushArgs = []string{"push", "-u", "origin", "HEAD"}
	}
	if _, err := run(ctx, vault, "git", pushArgs...); err != nil {
		return fmt.Errorf("git push failed (the vault was NOT backed up): %w", err)
	}

	if *doInit {
		fmt.Fprintln(stdout, "vault git-sync initialized and pushed.")
		fmt.Fprintln(stdout, gitSyncDisclosure)
	} else {
		fmt.Fprintln(stdout, "vault pushed to origin.")
	}
	return nil
}

// vaultRepoState classifies vault/.git. A real directory is the ONLY accepted
// repo marker: a gitfile (`gitdir: …` indirection, as worktrees and submodules
// use) or a symlink would make every git command here operate on some OTHER
// repository — `git add -A` would stage the vault into that repo and push it to
// that repo's remote. os.Lstat (never Stat) so a symlink is seen as itself.
func vaultRepoState(gitDir string) (isRepo bool, err error) {
	fi, statErr := os.Lstat(gitDir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, fmt.Errorf("inspecting %s: %w", gitDir, statErr)
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s exists but is not a plain directory (gitfile/symlink indirection) — refusing to operate on a repository that lives elsewhere", gitDir)
	}
	return true, nil
}

// commitIdentityArgs returns leading `-c user.name=… -c user.email=…` overrides
// for ONLY the identity fields the user has not configured, so sync commits
// succeed on a fresh machine without clobbering a real identity that is set.
func commitIdentityArgs(ctx context.Context, vault string, run execFunc) []string {
	var pre []string
	if !gitConfigSet(ctx, vault, "user.name", run) {
		pre = append(pre, "-c", "user.name=Mora")
	}
	if !gitConfigSet(ctx, vault, "user.email", run) {
		pre = append(pre, "-c", "user.email=mora@localhost")
	}
	return pre
}

// gitConfigSet reports whether a git config key resolves to a non-empty value.
func gitConfigSet(ctx context.Context, vault, key string, run execFunc) bool {
	out, err := run(ctx, vault, "git", "config", key)
	return err == nil && strings.TrimSpace(out) != ""
}

// configureRemote wires `origin` for `--init`. Precedence: --github (create a
// private repo via gh) > --remote (set/add the URL) > an already-configured
// origin > fail-loud. gh is invoked non-interactively with explicit flags.
func configureRemote(ctx context.Context, vault string, githubReq bool, githubName, remoteURL string, run execFunc) error {
	_, originErr := run(ctx, vault, "git", "remote", "get-url", "origin")
	hasOrigin := originErr == nil

	switch {
	case githubReq:
		// Idempotent re-run of `--init --github`: origin is already wired (most
		// likely by the earlier create). Re-creating would fail or orphan a
		// duplicate private repo — keep the configured origin; re-pointing is an
		// explicit `--init --remote <URL>`.
		if hasOrigin {
			return nil
		}
		// gh creates the private repo AND wires it as `origin` from --source.
		// --remote names it; we push separately (no --push) for a single, uniform
		// fail-loud push path below.
		if _, err := run(ctx, vault, "gh", "repo", "create", githubName,
			"--private", "--source", vault, "--remote", "origin"); err != nil {
			return fmt.Errorf("gh repo create %s: %w (is the gh CLI installed and authenticated? `gh auth login`)", githubName, err)
		}
	case remoteURL != "":
		sub := "add"
		if hasOrigin {
			sub = "set-url"
		}
		if _, err := run(ctx, vault, "git", "remote", sub, "origin", remoteURL); err != nil {
			return fmt.Errorf("git remote %s origin: %w", sub, err)
		}
	case hasOrigin:
		// Already configured (e.g. a re-run of --init) — nothing to do.
	default:
		return fmt.Errorf("no remote: pass --remote <URL> for a git-generic remote, or --github <name> to create a private GitHub repo")
	}
	return nil
}
