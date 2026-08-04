package mora

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/creativeprojects/go-selfupdate"
)

func TestMoraAppRoot(t *testing.T) {
	valid := filepath.Join(string(filepath.Separator), "Users", "adit", "Applications", "Mora.app", "Contents", "MacOS", "mora")
	if got, ok := moraAppRoot(valid); !ok || got != filepath.Join(string(filepath.Separator), "Users", "adit", "Applications", "Mora.app") {
		t.Fatalf("moraAppRoot(%q) = (%q, %v)", valid, got, ok)
	}
	for _, invalid := range []string{
		"/usr/local/bin/mora",
		"/Applications/Other.app/Contents/MacOS/mora",
		"/Applications/Mora.app/Contents/Helpers/mora",
		"/Applications/Mora.app/Contents/MacOS/not-mora",
	} {
		if got, ok := moraAppRoot(filepath.FromSlash(invalid)); ok {
			t.Fatalf("moraAppRoot(%q) unexpectedly matched %q", invalid, got)
		}
	}
}

func TestMoraAppAssetNameCannotMatchLegacyUpdaterSuffix(t *testing.T) {
	name, err := moraAppAssetName("v0.12.0", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if name != "mora_0.12.0_darwin_arm64_app.zip" {
		t.Fatalf("asset name = %q", name)
	}
	for _, suffix := range []string{"darwin_arm64.zip", "darwin-arm64.zip", "darwin_arm64", "darwin-arm64"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			t.Fatalf("app asset %q can be selected by legacy suffix %q", name, suffix)
		}
	}
	if _, err := moraAppAssetName("0.12.0", "ppc64"); err == nil {
		t.Fatal("unsupported app architecture was accepted")
	}
}

