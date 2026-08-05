package mora

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
)

const (
	moraAppName             = "Mora.app"
	moraAppBundleID         = "com.pyranthus.mora"
	moraAppleTeamID         = "VS8M5VJBZ5"
	moraAppChecksumFilename = "checksums-app.txt"
	maxMoraAppArchiveBytes  = int64(512 << 20)
	maxChecksumBytes        = int64(1 << 20)
)

type appReleaseCandidate struct {
	version      string
	assetName    string
	assetURL     string
	assetSize    int
	checksumURL  string
	checksumSize int
}

type moraAppRollbackFailure struct {
	verificationErr error
	rollbackErr     error
	recoveryPath    string
}

func (e *moraAppRollbackFailure) Error() string {
	return fmt.Sprintf(
		"new app failed post-swap verification (%v) and rollback failed (%v); previous Mora.app preserved for manual recovery at %s",
		e.verificationErr,
		e.rollbackErr,
		e.recoveryPath,
	)
}

func (e *moraAppRollbackFailure) Unwrap() error {
	return e.rollbackErr
}

var (
	newAppReleaseSource = func(token string) (selfupdate.Source, error) {
		return selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token})
	}
	downloadAppReleaseFile = downloadReleaseFile
	expandMoraAppArchive   = dittoExpandMoraAppArchive
	verifyStagedMoraApp    = verifyMoraAppBundle
	swapMoraAppDirectories = atomicSwapMoraAppDirectories
	appCommandOutput       = combinedCommandOutput
	postAppUpgradeRebuild  = postUpgradeRebuild
)

// moraAppRoot recognizes only Mora's frozen application layout. The caller
// resolves symlinks first, so a PATH shim into the app takes this branch while
// the standalone compatibility binary keeps using go-selfupdate.
func moraAppRoot(executable string) (string, bool) {
	clean := filepath.Clean(executable)
	if filepath.Base(clean) != "mora" {
		return "", false
	}
	macOSDir := filepath.Dir(clean)
	if filepath.Base(macOSDir) != "MacOS" {
		return "", false
	}
	contentsDir := filepath.Dir(macOSDir)
	if filepath.Base(contentsDir) != "Contents" {
		return "", false
	}
	appRoot := filepath.Dir(contentsDir)
	if filepath.Base(appRoot) != moraAppName {
		return "", false
	}
	return appRoot, true
}

func moraAppAssetName(version, arch string) (string, error) {
	parsed, err := semver.NewVersion(version)
	if err != nil {
		return "", fmt.Errorf("parsing app release version %q: %w", version, err)
	}
	switch arch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported macOS app architecture: %s", arch)
	}
	return fmt.Sprintf("mora_%s_darwin_%s_app.zip", parsed.String(), arch), nil
}

