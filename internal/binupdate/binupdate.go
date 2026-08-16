// Package binupdate owns standalone released-binary update mechanics and version/install classification.
package binupdate

import (
	"context"
	"fmt"
	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	RepoOwner = "pyranthus-hq"
	RepoName  = "mora"
)

type Route string

const (
	RouteApp      Route = "app"
	RouteHomebrew Route = "homebrew"
	RouteSource   Route = "source"
	RouteDirect   Route = "direct"
)

func Classify(exe, buildVersion string, isApp func(string) bool) Route {
	if isApp != nil && isApp(exe) {
		return RouteApp
	}
	if IsHomebrewManaged(exe) {
		return RouteHomebrew
	}
	if buildVersion == "" || buildVersion == "dev" {
		return RouteSource
	}
	if _, local := LocalBuildBase(buildVersion); local {
		return RouteSource
	}
	return RouteDirect
}

type Release struct {
	Version, AssetName string
	native             *selfupdate.Release
}
type Options struct {
	Current, Executable, Token, GOOS string
	CheckOnly                        bool
	Stdout                           io.Writer
	PostRebuild                      func(context.Context, string, io.Writer) error
	Detect                           func(context.Context) (Release, bool, error)
	Apply                            func(context.Context, Release, string) error
}
type Verdict int

const (
	// VerdictUpgrade: the latest release is genuinely newer — offer/install it.
	VerdictUpgrade Verdict = iota
	// VerdictUpToDate: the running binary already is (or is past) the latest release.
	VerdictUpToDate
	// VerdictLocalAhead: the running binary is a local git build at or ahead of
	// the latest release's tag — "upgrading" would be a downgrade; refuse.
	VerdictLocalAhead
)

func IsHomebrewManaged(resolvedExe string) bool {
	return strings.Contains(resolvedExe, "/Cellar/") ||
		strings.Contains(resolvedExe, "/Caskroom/")
}
func OldSavePath(exe, goos string) string {
	if goos != "windows" {
		return ""
	}
	// go-selfupdate already performs the Windows-safe swap by renaming the
	// running executable before moving the new file into place. Set OldSavePath
	// so the contract-visible backup is mora.exe.old instead of the library's
	// default hidden .mora.exe.old path.
	return filepath.Join(filepath.Dir(exe), filepath.Base(exe)+".old")
}
func Decide(current, latestVersion string) (verdict Verdict, isLocal bool, err error) {
	latest, err := semver.NewVersion(latestVersion)
	if err != nil {
		return 0, false, fmt.Errorf("parsing the latest release version %q: %w", latestVersion, err)
	}
	base, isLocal := LocalBuildBase(current)
	if isLocal {
		baseVersion, err := semver.NewVersion(base)
		if err != nil {
			return 0, true, fmt.Errorf("parsing the base tag %q of local build %q: %w", base, current, err)
		}
		if latest.Compare(baseVersion) <= 0 {
			return VerdictLocalAhead, true, nil
		}
		return VerdictUpgrade, true, nil
	}
	currentVersion, err := semver.NewVersion(current)
	if err != nil {
		return 0, false, fmt.Errorf("parsing the current version %q: %w", current, err)
	}
	if latest.Compare(currentVersion) <= 0 {
		return VerdictUpToDate, false, nil
	}
	return VerdictUpgrade, false, nil
}

var gitDescribeSuffixRe = regexp.MustCompile(`-\d+-g[0-9a-f]{4,64}$`)

func LocalBuildBase(version string) (base string, ok bool) {
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
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
func Run(ctx context.Context, o Options) error {
	current, exe, token, stdout, checkOnly := o.Current, o.Executable, o.Token, o.Stdout, o.CheckOnly
	detect, apply := o.Detect, o.Apply
	if detect == nil || apply == nil {
		source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token})
		if err != nil {
			return fmt.Errorf("setting up the release source: %w", err)
		}
		updater, err := selfupdate.NewUpdater(selfupdate.Config{Source: source, Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"}, OldSavePath: OldSavePath(exe, o.GOOS)})
		if err != nil {
			return fmt.Errorf("setting up the updater: %w", err)
		}
		repo := selfupdate.NewRepositorySlug(RepoOwner, RepoName)
		detect = func(ctx context.Context) (Release, bool, error) {
			r, found, err := updater.DetectLatest(ctx, repo)
			if err != nil || !found {
				return Release{}, found, err
			}
			return Release{Version: r.Version(), AssetName: r.AssetName, native: r}, true, nil
		}
		apply = func(ctx context.Context, r Release, path string) error { return updater.UpdateTo(ctx, r.native, path) }
	}
	latest, found, err := detect(ctx)
	if err != nil {
		return fmt.Errorf("checking for updates failed: %w", err)
	}
	if !found {
		fmt.Fprintln(stdout, "no published releases found")
		return nil
	}

	verdict, isLocalBuild, err := Decide(current, latest.Version)
	if err != nil {
		return err
	}
	switch verdict {
	case VerdictLocalAhead:
		fmt.Fprintf(stdout, "you are running a local build ahead of the latest release (%s > %s) — nothing to upgrade\n", current, latest.Version)
		return nil
	case VerdictUpToDate:
		fmt.Fprintf(stdout, "mora is up to date (%s)\n", current)
		return nil
	}

	fmt.Fprintf(stdout, "update available: %s → %s\n", current, latest.Version)
	if isLocalBuild {
		fmt.Fprintf(stdout, "note: this replaces your local source build (%s) with the released binary\n", current)
	}
	if checkOnly {
		fmt.Fprintln(stdout, "run `mora upgrade` to install it")
		return nil
	}

	fmt.Fprintf(stdout, "downloading %s …\n", latest.AssetName)
	if err := apply(ctx, latest, exe); err != nil {
		return fmt.Errorf("update failed (binary left unchanged): %w", err)
	}
	fmt.Fprintf(stdout, "✓ updated mora to %s\n", latest.Version)
	// Re-index with the NEW binary so a schema change never serves a stale
	// index (the new code refuses one with an error; this is the consented
	// slow moment to pay the rebuild). Warn-don't-fail: the swap already
	// succeeded, and the index error message names the same fix.
	if err := o.PostRebuild(ctx, exe, stdout); err != nil {
		fmt.Fprintf(stdout, "warning: index rebuild failed: %v\n", err)
		fmt.Fprintln(stdout, "  finish the upgrade with: mora index rebuild")
	}
	fmt.Fprintln(stdout, "  run `mora version` to confirm")
	return nil
}
