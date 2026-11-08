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
	nested := filepath.Join(root, "nested-repository")
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd string
		want      bool
	}{
		{"project", root, true},
		{"subdirectory", subdir, true},
		{"nested-repo-is-still-in-directory-subtree", nested, true},
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

func TestHookProjectInstallRefusesAlternateConfig(t *testing.T) {
	withTempHookHome(t)
	project, alternate := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("CLAUDE_CONFIG_DIR", alternate)
	body := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"mora hook session-start #mora-managed:session-start"}]}]}}`)
	path := filepath.Join(alternate, "settings.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := cmdHook(testCtx(t), []string{"install", "--project", "--scope", "project:demo"}, &out, testStderr, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
		t.Fatalf("expected alternate-config refusal, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("alternate settings changed: %s, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(project, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("refusal wrote project settings: %v", err)
	}
}

func TestHookProjectInstallRefusesGitLayouts(t *testing.T) {
	for _, layout := range []string{"linked-worktree", "repo-subdirectory"} {
		subRun(t, layout, func(t *testing.T) {
			withTempHookHome(t)
			main, project := t.TempDir(), t.TempDir()
			body := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"mora hook session-start #mora-managed:session-start"}]}]}}`)
			mainLocal := filepath.Join(main, ".claude", "settings.local.json")
			if err := os.MkdirAll(filepath.Dir(mainLocal), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mainLocal, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if layout == "linked-worktree" {
				if err := os.WriteFile(filepath.Join(project, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "linked")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(filepath.Join(main, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				project = filepath.Join(main, "src")
				if err := os.Mkdir(project, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(project)
			var out bytes.Buffer
			err := cmdHook(testCtx(t), []string{"install", "--project", "--scope", "project:demo"}, &out, testStderr, strings.NewReader(""))
			if err == nil {
				t.Fatal("unsupported effective settings layout was accepted")
			}
			got, err := os.ReadFile(mainLocal)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("main-checkout settings changed: %s, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(project, ".claude")); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote project settings: %v", err)
			}
		})
	}
}

func TestHookProjectInstallRefusesSettingsSymlinks(t *testing.T) {
	for _, target := range []string{"directory", "local-file", "shared-file"} {
		subRun(t, target, func(t *testing.T) {
			withTempHookHome(t)
			project, other := t.TempDir(), t.TempDir()
			t.Chdir(project)
			body := []byte(`{"theme":"PRIVATE_OTHER_PROJECT"}`)
			foreign := filepath.Join(other, "settings.local.json")
			if err := os.WriteFile(foreign, body, 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(project, ".claude")
			destination := other
			if target != "directory" {
				if err := os.Mkdir(link, 0o700); err != nil {
					t.Fatal(err)
				}
				name := "settings.local.json"
				if target == "shared-file" {
					name = "settings.json"
				}
				link = filepath.Join(link, name)
				destination = foreign
			}
			if err := os.Symlink(destination, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			for _, command := range [][]string{{"install", "--project", "--scope", "project:demo"}, {"uninstall", "--project"}} {
				var out bytes.Buffer
				if err := cmdHook(testCtx(t), command, &out, testStderr, strings.NewReader("")); err == nil {
					t.Fatalf("settings link accepted: %v", command)
				}
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("settings link replaced: %v", err)
			}
			got, err := os.ReadFile(foreign)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("foreign settings modified: %s, %v", got, err)
			}
		})
	}
}

func TestHookProjectRecallRenamedRootIsSilent(t *testing.T) {
	withTempHome(t)
	parent := t.TempDir()
	root, moved := filepath.Join(parent, "original"), filepath.Join(parent, "renamed")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := cmdHook(testCtx(t), []string{"recall", "--scope", "project:demo", "--project-dir", root}, &out, testStderr,
		strings.NewReader(`{"prompt":"Always install pnpm", "cwd":`+quoteJSON(moved)+`}`))
	if err != nil || out.Len() != 0 {
		t.Fatalf("renamed root must not use the old binding: %s, %v", &out, err)
	}
}

func TestHookProjectLaterGlobalHooksRemainBroad(t *testing.T) {
	withTempHookHome(t)
	root := t.TempDir()
	t.Chdir(root)
	run(t, "init")
	run(t, "write", "--scope", "project:demo", "--type", "decision", "--title", "pnpm", "--text", "Always install pnpm LOCAL_CORRECTION")
	run(t, "write", "--scope", "personal", "--type", "decision", "--title", "pnpm", "--text", "Always install pnpm PRIVATE_PERSONAL")
	run(t, "hook", "install", "--project", "--scope", "project:demo")
	run(t, "hook", "install")
	input := `{"prompt":"Always install pnpm", "cwd":` + quoteJSON(root) + `}`
	var scoped, global bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"recall", "--scope", "project:demo", "--project-dir", root}, &scoped, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scoped.String(), "LOCAL_CORRECTION") || strings.Contains(scoped.String(), "PRIVATE_PERSONAL") {
		t.Fatalf("scoped command changed: %s", &scoped)
	}
	if err := cmdHook(testCtx(t), []string{"recall"}, &global, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(global.String(), "PRIVATE_PERSONAL") {
		t.Fatalf("fixture must demonstrate that a later global hook can still expose personal context: %s", &global)
	}
}