func TestDetectLatestAppRelease(t *testing.T) {
	assetName := "mora_0.12.0_darwin_arm64_app.zip"
	source := &fakeAppSource{releases: []selfupdate.SourceRelease{
		fakeAppRelease{tag: "v0.11.4", assets: []selfupdate.SourceAsset{
			fakeAppAsset{name: "mora_0.11.4_darwin_arm64.tar.gz", size: 10, url: githubAssetURL("v0.11.4", "raw")},
		}},
		fakeAppRelease{tag: "v0.12.0", assets: []selfupdate.SourceAsset{
			fakeAppAsset{name: assetName, size: 100, url: githubAssetURL("v0.12.0", assetName)},
			fakeAppAsset{name: moraAppChecksumFilename, size: 100, url: githubAssetURL("v0.12.0", moraAppChecksumFilename)},
		}},
	}}
	candidate, found, err := detectLatestAppRelease(context.Background(), source, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if !found || candidate.version != "0.12.0" || candidate.assetName != assetName {
		t.Fatalf("candidate = %#v, found=%v", candidate, found)
	}
}

func TestDetectLatestAppReleaseFailsClosedWhenNewestReleaseHasNoApp(t *testing.T) {
	validOld := fakeAppRelease{tag: "v0.12.0", assets: []selfupdate.SourceAsset{
		fakeAppAsset{name: "mora_0.12.0_darwin_arm64_app.zip", size: 100, url: githubAssetURL("v0.12.0", "app")},
		fakeAppAsset{name: moraAppChecksumFilename, size: 100, url: githubAssetURL("v0.12.0", "sum")},
	}}
	missingNew := fakeAppRelease{tag: "v0.12.1", assets: []selfupdate.SourceAsset{
		fakeAppAsset{name: "mora_0.12.1_darwin_arm64.tar.gz", size: 100, url: githubAssetURL("v0.12.1", "raw")},
	}}
	_, _, err := detectLatestAppRelease(context.Background(), &fakeAppSource{releases: []selfupdate.SourceRelease{validOld, missingNew}}, "arm64")
	if err == nil || !strings.Contains(err.Error(), "exactly one mora_0.12.1_darwin_arm64_app.zip") {
		t.Fatalf("missing newest app error = %v", err)
	}
}

func TestDetectLatestAppReleaseRejectsDuplicateOrUnsafeAssets(t *testing.T) {
	name := "mora_0.12.0_darwin_arm64_app.zip"
	base := []selfupdate.SourceAsset{
		fakeAppAsset{name: name, size: 100, url: githubAssetURL("v0.12.0", name)},
		fakeAppAsset{name: moraAppChecksumFilename, size: 100, url: githubAssetURL("v0.12.0", moraAppChecksumFilename)},
	}
	duplicate := append(append([]selfupdate.SourceAsset{}, base...), base[0])
	_, _, err := detectLatestAppRelease(context.Background(), &fakeAppSource{releases: []selfupdate.SourceRelease{fakeAppRelease{tag: "v0.12.0", assets: duplicate}}}, "arm64")
	if err == nil || !strings.Contains(err.Error(), "found 2") {
		t.Fatalf("duplicate asset error = %v", err)
	}

	unsafe := append([]selfupdate.SourceAsset{}, base...)
	unsafe[0] = fakeAppAsset{name: name, size: 100, url: "https://evil.example/mora.zip"}
	_, _, err = detectLatestAppRelease(context.Background(), &fakeAppSource{releases: []selfupdate.SourceRelease{fakeAppRelease{tag: "v0.12.0", assets: unsafe}}}, "arm64")
	if err == nil || !strings.Contains(err.Error(), "invalid app asset URL") {
		t.Fatalf("unsafe URL error = %v", err)
	}
}

func TestCmdUpgradeAppReplacesWholeBundle(t *testing.T) {
	parent := t.TempDir()
	installed := writeAppLayout(t, parent)
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		t.Skip("supported release architecture")
	}
	assetName, err := moraAppAssetName("0.12.1", arch)
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeAppSource{releases: []selfupdate.SourceRelease{fakeAppRelease{tag: "v0.12.1", assets: []selfupdate.SourceAsset{
		fakeAppAsset{name: assetName, size: 7, url: githubAssetURL("v0.12.1", assetName)},
		fakeAppAsset{name: moraAppChecksumFilename, size: 100, url: githubAssetURL("v0.12.1", moraAppChecksumFilename)},
	}}}}
	archiveFixture := writeZipFixture(t, []zipTestEntry{
		{name: "Mora.app/Contents/Info.plist", body: "plist"},
		{name: "Mora.app/Contents/MacOS/mora", body: "binary", mode: 0o755},
		{name: "Mora.app/Contents/Resources/Mora.icns", body: "icon"},
	})
	archiveBytes, err := os.ReadFile(archiveFixture)
	if err != nil {
		t.Fatal(err)
	}

	originalGOOS := runtimeGOOS
	originalSource := newAppReleaseSource
	originalDownload := downloadAppReleaseFile
	originalExpand := expandMoraAppArchive
	originalVerify := verifyStagedMoraApp
	originalSwap := swapMoraAppDirectories
	originalRebuild := postAppUpgradeRebuild
	t.Cleanup(func() {
		runtimeGOOS = originalGOOS
		newAppReleaseSource = originalSource
		downloadAppReleaseFile = originalDownload
		expandMoraAppArchive = originalExpand
		verifyStagedMoraApp = originalVerify
		swapMoraAppDirectories = originalSwap
		postAppUpgradeRebuild = originalRebuild
	})
	runtimeGOOS = func() string { return "darwin" }
	newAppReleaseSource = func(string) (selfupdate.Source, error) { return source, nil }
	downloads := 0
	downloadAppReleaseFile = func(_ context.Context, _ string, _ string, destination string, _ int64) error {
		downloads++
		if filepath.Base(destination) == moraAppChecksumFilename {
			sum := sha256.Sum256(archiveBytes)
			return os.WriteFile(destination, []byte(fmt.Sprintf("%x  %s\n", sum[:], assetName)), 0o600)
		}
		return os.WriteFile(destination, archiveBytes, 0o600)
	}
	expandMoraAppArchive = func(_ context.Context, _ string, destination string) error {
		writeAppLayout(t, destination)
		return nil
	}
	verifications := 0
	verifyStagedMoraApp = func(context.Context, string, string, string) error {
		verifications++
		return nil
	}
	swaps := 0
	swapMoraAppDirectories = func(_, _ string) error {
		swaps++
		return nil
	}
	rebuilds := 0
	postAppUpgradeRebuild = func(context.Context, string, io.Writer) error {
		rebuilds++
		return nil
	}

	var stdout strings.Builder
	if err := cmdUpgradeApp(context.Background(), "0.12.0", installed, false, "", &stdout); err != nil {
		t.Fatal(err)
	}
	if downloads != 2 || swaps != 1 || verifications != 3 || rebuilds != 1 {
		t.Fatalf("downloads=%d swaps=%d verifications=%d rebuilds=%d", downloads, swaps, verifications, rebuilds)
	}
	if !strings.Contains(stdout.String(), "✓ updated Mora.app to 0.12.1") {
		t.Fatalf("upgrade output = %q", stdout.String())
	}
}

