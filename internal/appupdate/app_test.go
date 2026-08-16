package appupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
)

var runtimeGOOS = func() string { return runtime.GOOS }
var postAppUpgradeRebuild = func(context.Context, string, io.Writer) error { return nil }

func cmdUpgradeApp(ctx context.Context, current, root string, check bool, token string, out io.Writer) error {
	return Run(ctx, Options{CurrentVersion: current, AppRoot: root, CheckOnly: check, Token: token, Stdout: out, GOOS: runtimeGOOS(), Arch: runtime.GOARCH, RepoOwner: "pyranthus-hq", RepoName: "mora", Decide: func(current, latest string) (Decision, bool, error) {
		c, err := semver.NewVersion(current)
		if err != nil {
			return "", false, err
		}
		l, err := semver.NewVersion(latest)
		if err != nil {
			return "", false, err
		}
		if c.GreaterThan(l) {
			return DecisionLocalAhead, false, nil
		}
		if c.Equal(l) {
			return DecisionUpToDate, false, nil
		}
		return DecisionUpgrade, false, nil
	}, PostRebuild: postAppUpgradeRebuild})
}

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

func TestDetectLatestAppReleaseIgnoresNonCanonicalTags(t *testing.T) {
	assetName := "mora_1.2.3_darwin_arm64_app.zip"
	valid := fakeAppRelease{tag: "v1.2.3", assets: []selfupdate.SourceAsset{
		fakeAppAsset{name: assetName, size: 100, url: githubAssetURL("v1.2.3", assetName)},
		fakeAppAsset{name: moraAppChecksumFilename, size: 100, url: githubAssetURL("v1.2.3", moraAppChecksumFilename)},
	}}
	releases := []selfupdate.SourceRelease{valid}
	for _, tag := range []string{"v2", "v1.2", "2.0.0", "v02.0.0", "v2.0.0-rc.1", "v2.0.0+build"} {
		releases = append(releases, fakeAppRelease{tag: tag})
	}
	candidate, found, err := detectLatestAppRelease(context.Background(), &fakeAppSource{releases: releases}, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if !found || candidate.version != "1.2.3" || candidate.assetName != assetName {
		t.Fatalf("candidate=%+v found=%v", candidate, found)
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

func TestValidateMoraDesignatedRequirementAcceptsCodesignOutput(t *testing.T) {
	// This is the newline-terminated shape emitted by `codesign -d -r-` for the
	// real signed Mora.app. Keep it literal: a fixture without the final newline
	// missed the v0.12.1/v0.12.2 self-update failure.
	actual := `Executable=/Users/adit/Applications/Mora.app/Contents/MacOS/mora
designated => identifier "com.pyranthus.mora" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */ and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */ and certificate leaf[subject.OU] = VS8M5VJBZ5
`
	if err := validateMoraDesignatedRequirement(actual); err != nil {
		t.Fatalf("real codesign requirement was rejected: %v", err)
	}

	quoted := strings.Replace(actual, "= VS8M5VJBZ5", `= "VS8M5VJBZ5" and certificate leaf[subject.CN] = "Developer ID"`, 1)
	if err := validateMoraDesignatedRequirement(quoted); err != nil {
		t.Fatalf("quoted team requirement was rejected: %v", err)
	}

	for name, sabotage := range map[string]string{
		"wrong-identifier": strings.Replace(actual, moraAppBundleID, "com.example.mora", 1),
		"wrong-team":       strings.Replace(actual, moraAppleTeamID, "ABCDEFGHIJ", 1),
		"team-prefix":      strings.Replace(actual, moraAppleTeamID, moraAppleTeamID+"X", 1),
		"missing-subject":  strings.Replace(actual, "subject.OU", "subject.CN", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateMoraDesignatedRequirement(sabotage); err == nil {
				t.Fatal("sabotaged designated requirement was accepted")
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

func TestReleaseURLPolicy(t *testing.T) {
	for _, tt := range []struct {
		url string
		ok  bool
	}{{"https://github.com/pyranthus-hq/mora/releases/download/v1/a.zip", true}, {"http://github.com/x", false}, {"https://evil.example/x", false}, {"https://user@github.com/x", false}, {"https://github.com/x?q=1", false}, {"https://github.com/x#frag", false}} {
		err := validateGitHubReleaseURL(tt.url)
		if (err == nil) != tt.ok {
			t.Errorf("validate(%q) err=%v ok=%v", tt.url, err, tt.ok)
		}
	}
}
func TestReleaseRedirectPolicy(t *testing.T) {
	for _, tt := range []struct {
		url string
		ok  bool
	}{{"https://github.com/x", true}, {"https://objects.githubusercontent.com/x?q=1", true}, {"https://release-assets.githubusercontent.com/x", true}, {"https://evilgithubusercontent.com/x", false}, {"http://objects.githubusercontent.com/x", false}, {"https://user@github.com/x", false}} {
		if got := allowedReleaseRedirect(tt.url); got != tt.ok {
			t.Errorf("allowed(%q)=%v want %v", tt.url, got, tt.ok)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDownloadReleaseFileBoundsCleanupAndAuthRedirect(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	calls := 0
	secondAuth := ""
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 302, Status: "302 Found", Header: http.Header{"Location": []string{"https://objects.githubusercontent.com/asset"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		secondAuth = r.Header.Get("Authorization")
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("payload")), ContentLength: -1, Request: r}, nil
	})
	dest := filepath.Join(t.TempDir(), "asset")
	if err := downloadReleaseFile(context.Background(), "https://github.com/release", "secret", dest, 10); err != nil {
		t.Fatal(err)
	}
	if secondAuth != "" {
		t.Fatalf("authorization leaked across redirect: %q", secondAuth)
	}
	body, err := os.ReadFile(dest)
	if err != nil || string(body) != "payload" {
		t.Fatalf("body=%q err=%v", body, err)
	}
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("12345678901")), ContentLength: -1, Request: r}, nil
	})
	overflow := filepath.Join(t.TempDir(), "overflow")
	if err := downloadReleaseFile(context.Background(), "https://github.com/release", "", overflow, 10); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("overflow err=%v", err)
	}
	if _, err := os.Stat(overflow); !os.IsNotExist(err) {
		t.Fatalf("partial download remains: %v", err)
	}
}
