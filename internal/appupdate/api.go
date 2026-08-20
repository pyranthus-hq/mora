package appupdate

import (
	"context"
	"github.com/creativeprojects/go-selfupdate"
)

const (
	AppName          = moraAppName
	ChecksumFilename = moraAppChecksumFilename
	MaxArchiveBytes  = maxMoraAppArchiveBytes
	MaxChecksumBytes = maxChecksumBytes
)

type Candidate = appReleaseCandidate

func (c Candidate) Version() string                  { return c.version }
func (c Candidate) AssetName() string                { return c.assetName }
func (c Candidate) AssetURL() string                 { return c.assetURL }
func (c Candidate) AssetSize() int                   { return c.assetSize }
func (c Candidate) ChecksumURL() string              { return c.checksumURL }
func (c Candidate) ChecksumSize() int                { return c.checksumSize }
func Root(executable string) (string, bool)          { return moraAppRoot(executable) }
func AssetName(version, arch string) (string, error) { return moraAppAssetName(version, arch) }
func DetectLatest(ctx context.Context, source selfupdate.Source, arch, owner, repo string) (Candidate, bool, error) {
	return detectLatestAppReleaseForRepo(ctx, source, arch, owner, repo)
}
func Download(ctx context.Context, rawURL, token, dest string, maxBytes int64) error {
	return downloadReleaseFile(ctx, rawURL, token, dest, maxBytes)
}
func VerifyArchiveChecksum(archive, manifest, asset string) error {
	return verifyAppArchiveChecksum(archive, manifest, asset)
}
func ExtractArchive(ctx context.Context, archive, dest string) (string, error) {
	return extractMoraAppArchive(ctx, archive, dest)
}
func VerifyBundle(ctx context.Context, root, version, arch string) error {
	return verifyMoraAppBundle(ctx, root, version, arch)
}
func AtomicSwap(installed, staged string) error {
	return atomicSwapMoraAppDirectories(installed, staged)
}
func ReplaceBundle(ctx context.Context, installed, staged, version, arch string) error {
	return replaceMoraAppBundle(ctx, installed, staged, version, arch)
}
