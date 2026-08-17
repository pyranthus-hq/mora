package mora

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	gitsyncpkg "github.com/pyranthus-hq/mora/internal/gitsync"
)

type execFunc = gitsyncpkg.Runner

const gitignoreBody = gitsyncpkg.GitignoreBody

func redactCredentials(s string) string { return gitsyncpkg.RedactCredentials(s) }
func realExec(ctx context.Context, dir, name string, args ...string) (string, error) {
	return gitsyncpkg.RealExec(ctx, dir, name, args...)
}
func vaultRepoState(path string) (bool, error) { return gitsyncpkg.VaultRepoState(path) }
func commitIdentityArgs(ctx context.Context, vault string, run execFunc) []string {
	return gitsyncpkg.CommitIdentityArgs(ctx, vault, run)
}
func configureRemote(ctx context.Context, vault string, github bool, name, remote string, run execFunc) error {
	return gitsyncpkg.ConfigureRemote(ctx, vault, github, name, remote, run)
}

const gitSyncDisclosure = `
  ⚠ Your Mora vault now LEAVES THIS DEVICE on every sync.
    It contains decoded iMessages + Gmail threads in PLAINTEXT.
    The remote must be a PRIVATE repository you control — Mora runs no server.
    Want ciphertext at rest on the remote? Layer git-remote-gcrypt over your remote.
    Restore on a new machine: git clone <remote> ~/vault/mora && mora index rebuild`

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
	err := gitsyncpkg.Sync(ctx, gitsyncpkg.Options{Vault: cfg.VaultDir, Init: *doInit, Remote: *remote, GitHub: *github, RepoName: *repoName, CommitMessage: *msg, Now: time.Now()}, run)
	if err != nil {
		return err
	}
	if *doInit {
		fmt.Fprintln(stdout, "vault git-sync initialized and pushed.")
		fmt.Fprintln(stdout, gitSyncDisclosure)
	} else {
		fmt.Fprintln(stdout, "vault pushed to origin.")
	}
	return nil
}
