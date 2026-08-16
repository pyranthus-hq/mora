package binupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"github.com/creativeprojects/go-selfupdate"
	"io"
	"path/filepath"
	"strings"
	"testing"
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
			if got := IsHomebrewManaged(tc.path); got != tc.want {
				t.Fatalf("IsHomebrewManaged(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
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
			base, ok := LocalBuildBase(tc.version)
			if base != tc.wantBase || ok != tc.wantOK {
				t.Fatalf("LocalBuildBase(%q) = (%q, %v), want (%q, %v)", tc.version, base, ok, tc.wantBase, tc.wantOK)
			}
		})
	}
}
func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "", "tok"); got != "tok" {
		t.Fatalf("got %q, want tok", got)
	}
	if got := FirstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
func TestUpgradeOldSavePathWindows(t *testing.T) {
	exe := filepath.Join("C:", "Users", "Adit", "AppData", "Local", "Mora", "bin", "mora.exe")
	want := filepath.Join(filepath.Dir(exe), "mora.exe.old")
	if got := OldSavePath(exe, "windows"); got != want {
		t.Fatalf("upgradeOldSavePath(%q) = %q, want %q", exe, got, want)
	}
}
func TestUpgradeOldSavePathNonWindowsUsesLibraryDefault(t *testing.T) {
	if got := OldSavePath("/Users/adit/.local/bin/mora", "darwin"); got != "" {
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

func TestRunDecisionAndApplyBranches(t *testing.T) {
	release := Release{Version: "1.2.0", AssetName: "mora_linux_amd64.tar.gz"}
	cases := []struct {
		name, current                      string
		found, check, applyErr, rebuildErr bool
		want                               string
		wantErr                            bool
	}{{"none", "1.0.0", false, false, false, false, "no published releases found", false}, {"ahead", "v2.0.0-1-gabcde", true, false, false, false, "local build ahead", false}, {"current", "1.2.0", true, false, false, false, "up to date", false}, {"check", "1.0.0", true, true, false, false, "run `mora upgrade`", false}, {"apply", "1.0.0", true, false, false, true, "updated mora to 1.2.0", false}, {"apply error", "1.0.0", true, false, true, false, "", true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			applied := 0
			o := Options{Current: tc.current, Executable: "/tmp/mora", GOOS: "linux", CheckOnly: tc.check, Stdout: &out, Detect: func(context.Context) (Release, bool, error) { return release, tc.found, nil }, Apply: func(context.Context, Release, string) error {
				applied++
				if tc.applyErr {
					return errors.New("swap")
				}
				return nil
			}, PostRebuild: func(context.Context, string, io.Writer) error {
				if tc.rebuildErr {
					return errors.New("rebuild")
				}
				return nil
			}}
			err := Run(context.Background(), o)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if tc.want != "" && !strings.Contains(out.String(), tc.want) {
				t.Fatalf("out=%q want %q", out.String(), tc.want)
			}
			if tc.check && applied != 0 {
				t.Fatal("check-only applied")
			}
		})
	}
}

func TestClassifyRoutes(t *testing.T) {
	isApp := func(p string) bool { return strings.Contains(p, "Mora.app") }
	cases := []struct {
		exe, version string
		want         Route
	}{{"/Applications/Mora.app/Contents/MacOS/mora", "1.0.0", RouteApp}, {"/opt/homebrew/Cellar/mora/1/bin/mora", "1.0.0", RouteHomebrew}, {"/tmp/mora", "dev", RouteSource}, {"/tmp/mora", "v1.0.0-2-gabcde", RouteSource}, {"/tmp/mora", "1.0.0", RouteDirect}}
	for _, tc := range cases {
		if got := Classify(tc.exe, tc.version, isApp); got != tc.want {
			t.Errorf("Classify(%q,%q)=%q want %q", tc.exe, tc.version, got, tc.want)
		}
	}
}