func cmdUpgradeApp(ctx context.Context, current, appRoot string, checkOnly bool, token string, stdout io.Writer) error {
	if runtimeGOOS() != "darwin" {
		return fmt.Errorf("Mora.app whole-bundle updates require macOS")
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported macOS app architecture: %s", arch)
	}
	currentVersion, err := semver.NewVersion(current)
	if err != nil {
		return fmt.Errorf("parsing installed app version %q: %w", current, err)
	}
	if err := verifyStagedMoraApp(ctx, appRoot, currentVersion.String(), arch); err != nil {
		return fmt.Errorf("installed Mora.app failed verification; refusing to update a broken bundle: %w", err)
	}

	source, err := newAppReleaseSource(token)
	if err != nil {
		return fmt.Errorf("setting up the app release source: %w", err)
	}
	candidate, found, err := detectLatestAppRelease(ctx, source, arch)
	if err != nil {
		return fmt.Errorf("checking for app updates failed: %w", err)
	}
	if !found {
		fmt.Fprintln(stdout, "no published app releases found")
		return nil
	}

	verdict, isLocalBuild, err := decideUpgrade(current, candidate.version)
	if err != nil {
		return err
	}
	switch verdict {
	case verdictLocalAhead:
		fmt.Fprintf(stdout, "you are running a local build ahead of the latest app release (%s > %s) — nothing to upgrade\n", current, candidate.version)
		return nil
	case verdictUpToDate:
		fmt.Fprintf(stdout, "mora is up to date (%s)\n", current)
		return nil
	}

	fmt.Fprintf(stdout, "update available: %s → %s\n", current, candidate.version)
	if isLocalBuild {
		fmt.Fprintf(stdout, "note: this replaces your local app build (%s) with the released app bundle\n", current)
	}
	if checkOnly {
		fmt.Fprintln(stdout, "run `mora upgrade` to install it")
		return nil
	}

	parent := filepath.Dir(appRoot)
	stageDir, err := os.MkdirTemp(parent, ".mora-app-upgrade.")
	if err != nil {
		return fmt.Errorf("creating same-volume app staging directory: %w", err)
	}
	preserveStage := false
	defer func() {
		if preserveStage {
			return
		}
		if err := os.RemoveAll(stageDir); err != nil {
			fmt.Fprintf(stdout, "warning: could not remove app upgrade staging directory %s: %v\n", stageDir, err)
		}
	}()

	archivePath := filepath.Join(stageDir, candidate.assetName)
	checksumPath := filepath.Join(stageDir, moraAppChecksumFilename)
	fmt.Fprintf(stdout, "downloading %s …\n", candidate.assetName)
	if err := downloadAppReleaseFile(ctx, candidate.assetURL, token, archivePath, maxMoraAppArchiveBytes); err != nil {
		return fmt.Errorf("downloading app release: %w", err)
	}
	if err := downloadAppReleaseFile(ctx, candidate.checksumURL, token, checksumPath, maxChecksumBytes); err != nil {
		return fmt.Errorf("downloading %s: %w", moraAppChecksumFilename, err)
	}
	if err := verifyAppArchiveChecksum(archivePath, checksumPath, candidate.assetName); err != nil {
		return err
	}

	extractDir := filepath.Join(stageDir, "expanded")
	if err := os.Mkdir(extractDir, 0o700); err != nil {
		return fmt.Errorf("creating app extraction directory: %w", err)
	}
	stagedApp, err := extractMoraAppArchive(ctx, archivePath, extractDir)
	if err != nil {
		return fmt.Errorf("extracting app release: %w", err)
	}
	if err := verifyStagedMoraApp(ctx, stagedApp, candidate.version, arch); err != nil {
		return fmt.Errorf("staged Mora.app failed verification: %w", err)
	}
	if err := replaceMoraAppBundle(ctx, appRoot, stagedApp, candidate.version, arch); err != nil {
		var rollbackFailure *moraAppRollbackFailure
		if errors.As(err, &rollbackFailure) {
			preserveStage = true
			fmt.Fprintf(stdout, "warning: preserving the previous Mora.app for manual recovery at %s\n", rollbackFailure.recoveryPath)
		}
		return fmt.Errorf("whole-bundle update failed: %w", err)
	}

	fmt.Fprintf(stdout, "✓ updated Mora.app to %s\n", candidate.version)
	newExecutable := filepath.Join(appRoot, "Contents", "MacOS", "mora")
	if err := postAppUpgradeRebuild(ctx, newExecutable, stdout); err != nil {
		fmt.Fprintf(stdout, "warning: index rebuild failed: %v\n", err)
		fmt.Fprintln(stdout, "  finish the upgrade with: mora index rebuild")
	}
	fmt.Fprintln(stdout, "  run `mora version` to confirm")
	return nil
}

