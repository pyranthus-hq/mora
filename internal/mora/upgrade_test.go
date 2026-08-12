package mora

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creativeprojects/go-selfupdate"
)

func TestIsHomebrewManaged(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"cask symlink target", "/opt/homebrew/Caskroom/mora/0.2.0/mora", true},
		{"intel cask", "/usr/local/Caskroom/mora/0.2.0/mora", true},
		{"formula cellar", "/opt/homebrew/Cellar/mora/0.2.0/bin/mora", true},
		{"intel cellar", "/usr/local/Cellar/mora/0.2.0/bin/mora", true},
		// install.sh drops a REAL file in these dirs — not Homebrew-managed.
		{"self-install opt-homebrew bin", "/opt/homebrew/bin/mora", false},
		{"self-install usr-local bin", "/usr/local/bin/mora", false},
		{"self-install local bin", "/Users/neil/.local/bin/mora", false},
		{"go build in repo", "/Users/dev/src/mora/mora", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHomebrewManaged(tc.path); got != tc.want {
				t.Fatalf("isHomebrewManaged(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestClassifyUpgradeInstall(t *testing.T) {
	oldVersion := BuildVersion
	t.Cleanup(func() { BuildVersion = oldVersion })
	cases := []struct {
		name, version, exe string
		want               upgradeInstallRoute
	}{
		{"signed app", "1.2.3", "/Applications/Mora.app/Contents/MacOS/mora", upgradeRouteApp},
		{"Caskroom app", "1.2.3", "/opt/homebrew/Caskroom/mora/1.2.3/Mora.app/Contents/MacOS/mora", upgradeRouteApp},
		{"raw formula", "1.2.3", "/opt/homebrew/Cellar/mora/1.2.3/bin/mora", upgradeRouteHomebrew},
		{"source", "dev", "/Users/dev/src/mora/mora", upgradeRouteSource},
		{"release archive", "1.2.3", "/Users/me/.local/bin/mora", upgradeRouteDirect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			BuildVersion = tc.version
			if got := classifyUpgradeInstall(tc.exe); got != tc.want {
				t.Fatalf("classifyUpgradeInstall(%q) = %q, want %q", tc.exe, got, tc.want)
			}
		})
	}
}

func TestCmdUpgradeRoutesHomebrewWithoutNetwork(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	oldExe, oldVersion := upgradeExecutable, BuildVersion
	t.Cleanup(func() { upgradeExecutable, BuildVersion = oldExe, oldVersion })
	BuildVersion = "1.2.3"
	upgradeExecutable = func() (string, error) { return "/opt/homebrew/Cellar/mora/1.2.3/bin/mora", nil }
	var out bytes.Buffer
	if err := cmdUpgrade(context.Background(), []string{"--check"}, &out); err != nil {
		t.Fatalf("cmdUpgrade: %v", err)
	}
	if !strings.Contains(out.String(), "brew upgrade pyranthus-hq/tap/mora") {
		t.Fatalf("Homebrew route output = %q", out.String())
	}
}

func TestLocalBuildBase(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		wantBase string
		wantOK   bool
	}{
		{"git-describe ahead of tag", "v0.10.0-60-g2d08334", "v0.10.0", true},
		{"git-describe dirty", "v0.10.0-60-g2d08334-dirty", "v0.10.0", true},
		{"dirty on exact tag", "v0.10.0-dirty", "v0.10.0", true},
		{"long sha", "v0.9.1-5-g2d083341c2a94b7e9d3f5a6b7c8d9e0f1a2b3c4d", "v0.9.1", true},
		{"full sha-256", "v0.9.1-5-g" + strings.Repeat("2d08fe1c", 8), "v0.9.1", true},
		{"clean release", "v0.10.0", "", false},
		{"clean release without v", "0.9.1", "", false},
		{"prerelease tag is not a local build", "v0.10.0-rc1", "", false},
		{"literal dev", "dev", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := localBuildBase(tc.version)
			if base != tc.wantBase || ok != tc.wantOK {
				t.Fatalf("localBuildBase(%q) = (%q, %v), want (%q, %v)", tc.version, base, ok, tc.wantBase, tc.wantOK)
			}
		})
	}
}

func TestDecideUpgrade(t *testing.T) {
	cases := []struct {
		name        string
		current     string
		latest      string
		wantVerdict upgradeVerdict
		wantLocal   bool
		wantErr     bool
	}{
		// The live bug: a local build 60 commits past the v0.10.0 tag must
		// NOT be offered the older v0.10.0 release as an "update".
		{"local build ahead of equal base tag", "v0.10.0-60-g2d08334", "0.10.0", verdictLocalAhead, true, false},
		{"local build ahead, dirty", "v0.10.0-60-g2d08334-dirty", "0.10.0", verdictLocalAhead, true, false},
		{"dirty exact tag", "v0.10.0-dirty", "0.10.0", verdictLocalAhead, true, false},
		{"local build with base past latest", "v0.11.0-2-gabc1234", "0.10.0", verdictLocalAhead, true, false},
		{"local build behind latest", "v0.9.1-5-gabc1234", "0.10.0", verdictUpgrade, true, false},
		{"clean release behind latest", "0.9.1", "0.10.0", verdictUpgrade, false, false},
		{"clean release current", "0.10.0", "0.10.0", verdictUpToDate, false, false},
		{"clean release ahead of latest", "0.11.0", "0.10.0", verdictUpToDate, false, false},
		// "dev" never reaches decideUpgrade (cmdUpgrade refuses it first) —
		// if it somehow did, fail loudly instead of comparing garbage.
		{"literal dev fails parse", "dev", "0.10.0", 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, isLocal, err := decideUpgrade(tc.current, tc.latest)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decideUpgrade(%q, %q) expected an error, got (%v, %v)", tc.current, tc.latest, verdict, isLocal)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideUpgrade(%q, %q): %v", tc.current, tc.latest, err)
			}
			if verdict != tc.wantVerdict || isLocal != tc.wantLocal {
				t.Fatalf("decideUpgrade(%q, %q) = (%v, %v), want (%v, %v)", tc.current, tc.latest, verdict, isLocal, tc.wantVerdict, tc.wantLocal)
			}
		})
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "tok"); got != "tok" {
		t.Fatalf("got %q, want tok", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestUpgradeOldSavePathWindows(t *testing.T) {
	withRuntimeGOOS(t, "windows")
	exe := filepath.Join("C:", "Users", "Adit", "AppData", "Local", "Mora", "bin", "mora.exe")
	want := filepath.Join(filepath.Dir(exe), "mora.exe.old")
	if got := upgradeOldSavePath(exe); got != want {
		t.Fatalf("upgradeOldSavePath(%q) = %q, want %q", exe, got, want)
	}
}

func TestUpgradeOldSavePathNonWindowsUsesLibraryDefault(t *testing.T) {
	withRuntimeGOOS(t, "darwin")
	if got := upgradeOldSavePath("/Users/adit/.local/bin/mora"); got != "" {
		t.Fatalf("upgradeOldSavePath on darwin = %q, want empty library default", got)
	}
}

func TestGoSelfupdateFindsWindowsExeInZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("mora.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("new windows binary")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := selfupdate.DecompressCommand(bytes.NewReader(buf.Bytes()), "mora_1.2.3_windows_amd64.zip", "mora.exe", "windows", "amd64")
	if err != nil {
		t.Fatalf("go-selfupdate should find mora.exe in windows zip: %v", err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new windows binary" {
		t.Fatalf("decompressed body = %q", body)
	}
}
