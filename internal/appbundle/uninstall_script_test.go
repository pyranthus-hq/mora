package appbundle

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUninstallAppScriptRemovesOnlyVerifiedAppAndManagedLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX uninstaller")
	}
	script := uninstallAppScriptPath(t)
	home := t.TempDir()
	appParent := filepath.Join(home, "Applications")
	app := writeRunnableAppLayout(t, appParent, "0.12.0")
	linkDir := filepath.Join(home, "bin")
	mockBin := filepath.Join(home, "mock-bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mockBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeUninstallAppMocks(t, mockBin)
	link := filepath.Join(linkDir, "mora")
	if err := os.Symlink(filepath.Join(app, "Contents", "MacOS", "mora"), link); err != nil {
		t.Fatal(err)
	}
	backup := link + ".standalone-backup"
	if err := os.WriteFile(backup, []byte("preserved standalone"), 0o755); err != nil {
		t.Fatal(err)
	}
	vaultMarker := filepath.Join(home, "vault", "mora", "marker")
	if err := os.MkdirAll(filepath.Dir(vaultMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultMarker, []byte("private memory"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", script)
	command.Env = uninstallAppTestEnv(home, appParent, linkDir, mockBin, "VS8M5VJBZ5")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("uninstall-app.sh failed: %v\n%s", err, output)
	}
	if _, err := os.Lstat(app); !os.IsNotExist(err) {
		t.Fatalf("Mora.app still exists after uninstall: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("managed PATH symlink still exists: %v", err)
	}
	assertFileBody(t, backup, "preserved standalone")
	assertFileBody(t, vaultMarker, "private memory")
	if !strings.Contains(string(output), "vault, config, state") {
		t.Fatalf("uninstaller did not report preserved data: %s", output)
	}
}

func TestUninstallAppScriptRejectsWrongTeamBeforeMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX uninstaller")
	}
	script := uninstallAppScriptPath(t)
	home := t.TempDir()
	appParent := filepath.Join(home, "Applications")
	app := writeRunnableAppLayout(t, appParent, "0.12.0")
	linkDir := filepath.Join(home, "bin")
	mockBin := filepath.Join(home, "mock-bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mockBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeUninstallAppMocks(t, mockBin)
	link := filepath.Join(linkDir, "mora")
	if err := os.Symlink(filepath.Join(app, "Contents", "MacOS", "mora"), link); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", script)
	command.Env = uninstallAppTestEnv(home, appParent, linkDir, mockBin, "ABCDEFGHIJ")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "wrong Apple team") {
		t.Fatalf("wrong-team run err=%v output=%s", err, output)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatalf("wrong-team run changed Mora.app: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(app, "Contents", "MacOS", "mora") {
		t.Fatalf("wrong-team run changed managed link: target=%q err=%v", target, err)
	}
}

func TestUninstallAppScriptRollsBackWhenManagedLinkRemovalFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX uninstaller")
	}
	script := uninstallAppScriptPath(t)
	home := t.TempDir()
	appParent := filepath.Join(home, "Applications")
	app := writeRunnableAppLayout(t, appParent, "0.12.0")
	firstLinkDir := filepath.Join(home, "first-bin")
	secondLinkDir := filepath.Join(home, "second-bin")
	mockBin := filepath.Join(home, "mock-bin")
	for _, directory := range []string{firstLinkDir, secondLinkDir, mockBin} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeUninstallAppMocks(t, mockBin)
	firstLink := filepath.Join(firstLinkDir, "mora")
	secondLink := filepath.Join(secondLinkDir, "mora")
	appExecutable := filepath.Join(app, "Contents", "MacOS", "mora")
	for _, link := range []string{firstLink, secondLink} {
		if err := os.Symlink(appExecutable, link); err != nil {
			t.Fatal(err)
		}
	}

	command := exec.Command("sh", script)
	command.Env = []string{
		"PATH=" + mockBin + ":" + firstLinkDir + ":" + secondLinkDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"MORA_APP_DIR=" + appParent,
		"MOCK_TEAM=VS8M5VJBZ5",
		"MOCK_UNLINK_FAIL_PATH=" + secondLink,
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "restored") {
		t.Fatalf("unlink-failure run err=%v output=%s", err, output)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatalf("rollback did not restore Mora.app: %v", err)
	}
	for _, link := range []string{firstLink, secondLink} {
		if target, err := os.Readlink(link); err != nil || target != appExecutable {
			t.Fatalf("rollback did not preserve managed link %s: target=%q err=%v", link, target, err)
		}
	}
	stages, err := filepath.Glob(filepath.Join(appParent, ".mora-app-uninstall.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("rollback left private staging paths: %v", stages)
	}
}

func TestUninstallAppScriptFindsManagedLinkHiddenLaterOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX uninstaller")
	}
	script := uninstallAppScriptPath(t)
	home := t.TempDir()
	appParent := filepath.Join(home, "Applications")
	app := writeRunnableAppLayout(t, appParent, "0.12.0")
	shadowDir := filepath.Join(home, "shadow-bin")
	managedDir := filepath.Join(home, "managed-bin")
	mockBin := filepath.Join(home, "mock-bin")
	for _, directory := range []string{shadowDir, managedDir, mockBin} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeUninstallAppMocks(t, mockBin)
	shadow := filepath.Join(shadowDir, "mora")
	writeExecutable(t, shadow, "#!/bin/sh\nexit 0\n")
	managedLink := filepath.Join(managedDir, "mora")
	if err := os.Symlink(filepath.Join(app, "Contents", "MacOS", "mora"), managedLink); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", script)
	command.Env = []string{
		"PATH=" + mockBin + ":" + shadowDir + ":" + managedDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"MORA_APP_DIR=" + appParent,
		"MOCK_TEAM=VS8M5VJBZ5",
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("uninstall-app.sh failed: %v\n%s", err, output)
	}
	if _, err := os.Lstat(managedLink); !os.IsNotExist(err) {
		t.Fatalf("managed link hidden later on PATH still exists: %v", err)
	}
	if _, err := os.Stat(shadow); err != nil {
		t.Fatalf("uninstaller changed unrelated active mora: %v", err)
	}
}

