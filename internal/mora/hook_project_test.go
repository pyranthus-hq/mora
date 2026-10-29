package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestHookProjectInstallLifecycle(t *testing.T) {
	home := withTempHookHome(t)
	project := t.TempDir()
	t.Chdir(project)
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(project, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(`{"theme":"dark","hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"other-hook"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "hook", "install", "--project", "--scope", "project:demo")
	run(t, "hook", "install", "--project", "--scope", "project:demo")
	body, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dark", "other-hook", "--scope 'project:demo'", "--project-dir", hookMarker + ":recall"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("project settings missing %q: %s", want, body)
		}
	}
	if strings.Contains(string(body), "session-start") || strings.Count(string(body), hookMarker+":recall") != 1 {
		t.Fatalf("project install must contain one recall and no global brief: %s", body)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("project install touched global settings: %v", err)
	}
	status := run(t, "hook", "status", "--project", "--json")
	var payload hookStatusPayload
	if err := json.Unmarshal([]byte(status), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Path != local || !payload.Installed || len(payload.Harnesses) != 1 ||
		payload.Harnesses[0].Name != "claude" || !payload.Harnesses[0].Installed ||
		len(payload.Harnesses[0].Events) != 1 || payload.Harnesses[0].Events[0] != "UserPromptSubmit" {
		t.Fatalf("wrong project status: %s", status)
	}
	run(t, "hook", "uninstall", "--project")
	body, err = os.ReadFile(local)
	if err != nil || strings.Contains(string(body), hookMarker) || !strings.Contains(string(body), "other-hook") {
		t.Fatalf("project uninstall must preserve other settings: %s, %v", body, err)
	}
}

func TestHookProjectInstallRefusesGlobalHooks(t *testing.T) {
	withTempHookHome(t)
	project := t.TempDir()
	t.Chdir(project)
	run(t, "hook", "install")
	var out bytes.Buffer
	err := cmdHook(testCtx(t), []string{"install", "--project", "--scope", "project:demo"}, &out, testStderr, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "mora hook uninstall") {
		t.Fatalf("expected actionable global-hook refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("refused install touched project settings: %v", err)
	}
}

func TestHookProjectRecallIsolation(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	root := t.TempDir()
	other := t.TempDir()
	for i := 0; i < 20; i++ {
		run(t, "write", "--scope", "project:other", "--type", "decision", "--title", "pnpm", "--text", "Always install pnpm PRIVATE_OTHER_PROJECT")
	}
	run(t, "write", "--scope", "personal", "--type", "decision", "--title", "pnpm", "--text", "Always install pnpm PRIVATE_PERSONAL")
	run(t, "write", "--scope", "project:demo", "--type", "decision", "--title", "pnpm", "--text", "Always install pnpm LOCAL_CORRECTION")
	run(t, "index", "rebuild")
	subdir := filepath.Join(root, "src")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd string
		want      bool
	}{
		{"project", root, true},
		{"subdirectory", subdir, true},
		{"other", other, false},
		{"sibling-prefix", root + "-other", false},
		{"missing-cwd", "", false},
		{"relative-cwd", ".", false},
	} {
		subRun(t, tc.name, func(t *testing.T) {
			var out bytes.Buffer
			args := []string{"recall", "--scope", "project:demo", "--project-dir", root}
			input := `{"prompt":"Always install pnpm", "cwd":` + quoteJSON(tc.cwd) + `}`
			if err := cmdHook(testCtx(t), args, &out, testStderr, strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "PRIVATE_") {
				t.Fatalf("foreign scope leaked: %s", &out)
			}
			if tc.want {
				if !hookWithinProject(root, tc.cwd) {
					t.Fatalf("project containment failed: root=%q cwd=%q", root, tc.cwd)
				}
				got := decodeHookOutput(t, out.String()).HookSpecificOutput.AdditionalContext
				if !strings.Contains(got, "LOCAL_CORRECTION") || !strings.Contains(got, "id: ") {
					t.Fatalf("missing local correction/provenance: %s", got)
				}
			} else if out.Len() != 0 {
				t.Fatalf("outside project must be silent: %s", &out)
			}
		})
	}
}

func TestHookProjectRecallOmitsGlobalHealth(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source: "gmail", LastAttemptAt: time.Now().UTC().Format(time.RFC3339),
		LastError: "PRIVATE_FOREIGN_ACCOUNT /private/other-project",
	})
	if banner := healthBannerFrom(healthOf(cfg, hookNow())); banner == "" {
		t.Fatal("fixture must have an unhealthy global banner")
	}
	var out bytes.Buffer
	root := t.TempDir()
	err := cmdHook(testCtx(t), []string{"recall", "--scope", "project:demo", "--project-dir", root}, &out, testStderr,
		strings.NewReader(`{"prompt":"Always install pnpm", "cwd":`+quoteJSON(root)+`}`))
	if err != nil || out.Len() != 0 {
		t.Fatalf("scoped recall must not inject global health: %s, %v", &out, err)
	}
}

func TestHookProjectRecallRetractUndo(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	root := t.TempDir()
	saved := run(t, "write", "--scope", "project:demo", "--type", "decision", "--title", "Use pnpm", "--text", "Always install pnpm", "--json")
	var receipt writeReceipt
	if err := json.Unmarshal([]byte(saved), &receipt); err != nil || receipt.ID == "" {
		t.Fatalf("invalid write receipt: %s, %v", saved, err)
	}
	check := func(want bool) {
		t.Helper()
		var out bytes.Buffer
		err := cmdHook(testCtx(t), []string{"recall", "--scope", "project:demo", "--project-dir", root}, &out, testStderr,
			strings.NewReader(`{"prompt":"Always install pnpm", "cwd":`+quoteJSON(root)+`}`))
		if err != nil {
			t.Fatal(err)
		}
		if want {
			got := decodeHookOutput(t, out.String()).HookSpecificOutput.AdditionalContext
			if !strings.Contains(got, "Always install pnpm") || !strings.Contains(got, "id: "+receipt.ID) {
				t.Fatalf("missing correction/provenance: %s", got)
			}
		} else if out.Len() != 0 {
			t.Fatalf("retracted correction still injected: %s", &out)
		}
	}
	check(true)
	var retracted struct {
		Decision govEntry `json:"decision"`
	}
	result := run(t, "teach", "memory", "retract", "--id", receipt.ID, "--yes", "--json")
	if err := json.Unmarshal([]byte(result), &retracted); err != nil || retracted.Decision.ID == "" {
		t.Fatalf("invalid retract receipt: %s, %v", result, err)
	}
	check(false)
	run(t, "teach", "undo", retracted.Decision.ID)
	check(true)
}

func TestHookProjectRecallSymlinkContainment(t *testing.T) {
	withTempHome(t)
	root, other := t.TempDir(), t.TempDir()
	alias, escape := filepath.Join(other, "alias"), filepath.Join(root, "escape")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(other, escape); err != nil {
		t.Fatal(err)
	}
	if !hookWithinProject(root, alias) || hookWithinProject(root, escape) {
		t.Fatal("symlink aliases must resolve inside the project; escapes must be refused")
	}
}
