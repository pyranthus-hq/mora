package mora

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/creativeprojects/go-selfupdate"
)

// upgradeRepoOwner / upgradeRepoName point self-update at the release source.
const (
	upgradeRepoOwner = "pyranthus-hq"
	upgradeRepoName  = "mora"
)

// cmdUpgrade implements `mora upgrade [--check]`: in-place self-update from the
// latest GitHub release, mirroring how Claude Code keeps itself current. Homebrew
// installs are deferred to `brew upgrade`; source/dev builds are refused.
func cmdUpgrade(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(stdout)
	checkOnly := fs.Bool("check", false, "only report whether an update is available; don't install")
	if err := fs.Parse(args); err != nil {
		return err
	}

	current := BuildVersion
	if current == "dev" || current == "" {
		return fmt.Errorf("this is a source build (version %q) — self-update only works on a released binary; use `git pull && go build`, or install a release", current)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	if isHomebrewManaged(exe) {
		fmt.Fprintln(stdout, "mora was installed via Homebrew. Update it with:")
		fmt.Fprintln(stdout, "  brew upgrade pyranthus-hq/tap/mora")
		return nil
	}

	// A token lets self-update read the (currently private) repo's releases.
	token := firstNonEmpty(os.Getenv("MORA_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token})
	if err != nil {
		return fmt.Errorf("setting up the release source: %w", err)
	}
	// Verify the downloaded archive against the release's published checksums.txt
	// (GoReleaser/build-release.sh emit it) before swapping the binary in — don't
	// trust TLS + the GitHub API alone.
	updater, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:    source,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
	})
	if err != nil {
		return fmt.Errorf("setting up the updater: %w", err)
	}

	repo := selfupdate.NewRepositorySlug(upgradeRepoOwner, upgradeRepoName)
	latest, found, err := updater.DetectLatest(ctx, repo)
	if err != nil {
		hint := ""
		if token == "" {
			hint = " (the repo is private — set GITHUB_TOKEN, e.g. `export GITHUB_TOKEN=$(gh auth token)`)"
		}
		return fmt.Errorf("checking for updates failed: %w%s", err, hint)
	}
	if !found {
		fmt.Fprintln(stdout, "no published releases found")
		return nil
	}

	if latest.LessOrEqual(current) {
		fmt.Fprintf(stdout, "mora is up to date (%s)\n", current)
		return nil
	}

	fmt.Fprintf(stdout, "update available: %s → %s\n", current, latest.Version())
	if *checkOnly {
		fmt.Fprintln(stdout, "run `mora upgrade` to install it")
		return nil
	}

	fmt.Fprintf(stdout, "downloading %s …\n", latest.AssetName)
	if err := updater.UpdateTo(ctx, latest, exe); err != nil {
		return fmt.Errorf("update failed (binary left unchanged): %w", err)
	}
	fmt.Fprintf(stdout, "✓ updated mora to %s\n", latest.Version())
	fmt.Fprintln(stdout, "  run `mora version` to confirm")
	return nil
}

// isHomebrewManaged reports whether the resolved binary path lives inside a
// Homebrew Cellar (formula) or Caskroom (cask). Homebrew symlinks its bin/
// entries into those, so we resolve symlinks before calling this — a binary that
// merely sits in /opt/homebrew/bin via `install.sh` is a real file there and is
// NOT flagged.
func isHomebrewManaged(resolvedExe string) bool {
	return strings.Contains(resolvedExe, "/Cellar/") ||
		strings.Contains(resolvedExe, "/Caskroom/")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