func TestCmdUpgradeAppPreservesOldAppWhenRollbackFails(t *testing.T) {
	parent := t.TempDir()
	installed := writeAppLayout(t, parent)
	if err := os.WriteFile(filepath.Join(installed, "marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		t.Skip("supported release architecture")
	}
	assetName, err := moraAppAssetName("0.12.1", arch)
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeAppSource{releases: []selfupdate.SourceRelease{fakeAppRelease{tag: "v0.12.1", assets: []selfupdate.SourceAsset{
		fakeAppAsset{name: assetName, size: 7, url: githubAssetURL("v0.12.1", assetName)},
		fakeAppAsset{name: moraAppChecksumFilename, size: 100, url: githubAssetURL("v0.12.1", moraAppChecksumFilename)},
	}}}}
	archiveFixture := writeZipFixture(t, []zipTestEntry{
		{name: "Mora.app/Contents/Info.plist", body: "plist"},
		{name: "Mora.app/Contents/MacOS/mora", body: "binary", mode: 0o755},
		{name: "Mora.app/Contents/Resources/Mora.icns", body: "icon"},
	})
	archiveBytes, err := os.ReadFile(archiveFixture)
	if err != nil {
		t.Fatal(err)
	}

	originalGOOS := runtimeGOOS
	originalSource := newAppReleaseSource
	originalDownload := downloadAppReleaseFile
	originalExpand := expandMoraAppArchive
	originalVerify := verifyStagedMoraApp
	originalSwap := swapMoraAppDirectories
	t.Cleanup(func() {
		runtimeGOOS = originalGOOS
		newAppReleaseSource = originalSource
		downloadAppReleaseFile = originalDownload
		expandMoraAppArchive = originalExpand
		verifyStagedMoraApp = originalVerify
		swapMoraAppDirectories = originalSwap
	})
	runtimeGOOS = func() string { return "darwin" }
	newAppReleaseSource = func(string) (selfupdate.Source, error) { return source, nil }
	downloadAppReleaseFile = func(_ context.Context, _ string, _ string, destination string, _ int64) error {
		if filepath.Base(destination) == moraAppChecksumFilename {
			sum := sha256.Sum256(archiveBytes)
			return os.WriteFile(destination, []byte(fmt.Sprintf("%x  %s\n", sum[:], assetName)), 0o600)
		}
		return os.WriteFile(destination, archiveBytes, 0o600)
	}
	expandMoraAppArchive = func(_ context.Context, _ string, destination string) error {
		app := writeAppLayout(t, destination)
		return os.WriteFile(filepath.Join(app, "marker"), []byte("new"), 0o600)
	}
	verifications := 0
	verifyStagedMoraApp = func(context.Context, string, string, string) error {
		verifications++
		if verifications == 3 {
			return errors.New("broken post-swap seal")
		}
		return nil
	}
	swaps := 0
	swapMoraAppDirectories = func(left, right string) error {
		swaps++
		if swaps == 2 {
			return errors.New("rollback refused")
		}
		temporary := left + ".test-swap"
		if err := os.Rename(left, temporary); err != nil {
			return err
		}
		if err := os.Rename(right, left); err != nil {
			return err
		}
		return os.Rename(temporary, right)
	}

	var stdout strings.Builder
	upgradeErr := cmdUpgradeApp(context.Background(), "0.12.0", installed, false, "", &stdout)
	if upgradeErr == nil || !strings.Contains(upgradeErr.Error(), "rollback failed") {
		t.Fatalf("upgrade error = %v", upgradeErr)
	}
	stages, err := filepath.Glob(filepath.Join(parent, ".mora-app-upgrade.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 {
		t.Fatalf("preserved stage paths = %v, want exactly one", stages)
	}
	recoveryApp := filepath.Join(stages[0], "expanded", moraAppName)
	assertFileBody(t, filepath.Join(recoveryApp, "marker"), "old")
	assertFileBody(t, filepath.Join(installed, "marker"), "new")
	if !strings.Contains(upgradeErr.Error(), recoveryApp) || !strings.Contains(stdout.String(), recoveryApp) {
		t.Fatalf("recovery path missing: error=%v output=%q", upgradeErr, stdout.String())
	}
}

func TestVerifyAppArchiveChecksum(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "mora_0.12.0_darwin_arm64_app.zip")
	body := []byte("signed stapled app bytes")
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sumBytes := sha256.Sum256(body)
	sum := fmt.Sprintf("%x", sumBytes[:])
	manifest := filepath.Join(dir, moraAppChecksumFilename)
	if err := os.WriteFile(manifest, []byte(sum+"  "+filepath.Base(archive)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyAppArchiveChecksum(archive, manifest, filepath.Base(archive)); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(manifest, []byte(strings.Repeat("0", 64)+"  "+filepath.Base(archive)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyAppArchiveChecksum(archive, manifest, filepath.Base(archive)); err == nil || !strings.Contains(err.Error(), "CHECKSUM MISMATCH") {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestVerifyAppArchiveChecksumRejectsAmbiguousManifest(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "app.zip")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, moraAppChecksumFilename)
	valid := strings.Repeat("a", 64) + "  app.zip\n"
	for name, body := range map[string]string{
		"duplicate": valid + valid,
		"missing":   strings.Repeat("a", 64) + "  other.zip\n",
		"malformed": "not-a-hash  app.zip\n",
		"extra":     strings.Repeat("a", 64) + " app.zip unexpected\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyAppArchiveChecksum(archive, manifest, "app.zip"); err == nil {
				t.Fatal("ambiguous checksum manifest was accepted")
			}
		})
	}
}

func TestPreflightMoraAppZip(t *testing.T) {
	valid := []zipTestEntry{
		{name: "Mora.app/Contents/Info.plist", body: "plist"},
		{name: "Mora.app/Contents/MacOS/mora", body: "binary", mode: 0o755},
		{name: "Mora.app/Contents/Resources/Mora.icns", body: "icon"},
		{name: "Mora.app/Contents/_CodeSignature/CodeResources", body: "seal"},
		{name: "__MACOSX/._Mora.app", body: "appledouble"},
	}
	archive := writeZipFixture(t, valid)
	if err := preflightMoraAppZip(archive); err != nil {
		t.Fatal(err)
	}

	cases := map[string][]zipTestEntry{
		"traversal": appendCopy(valid, zipTestEntry{name: "Mora.app/../../owned", body: "x"}),
		"self-parent": appendCopy(valid,
			zipTestEntry{name: "Mora.app/..", body: "x"}),
		"absolute":  appendCopy(valid, zipTestEntry{name: "/tmp/owned", body: "x"}),
		"backslash": appendCopy(valid, zipTestEntry{name: `Mora.app\..\owned`, body: "x"}),
		"other-root": appendCopy(valid,
			zipTestEntry{name: "Other.app/Contents/MacOS/mora", body: "x"}),
		"symlink": appendCopy(valid,
			zipTestEntry{name: "Mora.app/Contents/Resources/link", body: "../../outside", mode: os.ModeSymlink | 0o777}),
		"case-duplicate": appendCopy(valid,
			zipTestEntry{name: "mora.app/Contents/Info.plist", body: "other"}),
		"missing-icon": {
			{name: "Mora.app/Contents/Info.plist", body: "plist"},
			{name: "Mora.app/Contents/MacOS/mora", body: "binary", mode: 0o755},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			if err := preflightMoraAppZip(writeZipFixture(t, entries)); err == nil {
				t.Fatal("unsafe app zip was accepted")
			}
		})
	}
}

func TestValidateMoraAppLayout(t *testing.T) {
	app := writeAppLayout(t, t.TempDir())
	if err := validateMoraAppLayout(app); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(app, "Contents", "Resources", "Mora.icns")); err != nil {
		t.Fatal(err)
	}
	if err := validateMoraAppLayout(app); err == nil {
		t.Fatal("missing icon was accepted")
	}
}

