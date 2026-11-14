package mora

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/rules"
)

// clearAgentEnv removes the agent-session variables for this test, so the
// approval gate sees a person's terminal even when the suite itself runs
// inside an agent session. t.Setenv restores them afterwards.
func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, key := range rulesAgentEnv {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// stubRulesTerminal makes approve and off see an interactive terminal where
// the person types typed.
func stubRulesTerminal(t *testing.T, typed string) {
	t.Helper()
	withTestVerifiedOwnerAction(t)
	clearAgentEnv(t)
	prevInteractive, prevConfirm := rulesInteractive, rulesReadConfirm
	rulesInteractive = func(io.Reader) bool { return true }
	rulesReadConfirm = func(io.Reader) string { return typed + "\n" }
	t.Cleanup(func() { rulesInteractive, rulesReadConfirm = prevInteractive, prevConfirm })
}

func rulesActiveIDs(t *testing.T, cfg Config) []string {
	t.Helper()
	set, present, err := rules.LoadActive(rules.ActivePath(cfg.StateDir))
	if err != nil || !present {
		t.Fatalf("active.json: present=%v err=%v", present, err)
	}
	ids := []string{}
	for _, r := range set.Rules {
		ids = append(ids, r.ID)
	}
	return ids
}

func rulesListForTest(t *testing.T) []rulesListRow {
	t.Helper()
	var doc struct {
		Schema string         `json:"schema"`
		Rules  []rulesListRow `json:"rules"`
	}
	if err := json.Unmarshal([]byte(run(t, "rules", "list", "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != "mora.rules.list" {
		t.Fatalf("schema = %q", doc.Schema)
	}
	return doc.Rules
}

func TestRulesAddWritesFileAndRefusesDuplicate(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	out := run(t, "rules", "add", "--template", "compound-cd")
	if out != "added rule compound-cd (inactive). Approve it in your own terminal: mora rules approve compound-cd --mode warn\n" {
		t.Fatalf("add output = %q", out)
	}
	r, err := rules.ReadRule(cfg.VaultDir, "compound-cd")
	if err != nil {
		t.Fatal(err)
	}
	if rules.Hash(r) != preToolActiveFixture(t).Rules[0].Hash {
		t.Fatalf("template hash %s does not match the hook fixture", rules.Hash(r))
	}
	if _, err := runErr(t, "rules", "add", "--template", "compound-cd"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate add err = %v", err)
	}
	run(t, "rules", "add", "--id", "no-sudo", "--match", `^\s*sudo\b`, "--message", "Do not use sudo.", "--title", "No sudo")
	if rows := rulesListForTest(t); len(rows) != 2 || rows[0].State != rules.StateInactive || rows[1].ID != "no-sudo" {
		t.Fatalf("list = %+v", rows)
	}
}

func TestRulesAddRejectsPythonOnlySyntax(t *testing.T) {
	withTempHome(t)
	for _, tc := range []struct{ match, want string }{
		{`cd(?!x)`, "lookahead is not supported"},
		{`(?<!x)cd`, "lookbehind is not supported"},
	} {
		_, err := runErr(t, "rules", "add", "--id", "x", "--match", tc.match, "--message", "m")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("match %q: err = %v", tc.match, err)
		}
	}
	if _, err := runErr(t, "rules", "add", "--id", "x", "--match", "y"); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("missing message: err = %v", err)
	}
}

func TestRulesApproveRefusesNonInteractive(t *testing.T) {
	withTestVerifiedOwnerAction(t)
	withTempHome(t)
	clearAgentEnv(t)
	run(t, "rules", "add", "--template", "compound-cd")
	prev := rulesInteractive
	rulesInteractive = func(io.Reader) bool { return false }
	t.Cleanup(func() { rulesInteractive = prev })
	for _, args := range [][]string{
		{"rules", "approve", "compound-cd", "--mode", "warn", "--holdout"},
		{"rules", "off", "compound-cd"},
	} {
		_, err := runErr(t, args...)
		if err == nil || err.Error() != rulesRefusal {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
	if _, err := os.Stat(rules.ApprovalsPath(mustConfig(t).StateDir)); !os.IsNotExist(err) {
		t.Fatalf("a refused approval must write nothing, stat err = %v", err)
	}
	// The real gate refuses the non-TTY stdin the test harness passes.
	rulesInteractive = prev
	if _, err := runErr(t, "rules", "approve", "compound-cd", "--mode", "warn"); err == nil || err.Error() != rulesRefusal {
		t.Fatalf("non-TTY stdin: err = %v", err)
	}
}

func TestRulesApproveRefusesInsideAgentSession(t *testing.T) {
	withTempHome(t)
	run(t, "rules", "add", "--template", "compound-cd")
	for _, key := range rulesAgentEnv {
		subRun(t, key, func(t *testing.T) {
			stubRulesTerminal(t, "compound-cd")
			t.Setenv(key, "1")
			want := fmt.Sprintf(rulesAgentRefusal, key)
			for _, args := range [][]string{
				{"rules", "approve", "compound-cd", "--mode", "warn"},
				{"rules", "off", "compound-cd"},
			} {
				if _, err := runErr(t, args...); err == nil || err.Error() != want {
					t.Fatalf("%v: err = %v", args, err)
				}
			}
		})
	}
}

// Codex user settings such as CODEX_HOME live in people's own shell profiles.
// They must not lock the person out of approving rules.
func TestRulesApproveAllowsCodexUserSettings(t *testing.T) {
	withTempHome(t)
	run(t, "rules", "add", "--template", "compound-cd")
	stubRulesTerminal(t, "compound-cd")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), ".codex"))
	t.Setenv("CODEX_API_KEY", "x")
	if _, err := runErr(t, "rules", "approve", "compound-cd", "--mode", "warn"); err != nil {
		t.Fatalf("approve with only Codex user settings set: %v", err)
	}
	if got := rulesActiveIDs(t, mustConfig(t)); len(got) != 1 || got[0] != "compound-cd" {
		t.Fatalf("active = %v", got)
	}
}

