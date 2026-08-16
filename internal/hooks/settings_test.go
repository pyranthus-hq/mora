package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommandAndGroupPreserveUnknownFields(t *testing.T) {
	input := []byte(`{"matcher":"x","future_group":{"v":1},"hooks":[{"type":"command","command":"other","timeout":3,"future_hook":[1,2]}]}`)
	var group Group
	if err := json.Unmarshal(input, &group); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(group)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["future_group"]; !ok {
		t.Fatal("group field lost")
	}
	hooks := got["hooks"].([]any)
	if _, ok := hooks[0].(map[string]any)["future_hook"]; !ok {
		t.Fatal("hook field lost")
	}
	var command Command
	if err := json.Unmarshal([]byte(`[`), &command); err == nil {
		t.Fatal("invalid command JSON accepted")
	}
	if err := json.Unmarshal([]byte(`[`), &group); err == nil {
		t.Fatal("invalid group JSON accepted")
	}
}
func TestInstallIdempotentMergeStatusAndThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	initial := []byte(`{"theme":"dark","hooks":{"SessionStart":[{"matcher":"external","future_group":true,"hooks":[{"type":"command","command":"external","future_hook":7}]}]}}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Install(path, "/bin/mora", 0.25); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(path, "/bin/mora", 0.25); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) {
		t.Fatal("second install changed bytes")
	}
	start, recall, err := Status(path)
	if err != nil || start != "installed" || recall != "installed" {
		t.Fatalf("status=(%q,%q,%v)", start, recall, err)
	}
	text := string(first)
	for _, want := range []string{`"theme": "dark"`, `"future_group": true`, `"future_hook": 7`, `hook recall --threshold 0.25`, `#mora-managed:session-start`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}
func TestUninstallRemovesOnlyManagedHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Install(path, "mora", 0); err != nil {
		t.Fatal(err)
	}
	settings, groups, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	groups["SessionStart"] = append(groups["SessionStart"], Group{Hooks: []Command{{Type: "command", Command: "external", Timeout: 1}}})
	settings["hooks"] = groups
	if err := Write(path, settings); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), Marker) || !strings.Contains(string(body), "external") {
		t.Fatalf("unexpected settings: %s", body)
	}
	start, recall, err := Status(path)
	if err != nil || start != "not installed" || recall != "not installed" {
		t.Fatalf("status=(%q,%q,%v)", start, recall, err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
}
func TestSettingsFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	bad := []byte(`{"hooks":`)
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []func(string) error{func(p string) error { return Install(p, "mora", 0) }, Uninstall} {
		if err := op(path); err == nil {
			t.Fatal("malformed settings accepted")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, bad) {
			t.Fatal("malformed settings overwritten")
		}
	}
	if err := os.WriteFile(path, []byte(`{"hooks":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "malformed Claude settings hooks") {
		t.Fatalf("hooks error=%v", err)
	}
	if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "reading Claude settings") {
		t.Fatalf("directory error=%v", err)
	}
	parentFile := filepath.Join(dir, "parent")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(filepath.Join(parentFile, "settings.json"), map[string]any{}); err == nil {
		t.Fatal("write through file parent succeeded")
	}
}
func TestDanglingSymlinkRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "broken symlink") {
		t.Fatalf("error=%v", err)
	}
}
func TestFreshSettingsAndPath(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	path, err := SettingsPath()
	if err != nil || path != filepath.Join(home, ".claude", "settings.json") {
		t.Fatalf("path=%q err=%v", path, err)
	}
	fresh := filepath.Join(t.TempDir(), "new", "settings.json")
	start, recall, err := Status(fresh)
	if err != nil || start != "not installed" || recall != "not installed" {
		t.Fatalf("fresh status=(%q,%q,%v)", start, recall, err)
	}
	if err := Install(fresh, "mora", 0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v", info.Mode().Perm())
	}
}

func TestSettingsSerializationErrorEdges(t *testing.T) {
	if _, err := (Command{Extra: map[string]json.RawMessage{"bad": json.RawMessage(`{`)}}).MarshalJSON(); err == nil {
		t.Fatal("invalid command extra encoded")
	}
	if _, err := (Group{Hooks: []Command{{Extra: map[string]json.RawMessage{"bad": json.RawMessage(`{`)}}}}).MarshalJSON(); err == nil {
		t.Fatal("invalid group hook encoded")
	}
	if err := Write(filepath.Join(t.TempDir(), "settings.json"), map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("unsupported settings encoded")
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Status(path); err == nil {
		t.Fatal("status accepted malformed settings")
	}
	fresh := filepath.Join(t.TempDir(), "settings.json")
	if err := Install(fresh, "mora", 0); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(fresh); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(fresh)
	if strings.Contains(string(body), "hooks") {
		t.Fatalf("empty managed hooks key retained: %s", body)
	}
}
