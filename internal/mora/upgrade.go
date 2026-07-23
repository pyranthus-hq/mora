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

	"github.com/Masterminds/semver/v3"
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
	// before swapping the binary in — don't trust TLS + the GitHub API alone.
	//
	// CONTRACT: every release MUST carry a checksum asset named exactly
	// "checksums.txt" (goreleaser's checksum.name_template AND scripts/
	// package.sh both emit it). v0.6.0 shipped only SHA256SUMS and silently
	// broke `mora upgrade` for every install; if you rename one side, rename
	// all three.
	updater, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:      source,
		Validator:   &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
		OldSavePath: upgradeOldSavePath(exe),
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

	verdict, isLocalBuild, err := decideUpgrade(current, latest.Version())
	if err != nil {
		return err
	}
	switch verdict {
	case verdictLocalAhead:
		fmt.Fprintf(stdout, "you are running a local build ahead of the latest release (%s > %s) — nothing to upgrade\n", current, latest.Version())
		return nil
	case verdictUpToDate:
		fmt.Fprintf(stdout, "mora is up to date (%s)\n", current)
		return nil
	}

	fmt.Fprintf(stdout, "update available: %s → %s\n", current, latest.Version())
	if isLocalBuild {
		fmt.Fprintf(stdout, "note: this replaces your local source build (%s) with the released binary\n", current)
	}
	if *checkOnly {
		fmt.Fprintln(stdout, "run `mora upgrade` to install it")
		return nil
	}

	fmt.Fprintf(stdout, "downloading %s …\n", latest.AssetName)
	if err := updater.UpdateTo(ctx, latest, exe); err != nil {
		return fmt.Errorf("update failed (binary left unchanged): %w", err)
	}
	fmt.Fprintf(stdout, "✓ updated mora to %s\n", latest.Version())
	// Re-index with the NEW binary so a schema change never serves a stale
	// index (the new code refuses one with an error; this is the consented
	// slow moment to pay the rebuild). Warn-don't-fail: the swap already
	// succeeded, and the index error message names the same fix.
	if err := postUpgradeRebuild(ctx, exe, stdout); err != nil {
		fmt.Fprintf(stdout, "warning: index rebuild failed: %v\n", err)
		fmt.Fprintln(stdout, "  finish the upgrade with: mora index rebuild")
	}
	fmt.Fprintln(stdout, "  run `mora version` to confirm")
	return nil
}

// postUpgradeRebuild rebuilds the search index by exec-ing the freshly
// swapped-in binary — the running process is still the OLD code, and schema
// knowledge (indexSchemaVersion, table shapes) lives in the new executable.
func postUpgradeRebuild(ctx context.Context, exe string, stdout io.Writer) error {
	fmt.Fprintln(stdout, "rebuilding the search index for the new version …")
	cmd := exec.CommandContext(ctx, exe, "index", "rebuild")
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	return cmd.Run()
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

func upgradeOldSavePath(exe string) string {
	if runtimeGOOS() != "windows" {
		return ""
	}
	// go-selfupdate already performs the Windows-safe swap by renaming the
	// running executable before moving the new file into place. Set OldSavePath
	// so the contract-visible backup is mora.exe.old instead of the library's
	// default hidden .mora.exe.old path.
	return filepath.Join(filepath.Dir(exe), filepath.Base(exe)+".old")
}

// upgradeVerdict classifies what `mora upgrade` should do given the running
// version and the latest published release.
type upgradeVerdict int

const (
	// verdictUpgrade: the latest release is genuinely newer — offer/install it.
	verdictUpgrade upgradeVerdict = iota
	// verdictUpToDate: the running binary already is (or is past) the latest release.
	verdictUpToDate
	// verdictLocalAhead: the running binary is a local git build at or ahead of
	// the latest release's tag — "upgrading" would be a downgrade; refuse.
	verdictLocalAhead
)

// decideUpgrade compares the running version against the latest release.
//
// A git-describe version like "v0.10.0-60-g2d08334" parses as a PRERELEASE of
// 0.10.0 under semver, so naive comparison sorts it BELOW the v0.10.0 release
// and `mora upgrade` used to offer that release as a downgrade. Instead, local
// builds are compared by the tag they were built past: at-or-ahead of the
// latest release refuses; genuinely behind upgrades as usual. isLocal reports
// that current is such a local build so callers can flag the binary swap.
func decideUpgrade(current, latestVersion string) (verdict upgradeVerdict, isLocal bool, err error) {
	latest, err := semver.NewVersion(latestVersion)
	if err != nil {
		return 0, false, fmt.Errorf("parsing the latest release version %q: %w", latestVersion, err)
	}
	base, isLocal := localBuildBase(current)
	if isLocal {
		baseVersion, err := semver.NewVersion(base)
		if err != nil {
			return 0, true, fmt.Errorf("parsing the base tag %q of local build %q: %w", base, current, err)
		}
		if latest.Compare(baseVersion) <= 0 {
			return verdictLocalAhead, true, nil
		}
		return verdictUpgrade, true, nil
	}
	currentVersion, err := semver.NewVersion(current)
	if err != nil {
		return 0, false, fmt.Errorf("parsing the current version %q: %w", current, err)
	}
	if latest.Compare(currentVersion) <= 0 {
		return verdictUpToDate, false, nil
	}
	return verdictUpgrade, false, nil
}

// gitDescribeSuffixRe matches the "-<commits-ahead>-g<sha>" suffix that
// `git describe --tags` appends when HEAD is past the nearest tag.
var gitDescribeSuffixRe = regexp.MustCompile(`-\d+-g[0-9a-f]{4,40}$`)

// localBuildBase extracts the release tag a git-describe build version was cut
// from: "v0.10.0-60-g2d08334", "v0.10.0-60-g2d08334-dirty", and "v0.10.0-dirty"
// all yield ("v0.10.0", true). Clean release versions (including prerelease
// tags like "v0.10.0-rc1") yield ("", false).
func localBuildBase(version string) (base string, ok bool) {
	trimmed := strings.TrimSuffix(version, "-dirty")
	dirty := trimmed != version
	if loc := gitDescribeSuffixRe.FindStringIndex(trimmed); loc != nil {
		return trimmed[:loc[0]], true
	}
	if dirty && trimmed != "" {
		return trimmed, true
	}
	return "", false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
