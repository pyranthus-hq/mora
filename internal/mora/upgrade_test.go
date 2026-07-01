package mora

import (
	"archive/zip"
	"bytes"
	"io"
	"path/filepath"
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
