package appbundle

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const moraAppName = "Mora.app"

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
		`Replacing stale or damaged Mora.app`,
		`PREVIOUS_APP="$APP_REPLACE_DIR/Mora.app"`,
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
		"VERSION=0.12.0",
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

func TestInstallAppScriptRepairsDamagedBundleAndPathDrift(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(app, "old-marker"), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(legacy, []byte("drifted local build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy+".standalone-backup", []byte("original migration backup"), 0o755); err != nil {
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
		"MOCK_INSTALLED_INVALID=1",
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install-app.sh repair failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "replaced stale or damaged Mora.app") {
		t.Fatalf("repair output omitted replacement receipt: %s", output)
	}
	if _, err := os.Lstat(filepath.Join(app, "old-marker")); !os.IsNotExist(err) {
		t.Fatalf("damaged app marker survived replacement: %v", err)
	}
	linkTarget, err := os.Readlink(legacy)
	if err != nil {
		t.Fatalf("drifted PATH entry was not repaired to a symlink: %v", err)
	}
	if want := filepath.Join(app, "Contents", "MacOS", "mora"); linkTarget != want {
		t.Fatalf("PATH symlink = %q, want %q", linkTarget, want)
	}
	assertFileBody(t, legacy+".standalone-backup", "original migration backup")
	assertFileBody(t, legacy+".standalone-backup.1", "drifted local build")
}

func TestInstallAppScriptRestoresPreviousBundleWhenReplacementIsInterrupted(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(app, "old-marker"), []byte("previous app"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(legacy, []byte("path entry must remain untouched"), 0o755); err != nil {
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
		"MOCK_INSTALLED_INVALID=1",
		"MOCK_INTERRUPT_AFTER_INSTALL=1",
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("interrupted replacement unexpectedly succeeded:\n%s", output)
	}
	assertFileBody(t, filepath.Join(app, "old-marker"), "previous app")
	assertFileBody(t, legacy, "path entry must remain untouched")
	if matches, globErr := filepath.Glob(filepath.Join(appParent, ".mora-app.replace.*")); globErr != nil || len(matches) != 0 {
		t.Fatalf("interrupted replacement left recovery directories %v (glob err=%v)\n%s", matches, globErr, output)
	}
}

func TestInstallAppScriptRefusesToDowngradeValidSignedBundle(t *testing.T) {
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
	app := writeRunnableAppLayout(t, appParent, "0.12.2")
	if err := os.WriteFile(filepath.Join(app, "newer-marker"), []byte("valid newer app"), 0o600); err != nil {
		t.Fatal(err)
	}
	mockBin := filepath.Join(home, "mock-bin")
	linkDir := filepath.Join(home, "path-bin")
	for _, directory := range []string{mockBin, linkDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeAppInstallerMocks(t, mockBin)
	legacy := filepath.Join(linkDir, "mora")
	if err := os.WriteFile(legacy, []byte("path entry must remain untouched"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", script)
	command.Env = []string{
		"PATH=" + mockBin + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"PREFIX=" + linkDir,
		"MORA_APP_DIR=" + appParent,
		"MORA_VAULT=" + filepath.Join(home, "vault", "mora"),
		"VERSION=0.12.1",
		"MOCK_INSTALLED_VERSION=0.12.2",
		"LANG=C",
	}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "refusing to downgrade signed Mora.app from 0.12.2 to 0.12.1") {
		t.Fatalf("downgrade run err=%v output=%s", err, output)
	}
	assertFileBody(t, filepath.Join(app, "newer-marker"), "valid newer app")
	assertFileBody(t, legacy, "path entry must remain untouched")
	if _, err := os.Lstat(legacy + ".standalone-backup"); !os.IsNotExist(err) {
		t.Fatalf("downgrade refusal created a PATH backup: %v", err)
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
  CFBundleShortVersionString|CFBundleVersion) printf '%s\n' "${MOCK_INSTALLED_VERSION:-0.12.0}" ;;
  LSUIElement) printf 'true\n' ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "codesign"), `#!/bin/sh
team="${MOCK_TEAM:-VS8M5VJBZ5}"
target=""
for argument in "$@"; do target="$argument"; done
case "$1" in
  -dvvv)
    printf 'Identifier=com.pyranthus.mora\nAuthority=Developer ID Application: Test (%s)\nTeamIdentifier=%s\nCodeDirectory flags=0x10000(runtime)\nTimestamp=now\n' "$team" "$team" >&2
    ;;
  -d)
    printf 'designated => identifier "com.pyranthus.mora" and certificate leaf[subject.OU] = "%s"\n' "$team" >&2
    ;;
  *)
    if [ "${MOCK_INSTALLED_INVALID:-0}" = 1 ] && [ "$target" = "$MORA_APP_DIR/Mora.app" ] && [ -f "$target/old-marker" ]; then
      exit 1
    fi
    if [ "${MOCK_INTERRUPT_AFTER_INSTALL:-0}" = 1 ] && [ "$target" = "$MORA_APP_DIR/Mora.app" ] && [ ! -f "$target/old-marker" ]; then
      kill -TERM "$PPID"
      sleep 1
      exit 1
    fi
    exit 0
    ;;
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
