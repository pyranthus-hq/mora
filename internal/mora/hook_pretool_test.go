package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/rules"
)

const (
	preToolInputS1  = `{"session_id":"s1","transcript_path":"/home/u/.claude/projects/x/s1.jsonl","cwd":"/tmp","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_1","tool_input":{"command":"cd /tmp && ls","description":"list"}}`
	preToolInputAbc = `{"session_id":"abc","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_2","tool_input":{"command":"cd /tmp && ls"}}`
)

// preToolActiveFixture is the compound-cd holdout set the rules package pins
// to the template's real hash.
func preToolActiveFixture(t *testing.T) rules.ActiveSet {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "rules", "testdata", "active_compound_cd_holdout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var set rules.ActiveSet
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatal(err)
	}
	return set
}

// seedPreToolActive writes active.json straight into a temp StateDir, the way
// Compile would, so the hook tests do not depend on the approval flow.
func seedPreToolActive(t *testing.T, edit func(*rules.ActiveSet)) Config {
	t.Helper()
	withTempHome(t)
	cfg := mustConfig(t)
	set := preToolActiveFixture(t)
	if edit != nil {
		edit(&set)
	}
	if err := os.MkdirAll(rules.Dir(cfg.StateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rules.WriteActive(rules.ActivePath(cfg.StateDir), set); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// runPreTool runs the hook and fails on an error or any stderr.
func runPreTool(t *testing.T, input string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"pre-tool-use"}, &out, &errb, strings.NewReader(input)); err != nil {
		t.Fatalf("pre-tool-use returned %v", err)
	}
	if errb.Len() != 0 {
		t.Fatalf("pre-tool-use wrote stderr: %q", errb.String())
	}
	return out.String()
}

func readPreToolLog(t *testing.T, cfg Config) []rules.Row {
	t.Helper()
	body, err := os.ReadFile(rules.LogPath(cfg.StateDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var rows []rules.Row
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		var row rules.Row
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func assertNoPreToolLog(t *testing.T, cfg Config) {
	t.Helper()
	if _, err := os.Stat(rules.LogPath(cfg.StateDir)); !os.IsNotExist(err) {
		t.Fatalf("hook log should not exist, stat err = %v", err)
	}
}

func lastPreToolRow(t *testing.T, cfg Config) rules.Row {
	t.Helper()
	rows := readPreToolLog(t, cfg)
	if len(rows) == 0 {
		t.Fatal("no hook log row")
	}
	return rows[len(rows)-1]
}

func TestHookPreToolWarnedGroupPrintsGolden(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	got := runPreTool(t, preToolInputS1)
	want, err := os.ReadFile(filepath.Join("testdata", "hooks", "pretool_warn.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("output drifted from golden\nwant: %q\ngot:  %q", want, got)
	}
	row := lastPreToolRow(t, cfg)
	if row.Group != rules.GroupWarned || strings.Join(row.Warned, ",") != "compound-cd" || strings.Join(row.Matched, ",") != "compound-cd" {
		t.Fatalf("row = %+v", row)
	}
	if row.SessionID != "s1" || row.AgentID != nil || row.ToolUseID == nil || *row.ToolUseID != "toolu_1" || row.Harness != rules.HarnessClaude {
		t.Fatalf("row ids = %+v", row)
	}
	if strings.Join(row.Holdout, ",") != "compound-cd" || len(row.Rules) != 1 || row.Rules[0].H != "fdac46be8abf" || row.Rules[0].Mode != "warn" {
		t.Fatalf("row rules = %+v", row)
	}
	if !strings.HasPrefix(row.RuleVersion, "mora-active:") || row.Mora != BuildVersion || row.Note != "" {
		t.Fatalf("row versions = %+v", row)
	}
}

func TestHookPreToolSilentGroupLogsOnly(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	if got := runPreTool(t, preToolInputAbc); got != "" {
		t.Fatalf("silent group printed %q", got)
	}
	row := lastPreToolRow(t, cfg)
	if row.Group != rules.GroupSilent || strings.Join(row.Matched, ",") != "compound-cd" || len(row.Warned) != 0 {
		t.Fatalf("row = %+v", row)
	}
}

func TestHookPreToolNoHoldoutWarnsBothGroups(t *testing.T) {
	seedPreToolActive(t, func(s *rules.ActiveSet) { s.Rules[0].Holdout = false })
	for _, input := range []string{preToolInputS1, preToolInputAbc} {
		got := decodeHookOutput(t, runPreTool(t, input))
		if got.HookSpecificOutput.HookEventName != "PreToolUse" || !strings.HasPrefix(got.HookSpecificOutput.AdditionalContext, "Use one command per Bash call.") {
			t.Fatalf("output = %+v", got)
		}
	}
}

func TestHookPreToolShadowNeverPrints(t *testing.T) {
	cfg := seedPreToolActive(t, func(s *rules.ActiveSet) { s.Rules[0].Mode = rules.ModeShadow })
	if got := runPreTool(t, preToolInputS1); got != "" {
		t.Fatalf("shadow printed %q", got)
	}
	row := lastPreToolRow(t, cfg)
	if strings.Join(row.Matched, ",") != "compound-cd" || len(row.Warned) != 0 || row.Rules[0].Mode != "shadow" {
		t.Fatalf("row = %+v", row)
	}
}

func TestHookPreToolLogsMissesToo(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	if got := runPreTool(t, `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"ls -la"}}`); got != "" {
		t.Fatalf("miss printed %q", got)
	}
	row := lastPreToolRow(t, cfg)
	if len(row.Matched) != 0 || len(row.Warned) != 0 || row.ToolUseID != nil {
		t.Fatalf("row = %+v", row)
	}
}

func TestHookPreToolIgnoresNonBashAndGrok(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	for _, input := range []string{
		`{"session_id":"s1","tool_name":"Edit","tool_input":{"command":"cd /tmp && ls"}}`,
		`{"sessionId":"s1","toolName":"Bash","toolInput":{"command":"cd /tmp && ls"},"tool_name":"Bash","tool_input":{"command":"cd /tmp && ls"}}`,
		`{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"cd /tmp && ls"}}`,
	} {
		if got := runPreTool(t, input); got != "" {
			t.Fatalf("input %s printed %q", input, got)
		}
	}
	assertNoPreToolLog(t, cfg)
}

func TestHookPreToolNoActiveFileNoLog(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	if got := runPreTool(t, preToolInputS1); got != "" {
		t.Fatalf("no active.json printed %q", got)
	}
	assertNoPreToolLog(t, cfg)
}

func TestHookPreToolEmptyActiveSetNoLog(t *testing.T) {
	cfg := seedPreToolActive(t, func(s *rules.ActiveSet) { s.Rules = nil })
	if got := runPreTool(t, preToolInputS1); got != "" {
		t.Fatalf("empty set printed %q", got)
	}
	assertNoPreToolLog(t, cfg)
}

func TestHookPreToolOffEnv(t *testing.T) {
	t.Setenv("MORA_PRETOOL_OFF", "1")
	cfg := seedPreToolActive(t, nil)
	if got := runPreTool(t, preToolInputS1); got != "" {
		t.Fatalf("MORA_PRETOOL_OFF=1 printed %q", got)
	}
	assertNoPreToolLog(t, cfg)
}

func TestHookPreToolLogHasNoCommandText(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	input := `{"session_id":"s1","transcript_path":"/home/u/.claude/projects/x/TRANSCRIPT_9c1e.jsonl","cwd":"/tmp/CWD_4b2d","prompt":"PROMPT_e5a0","tool_name":"Bash","tool_use_id":"t","tool_input":{"command":"cd /tmp/CANARY_7f3a && ls"}}`
	runPreTool(t, input)
	body, err := os.ReadFile(rules.LogPath(cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"CANARY_7f3a", "CWD_4b2d", "PROMPT_e5a0", "TRANSCRIPT_9c1e", `"command"`, `"cwd"`, `"prompt"`, `"transcript_path"`} {
		if bytes.Contains(body, []byte(leak)) {
			t.Fatalf("hook log leaked %q: %s", leak, body)
		}
	}
}

func TestHookPreToolSubagentRow(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	runPreTool(t, `{"session_id":"s1","agent_id":"a7","tool_name":"Bash","tool_input":{"command":"cd x; y"}}`)
	row := lastPreToolRow(t, cfg)
	if row.AgentID == nil || *row.AgentID != "a7" || row.Group != rules.Group("s1", "a7") {
		t.Fatalf("row = %+v", row)
	}
}

func TestHookInstallPreToolThroughRun(t *testing.T) {
	tmp := withTempHookHome(t)
	writeClaudeSettingsFixture(t, tmp, `{"theme":"dark","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"user-guard"}]}]}}`+"\n")
	out := run(t, "hook", "install", "--pre-tool")
	if out != "installed mora Claude hook: PreToolUse (Bash)\n" {
		t.Fatalf("install output = %q", out)
	}
	settings := readClaudeSettingsForTest(t, tmp)
	assertHookInstalled(t, settings, "PreToolUse", "mora' hook pre-tool-use 2>/dev/null || true #mora-managed:pre-tool-use")
	assertHookInstalled(t, settings, "PreToolUse", "user-guard")
	hooks := hookGroupsForTest(t, settings)
	if _, ok := hooks["SessionStart"]; ok {
		t.Fatalf("--pre-tool must not install the other hooks: %#v", hooks)
	}
	if out := run(t, "hook", "status"); !strings.Contains(out, "SessionStart: not installed\n") || !strings.HasSuffix(out, "PreToolUse: installed\n") {
		t.Fatalf("status = %q", out)
	}
	if out := run(t, "hook", "status", "--pre-tool"); out != "PreToolUse: installed\n" {
		t.Fatalf("status --pre-tool = %q", out)
	}
}

// The PreToolUse hook is global only, so --project status still reports it
// from the global settings.
func TestHookStatusProjectReportsGlobalPreTool(t *testing.T) {
	withTempHookHome(t)
	t.Chdir(t.TempDir())
	run(t, "hook", "install", "--pre-tool")
	if out := run(t, "hook", "status", "--project"); !strings.HasSuffix(out, "PreToolUse: installed\n") {
		t.Fatalf("status --project = %q", out)
	}
	var payload hookStatusPayload
	if err := json.Unmarshal([]byte(run(t, "hook", "status", "--project", "--json")), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PreToolUse != "installed" {
		t.Fatalf("pre_tool_use = %q", payload.PreToolUse)
	}
}

func TestHookUninstallPreToolKeepsOtherHooks(t *testing.T) {
	tmp := withTempHookHome(t)
	run(t, "hook", "install")
	run(t, "hook", "install", "--pre-tool")
	run(t, "hook", "uninstall", "--pre-tool")
	settings := readClaudeSettingsForTest(t, tmp)
	hooks := hookGroupsForTest(t, settings)
	if _, ok := hooks["PreToolUse"]; ok {
		t.Fatalf("PreToolUse should be gone: %#v", hooks)
	}
	assertHookInstalled(t, settings, "SessionStart", "mora' hook session-start")
	assertHookInstalled(t, settings, "UserPromptSubmit", "mora' hook recall")

	// Plain uninstall removes every managed entry, the PreToolUse one included.
	run(t, "hook", "install", "--pre-tool")
	run(t, "hook", "uninstall")
	if hooks := hookGroupsForTest(t, readClaudeSettingsForTest(t, tmp)); len(hooks) != 0 {
		t.Fatalf("plain uninstall left %#v", hooks)
	}
}

func TestHookStatusPreToolJSON(t *testing.T) {
	withTempHookHome(t)
	decode := func() hookStatusPayload {
		var got hookStatusPayload
		if err := json.Unmarshal([]byte(run(t, "hook", "status", "--json")), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := decode(); got.PreToolUse != "not installed" {
		t.Fatalf("fresh = %+v", got)
	}
	run(t, "hook", "install", "--pre-tool")
	if got := decode(); got.PreToolUse != "installed" || got.Installed {
		t.Fatalf("after --pre-tool install = %+v", got)
	}
}

func TestHookInstallPreToolRejectsProject(t *testing.T) {
	withTempHookHome(t)
	for _, args := range [][]string{
		{"hook", "install", "--pre-tool", "--project", "--scope", "project:x"},
		{"hook", "uninstall", "--pre-tool", "--project"},
		{"hook", "status", "--pre-tool", "--project"},
	} {
		if _, err := runErr(t, args...); err == nil || !strings.Contains(err.Error(), "global only") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}