func TestRulesApproveWrongIDRefuses(t *testing.T) {
	withTempHome(t)
	run(t, "rules", "add", "--template", "compound-cd")
	stubRulesTerminal(t, "compound")
	if _, err := runErr(t, "rules", "approve", "compound-cd", "--mode", "warn"); err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(rules.ApprovalsPath(mustConfig(t).StateDir)); !os.IsNotExist(err) {
		t.Fatalf("a refused approval must write nothing, stat err = %v", err)
	}
	if _, err := runErr(t, "rules", "approve", "compound-cd", "--mode", "block"); err == nil || !strings.Contains(err.Error(), "block is not supported") {
		t.Fatalf("block: err = %v", err)
	}
}

func TestRulesApproveCompilesActive(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	run(t, "rules", "add", "--template", "compound-cd")
	stubRulesTerminal(t, "compound-cd")
	out := run(t, "rules", "approve", "compound-cd", "--mode", "warn", "--holdout")
	if !strings.Contains(out, "fdac46be8abf") || !strings.HasSuffix(out, "approved compound-cd (warn, holdout). Active rules: 1\n") {
		t.Fatalf("approve output = %q", out)
	}
	if ids := rulesActiveIDs(t, cfg); strings.Join(ids, ",") != "compound-cd" {
		t.Fatalf("active = %v", ids)
	}
	rows := rulesListForTest(t)
	if len(rows) != 1 || rows[0].State != rules.StateActive || rows[0].Mode != "warn" || !rows[0].Holdout || rows[0].Hash != "fdac46be8abf" {
		t.Fatalf("list = %+v", rows)
	}
	// The approved rule now warns through the real hook.
	got := decodeHookOutput(t, runPreTool(t, preToolInputS1))
	if !strings.HasPrefix(got.HookSpecificOutput.AdditionalContext, "Use one command per Bash call.") {
		t.Fatalf("hook output = %+v", got)
	}
}

func TestRulesEditedFileShowsChangedAndCompileDrops(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	run(t, "rules", "add", "--template", "compound-cd")
	stubRulesTerminal(t, "compound-cd")
	run(t, "rules", "approve", "compound-cd", "--mode", "warn")
	path := rules.RulePath(cfg.VaultDir, "compound-cd")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "Use one command", "Please use one command", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if rows := rulesListForTest(t); len(rows) != 1 || rows[0].State != rules.StateChanged {
		t.Fatalf("list = %+v", rows)
	}
	if out := run(t, "rules", "list"); !strings.Contains(out, "changed since approval") {
		t.Fatalf("text list = %q", out)
	}
	if ids := rulesActiveIDs(t, cfg); len(ids) != 1 {
		t.Fatalf("before compile = %v", ids)
	}
	if out := run(t, "rules", "compile"); !strings.HasSuffix(out, "Active rules: 0\n") {
		t.Fatalf("compile output = %q", out)
	}
	if ids := rulesActiveIDs(t, cfg); len(ids) != 0 {
		t.Fatalf("edited rule must drop out, active = %v", ids)
	}
}

