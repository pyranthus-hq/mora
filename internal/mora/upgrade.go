package mora

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	binupdatepkg "github.com/pyranthus-hq/mora/internal/binupdate"
)

// upgradeRepoOwner / upgradeRepoName point self-update at the release source.
const (
	upgradeRepoOwner = binupdatepkg.RepoOwner
	upgradeRepoName  = binupdatepkg.RepoName
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
	r := binupdatepkg.Classify(exe, BuildVersion, func(path string) bool { _, ok := moraAppRoot(path); return ok })
	return upgradeInstallRoute(r)
}

// cmdUpgrade implements `mora upgrade [--check]`: in-place self-update from the
// latest GitHub release, mirroring how Claude Code keeps itself current. Homebrew
// installs are deferred to `brew upgrade`; source/dev builds are refused.
func cmdUpgrade(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
		return cmdUpgradeApp(ctx, current, appRoot, *checkOnly, token, stdout, stderr)
	case upgradeRouteHomebrew:
		fmt.Fprintln(stdout, "mora was installed via Homebrew. Update it with:")
		fmt.Fprintln(stdout, "  brew upgrade pyranthus-hq/tap/mora")
		return nil
	case upgradeRouteSource:
		return fmt.Errorf("this is a source build (version %q) — self-update is disabled; use `git pull && go build`, or install a release", current)
	}
	return binupdatepkg.Run(ctx, binupdatepkg.Options{Current: current, Executable: exe, Token: token, GOOS: runtimeGOOS(), CheckOnly: *checkOnly, Stdout: stdout, Stderr: stderr, PostRebuild: postUpgradeRebuild})
}

// postUpgradeRebuild rebuilds the search index by exec-ing the freshly
// swapped-in binary — the running process is still the OLD code, and schema
// knowledge (indexSchemaVersion, table shapes) lives in the new executable.
func isHomebrewManaged(path string) bool { return binupdatepkg.IsHomebrewManaged(path) }

type upgradeVerdict = binupdatepkg.Verdict

const (
	verdictUpgrade    = binupdatepkg.VerdictUpgrade
	verdictUpToDate   = binupdatepkg.VerdictUpToDate
	verdictLocalAhead = binupdatepkg.VerdictLocalAhead
)

func decideUpgrade(current, latest string) (upgradeVerdict, bool, error) {
	return binupdatepkg.Decide(current, latest)
}
func localBuildBase(v string) (string, bool) { return binupdatepkg.LocalBuildBase(v) }
func firstNonEmpty(v ...string) string       { return binupdatepkg.FirstNonEmpty(v...) }

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

// upgradeVerdict classifies what `mora upgrade` should do given the running
// version and the latest published release.

// decideUpgrade compares the running version against the latest release.
//
// A git-describe version like "v0.10.0-60-g2d08334" parses as a PRERELEASE of
// 0.10.0 under semver, so naive comparison sorts it BELOW the v0.10.0 release
// and `mora upgrade` used to offer that release as a downgrade. Instead, local
// builds are compared by the tag they were built past: at-or-ahead of the
// latest release refuses; genuinely behind upgrades as usual. isLocal reports
// that current is such a local build so callers can flag the binary swap.

// gitDescribeSuffixRe matches the "-<commits-ahead>-g<sha>" suffix that
// `git describe --tags` appends when HEAD is past the nearest tag. The sha
// spans git's minimum abbreviation (4) up to a full SHA-256 (64) so an
// unmatched long hash can never fall through to prerelease comparison and
// resurrect the downgrade bug.
// localBuildBase extracts the release tag a git-describe build version was cut
// from: "v0.10.0-60-g2d08334", "v0.10.0-60-g2d08334-dirty", and "v0.10.0-dirty"
// all yield ("v0.10.0", true). Clean release versions (including prerelease
// tags like "v0.10.0-rc1") yield ("", false).