func TestUninstallAppScriptContract(t *testing.T) {
	bodyBytes, err := os.ReadFile(uninstallAppScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	for _, required := range []string{
		`APP_DEST="$APP_PARENT/Mora.app"`,
		`CFBundleIdentifier`,
		`TeamIdentifier=$MACOS_TEAM_ID`,
		`codesign --verify --deep --strict`,
		`readlink "$link"`,
		`= "$APP_EXECUTABLE"`,
		`find "$DETACHED_APP" -depth -delete`,
		`rollback_detach`,
		`vault, config, state`,
		`.standalone-backup`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("uninstall-app.sh missing contract text %q", required)
		}
	}
	for _, forbidden := range []string{
		`rm -rf "$HOME"`,
		`rm -rf "$APP_PARENT"`,
		`xattr -d`,
		`codesign --force --sign`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("uninstall-app.sh contains forbidden mutation %q", forbidden)
		}
	}
}

func uninstallAppScriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "uninstall-app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func uninstallAppTestEnv(home, appParent, linkDir, mockBin, team string) []string {
	return []string{
		"PATH=" + mockBin + ":" + linkDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"PREFIX=" + linkDir,
		"MORA_APP_DIR=" + appParent,
		"MOCK_TEAM=" + team,
		"LANG=C",
	}
}

func writeUninstallAppMocks(t *testing.T, directory string) {
	t.Helper()
	writeExecutable(t, filepath.Join(directory, "uname"), "#!/bin/sh\nprintf 'Darwin\\n'\n")
	writeExecutable(t, filepath.Join(directory, "plutil"), `#!/bin/sh
case "$2" in
  CFBundleIdentifier) printf 'com.pyranthus.mora\n' ;;
  CFBundleExecutable) printf 'mora\n' ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "codesign"), `#!/bin/sh
team="${MOCK_TEAM:-VS8M5VJBZ5}"
case "$1" in
  -dvvv)
    printf 'Identifier=com.pyranthus.mora\nAuthority=Developer ID Application: Test (%s)\nTeamIdentifier=%s\n' "$team" "$team" >&2
    ;;
  *) exit 0 ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "unlink"), `#!/bin/sh
if [ -n "${MOCK_UNLINK_FAIL_PATH:-}" ] && [ "$1" = "$MOCK_UNLINK_FAIL_PATH" ]; then
  exit 73
fi
exec /bin/unlink "$@"
`)
}
