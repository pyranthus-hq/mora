package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectTree(t *testing.T) string {
	t.Helper()
	w, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"a/b/c", "a/bc"} {
		if err := os.MkdirAll(filepath.Join(w, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func which(t *testing.T, dir string) (string, bool) {
	t.Helper()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	scope, _, found, err := lookupProject(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	return scope, found
}

func TestProjectBindListLookup(t *testing.T) {
	withTempHome(t)
	w := projectTree(t)
	run(t, "project", "bind", "--scope", "project:alpha", filepath.Join(w, "a"))
	run(t, "project", "bind", "--scope", "project:beta", filepath.Join(w, "a/b"))

	var list []projectBinding
	if err := json.Unmarshal([]byte(run(t, "project", "list", "--json")), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0] != (projectBinding{filepath.Join(w, "a"), "project:alpha"}) {
		t.Fatalf("list = %+v", list)
	}
	for dir, want := range map[string]string{"a/b/c": "project:beta", "a/b": "project:beta", "a/bc": "project:alpha", "a": "project:alpha"} {
		if got, found := which(t, filepath.Join(w, dir)); !found || got != want {
			t.Errorf("%s: got %q found=%v want %q", dir, got, found, want)
		}
	}
	if _, found := which(t, w); found {
		t.Error("unbound folder must not match")
	}
	if got, found := which(t, filepath.Join(w, "a/b/not-yet")); !found || got != "project:beta" {
		t.Errorf("missing subfolder: %q %v", got, found)
	}
}

func TestProjectBindSiblingPrefixDoesNotMatch(t *testing.T) {
	withTempHome(t)
	w := projectTree(t)
	run(t, "project", "bind", "--scope", "project:beta", filepath.Join(w, "a/b"))
	if _, found := which(t, filepath.Join(w, "a/bc")); found {
		t.Error("/a/b must not match /a/bc")
	}
}

func TestProjectBindSymlink(t *testing.T) {
	withTempHome(t)
	w := projectTree(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(w, "a"), link); err != nil {
		t.Skip("symlinks unavailable")
	}
	run(t, "project", "bind", "--scope", "project:alpha", link)
	if got, found := which(t, filepath.Join(w, "a/b")); !found || got != "project:alpha" {
		t.Errorf("real path: %q %v", got, found)
	}
	if got, found := which(t, filepath.Join(link, "b")); !found || got != "project:alpha" {
		t.Errorf("link path: %q %v", got, found)
	}
}

func TestProjectBindRebindUnbindAndDefaultScope(t *testing.T) {
	withTempHome(t)
	w := projectTree(t)
	a := filepath.Join(w, "a")
	run(t, "project", "bind", "--scope", "project:alpha", a)
	if out := run(t, "project", "bind", "--scope", "project:gamma", a); !strings.Contains(out, "replaced project:alpha") {
		t.Errorf("rebind output: %q", out)
	}
	if got, _ := which(t, a); got != "project:gamma" {
		t.Errorf("rebind scope = %q", got)
	}
	run(t, "project", "unbind", a)
	if _, found := which(t, a); found {
		t.Error("still bound after unbind")
	}
	if err := Run(testCtx(t), []string{"project", "unbind", a}, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader("")); err == nil {
		t.Error("unbind of unbound folder must fail")
	}
	upper := filepath.Join(w, "MyApp")
	if err := os.Mkdir(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "project", "bind", upper)
	if got, _ := which(t, upper); got != "project:myapp" {
		t.Errorf("default scope = %q", got)
	}
}

func TestProjectBindRefusals(t *testing.T) {
	withTempHome(t)
	w := projectTree(t)
	for _, scope := range []string{"global", "personal", "project:", "project", "project:a/../b", "alpha"} {
		err := Run(testCtx(t), []string{"project", "bind", "--scope", scope, w}, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
		if err == nil {
			t.Errorf("scope %q must be refused", scope)
		}
	}
	err := Run(testCtx(t), []string{"project", "bind", "--scope", "project:x", filepath.Join(w, "missing")}, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	if err == nil {
		t.Error("missing folder must be refused")
	}
	if got := run(t, "project", "list"); got != "" {
		t.Errorf("refusals stored something: %q", got)
	}
}

func TestProjectBindStoreSurvivesReload(t *testing.T) {
	withTempHome(t)
	w := projectTree(t)
	run(t, "project", "bind", "--scope", "project:alpha", filepath.Join(w, "a"))
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := loadProjectBindings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadProjectBindings(cfg)
	if err != nil || len(first) != 1 || first[0] != second[0] {
		t.Fatalf("reload: %+v %+v %v", first, second, err)
	}
	if info, err := os.Stat(projectBindingsPath(cfg)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("store mode: %v %v", info, err)
	}
}

func TestProjectBindHookInstallRecordsBinding(t *testing.T) {
	withTempHookHome(t)
	project := t.TempDir()
	t.Chdir(project)
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	run(t, "hook", "install", "--project", "--scope", "project:demo")
	if got, found := which(t, filepath.Join(project, "src")); !found || got != "project:demo" {
		t.Errorf("hook install binding: %q %v", got, found)
	}
	run(t, "hook", "uninstall", "--project")
	if _, found := which(t, project); !found {
		t.Error("hook uninstall --project must leave the binding")
	}
}