func TestValidateMoraSigningMetadata(t *testing.T) {
	valid := strings.Join([]string{
		"Identifier=" + moraAppBundleID,
		"Authority=Developer ID Application: ADIT ABHIJIT KARODE (" + moraAppleTeamID + ")",
		"TeamIdentifier=" + moraAppleTeamID,
		"CodeDirectory v=20500 size=1 flags=0x10000(runtime)",
		"Timestamp=Jul 31, 2026 at 10:00:00 AM",
	}, "\n")
	if err := validateMoraSigningMetadata(valid); err != nil {
		t.Fatal(err)
	}
	for name, sabotage := range map[string]string{
		"wrong-team":   strings.ReplaceAll(valid, moraAppleTeamID, "ABCDEFGHIJ"),
		"no-runtime":   strings.Replace(valid, "(runtime)", "", 1),
		"no-timestamp": strings.Replace(valid, "Timestamp=", "NotTimestamp=", 1),
		"adhoc":        strings.Replace(valid, "Authority=Developer ID Application:", "Authority=adhoc:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateMoraSigningMetadata(sabotage); err == nil {
				t.Fatal("sabotaged signing metadata was accepted")
			}
		})
	}
}

func TestVerifyMoraAppBundle(t *testing.T) {
	app := writeAppLayout(t, t.TempDir())
	original := appCommandOutput
	t.Cleanup(func() { appCommandOutput = original })
	appCommandOutput = fakeVerifiedAppCommands(t, app, "0.12.0", "arm64", false)
	if err := verifyMoraAppBundle(context.Background(), app, "0.12.0", "arm64"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyMoraAppBundleRejectsWrongTeam(t *testing.T) {
	app := writeAppLayout(t, t.TempDir())
	original := appCommandOutput
	t.Cleanup(func() { appCommandOutput = original })
	appCommandOutput = fakeVerifiedAppCommands(t, app, "0.12.0", "arm64", true)
	if err := verifyMoraAppBundle(context.Background(), app, "0.12.0", "arm64"); err == nil || !strings.Contains(err.Error(), "TeamIdentifier") {
		t.Fatalf("wrong-team verification error = %v", err)
	}
}

func TestVerifyMoraAppBundleRejectsGatekeeperFailure(t *testing.T) {
	app := writeAppLayout(t, t.TempDir())
	original := appCommandOutput
	t.Cleanup(func() { appCommandOutput = original })
	verified := fakeVerifiedAppCommands(t, app, "0.12.0", "arm64", false)
	appCommandOutput = func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if command == "/usr/sbin/spctl" {
			return []byte("rejected"), errors.New("exit status 3")
		}
		return verified(ctx, command, args...)
	}
	if err := verifyMoraAppBundle(context.Background(), app, "0.12.0", "arm64"); err == nil || !strings.Contains(err.Error(), "Gatekeeper rejected") {
		t.Fatalf("Gatekeeper rejection error = %v", err)
	}
}

func TestReplaceMoraAppBundleRollsBackOnPostSwapFailure(t *testing.T) {
	parent := t.TempDir()
	installed := writeAppLayout(t, parent)
	stageParent := filepath.Join(parent, ".mora-app-upgrade.test")
	if err := os.Mkdir(stageParent, 0o700); err != nil {
		t.Fatal(err)
	}
	staged := writeAppLayout(t, stageParent)
	originalSwap := swapMoraAppDirectories
	originalVerify := verifyStagedMoraApp
	t.Cleanup(func() {
		swapMoraAppDirectories = originalSwap
		verifyStagedMoraApp = originalVerify
	})
	swaps := 0
	swapMoraAppDirectories = func(_, _ string) error {
		swaps++
		return nil
	}
	verifyStagedMoraApp = func(context.Context, string, string, string) error {
		return errors.New("broken seal")
	}
	err := replaceMoraAppBundle(context.Background(), installed, staged, "0.12.0", "arm64")
	if err == nil || !strings.Contains(err.Error(), "post-swap") || swaps != 2 {
		t.Fatalf("replace error=%v swaps=%d, want rollback", err, swaps)
	}
}

func TestReplaceMoraAppBundleReportsRecoveryPathWhenRollbackFails(t *testing.T) {
	parent := t.TempDir()
	installed := writeAppLayout(t, parent)
	stageParent := filepath.Join(parent, ".mora-app-upgrade.test")
	if err := os.Mkdir(stageParent, 0o700); err != nil {
		t.Fatal(err)
	}
	staged := writeAppLayout(t, stageParent)
	originalSwap := swapMoraAppDirectories
	originalVerify := verifyStagedMoraApp
	t.Cleanup(func() {
		swapMoraAppDirectories = originalSwap
		verifyStagedMoraApp = originalVerify
	})
	swaps := 0
	swapMoraAppDirectories = func(_, _ string) error {
		swaps++
		if swaps == 2 {
			return errors.New("rollback refused")
		}
		return nil
	}
	verifyStagedMoraApp = func(context.Context, string, string, string) error {
		return errors.New("broken seal")
	}

	err := replaceMoraAppBundle(context.Background(), installed, staged, "0.12.0", "arm64")
	var rollbackFailure *moraAppRollbackFailure
	if !errors.As(err, &rollbackFailure) {
		t.Fatalf("replace error=%v, want moraAppRollbackFailure", err)
	}
	if rollbackFailure.recoveryPath != staged || !strings.Contains(err.Error(), staged) || swaps != 2 {
		t.Fatalf("rollback failure=%#v error=%v swaps=%d", rollbackFailure, err, swaps)
	}
}

func TestReplaceMoraAppBundleRequiresSameParentTree(t *testing.T) {
	installed := writeAppLayout(t, t.TempDir())
	staged := writeAppLayout(t, t.TempDir())
	if err := replaceMoraAppBundle(context.Background(), installed, staged, "0.12.0", "arm64"); err == nil || !strings.Contains(err.Error(), "under") {
		t.Fatalf("outside-stage error = %v", err)
	}
}

func TestAtomicSwapMoraAppDirectoriesDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin rename-swap primitive")
	}
	parent := t.TempDir()
	installed := filepath.Join(parent, "Mora.app")
	staged := filepath.Join(parent, ".stage", "Mora.app")
	if err := os.MkdirAll(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "marker"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicSwapMoraAppDirectories(installed, staged); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, filepath.Join(installed, "marker"), "new")
	assertFileBody(t, filepath.Join(staged, "marker"), "old")
	if err := atomicSwapMoraAppDirectories(installed, staged); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, filepath.Join(installed, "marker"), "old")
}

