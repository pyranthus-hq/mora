package mora

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// windowsDriveForms returns the same existing folder spelled the ways a
// Windows caller hands it over: backslashes, forward slashes, a trailing
// separator, all lower case and a flipped drive-letter case.
func windowsDriveForms(dir string) map[string]string {
	flipped := dir
	if c := dir[0]; c >= 'a' && c <= 'z' {
		flipped = strings.ToUpper(dir[:1]) + dir[1:]
	} else {
		flipped = strings.ToLower(dir[:1]) + dir[1:]
	}
	return map[string]string{
		"backslash":       dir,
		"forward slashes": filepath.ToSlash(dir),
		"trailing slash":  dir + `\`,
		"lower case":      strings.ToLower(dir),
		"drive case":      flipped,
	}
}

// windowsProjectFolder makes a folder with a space in its name and a subfolder,
// on a real drive of a real Windows host.
func windowsProjectFolder(t *testing.T) (dir, sub string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("needs a real Windows drive path")
	}
	dir = filepath.Join(t.TempDir(), "My App")
	sub = filepath.Join(dir, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) || filepath.VolumeName(dir) == "" {
		t.Fatalf("test folder %q is not a drive path", dir)
	}
	return dir, sub
}

// A project folder given as a Windows drive path binds, and the same folder
// found again through every spelling of that path, or through a subfolder.
func TestWindowsProjectBindAndLookupDrivePath(t *testing.T) {
	dir, sub := windowsProjectFolder(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	out := run(t, "project", "bind", "--scope", "project:app", dir)
	if !strings.Contains(out, "project:app") {
		t.Fatalf("bind output = %q", out)
	}
	bindings, err := loadProjectBindings(cfg)
	if err != nil || len(bindings) != 1 || bindings[0].Scope != "project:app" {
		t.Fatalf("bindings = %+v err=%v", bindings, err)
	}
	if !filepath.IsAbs(bindings[0].Dir) || filepath.Clean(bindings[0].Dir) != bindings[0].Dir || filepath.VolumeName(bindings[0].Dir) == "" {
		t.Fatalf("stored folder %q is not a clean drive path", bindings[0].Dir)
	}

	for name, form := range windowsDriveForms(dir) {
		scope, root, found, err := lookupProject(cfg, form)
		if err != nil || !found || scope != "project:app" || root != bindings[0].Dir {
			t.Errorf("%s: lookup(%q) = %q %q found=%v err=%v", name, form, scope, root, found, err)
		}
	}
	for name, form := range windowsDriveForms(sub) {
		if scope, _, found, err := lookupProject(cfg, form); err != nil || !found || scope != "project:app" {
			t.Errorf("subfolder %s: lookup(%q) = %q found=%v err=%v", name, form, scope, found, err)
		}
	}
	// A folder that does not exist yet still resolves to the bound project.
	if _, _, found, err := lookupProject(cfg, filepath.Join(sub, "not-yet")); err != nil || !found {
		t.Errorf("missing child of a bound folder: found=%v err=%v", found, err)
	}
	// A sibling that only shares a name prefix is not the project.
	sibling := dir + "-other"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := lookupProject(cfg, sibling); err != nil || found {
		t.Errorf("sibling %q: found=%v err=%v", sibling, found, err)
	}

	// Binding the same folder through another spelling replaces, not adds.
	run(t, "project", "bind", "--scope", "project:app2", filepath.ToSlash(strings.ToLower(dir)))
	bindings, err = loadProjectBindings(cfg)
	if err != nil || len(bindings) != 1 || bindings[0].Scope != "project:app2" {
		t.Fatalf("a respelled path made a second binding: %+v err=%v", bindings, err)
	}
	if which := run(t, "project", "which", filepath.ToSlash(sub)); !strings.HasPrefix(which, "project:app2\t") {
		t.Fatalf("project which = %q", which)
	}
}

// mora mcp serve --project-dir with a native Windows path binds the server to
// the folder's project, whichever way the path is spelled.
func TestWindowsMCPServeProjectDirNativePath(t *testing.T) {
	dir, sub := windowsProjectFolder(t)
	mcpProjectSetup(t)
	run(t, "project", "bind", "--scope", "project:winapp", dir)

	for name, form := range windowsDriveForms(dir) {
		replies, stderr := mcpProjectServe(t, []string{"--project-dir", form})
		if strings.Contains(stderr, "not bound") || !strings.Contains(replies[0].Result.Instructions, `"project:winapp"`) {
			t.Errorf("%s: --project-dir %q did not bind the server; stderr=%q instructions=%q", name, form, stderr, replies[0].Result.Instructions)
		}
	}
	replies, stderr := mcpProjectServe(t, []string{"--project-dir", sub})
	if strings.Contains(stderr, "not bound") || !strings.Contains(replies[0].Result.Instructions, `"project:winapp"`) {
		t.Errorf("a subfolder must bind too; stderr=%q instructions=%q", stderr, replies[0].Result.Instructions)
	}
	other := t.TempDir()
	replies, _ = mcpProjectServe(t, []string{"--project-dir", other})
	if strings.Contains(replies[0].Result.Instructions, "project:winapp") {
		t.Errorf("an unbound Windows folder was given the project: %q", replies[0].Result.Instructions)
	}
}

// The Claude Code hooks decide whether a session's cwd is inside the project
// folder. Claude Code on Windows reports cwd with backslashes.
func TestWindowsHookWithinProjectDrivePaths(t *testing.T) {
	dir, sub := windowsProjectFolder(t)
	for name, form := range windowsDriveForms(sub) {
		if !hookWithinProject(dir, form) {
			t.Errorf("%s: cwd %q is inside %q", name, form, dir)
		}
	}
	if !hookWithinProject(strings.ToLower(dir), sub) || !hookWithinProject(filepath.ToSlash(dir), sub) {
		t.Error("a differently spelled root must still contain its subfolder")
	}
	if hookWithinProject(dir, dir+"-other") {
		t.Error("a sibling that shares a name prefix is outside the project")
	}
	if hookWithinProject(dir, `relative\path`) {
		t.Error("a relative cwd is never inside")
	}
}