func detectLatestAppRelease(ctx context.Context, source selfupdate.Source, arch string) (appReleaseCandidate, bool, error) {
	releases, err := source.ListReleases(ctx, selfupdate.NewRepositorySlug(upgradeRepoOwner, upgradeRepoName))
	if err != nil {
		return appReleaseCandidate{}, false, err
	}
	type parsedRelease struct {
		release selfupdate.SourceRelease
		version *semver.Version
	}
	parsed := make([]parsedRelease, 0, len(releases))
	for _, release := range releases {
		if release.GetDraft() || release.GetPrerelease() {
			continue
		}
		version, err := semver.NewVersion(release.GetTagName())
		if err != nil {
			continue
		}
		parsed = append(parsed, parsedRelease{release: release, version: version})
	}
	if len(parsed) == 0 {
		return appReleaseCandidate{}, false, nil
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].version.GreaterThan(parsed[j].version) })
	latest := parsed[0]
	assetName, err := moraAppAssetName(latest.version.String(), arch)
	if err != nil {
		return appReleaseCandidate{}, false, err
	}
	var appAssets, checksumAssets []selfupdate.SourceAsset
	for _, asset := range latest.release.GetAssets() {
		switch asset.GetName() {
		case assetName:
			appAssets = append(appAssets, asset)
		case moraAppChecksumFilename:
			checksumAssets = append(checksumAssets, asset)
		}
	}
	if len(appAssets) != 1 {
		return appReleaseCandidate{}, false, fmt.Errorf("latest release %s must contain exactly one %s asset, found %d", latest.release.GetTagName(), assetName, len(appAssets))
	}
	if len(checksumAssets) != 1 {
		return appReleaseCandidate{}, false, fmt.Errorf("latest release %s must contain exactly one %s asset, found %d", latest.release.GetTagName(), moraAppChecksumFilename, len(checksumAssets))
	}
	appAsset := appAssets[0]
	checksumAsset := checksumAssets[0]
	if appAsset.GetSize() <= 0 || int64(appAsset.GetSize()) > maxMoraAppArchiveBytes {
		return appReleaseCandidate{}, false, fmt.Errorf("app asset has unsafe declared size: %d", appAsset.GetSize())
	}
	if checksumAsset.GetSize() <= 0 || int64(checksumAsset.GetSize()) > maxChecksumBytes {
		return appReleaseCandidate{}, false, fmt.Errorf("app checksum asset has unsafe declared size: %d", checksumAsset.GetSize())
	}
	if err := validateGitHubReleaseURL(appAsset.GetBrowserDownloadURL()); err != nil {
		return appReleaseCandidate{}, false, fmt.Errorf("invalid app asset URL: %w", err)
	}
	if err := validateGitHubReleaseURL(checksumAsset.GetBrowserDownloadURL()); err != nil {
		return appReleaseCandidate{}, false, fmt.Errorf("invalid app checksum URL: %w", err)
	}
	return appReleaseCandidate{
		version:      latest.version.String(),
		assetName:    assetName,
		assetURL:     appAsset.GetBrowserDownloadURL(),
		assetSize:    appAsset.GetSize(),
		checksumURL:  checksumAsset.GetBrowserDownloadURL(),
		checksumSize: checksumAsset.GetSize(),
	}, true, nil
}

func validateGitHubReleaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.User != nil || u.Hostname() != "github.com" {
		return fmt.Errorf("expected an https://github.com release URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("release URL must not contain a query or fragment")
	}
	return nil
}

func allowedReleaseRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "github.com" || host == "objects.githubusercontent.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

