package mora

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// skipWSLSimulationOnWindows skips a test that stands in for Mora running in
// WSL on a Linux host. On a Windows host every temp dir is itself a drive path
// (C:\...), so the simulated drive rewrite cannot tell the test's own folders
// from the Windows paths under test. A Windows host is covered by the tests in
// windows_paths_test.go, which use real drive paths.
func skipWSLSimulationOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("simulates WSL on a Linux host; a Windows temp dir is itself a drive path (see windows_paths_test.go)")
	}
}

func withWSLEnv(t *testing.T, goos, mountRoot string) {
	t.Helper()
	prevOS, prevRoot := wslHostOS, wslMountRoot
	wslHostOS, wslMountRoot = goos, mountRoot
	t.Cleanup(func() { wslHostOS, wslMountRoot = prevOS, prevRoot })
	withWSLDistro(t, "")
}

// withWSLDistro stands in for the WSL_DISTRO_NAME of the machine running the tests.
func withWSLDistro(t *testing.T, name string) {
	t.Helper()
	prev := wslDistro
	wslDistro = func() string { return name }
	t.Cleanup(func() { wslDistro = prev })
}

func TestNormalizeWSLWindowsPath(t *testing.T) {
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	withWSLEnv(t, "linux", mnt)
	cases := map[string]string{
		`C:\Users\Adit\my-app`:  filepath.Join(mnt, "c", "Users", "Adit", "my-app"),
		`c:/Users/Adit/my-app`:  filepath.Join(mnt, "c", "Users", "Adit", "my-app"),
		`C:\Users\Adit\my-app\`: filepath.Join(mnt, "c", "Users", "Adit", "my-app"),
		`C:\`:                   filepath.Join(mnt, "c"),
		`C:`:                    filepath.Join(mnt, "c"),
		// Not a drive path, or the drive is not mounted: unchanged.
		`D:\data`:           `D:\data`,
		`C:relative`:        `C:relative`,
		`/home/me/proj`:     `/home/me/proj`,
		`relative/dir`:      `relative/dir`,
		``:                  ``,
		`\\server\share\x`:  `\\server\share\x`,
		`1:\not-a-drive`:    `1:\not-a-drive`,
		`/mnt/c/Users/Adit`: `/mnt/c/Users/Adit`,
	}
	for in, want := range cases {
		if got := normalizeWSLWindowsPath(in); got != want {
			t.Errorf("normalizeWSLWindowsPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeWSLWindowsPathNoOpOnWindows(t *testing.T) {
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	withWSLEnv(t, "windows", mnt)
	if got := normalizeWSLWindowsPath(`C:\Users\Adit`); got != `C:\Users\Adit` {
		t.Fatalf("must not touch a Windows path on Windows, got %q", got)
	}
}

func TestNormalizeWSLWindowsPathNoOpWithoutMount(t *testing.T) {
	withWSLEnv(t, "linux", filepath.Join(t.TempDir(), "no-mnt"))
	if got := normalizeWSLWindowsPath(`C:\Users\Adit`); got != `C:\Users\Adit` {
		t.Fatalf("plain Linux (no /mnt/c) must be a no-op, got %q", got)
	}
}

// A Windows path handed to bind resolves to the WSL folder, so a hook that
// reports workspacePaths in Windows form finds the same binding.
func TestProjectBindAndLookupAcceptWindowsDrivePath(t *testing.T) {
	skipWSLSimulationOnWindows(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	mnt := t.TempDir()
	dir := filepath.Join(mnt, "c", "Users", "Adit", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	withWSLEnv(t, "linux", mnt)
	if _, _, err := bindProjectDir(testCtx(t), `C:\Users\Adit\app`, "project:app"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	scope, _, found, err := lookupProject(cfg, normalizeWSLWindowsPath(`C:/Users/Adit/app`))
	if err != nil || !found || scope != "project:app" {
		t.Fatalf("lookup via Windows path = %q found=%v err=%v", scope, found, err)
	}
}