func TestRulesOffRemovesFromActive(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	run(t, "rules", "add", "--template", "compound-cd")
	stubRulesTerminal(t, "compound-cd")
	run(t, "rules", "approve", "compound-cd", "--mode", "shadow")
	if out := run(t, "rules", "off", "compound-cd"); !strings.HasSuffix(out, "turned off compound-cd. Active rules: 0\n") {
		t.Fatalf("off output = %q", out)
	}
	if ids := rulesActiveIDs(t, cfg); len(ids) != 0 {
		t.Fatalf("active = %v", ids)
	}
	if rows := rulesListForTest(t); len(rows) != 1 || rows[0].State != rules.StateInactive || rows[0].Mode != "off" {
		t.Fatalf("list = %+v", rows)
	}
}

func TestRulesTestHitAndMiss(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	run(t, "rules", "add", "--template", "compound-cd")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"compound-cd", "--", "cd /tmp && ls"}, "hit\n"},
		{[]string{"compound-cd", "--", "cd /tmp", "&&", "make"}, "hit\n"},
		{[]string{"compound-cd", "--", "ls && cd x"}, "miss\n"},
		{[]string{"--match", `\bmora\s+read\b`, "--unless", `--json\b`, "--", "mora read x --json"}, "miss\n"},
		{[]string{"--match", `\bmora\s+read\b`, "--unless", `--json\b`, "--", "mora read x; mora search y --json"}, "hit\n"},
		{[]string{"--match", "x", "--context", "subagent", "--", "x"}, "miss\n"},
		{[]string{"--match", "x", "--context", "subagent", "--agent", "a1", "--", "x"}, "hit\n"},
	} {
		if got := run(t, append([]string{"rules", "test"}, tc.args...)...); got != tc.want {
			t.Fatalf("rules test %q = %q, want %q", tc.args, got, tc.want)
		}
	}
	if _, err := runErr(t, "rules", "test", "compound-cd"); err == nil || !strings.Contains(err.Error(), "ASCII only") {
		t.Fatalf("missing command: err = %v", err)
	}
	assertNoPreToolLog(t, cfg)
}

func TestRulesListJSON(t *testing.T) {
	withTempHome(t)
	if rows := rulesListForTest(t); len(rows) != 0 {
		t.Fatalf("fresh list = %+v", rows)
	}
	cfg := mustConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, "rules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rules.RulePath(cfg.VaultDir, "broken"), []byte("not a rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := rulesListForTest(t)
	if len(rows) != 1 || rows[0].ID != "broken" || !strings.HasPrefix(rows[0].State, "invalid: ") || rows[0].Hash != "" {
		t.Fatalf("list = %+v", rows)
	}
}

func TestRulesFilesAreNotListedAsMemories(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "rules", "add", "--template", "compound-cd")
	run(t, "index", "rebuild")
	mems, err := decodeMemoriesJSON(t, run(t, "list", "--json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if strings.Contains(m.Title, "One command per Bash call") || strings.Contains(m.Text, "chaining cd") {
			t.Fatalf("rule file was indexed as a memory: %+v", m)
		}
	}
}

func TestMCPCatalogHasNoRuleTool(t *testing.T) {
	for _, def := range mcppkg.ToolCatalog() {
		if strings.Contains(strings.ToLower(def.Name), "rule") {
			t.Fatalf("MCP tool %q touches rules; rule approval must stay out of agent reach", def.Name)
		}
	}
	files, err := filepath.Glob(filepath.Join("..", "mcp", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob internal/mcp: %v %v", files, err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		parsed, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == "github.com/pyranthus-hq/mora/internal/rules" {
				t.Fatalf("%s imports internal/rules", f)
			}
		}
	}
}

// The approval screen quotes any value a terminal would not print as itself.
func TestRulesShowQuotesNonGraphic(t *testing.T) {
	for in, want := range map[string]string{
		`^\s*cd\s+[^;&|\n]*?\s*(&&|;)`: `^\s*cd\s+[^;&|\n]*?\s*(&&|;)`,
		"plain message":                "plain message",
		".|\rmatch: x":                 `".|\rmatch: x"`,
		"a\x1b7b":                      `"a\x1b7b"`,
		"a\u202eb":                     `"a\u202eb"`,
	} {
		if got := rulesShow(in); got != want {
			t.Fatalf("rulesShow(%q) = %s, want %s", in, got, want)
		}
	}
}