func TestInstallAppScriptContract(t *testing.T) {
	path := filepath.Join("..", "..", "install-app.sh")
	bodyBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Git's Windows checkout may use CRLF, but the shell contract is
	// line-ending independent.
	body := strings.ReplaceAll(string(bodyBytes), "\r\n", "\n")
	for _, required := range []string{
		`VERSION="${VERSION:-`,
		`ASSET="mora_${VERSION}_darwin_${ARCH}_app.zip"`,
		`CHECKSUM_ASSET="checksums-app.txt"`,
		`ditto -x -k`,
		`xcrun stapler validate`,
		`-R='notarized'`,
		`spctl --assess --type execute`,
		`cp -p "$LINK_DEST" "$BACKUP"`,
		`ln -s "$APP_EXECUTABLE"`,
		`MORA_APP_DIR`,
		`VERSION must be a stable numeric release`,
		`*/..|`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("install-app.sh missing contract text %q", required)
		}
	}
	for _, forbidden := range []string{
		"xattr -d",
		"codesign --force --sign",
		`rm -rf "$APP_DEST"`,
		`mv "$APP_DEST/Contents/MacOS/mora"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("install-app.sh contains forbidden mutation %q", forbidden)
		}
	}
	versionAssignments := regexp.MustCompile(`(?m)^VERSION="\$\{VERSION:-[0-9]+\.[0-9]+\.[0-9]+\}"$`).FindAllString(body, -1)
	if len(versionAssignments) != 1 {
		t.Fatalf("install-app.sh must have exactly one release VERSION default, found %d", len(versionAssignments))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatal("install-app.sh is not executable")
	}
}

func TestInstallAppScriptRejectsUnsafeVersionBeforeMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install-app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	command := exec.Command("sh", script)
	command.Env = []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"VERSION=../../unexpected/path",
		"MORA_APP_DIR=" + filepath.Join(home, "Applications"),
		"PREFIX=" + filepath.Join(home, "bin"),
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "VERSION must be a stable numeric release") {
		t.Fatalf("unsafe version run err=%v output=%s", err, output)
	}
	if _, err := os.Lstat(filepath.Join(home, "Applications")); !os.IsNotExist(err) {
		t.Fatalf("unsafe version mutated the app destination: %v", err)
	}
}

func TestInstallAppScriptMigratesStandaloneToAppSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	script, err := filepath.Abs(filepath.Join(repoRoot, "install-app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	appParent := filepath.Join(home, "Applications")
	app := writeRunnableAppLayout(t, appParent, "0.12.0")
	mockBin := filepath.Join(home, "mock-bin")
	linkDir := filepath.Join(home, "path-bin")
	if err := os.MkdirAll(mockBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAppInstallerMocks(t, mockBin)
	legacy := filepath.Join(linkDir, "mora")
	if err := os.WriteFile(legacy, []byte("legacy signed binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", script)
	command.Env = []string{
		"PATH=" + mockBin + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"PREFIX=" + linkDir,
		"MORA_APP_DIR=" + appParent,
		"MORA_VAULT=" + filepath.Join(home, "vault", "mora"),
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install-app.sh failed: %v\n%s", err, output)
	}
	linkTarget, err := os.Readlink(legacy)
	if err != nil {
		t.Fatalf("PATH entry is not a symlink: %v", err)
	}
	wantTarget := filepath.Join(app, "Contents", "MacOS", "mora")
	if linkTarget != wantTarget {
		t.Fatalf("PATH symlink = %q, want %q", linkTarget, wantTarget)
	}
	assertFileBody(t, legacy+".standalone-backup", "legacy signed binary")
	if !strings.Contains(string(output), "Planned Mora.app Full Disk Access migration") ||
		!strings.Contains(string(output), "continuity is not proven") {
		t.Fatalf("installer output omitted FDA migration: %s", output)
	}
}

func TestInstallAppScriptRollsBackWhenPostRenameVerificationFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	script, err := filepath.Abs(filepath.Join(repoRoot, "install-app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	appParent := filepath.Join(home, "Applications")
	mockBin := filepath.Join(home, "mock-bin")
	linkDir := filepath.Join(home, "path-bin")
	for _, directory := range []string{mockBin, linkDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeAppInstallerMocks(t, mockBin)
	writeFreshAppInstallerMocks(t, mockBin)
	legacy := filepath.Join(linkDir, "mora")
	if err := os.WriteFile(legacy, []byte("legacy signed binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(home, "mora-app.zip")
	archiveBody := []byte("mock signed app zip")
	if err := os.WriteFile(archive, archiveBody, 0o600); err != nil {
		t.Fatal(err)
	}
	checksum := filepath.Join(home, "checksums-app.txt")
	sum := sha256.Sum256(archiveBody)
	checksumBody := fmt.Sprintf("%x  mora_0.12.0_darwin_arm64_app.zip\n", sum[:])
	if err := os.WriteFile(checksum, []byte(checksumBody), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", script)
	command.Env = []string{
		"PATH=" + mockBin + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"PREFIX=" + linkDir,
		"MORA_APP_DIR=" + appParent,
		"MORA_VAULT=" + filepath.Join(home, "vault", "mora"),
		"VERSION=0.12.0",
		"MOCK_APP_ARCHIVE=" + archive,
		"MOCK_APP_CHECKSUM=" + checksum,
		"MOCK_APP_VERSION=0.12.0",
		"MOCK_POST_INSTALL_FAIL=1",
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "incomplete app was removed") {
		t.Fatalf("post-rename failure run err=%v output=%s", err, output)
	}
	app := filepath.Join(appParent, moraAppName)
	if _, err := os.Lstat(app); !os.IsNotExist(err) {
		t.Fatalf("post-rename rollback left Mora.app: %v", err)
	}
	assertFileBody(t, legacy, "legacy signed binary")
	if _, err := os.Lstat(legacy + ".standalone-backup"); !os.IsNotExist(err) {
		t.Fatalf("post-rename rollback mutated PATH before app verification: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(appParent, ".mora-app.install.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("post-rename rollback left staging paths: %v", stages)
	}
}

func TestInstallAppScriptRejectsWrongSigningTeamBeforePathMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	script, err := filepath.Abs(filepath.Join(repoRoot, "install-app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	appParent := filepath.Join(home, "Applications")
	writeRunnableAppLayout(t, appParent, "0.12.0")
	mockBin := filepath.Join(home, "mock-bin")
	linkDir := filepath.Join(home, "path-bin")
	if err := os.MkdirAll(mockBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAppInstallerMocks(t, mockBin)
	legacy := filepath.Join(linkDir, "mora")
	if err := os.WriteFile(legacy, []byte("legacy signed binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", script)
	command.Env = []string{
		"PATH=" + mockBin + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"PREFIX=" + linkDir,
		"MORA_APP_DIR=" + appParent,
		"MORA_VAULT=" + filepath.Join(home, "vault", "mora"),
		"MOCK_TEAM=ABCDEFGHIJ",
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "wrong Apple team") {
		t.Fatalf("wrong-team run err=%v output=%s", err, output)
	}
	assertFileBody(t, legacy, "legacy signed binary")
	if _, err := os.Lstat(legacy + ".standalone-backup"); !os.IsNotExist(err) {
		t.Fatalf("wrong-team run created a backup before verification: %v", err)
	}
}

func githubAssetURL(tag, name string) string {
	return "https://github.com/pyranthus-hq/mora/releases/download/" + tag + "/" + name
}

type fakeAppSource struct {
	releases []selfupdate.SourceRelease
	err      error
}

func (s *fakeAppSource) ListReleases(context.Context, selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	return s.releases, s.err
}

func (*fakeAppSource) DownloadReleaseAsset(context.Context, *selfupdate.Release, int64) (io.ReadCloser, error) {
	return nil, errors.New("unexpected download")
}

type fakeAppRelease struct {
	tag        string
	draft      bool
	prerelease bool
	assets     []selfupdate.SourceAsset
}

func (r fakeAppRelease) GetID() int64              { return 1 }
func (r fakeAppRelease) GetTagName() string        { return r.tag }
func (r fakeAppRelease) GetDraft() bool            { return r.draft }
func (r fakeAppRelease) GetPrerelease() bool       { return r.prerelease }
func (r fakeAppRelease) GetPublishedAt() time.Time { return time.Now() }
func (r fakeAppRelease) GetReleaseNotes() string   { return "" }
func (r fakeAppRelease) GetName() string           { return r.tag }
func (r fakeAppRelease) GetURL() string {
	return "https://github.com/pyranthus-hq/mora/releases/tag/" + r.tag
}
func (r fakeAppRelease) GetAssets() []selfupdate.SourceAsset { return r.assets }

type fakeAppAsset struct {
	id   int64
	name string
	size int
	url  string
}

func (a fakeAppAsset) GetID() int64                  { return a.id }
func (a fakeAppAsset) GetName() string               { return a.name }
func (a fakeAppAsset) GetSize() int                  { return a.size }
func (a fakeAppAsset) GetBrowserDownloadURL() string { return a.url }

type zipTestEntry struct {
	name string
	body string
	mode os.FileMode
}

func appendCopy(entries []zipTestEntry, extra zipTestEntry) []zipTestEntry {
	result := append([]zipTestEntry{}, entries...)
	return append(result, extra)
}

func writeZipFixture(t *testing.T, entries []zipTestEntry) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "app-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func writeAppLayout(t *testing.T, parent string) string {
	t.Helper()
	app := filepath.Join(parent, moraAppName)
	for _, dir := range []string{
		filepath.Join(app, "Contents", "MacOS"),
		filepath.Join(app, "Contents", "Resources"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Resources", "Mora.icns"), []byte("icon"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "mora"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

func writeRunnableAppLayout(t *testing.T, parent, version string) string {
	t.Helper()
	app := writeAppLayout(t, parent)
	binary := filepath.Join(app, "Contents", "MacOS", "mora")
	script := "#!/bin/sh\ncase \"${1:-}\" in\n  version) printf 'mora " + version + "\\ncommit: test\\n' ;;\n  init) exit 0 ;;\n  config) printf 'vault_dir = %s\\n' \"${MORA_VAULT:-$HOME/vault/mora}\" ;;\n  *) exit 0 ;;\nesac\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

func writeAppInstallerMocks(t *testing.T, directory string) {
	t.Helper()
	writeExecutable(t, filepath.Join(directory, "uname"), `#!/bin/sh
case "${1:-}" in
  -s) printf 'Darwin\n' ;;
  -m) printf 'arm64\n' ;;
  *) printf 'Darwin\n' ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "plutil"), `#!/bin/sh
target=""
for argument in "$@"; do target="$argument"; done
case "$2" in
  CFBundleIdentifier)
    if [ "${MOCK_POST_INSTALL_FAIL:-0}" = 1 ] && [ "$target" = "$MORA_APP_DIR/Mora.app/Contents/Info.plist" ]; then
      printf 'com.example.broken\n'
    else
      printf 'com.pyranthus.mora\n'
    fi
    ;;
  CFBundleExecutable) printf 'mora\n' ;;
  CFBundleName|CFBundleDisplayName) printf 'Mora\n' ;;
  CFBundleIconFile) printf 'Mora\n' ;;
  CFBundlePackageType) printf 'APPL\n' ;;
  CFBundleShortVersionString|CFBundleVersion) printf '0.12.0\n' ;;
  LSUIElement) printf 'true\n' ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "codesign"), `#!/bin/sh
