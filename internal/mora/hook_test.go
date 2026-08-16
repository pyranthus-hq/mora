package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookSessionStart(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	restore := stubHookBrief(t, "today's local brief")
	defer restore()

	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"session-start"}, &out, strings.NewReader(`{"hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	got := decodeHookOutput(t, out.String())
	if got.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("event = %q, want SessionStart", got.HookSpecificOutput.HookEventName)
	}
	if got.HookSpecificOutput.AdditionalContext != "today's local brief" {
		t.Fatalf("additionalContext = %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestHookSessionStartCompactEmitsNothing(t *testing.T) {
	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"session-start"}, &out, strings.NewReader(`{"source":"compact"}`)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("compact SessionStart should be silent, got %q", out.String())
	}
}

func TestHookSessionStartFailOpen(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	prev := hookResolveBrief
	hookResolveBrief = func(Config, time.Time, briefOpts) (string, bool, error) {
		return "", false, errors.New("boom")
	}
	t.Cleanup(func() { hookResolveBrief = prev })

	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"session-start"}, &out, strings.NewReader(`{}`)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("failing SessionStart should be silent, got %q", out.String())
	}
}

func TestHookRecallSkipsCheapPrompts(t *testing.T) {
	for _, prompt := range []string{"tiny", "/compact please", "yes", "no", "ok", "y", "n", "continue", "go", "k"} {
		t.Run(prompt, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdHook(context.Background(), []string{"recall"}, &out, strings.NewReader(`{"prompt":`+quoteJSON(prompt)+`}`)); err != nil {
				t.Fatal(err)
			}
			if out.String() != "" {
				t.Fatalf("prompt %q should be silent, got %q", prompt, out.String())
			}
		})
	}
}

func TestHookRecallInjectsSeededMemory(t *testing.T) {
	cfg := seedRecallMemories(t, 5)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	input := `{"prompt":"What did we decide about eelpout recall token alpha?"}`
	if err := cmdHook(context.Background(), []string{"recall"}, &out, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	got := decodeHookOutput(t, out.String())
	ctx := got.HookSpecificOutput.AdditionalContext
	if got.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Fatalf("event = %q, want UserPromptSubmit", got.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(ctx, "eelpout recall token alpha") {
		t.Fatalf("expected seeded memory in recall context, got:\n%s", ctx)
	}
	if count := strings.Count(ctx, "\n- "); count > 3 {
		t.Fatalf("expected <=3 cited items, got %d:\n%s", count, ctx)
	}
	if len(ctx) > hookRecallByteLimit {
		t.Fatalf("additionalContext length = %d, want <= %d:\n%s", len(ctx), hookRecallByteLimit, ctx)
	}
	if !strings.Contains(ctx, "age: ") || !strings.Contains(ctx, "id: ") {
		t.Fatalf("expected provenance and age stamps, got:\n%s", ctx)
	}
}

func TestHookRecallNoMatchEmitsNothing(t *testing.T) {
	cfg := seedRecallMemories(t, 1)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	input := `{"prompt":"zzzznomatch uniquely absent query terms"}`
	if err := cmdHook(context.Background(), []string{"recall"}, &out, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("no-match recall should be silent, got %q", out.String())
	}
}

func TestHookRecallThresholdRespected(t *testing.T) {
	cfg := seedRecallMemories(t, 1)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	input := `{"prompt":"What did we decide about eelpout recall token alpha?"}`
	if err := cmdHook(context.Background(), []string{"recall", "--threshold", "-999"}, &out, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("strict threshold should suppress recall, got %q", out.String())
	}
}

func TestHookRecallGateDirection(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	mems := []Memory{
		{ID: "above", Title: "Above", Source: "test", CreatedAt: now.Format(time.RFC3339), Text: "above threshold", Score: 0.2},
		{ID: "equal", Title: "Equal", Source: "test", CreatedAt: now.Format(time.RFC3339), Text: "equal threshold", Score: 0.1},
		{ID: "below", Title: "Below", Source: "test", CreatedAt: now.Format(time.RFC3339), Text: "below threshold", Score: -0.5},
	}
	got := formatRecallContext(mems, 0.1, now)
	if strings.Contains(got, "id: above") {
		t.Fatalf("score above threshold must be excluded, got:\n%s", got)
	}
	for _, id := range []string{"equal", "below"} {
		if !strings.Contains(got, "id: "+id) {
			t.Fatalf("score at/below threshold should be included for %s, got:\n%s", id, got)
		}
	}
}

func TestHookInstallFreshSettings(t *testing.T) {
	tmp := withTempHookHome(t)
	out := run(t, "hook", "install")
	if !strings.Contains(out, "installed mora Claude hooks") {
		t.Fatalf("install output = %q", out)
	}
	settings := readClaudeSettingsForTest(t, tmp)
	assertHookInstalled(t, settings, "SessionStart", "mora hook session-start")
	assertHookInstalled(t, settings, "UserPromptSubmit", "mora hook recall")
}

func TestHookStatusReportsInstalledHooks(t *testing.T) {
	withTempHookHome(t)
	out := run(t, "hook", "status")
	if !strings.Contains(out, "SessionStart: not installed") || !strings.Contains(out, "UserPromptSubmit: not installed") {
		t.Fatalf("fresh status = %q", out)
	}
	run(t, "hook", "install")
	out = run(t, "hook", "status")
	if !strings.Contains(out, "SessionStart: installed") || !strings.Contains(out, "UserPromptSubmit: installed") {
		t.Fatalf("installed status = %q", out)
	}
}

func TestHookInstallUninstallBinaryNameIndependent(t *testing.T) {
	// The install/uninstall marker must not depend on the binary being named
	// "mora": users run renamed binaries (mora-dev/mora-new). Simulate one and
	// assert both hooks install with the abs exe path + sentinel and uninstall
	// removes them completely. (Fails before the #mora-managed sentinel fix.)
	tmp := withTempHookHome(t)
	fakeExe := "/opt/tools/mora-dev"
	hookExecutable = func() (string, error) { return fakeExe, nil }

	run(t, "hook", "install")
	// cmdHookInstall stores filepath.Abs(exe); derive the expectation the same
	// way so it matches on every OS (on Windows filepath.Abs prepends the drive
	// and flips separators -> C:\opt\tools\mora-dev; on Unix it is unchanged).
	wantExe, err := filepath.Abs(fakeExe)
	if err != nil {
		t.Fatal(err)
	}
	hooks := hookGroupsForTest(t, readClaudeSettingsForTest(t, tmp))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit"} {
		if !containsHookCommand(hooks[ev], wantExe+" hook") {
			t.Fatalf("%s command should use the absolute exe path, got %#v", ev, hooks[ev])
		}
		if !containsHookCommand(hooks[ev], hookMarker+":") {
			t.Fatalf("%s command should carry the %s sentinel, got %#v", ev, hookMarker, hooks[ev])
		}
	}

	run(t, "hook", "uninstall")
	hooks = hookGroupsForTest(t, readClaudeSettingsForTest(t, tmp))
	if containsHookCommand(hooks["SessionStart"], hookMarker+":") || containsHookCommand(hooks["UserPromptSubmit"], hookMarker+":") {
		t.Fatalf("renamed-binary mora hooks not fully removed on uninstall: %#v", hooks)
	}
	out := run(t, "hook", "status")
	if !strings.Contains(out, "SessionStart: not installed") || !strings.Contains(out, "UserPromptSubmit: not installed") {
		t.Fatalf("status after uninstall = %q", out)
	}
}

func stubHookBrief(t *testing.T, body string) func() {
	t.Helper()
	prev := hookResolveBrief
	hookResolveBrief = func(Config, time.Time, briefOpts) (string, bool, error) {
		return body, false, nil
	}
	return func() { hookResolveBrief = prev }
}

func seedRecallMemories(t *testing.T, n int) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for i := 0; i < n; i++ {
		m := Memory{
			ID:        newID(),
			Scope:     "project:eelpout",
			Type:      "decision",
			Title:     "Eelpout recall decision",
			Source:    "test",
			CreatedAt: "2026-06-01T00:00:00Z",
			Text:      "We decided about eelpout recall token alpha with local-only Claude hooks and no daemon.",
		}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func withTempHookHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	setTestHome(t, tmp)
	t.Setenv("MORA_CONFIG_DIR", "")
	prev := hookExecutable
	hookExecutable = func() (string, error) { return filepath.Join(tmp, "bin", "mora"), nil }
	t.Cleanup(func() { hookExecutable = prev })
	return tmp
}

func writeClaudeSettingsFixture(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeHookOutput(t *testing.T, body string) hookEnvelope {
	t.Helper()
	var got hookEnvelope
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode hook output: %v\n%s", err, body)
	}
	return got
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func readClaudeSettingsForTest(t *testing.T, home string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("decode settings: %v\n%s", err, body)
	}
	return settings
}

func hookGroupsForTest(t *testing.T, settings map[string]any) map[string][]claudeHookGroup {
	t.Helper()
	raw, ok := settings["hooks"]
	if !ok {
		return map[string][]claudeHookGroup{}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var hooks map[string][]claudeHookGroup
	if err := json.Unmarshal(b, &hooks); err != nil {
		t.Fatal(err)
	}
	return hooks
}

func assertHookInstalled(t *testing.T, settings map[string]any, event, commandSubstr string) {
	t.Helper()
	hooks := hookGroupsForTest(t, settings)
	if !containsHookCommand(hooks[event], commandSubstr) {
		t.Fatalf("missing %s hook containing %q in %#v", event, commandSubstr, hooks[event])
	}
}

func containsHookCommand(groups []claudeHookGroup, substr string) bool {
	for _, group := range groups {
		for _, h := range group.Hooks {
			if strings.Contains(h.Command, substr) {
				return true
			}
		}
	}
	return false
}

func TestHookUninstallRemovesOnlyMoraHooks(t *testing.T) {
	tmp := withTempHookHome(t)
	path := filepath.Join(tmp, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{
  "hooks": {
    "SessionStart": [
      {"hooks":[{"type":"command","command":"/tmp/mora hook session-start #mora-managed:session-start","timeout":15}]}
    ],
    "UserPromptSubmit": [
      {"hooks":[{"type":"command","command":"other-tool recall","timeout":3}]},
      {"hooks":[{"type":"command","command":"/tmp/mora hook recall #mora-managed:recall","timeout":10}]}
    ]
  }
}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	run(t, "hook", "uninstall")
	settings := readClaudeSettingsForTest(t, tmp)
	hooks := hookGroupsForTest(t, settings)
	if containsHookCommand(hooks["SessionStart"], "mora hook") || containsHookCommand(hooks["UserPromptSubmit"], "mora hook") {
		t.Fatalf("mora hooks still present after uninstall: %#v", hooks)
	}
	if !containsHookCommand(hooks["UserPromptSubmit"], "other-tool recall") {
		t.Fatalf("unrelated hook was removed: %#v", hooks["UserPromptSubmit"])
	}
	if _, ok := hooks["SessionStart"]; ok {
		t.Fatalf("empty SessionStart group should be pruned: %#v", hooks["SessionStart"])
	}
}

func TestHookInstallMergesAndIsIdempotent(t *testing.T) {
	tmp := withTempHookHome(t)
	path := filepath.Join(tmp, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	golden := `{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "other-tool recall",
            "timeout": 3
          }
        ]
      }
    ]
  },
  "theme": "dark"
}
`
	if err := os.WriteFile(path, []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}

	run(t, "hook", "install", "--threshold", "-0.25")
	once, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	run(t, "hook", "install", "--threshold", "-0.25")
	twice, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("install twice should be byte-identical\nonce:\n%s\ntwice:\n%s", once, twice)
	}
	settings := readClaudeSettingsForTest(t, tmp)
	if settings["theme"] != "dark" {
		t.Fatalf("top-level settings were not preserved: %#v", settings)
	}
	hooks := hookGroupsForTest(t, settings)
	if !containsHookCommand(hooks["UserPromptSubmit"], "other-tool recall") {
		t.Fatalf("unrelated hook was not preserved: %#v", hooks["UserPromptSubmit"])
	}
	assertHookInstalled(t, settings, "UserPromptSubmit", "mora hook recall --threshold -0.25")
}

func TestHookInstallMalformedHooksDoesNotOverwrite(t *testing.T) {
	tmp := withTempHookHome(t)
	path := writeClaudeSettingsFixture(t, tmp, `{"hooks":"not an object","theme":"dark"}`+"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "hook", "install"); err == nil {
		t.Fatal("install should fail on malformed hooks")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("malformed hooks file was overwritten\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestHookInstallUnparseableSettingsFailsClosed(t *testing.T) {
	// A settings.json that fails strict JSON parsing (JSONC comments, trailing
	// commas) must abort install with a clear error and leave the file
	// byte-identical. Before the fix, loadClaudeSettings silently discarded the
	// unparseable file and install wrote back ONLY the mora hooks, wiping every
	// other setting the user had.
	cases := []struct {
		name string
		body string
	}{
		{"jsonc comment", "{\n  // enable dark mode\n  \"theme\": \"dark\"\n}\n"},
		{"trailing comma", "{\n  \"theme\": \"dark\",\n}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := withTempHookHome(t)
			path := writeClaudeSettingsFixture(t, tmp, c.body)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = runErr(t, "hook", "install")
			if err == nil {
				t.Fatal("install must refuse to modify an unparseable settings file")
			}
			if !strings.Contains(err.Error(), "not valid JSON") || !strings.Contains(err.Error(), path) {
				t.Fatalf("error must name the file and the parse problem, got: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("unparseable settings file was rewritten\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}
