package mora

import (
	"context"
	"github.com/creativeprojects/go-selfupdate"
	appupdatepkg "github.com/pyranthus-hq/mora/internal/appupdate"
	"io"
	"runtime"
)

const (
	moraAppName             = appupdatepkg.AppName
	moraAppChecksumFilename = appupdatepkg.ChecksumFilename
	maxMoraAppArchiveBytes  = appupdatepkg.MaxArchiveBytes
	maxChecksumBytes        = appupdatepkg.MaxChecksumBytes
)

type appReleaseCandidate struct {
	version, assetName, assetURL string
	assetSize                    int
	checksumURL                  string
	checksumSize                 int
}

func moraAppRoot(exe string) (string, bool) { return appupdatepkg.Root(exe) }
func moraAppAssetName(version, arch string) (string, error) {
	return appupdatepkg.AssetName(version, arch)
}
func cmdUpgradeApp(ctx context.Context, current, root string, check bool, token string, out io.Writer) error {
	return appupdatepkg.Run(ctx, appupdatepkg.Options{CurrentVersion: current, AppRoot: root, CheckOnly: check, Token: token, Stdout: out, GOOS: runtimeGOOS(), Arch: runtime.GOARCH, RepoOwner: upgradeRepoOwner, RepoName: upgradeRepoName, Decide: func(current, latest string) (appupdatepkg.Decision, bool, error) {
		v, local, err := decideUpgrade(current, latest)
		if err != nil {
			return "", local, err
		}
		switch v {
		case verdictLocalAhead:
			return appupdatepkg.DecisionLocalAhead, local, nil
		case verdictUpToDate:
			return appupdatepkg.DecisionUpToDate, local, nil
		default:
			return appupdatepkg.DecisionUpgrade, local, nil
		}
	}, PostRebuild: postUpgradeRebuild})
}
func detectLatestAppRelease(ctx context.Context, source selfupdate.Source, arch string) (appReleaseCandidate, bool, error) {
	c, ok, err := appupdatepkg.DetectLatest(ctx, source, arch, upgradeRepoOwner, upgradeRepoName)
	return appReleaseCandidate{version: c.Version(), assetName: c.AssetName(), assetURL: c.AssetURL(), assetSize: c.AssetSize(), checksumURL: c.ChecksumURL(), checksumSize: c.ChecksumSize()}, ok, err
}

var newAppReleaseSource = func(token string) (selfupdate.Source, error) {
	return selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token})
}
var downloadAppReleaseFile = appupdatepkg.Download

func verifyAppArchiveChecksum(archive, manifest, asset string) error {
	return appupdatepkg.VerifyArchiveChecksum(archive, manifest, asset)
}
func extractMoraAppArchive(ctx context.Context, archive, dest string) (string, error) {
	return appupdatepkg.ExtractArchive(ctx, archive, dest)
}
func verifyMoraAppBundle(ctx context.Context, root, version, arch string) error {
	return appupdatepkg.VerifyBundle(ctx, root, version, arch)
}
func atomicSwapMoraAppDirectories(installed, staged string) error {
	return appupdatepkg.AtomicSwap(installed, staged)
}