team="${MOCK_TEAM:-VS8M5VJBZ5}"
case "$1" in
  -dvvv)
    printf 'Identifier=com.pyranthus.mora\nAuthority=Developer ID Application: Test (%s)\nTeamIdentifier=%s\nCodeDirectory flags=0x10000(runtime)\nTimestamp=now\n' "$team" "$team" >&2
    ;;
  -d)
    printf 'designated => identifier "com.pyranthus.mora" and certificate leaf[subject.OU] = "%s"\n' "$team" >&2
    ;;
  *) exit 0 ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "lipo"), "#!/bin/sh\nprintf 'arm64\\n'\n")
	for _, name := range []string{"spctl", "xcrun", "ditto", "zipinfo", "unzip"} {
		writeExecutable(t, filepath.Join(directory, name), "#!/bin/sh\nexit 0\n")
	}
}

func writeFreshAppInstallerMocks(t *testing.T, directory string) {
	t.Helper()
	writeExecutable(t, filepath.Join(directory, "curl"), `#!/bin/sh
destination=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    destination="$1"
  fi
  shift
done
[ -n "$destination" ] || exit 2
case "$destination" in
  */checksums-app.txt) cp "$MOCK_APP_CHECKSUM" "$destination" ;;
  *) cp "$MOCK_APP_ARCHIVE" "$destination" ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "zipinfo"), `#!/bin/sh
case "$1" in
  -1)
    printf '%s\n' \
      'Mora.app/Contents/Info.plist' \
      'Mora.app/Contents/MacOS/mora' \
      'Mora.app/Contents/Resources/Mora.icns'
    ;;
  -l) exit 0 ;;
  *) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "unzip"), `#!/bin/sh
[ "$1" = "-l" ] || exit 2
printf '3  3 files\n'
`)
	writeExecutable(t, filepath.Join(directory, "ditto"), `#!/bin/sh
destination=""
for argument in "$@"; do destination="$argument"; done
app="$destination/Mora.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
printf 'plist\n' > "$app/Contents/Info.plist"
printf 'icon\n' > "$app/Contents/Resources/Mora.icns"
cat > "$app/Contents/MacOS/mora" <<EOF
#!/bin/sh
case "\${1:-}" in
  version) printf 'mora %s\ncommit: test\n' "$MOCK_APP_VERSION" ;;
  init) exit 0 ;;
  config) printf 'vault_dir = %s\n' "\${MORA_VAULT:-\$HOME/vault/mora}" ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$app/Contents/MacOS/mora"
`)
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fakeVerifiedAppCommands(t *testing.T, app, version, arch string, wrongTeam bool) func(context.Context, string, ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command == "/usr/bin/plutil" {
			values := map[string]string{
				"CFBundleIdentifier":         moraAppBundleID,
				"CFBundleExecutable":         "mora",
				"CFBundleName":               "Mora",
				"CFBundleDisplayName":        "Mora",
				"CFBundleIconFile":           "Mora",
				"CFBundlePackageType":        "APPL",
				"CFBundleShortVersionString": version,
				"CFBundleVersion":            version,
				"LSUIElement":                "true",
			}
			return []byte(values[args[1]] + "\n"), nil
		}
		if command == "/usr/bin/codesign" && containsArg(args, "--display") && containsArg(args, "--verbose=4") {
			team := moraAppleTeamID
			if wrongTeam {
				team = "ABCDEFGHIJ"
			}
			return []byte("Identifier=" + moraAppBundleID + "\nAuthority=Developer ID Application: Test (" + team + ")\nTeamIdentifier=" + team + "\nCodeDirectory flags=(runtime)\nTimestamp=now\n"), nil
		}
		if command == "/usr/bin/codesign" && containsArg(args, "--requirements") {
			return []byte(`designated => identifier "` + moraAppBundleID + `" and certificate leaf[subject.OU] = "` + moraAppleTeamID + `"`), nil
		}
		if command == "/usr/bin/lipo" {
			if arch == "amd64" {
				return []byte("x86_64\n"), nil
			}
			return []byte(arch + "\n"), nil
		}
		if command == filepath.Join(app, "Contents", "MacOS", "mora") {
			return []byte("mora " + version + "\ncommit: test\n"), nil
		}
		return []byte("ok\n"), nil
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func assertFileBody(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}
