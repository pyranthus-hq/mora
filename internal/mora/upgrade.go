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

var upgradeExecutable = os.Executable

// upgradeInstallRoute selects the safe owner for an executable. App paths win
// over Homebrew path fragments: a Caskroom-linked Mora.app is still the signed
// whole-bundle update target, never a raw Homebrew CLI mutation target.
type upgradeInstallRoute string

const (
	upgradeRouteApp      upgradeInstallRoute = "app"
	upgradeRouteHomebrew upgradeInstallRoute = "homebrew"
	upgradeRouteSource   upgradeInstallRoute = "source"
	upgradeRouteDirect   upgradeInstallRoute = "direct"
)

func classifyUpgradeInstall(exe string) upgradeInstallRoute {
	if _, ok := moraAppRoot(exe); ok {
		return upgradeRouteApp
	}
	if isHomebrewManaged(exe) {
		return upgradeRouteHomebrew
	}
	if BuildVersion == "" || BuildVersion == "dev" {
		return upgradeRouteSource
	}
	if _, local := localBuildBase(BuildVersion); local {
		return upgradeRouteSource
	}
	return upgradeRouteDirect
}

// cmdUpgrade implements `mora upgrade [--check]`: in-place self-update from the
// latest GitHub release, mirroring how Claude Code keeps itself current. Homebrew
// installs are deferred to `brew upgrade`; source/dev builds are refused.
func cmdUpgrade(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	checkOnly := fs.Bool("check", false, "only report whether an update is available; don't install")
	policyFlag := fs.String("policy", "", "set automatic update policy: auto, notify, or off")
	statusOnly := fs.Bool("status", false, "show cached update policy and check status")
	jsonOut := fs.Bool("json", false, "emit machine-readable status (requires --status)")
	scheduledCheck := fs.Bool("scheduled-check", false, "internal check-and-notify seam; never installs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: mora upgrade [--check | --policy auto|notify|off | --status [--json] | --scheduled-check]")
	}
	selected := 0
	for _, on := range []bool{*checkOnly, *policyFlag != "", *statusOnly, *scheduledCheck} {
		if on {
			selected++
		}
	}
	if selected > 1 || (*jsonOut && !*statusOnly) {
		return fmt.Errorf("usage: mora upgrade [--check | --policy auto|notify|off | --status [--json] | --scheduled-check]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *policyFlag != "" {
		policy, err := parseUpdatePolicy(*policyFlag)
		if err != nil {
			return err
		}
		cfg.UpdatePolicy = string(policy)
		if err := writeConfig(cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "update policy = %s\n", policy)
		if policy == updatePolicyAuto {
			fmt.Fprintln(stdout, "verified automatic apply is enabled for the internal scheduled-check seam; no update schedule is installed yet")
		}
		return nil
	}
	if *statusOnly {
		return cmdUpgradeStatus(cfg, *jsonOut, stdout)
	}
	if *scheduledCheck {
		return runUpdateCheck(ctx, cfg, true, stdout)
	}

	current := BuildVersion
	if current == "dev" || current == "" {
		return fmt.Errorf("this is a source build (version %q) — self-update only works on a released binary; use `git pull && go build`, or install a release", current)
	}

	exe, err := upgradeExecutable()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// The repo is public, so no token is required; an optional token raises
	// GitHub API rate limits.
	token := firstNonEmpty(os.Getenv("MORA_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	switch classifyUpgradeInstall(exe) {
	case upgradeRouteApp:
		appRoot, _ := moraAppRoot(exe)
		return cmdUpgradeApp(ctx, current, appRoot, *checkOnly, token, stdout)
	case upgradeRouteHomebrew:
		fmt.Fprintln(stdout, "mora was installed via Homebrew. Update it with:")
		fmt.Fprintln(stdout, "  brew upgrade pyranthus-hq/tap/mora")
		return nil
	case upgradeRouteSource:
		return fmt.Errorf("this is a source build (version %q) — self-update is disabled; use `git pull && go build`, or install a release", current)
	}
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
		return fmt.Errorf("checking for updates failed: %w", err)
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
// `git describe --tags` appends when HEAD is past the nearest tag. The sha
// spans git's minimum abbreviation (4) up to a full SHA-256 (64) so an
// unmatched long hash can never fall through to prerelease comparison and
// resurrect the downgrade bug.
var gitDescribeSuffixRe = regexp.MustCompile(`-\d+-g[0-9a-f]{4,64}$`)

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