func downloadReleaseFile(ctx context.Context, rawURL, token, destination string, maxBytes int64) error {
	if err := validateGitHubReleaseURL(rawURL); err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || !allowedReleaseRedirect(req.URL.String()) {
				return fmt.Errorf("refusing unsafe release redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub returned %s", resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("download is too large: %d bytes", resp.ContentLength)
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	written, err := io.Copy(out, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if written > maxBytes {
		return fmt.Errorf("download exceeded %d bytes", maxBytes)
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func verifyAppArchiveChecksum(archivePath, manifestPath, expectedName string) error {
	manifest, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", moraAppChecksumFilename, err)
	}
	defer manifest.Close()
	var expected string
	scanner := bufio.NewScanner(io.LimitReader(manifest, maxChecksumBytes+1))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return fmt.Errorf("%s has an invalid line", moraAppChecksumFilename)
		}
		if fields[1] != expectedName {
			continue
		}
		if expected != "" {
			return fmt.Errorf("%s has duplicate entries for %s", moraAppChecksumFilename, expectedName)
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("%s has an invalid SHA-256 for %s", moraAppChecksumFilename, expectedName)
		}
		expected = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", moraAppChecksumFilename, err)
	}
	if expected == "" {
		return fmt.Errorf("%s has no entry for %s", moraAppChecksumFilename, expectedName)
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening app archive: %w", err)
	}
	defer archive.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, archive); err != nil {
		return fmt.Errorf("hashing app archive: %w", err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("CHECKSUM MISMATCH for %s (expected %s, got %s)", expectedName, expected, actual)
	}
	return nil
}

func extractMoraAppArchive(ctx context.Context, archivePath, destination string) (string, error) {
	if err := preflightMoraAppZip(archivePath); err != nil {
		return "", err
	}
	if err := expandMoraAppArchive(ctx, archivePath, destination); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() != moraAppName && entry.Name() != "__MACOSX" {
			return "", fmt.Errorf("archive produced unexpected top-level entry %q", entry.Name())
		}
	}
	appRoot := filepath.Join(destination, moraAppName)
	if err := validateMoraAppLayout(appRoot); err != nil {
		return "", err
	}
	return appRoot, nil
}

func preflightMoraAppZip(archivePath string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("opening app zip: %w", err)
	}
	defer reader.Close()
	seen := make(map[string]struct{}, len(reader.File))
	var total uint64
	required := map[string]bool{
		"Mora.app/Contents/Info.plist":          false,
		"Mora.app/Contents/MacOS/mora":          false,
		"Mora.app/Contents/Resources/Mora.icns": false,
	}
	for _, file := range reader.File {
		name := file.Name
		if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
			return fmt.Errorf("app zip contains unsafe path %q", name)
		}
		clean := path.Clean(name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
			return fmt.Errorf("app zip contains non-canonical path %q", name)
		}
		allowed := clean == moraAppName || strings.HasPrefix(clean, moraAppName+"/") ||
			clean == "__MACOSX" || strings.HasPrefix(clean, "__MACOSX/")
		if !allowed {
			return fmt.Errorf("app zip contains unexpected root path %q", name)
		}
		key := strings.ToLower(clean)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("app zip contains duplicate path %q", name)
		}
		seen[key] = struct{}{}
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) {
			return fmt.Errorf("app zip contains unsafe file type at %q", name)
		}
		total += file.UncompressedSize64
		if total > uint64(maxMoraAppArchiveBytes) {
			return fmt.Errorf("expanded app zip exceeds %d bytes", maxMoraAppArchiveBytes)
		}
		if _, ok := required[clean]; ok {
			required[clean] = true
		}
	}
	for name, present := range required {
		if !present {
			return fmt.Errorf("app zip is missing %s", name)
		}
	}
	return nil
}

func dittoExpandMoraAppArchive(ctx context.Context, archivePath, destination string) error {
	output, err := appCommandOutput(ctx, "/usr/bin/ditto", "-x", "-k", archivePath, destination)
	if err != nil {
		return fmt.Errorf("ditto extraction failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func validateMoraAppLayout(appRoot string) error {
	info, err := os.Lstat(appRoot)
	if err != nil {
		return fmt.Errorf("reading Mora.app: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || filepath.Base(appRoot) != moraAppName {
		return fmt.Errorf("Mora.app root must be a real directory named %s", moraAppName)
	}
	requiredFiles := []string{
		filepath.Join(appRoot, "Contents", "Info.plist"),
		filepath.Join(appRoot, "Contents", "MacOS", "mora"),
		filepath.Join(appRoot, "Contents", "Resources", "Mora.icns"),
	}
	for _, file := range requiredFiles {
		info, err := os.Lstat(file)
		if err != nil {
			return fmt.Errorf("required app file missing: %s", file)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("required app file is not a regular file: %s", file)
		}
	}
	binary := requiredFiles[1]
	info, err = os.Stat(binary)
	if err != nil {
		return fmt.Errorf("reading app executable: %w", err)
	}
	// Windows does not preserve POSIX executable bits. The whole-bundle
	// updater is Darwin-only, so enforce this bit where the bundle can run
	// while keeping the portable layout checks testable on Windows.
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return fmt.Errorf("app executable is not executable: %s", binary)
	}
	return filepath.WalkDir(appRoot, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Mora.app contains a symlink: %s", file)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("Mora.app contains an irregular file: %s", file)
		}
		return nil
	})
}

