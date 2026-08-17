// Package gitsync owns fail-loud, push-only local-vault Git backup mechanics.
package gitsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// Runner invokes a local process and returns combined output.
type Runner func(ctx context.Context, dir, name string, args ...string) (string, error)

// Options are parsed and authorized by the Mora composition root.
type Options struct {
	Vault         string
	Init          bool
	Remote        string
	GitHub        bool
	RepoName      string
	CommitMessage string
	Now           time.Time
}

var credentialURL = regexp.MustCompile(`(https?://)[^/@\s]+@`)

// RedactCredentials strips HTTP(S) URL userinfo from logs and errors.
func RedactCredentials(s string) string { return credentialURL.ReplaceAllString(s, "${1}<redacted>@") }

// RealExec runs a local command and redacts arguments and output on failure.
func RealExec(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		return text, fmt.Errorf("%s %s: %w\n%s", name, RedactCredentials(strings.Join(args, " ")), err, RedactCredentials(text))
	}
	return text, nil
}

const GitignoreBody = `# Mora vault — off-device git sync (managed by ` + "`mora sync git`" + `)
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

// Sync executes a validated push-only backup plan.
func Sync(ctx context.Context, opts Options, run Runner) error {
	githubReq := opts.GitHub
	githubName := opts.RepoName
	vault := opts.Vault
	if _, err := os.Stat(vault); err != nil {
		return fmt.Errorf("vault dir %s: %w", vault, err)
	}

	// Preflight: git must be on PATH. This is the only command run with dir="".
	if _, err := run(ctx, "", "git", "--version"); err != nil {
		return fmt.Errorf("git is required for vault sync but was not found: %w", err)
	}

	gitDir := filepath.Join(vault, ".git")
	isRepo, repoErr := VaultRepoState(gitDir)
	if repoErr != nil {
		return repoErr
	}

	if !opts.Init && !isRepo {
		return fmt.Errorf("vault is not a git repo — run `mora sync git --init --remote <URL>` (or --init --github) first")
	}

	if opts.Init {
		// Defensive .gitignore (idempotent: only written if absent, never clobbers
		// a user's edits).
		giPath := filepath.Join(vault, ".gitignore")
		if _, err := os.Stat(giPath); os.IsNotExist(err) {
			if werr := atomicio.Write(giPath, []byte(GitignoreBody), 0o644); werr != nil {
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
		if err := ConfigureRemote(ctx, vault, githubReq, githubName, opts.Remote, run); err != nil {
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
		commitMsg := opts.CommitMessage
		if commitMsg == "" {
			commitMsg = "mora vault sync " + opts.Now.Format(time.RFC3339)
		}
		// On a fresh machine with no global git identity, `git commit` aborts. A
		// backup must not break out of the box, so inject a fallback identity ONLY
		// for the field(s) the user has not set (their real name/email is preserved
		// when present — `-c` is omitted for a configured field).
		args := append(CommitIdentityArgs(ctx, vault, run), "commit", "-m", commitMsg)
		if _, err := run(ctx, vault, "git", args...); err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
	}

	// Push. On --init use `-u` to set upstream; thereafter a plain push to origin
	// HEAD. NO --force: a non-fast-forward rejection means the remote diverged and
	// is surfaced loudly rather than silently overwritten (the vault is a backup).
	pushArgs := []string{"push", "origin", "HEAD"}
	if opts.Init {
		pushArgs = []string{"push", "-u", "origin", "HEAD"}
	}
	if _, err := run(ctx, vault, "git", pushArgs...); err != nil {
		return fmt.Errorf("git push failed (the vault was NOT backed up): %w", err)
	}

	return nil
}

func VaultRepoState(gitDir string) (isRepo bool, err error) {
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

func CommitIdentityArgs(ctx context.Context, vault string, run Runner) []string {
	var pre []string
	if !gitConfigSet(ctx, vault, "user.name", run) {
		pre = append(pre, "-c", "user.name=Mora")
	}
	if !gitConfigSet(ctx, vault, "user.email", run) {
		pre = append(pre, "-c", "user.email=mora@localhost")
	}
	return pre
}

func gitConfigSet(ctx context.Context, vault, key string, run Runner) bool {
	out, err := run(ctx, vault, "git", "config", key)
	return err == nil && strings.TrimSpace(out) != ""
}

func ConfigureRemote(ctx context.Context, vault string, githubReq bool, githubName, remoteURL string, run Runner) error {
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
