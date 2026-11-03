package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const userBashGroup = `{"matcher":"Bash","future_group":1,"hooks":[{"type":"command","command":"user-guard","timeout":2}]}`

func writePreToolFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	body := `{"theme":"dark","future_top":{"x":1},"hooks":{"PreToolUse":[` + userBashGroup + `]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func preToolGroups(t *testing.T, path string) []Group {
	t.Helper()
	_, groups, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return groups["PreToolUse"]
}

func TestInstallPreToolIdempotentAndPreservesUserGroups(t *testing.T) {
	path := writePreToolFixture(t)
	if err := InstallPreTool(path, "/opt/mora bin/mora"); err != nil {
		t.Fatal(err)
	}
	once, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallPreTool(path, "/opt/mora bin/mora"); err != nil {
		t.Fatal(err)
	}
	twice, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("second install changed bytes\nonce:\n%s\ntwice:\n%s", once, twice)
	}
	var settings map[string]any
	if err := json.Unmarshal(once, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "dark" || settings["future_top"] == nil {
		t.Fatalf("top-level settings lost: %s", once)
	}
	groups := preToolGroups(t, path)
	if len(groups) != 2 {
		t.Fatalf("want the user's group and Mora's, got %#v", groups)
	}
	user, mora := groups[0], groups[1]
	if user.Matcher != "Bash" || len(user.Hooks) != 1 || user.Hooks[0].Command != "user-guard" || string(user.Extra["future_group"]) != "1" {
		t.Fatalf("user group changed: %#v", user)
	}
	want := `'/opt/mora bin/mora' hook pre-tool-use 2>/dev/null || true #mora-managed:pre-tool-use`
	if mora.Matcher != "Bash" || len(mora.Hooks) != 1 || mora.Hooks[0].Command != want || mora.Hooks[0].Timeout != 5 || mora.Hooks[0].Type != "command" {
		t.Fatalf("mora group = %#v", mora)
	}
}

func TestInstallPreToolReplacesInPlace(t *testing.T) {
	path := writePreToolFixture(t)
	if err := InstallPreTool(path, "/old/mora"); err != nil {
		t.Fatal(err)
	}
	if err := InstallPreTool(path, "/new/mora"); err != nil {
		t.Fatal(err)
	}
	groups := preToolGroups(t, path)
	if len(groups) != 2 || !strings.HasPrefix(groups[1].Hooks[0].Command, "'/new/mora' ") {
		t.Fatalf("reinstall should replace Mora's entry, got %#v", groups)
	}
}

func TestUninstallSubRemovesOnlyMoraPreTool(t *testing.T) {
	path := writePreToolFixture(t)
	if err := Install(path, "/bin/mora", 0); err != nil {
		t.Fatal(err)
	}
	if err := InstallPreTool(path, "/bin/mora"); err != nil {
		t.Fatal(err)
	}
	if err := UninstallSub(path, PreToolSub); err != nil {
		t.Fatal(err)
	}
	groups := preToolGroups(t, path)
	if len(groups) != 1 || groups[0].Hooks[0].Command != "user-guard" {
		t.Fatalf("only Mora's PreToolUse entry should go, got %#v", groups)
	}
	start, recall, err := Status(path)
	if err != nil || start != "installed" || recall != "installed" {
		t.Fatalf("other managed hooks must stay: %q %q %v", start, recall, err)
	}

	// Without a user group the whole PreToolUse event goes away.
	bare := filepath.Join(t.TempDir(), "settings.json")
	if err := InstallPreTool(bare, "/bin/mora"); err != nil {
		t.Fatal(err)
	}
	if err := UninstallSub(bare, PreToolSub); err != nil {
		t.Fatal(err)
	}
	if _, groups, err := Load(bare); err != nil || len(groups) != 0 {
		t.Fatalf("empty PreToolUse should be dropped, got %#v %v", groups, err)
	}
}

// A user's group with an empty hooks array is not Mora's to touch, and must
// not come back as "hooks": null.
func TestUninstallSubKeepsUserEmptyGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallPreTool(path, "/bin/mora"); err != nil {
		t.Fatal(err)
	}
	if err := UninstallSub(path, PreToolSub); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hooks map[string][]map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	post := got.Hooks["PostToolUse"]
	if len(got.Hooks) != 1 || len(post) != 1 || string(post[0]["hooks"]) != "[]" {
		t.Fatalf("user's empty group changed:\n%s", body)
	}
}

func TestPreToolStatus(t *testing.T) {
	path := writePreToolFixture(t)
	if got, err := PreToolStatus(path); err != nil || got != "not installed" {
		t.Fatalf("status before install = %q %v", got, err)
	}
	if err := InstallPreTool(path, "/bin/mora"); err != nil {
		t.Fatal(err)
	}
	if got, err := PreToolStatus(path); err != nil || got != "installed" {
		t.Fatalf("status after install = %q %v", got, err)
	}
	if got, err := PreToolStatus(filepath.Join(t.TempDir(), "missing.json")); err != nil || got != "not installed" {
		t.Fatalf("missing settings = %q %v", got, err)
	}
}

func TestInstallAndProjectRecallWithGlobalPreTool(t *testing.T) {
	path := writePreToolFixture(t)
	if err := InstallPreTool(path, "/bin/mora"); err != nil {
		t.Fatal(err)
	}
	if err := Install(path, "/bin/mora", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := PreToolStatus(path); got != "installed" {
		t.Fatalf("Install must keep the PreToolUse entry, status %q", got)
	}
	start, recall, err := Status(path)
	if err != nil || start != "installed" || recall != "installed" {
		t.Fatalf("Install with a pre-tool entry: %q %q %v", start, recall, err)
	}

	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(project, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallProjectRecall(local, "/bin/mora", "project:x", project, 0); err != nil {
		t.Fatalf("project recall with a global pre-tool entry: %v", err)
	}
	if got, _ := PreToolStatus(path); got != "installed" {
		t.Fatalf("project install must not touch the global entry, status %q", got)
	}
}

func TestInstallPreToolRefusesNullSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallPreTool(path, "/bin/mora"); err == nil {
		t.Fatal("null settings must be refused, not overwritten")
	}
	if body, _ := os.ReadFile(path); string(body) != "null\n" {
		t.Fatalf("settings rewritten: %q", body)
	}
}

// TestInstallRefusesNullHooks covers "hooks": null, which Load returns as a
// nil map. Both installers used to panic writing into it. They now refuse
// and leave the file alone, as InstallProjectRecall already did.
func TestInstallRefusesNullHooks(t *testing.T) {
	for name, install := range map[string]func(string) error{
		"pre-tool": func(p string) error { return InstallPreTool(p, "/opt/mora") },
		"session":  func(p string) error { return Install(p, "/opt/mora", 0) },
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"theme":"dark","hooks":null}`
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := install(path); err == nil || !strings.Contains(err.Error(), "must be JSON objects") {
				t.Fatalf("install over null hooks: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != body {
				t.Fatalf("refused install changed settings: %s, %v", got, err)
			}
		})
	}
}