func verifyMoraAppBundle(ctx context.Context, appRoot, expectedVersion, expectedArch string) error {
	if err := validateMoraAppLayout(appRoot); err != nil {
		return err
	}
	binary := filepath.Join(appRoot, "Contents", "MacOS", "mora")
	plist := filepath.Join(appRoot, "Contents", "Info.plist")
	plistExpectations := map[string]string{
		"CFBundleIdentifier":         moraAppBundleID,
		"CFBundleExecutable":         "mora",
		"CFBundleName":               "Mora",
		"CFBundleDisplayName":        "Mora",
		"CFBundleIconFile":           "Mora",
		"CFBundlePackageType":        "APPL",
		"CFBundleShortVersionString": expectedVersion,
		"CFBundleVersion":            expectedVersion,
		"LSUIElement":                "true",
	}
	for key, expected := range plistExpectations {
		output, err := appCommandOutput(ctx, "/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist)
		if err != nil {
			return fmt.Errorf("reading %s from Info.plist: %w: %s", key, err, strings.TrimSpace(string(output)))
		}
		if actual := strings.TrimSpace(string(output)); actual != expected {
			return fmt.Errorf("Info.plist %s = %q, want %q", key, actual, expected)
		}
	}
	if output, err := appCommandOutput(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", appRoot); err != nil {
		return fmt.Errorf("app signature verification failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for _, target := range []string{appRoot, binary} {
		metadata, err := appCommandOutput(ctx, "/usr/bin/codesign", "--display", "--verbose=4", target)
		if err != nil {
			return fmt.Errorf("reading code-signing metadata for %s: %w", target, err)
		}
		if err := validateMoraSigningMetadata(string(metadata)); err != nil {
			return fmt.Errorf("invalid code-signing metadata for %s: %w", target, err)
		}
		requirement, err := appCommandOutput(ctx, "/usr/bin/codesign", "--display", "--requirements", "-", target)
		if err != nil {
			return fmt.Errorf("reading designated requirement for %s: %w", target, err)
		}
		if err := validateMoraDesignatedRequirement(string(requirement)); err != nil {
			return fmt.Errorf("designated requirement for %s does not pin Mora's identifier and team: %w", target, err)
		}
	}
	if output, err := appCommandOutput(ctx, "/usr/bin/xcrun", "stapler", "validate", appRoot); err != nil {
		return fmt.Errorf("Mora.app has no valid stapled notarization ticket: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := appCommandOutput(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", "-R=notarized", appRoot); err != nil {
		return fmt.Errorf("Mora.app does not satisfy Apple's notarized code requirement: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := appCommandOutput(ctx, "/usr/sbin/spctl", "--assess", "--type", "execute", "--verbose=4", appRoot); err != nil {
		return fmt.Errorf("Gatekeeper rejected Mora.app: %w: %s", err, strings.TrimSpace(string(output)))
	}
	archOutput, err := appCommandOutput(ctx, "/usr/bin/lipo", "-archs", binary)
	if err != nil {
		return fmt.Errorf("reading app executable architecture: %w: %s", err, strings.TrimSpace(string(archOutput)))
	}
	wantArch := expectedArch
	if wantArch == "amd64" {
		wantArch = "x86_64"
	}
	if actual := strings.TrimSpace(string(archOutput)); actual != wantArch {
		return fmt.Errorf("app executable architecture = %q, want %q", actual, wantArch)
	}
	versionOutput, err := appCommandOutput(ctx, binary, "version")
	if err != nil {
		return fmt.Errorf("running staged app version: %w: %s", err, strings.TrimSpace(string(versionOutput)))
	}
	firstLine := strings.SplitN(strings.TrimSpace(string(versionOutput)), "\n", 2)[0]
	if firstLine != "mora "+expectedVersion {
		return fmt.Errorf("staged app version line = %q, want %q", firstLine, "mora "+expectedVersion)
	}
	return nil
}

func validateMoraSigningMetadata(metadata string) error {
	requiredExact := []string{
		"Identifier=" + moraAppBundleID,
		"TeamIdentifier=" + moraAppleTeamID,
	}
	lines := strings.Split(metadata, "\n")
	for _, required := range requiredExact {
		found := false
		for _, line := range lines {
			if line == required {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("missing %s", required)
		}
	}
	hasAuthority := false
	hasRuntime := false
	hasTimestamp := false
	for _, line := range lines {
		if strings.HasPrefix(line, "Authority=Developer ID Application:") {
			hasAuthority = true
		}
		if strings.HasPrefix(line, "CodeDirectory ") && strings.Contains(line, "flags=") && strings.Contains(line, "(runtime)") {
			hasRuntime = true
		}
		if strings.HasPrefix(line, "Timestamp=") && strings.TrimPrefix(line, "Timestamp=") != "" {
			hasTimestamp = true
		}
	}
	if !hasAuthority {
		return fmt.Errorf("missing Developer ID Application authority")
	}
	if !hasRuntime {
		return fmt.Errorf("missing hardened runtime")
	}
	if !hasTimestamp {
		return fmt.Errorf("missing secure timestamp")
	}
	return nil
}

func validateMoraDesignatedRequirement(requirement string) error {
	// codesign writes its display output to stderr and terminates the
	// requirement with a newline. Match the semantic requirement after trimming
	// command framing whitespace; requiring absolute end-of-string on the raw
	// output rejects every genuine signed app before an upgrade can begin.
	requirement = strings.TrimSpace(requirement)
	if !strings.Contains(requirement, `identifier "`+moraAppBundleID+`"`) {
		return fmt.Errorf("missing bundle identifier")
	}
	teamPattern := regexp.MustCompile(`subject\.OU[^=]*=[[:space:]]*"?` + regexp.QuoteMeta(moraAppleTeamID) + `"?(?:[[:space:]]|$)`)
	if !teamPattern.MatchString(requirement) {
		return fmt.Errorf("missing Apple team")
	}
	return nil
}

func replaceMoraAppBundle(ctx context.Context, installedApp, stagedApp, version, arch string) error {
	if filepath.Clean(installedApp) == filepath.Clean(stagedApp) {
		return fmt.Errorf("installed and staged app paths must differ")
	}
	if err := validateMoraAppLayout(installedApp); err != nil {
		return fmt.Errorf("installed app layout: %w", err)
	}
	if err := validateMoraAppLayout(stagedApp); err != nil {
		return fmt.Errorf("staged app layout: %w", err)
	}
	installedParent := filepath.Dir(installedApp)
	relativeStage, err := filepath.Rel(installedParent, stagedApp)
	if err != nil || relativeStage == ".." || strings.HasPrefix(relativeStage, ".."+string(filepath.Separator)) {
		return fmt.Errorf("staged app must be on the installed app's volume under %s", installedParent)
	}
	if err := swapMoraAppDirectories(installedApp, stagedApp); err != nil {
		return fmt.Errorf("atomically swapping app directories: %w", err)
	}
	if err := verifyStagedMoraApp(ctx, installedApp, version, arch); err != nil {
		rollbackErr := swapMoraAppDirectories(installedApp, stagedApp)
		if rollbackErr != nil {
			return &moraAppRollbackFailure{
				verificationErr: err,
				rollbackErr:     rollbackErr,
				recoveryPath:    stagedApp,
			}
		}
		return fmt.Errorf("new app failed post-swap verification: %w", err)
	}
	return nil
}

func combinedCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	return command.CombinedOutput()
}
