package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func runHookRaw(t *testing.T, args []string, input string) string {
	t.Helper()
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), args, &out, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func decodeAntigravity(t *testing.T, raw string) antigravityHookEnvelope {
	t.Helper()
	if strings.Contains(raw, "hookSpecificOutput") {
		t.Fatalf("Claude envelope on an Antigravity hook: %s", raw)
	}
	var env antigravityHookEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	return env
}

func antigravityRecallInput(t *testing.T, conv, prompt string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"conversationId": conv, "workspacePaths": []string{t.TempDir()},
		"transcriptPath": "/x/transcript.jsonl", "modelName": "auto", "lastUserInput": prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const eelpoutPrompt = "What did we decide about eelpout recall token alpha?"

func TestAntigravityRecallEnvelope(t *testing.T) {
	cfg := seedRecallMemories(t, 2)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	raw := runHookRaw(t, []string{"recall"}, antigravityRecallInput(t, "conv-1", eelpoutPrompt))
	env := decodeAntigravity(t, raw)
	if len(env.InjectSteps) != 1 {
		t.Fatalf("want one injected step, got %s", raw)
	}
	msg := env.InjectSteps[0].SystemMessage
	if !strings.Contains(msg.SystemMessage, "eelpout recall token alpha") || !strings.Contains(msg.SystemMessage, "Mora recall") {
		t.Fatalf("recall context missing from systemMessage: %s", raw)
	}
	if msg.Metadata == nil || !strings.Contains(msg.Metadata.UserMarkdown, "Mora stepped in") {
		t.Fatalf("user_markdown banner missing: %s", raw)
	}
	if strings.Contains(msg.SystemMessage, "stepped in") {
		t.Fatalf("the user banner must not reach the model: %s", raw)
	}
	// The hand-over is logged with the real caller, not claude-code.
	lines := readGivenLines(cfg)
	if len(lines) == 0 {
		t.Fatal("no given line written")
	}
	var g givenLine
	if err := json.Unmarshal(lines[len(lines)-1], &g); err != nil || g.Caller != "antigravity" {
		t.Fatalf("given caller = %q (%v)", g.Caller, err)
	}
}

// PreInvocation runs before every model call. The same user message must not
// re-inject the same notes on each tool step of the turn.
func TestAntigravityRecallOncePerUserMessage(t *testing.T) {
	cfg := seedRecallMemories(t, 1)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	in := antigravityRecallInput(t, "conv-2", eelpoutPrompt)
	if first := runHookRaw(t, []string{"recall"}, in); len(decodeAntigravity(t, first).InjectSteps) != 1 {
		t.Fatalf("first call must inject: %s", first)
	}
	if second := runHookRaw(t, []string{"recall"}, in); strings.TrimSpace(second) != "{}" {
		t.Fatalf("repeat of the same message must answer {}, got %s", second)
	}
	other := antigravityRecallInput(t, "conv-2", "And what about eelpout recall token alpha later?")
	if third := runHookRaw(t, []string{"recall"}, other); len(decodeAntigravity(t, third).InjectSteps) != 1 {
		t.Fatalf("a new user message must inject again: %s", third)
	}
}

func TestAntigravityRecallNothingToInjectIsEmptyObject(t *testing.T) {
	cfg := seedRecallMemories(t, 1)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"no match":     antigravityRecallInput(t, "c3", "zzzznomatch uniquely absent query terms"),
		"cheap prompt": antigravityRecallInput(t, "c4", "ok"),
		"bad json":     `not json`,
	} {
		raw := runHookRaw(t, []string{"recall", "--client", "antigravity"}, in)
		if strings.TrimSpace(raw) != "{}" {
			t.Errorf("%s: want {}, got %q", name, raw)
		}
	}
}

func TestAntigravityClientFlagWithClaudeShapedInput(t *testing.T) {
	cfg := seedRecallMemories(t, 1)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	raw := runHookRaw(t, []string{"recall", "--client", "antigravity"}, `{"prompt":`+quoteJSON(eelpoutPrompt)+`}`)
	if len(decodeAntigravity(t, raw).InjectSteps) != 1 {
		t.Fatalf("--client antigravity must use the Antigravity envelope: %s", raw)
	}
}

func TestAntigravitySessionStartEnvelope(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubHookBrief(t, "today's local brief")()
	in := `{"conversationId":"c","workspacePaths":["` + filepath.ToSlash(t.TempDir()) + `"],"modelName":"auto"}`
	env := decodeAntigravity(t, runHookRaw(t, []string{"session-start"}, in))
	if len(env.InjectSteps) != 1 || env.InjectSteps[0].SystemMessage.SystemMessage != "today's local brief" {
		t.Fatalf("session start envelope = %+v", env)
	}
	if md := env.InjectSteps[0].SystemMessage.Metadata; md == nil || !strings.Contains(md.UserMarkdown, "Mora") {
		t.Fatalf("missing user_markdown: %+v", env)
	}
}

func TestAntigravitySessionStartSilentIsEmptyObject(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubHookBrief(t, "  ")()
	raw := runHookRaw(t, []string{"session-start", "--client", "antigravity"}, `{"workspacePaths":["`+filepath.ToSlash(t.TempDir())+`"]}`)
	if strings.TrimSpace(raw) != "{}" {
		t.Fatalf("want {}, got %q", raw)
	}
}

func TestAntigravityStopAnswersEmptyObject(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	raw := runHookRaw(t, []string{"session-end", "--client", "antigravity"}, `{"workspacePaths":["`+filepath.ToSlash(t.TempDir())+`"],"transcriptPath":"/x/t.jsonl"}`)
	if strings.TrimSpace(raw) != "{}" {
		t.Fatalf("want {}, got %q", raw)
	}
}

// A Windows workspace path in the payload reaches the project binding on WSL.
func TestAntigravityWorkspacePathWindowsFormUsesBinding(t *testing.T) {
	skipWSLSimulationOnWindows(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	mnt := t.TempDir()
	dir := filepath.Join(mnt, "c", "Users", "Adit", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	withWSLEnv(t, "linux", mnt)
	if _, _, err := bindProjectDir(testCtx(t), dir, "project:app"); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "project:app", Type: "decision", Source: "manual",
		CreatedAt: "2026-06-01T00:00:00Z", Title: "App owner rule", Text: "Deploys go through the staging gate."}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"workspacePaths": []string{`C:\Users\Adit\app`}})
	raw := runHookRaw(t, []string{"session-start"}, string(in))
	env := decodeAntigravity(t, raw)
	if len(env.InjectSteps) != 1 || !strings.Contains(env.InjectSteps[0].SystemMessage.SystemMessage, "staging gate") {
		t.Fatalf("bound project brief not found through a Windows path: %s", raw)
	}
}

func readHooksJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("hooks.json is not valid JSON: %v\n%s", err, b)
	}
	return m
}

const orcaHooksFixture = `{
  "orca-status": {
    "PreInvocation": [{"type": "command", "command": "C:\\orca\\pre.cmd", "timeout": 10}],
    "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "C:\\orca\\post.cmd"}]}]
  }
}`

func TestAntigravityInstallPreservesOtherHooksAndIsIdempotent(t *testing.T) {
	home := withTempHookHome(t)
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(orcaHooksFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(orcaHooksFixture), &want)

	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := cmdHook(testCtx(t), []string{"install", "--client", "antigravity"}, &out, testStderr, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
	}
	got := readHooksJSON(t, path)
	if !reflect.DeepEqual(got["orca-status"], want["orca-status"]) {
		t.Fatalf("a foreign hook was changed: %v", got["orca-status"])
	}
	mora, ok := got["mora"].(map[string]any)
	if !ok {
		t.Fatalf("no mora entry: %v", got)
	}
	for ev, sub := range map[string]string{"SessionStart": "hook session-start --client antigravity", "PreInvocation": "hook recall --client antigravity"} {
		list, _ := mora[ev].([]any)
		if len(list) != 1 {
			t.Fatalf("%s: want exactly one handler after installing twice, got %v", ev, mora[ev])
		}
		h := list[0].(map[string]any)
		if h["type"] != "command" || !strings.Contains(h["command"].(string), sub) {
			t.Fatalf("%s handler = %v", ev, h)
		}
	}
	if _, has := mora["Stop"]; has {
		t.Fatal("Stop is not installed: session-end reads Claude transcripts")
	}

	var st bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"status", "--client", "antigravity"}, &st, testStderr, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.String(), "SessionStart: installed") || !strings.Contains(st.String(), "PreInvocation: installed") {
		t.Fatalf("status = %q", st.String())
	}

	out.Reset()
	if err := cmdHook(testCtx(t), []string{"uninstall", "--client", "antigravity"}, &out, testStderr, nil); err != nil {
		t.Fatal(err)
	}
	after := readHooksJSON(t, path)
	if _, has := after["mora"]; has {
		t.Fatal("mora entry survived uninstall")
	}
	if !reflect.DeepEqual(after["orca-status"], want["orca-status"]) {
		t.Fatal("uninstall touched a foreign hook")
	}
}

func TestAntigravityInstallCreatesFileAndHonoursHooksFile(t *testing.T) {
	withTempHookHome(t)
	path := filepath.Join(t.TempDir(), "nested", "hooks.json")
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"install", "--client", "antigravity", "--hooks-file", path, "--threshold", "-1"}, &out, testStderr, nil); err != nil {
		t.Fatal(err)
	}
	mora := readHooksJSON(t, path)["mora"].(map[string]any)
	cmd := mora["PreInvocation"].([]any)[0].(map[string]any)["command"].(string)
	if !strings.Contains(cmd, "--threshold -1") {
		t.Fatalf("threshold not carried into the recall command: %s", cmd)
	}
}

func TestAntigravityInstallRefusesMalformedFile(t *testing.T) {
	withTempHookHome(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	const body = `["not", "an", "object"]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"install", "--client", "antigravity", "--hooks-file", path}, &out, testStderr, nil); err == nil {
		t.Fatal("a hooks.json that is not an object must be refused")
	}
	if b, _ := os.ReadFile(path); string(b) != body {
		t.Fatalf("a refused install must not rewrite the file: %s", b)
	}
}

func TestHookClientFlagRejectsUnknownClient(t *testing.T) {
	withTempHookHome(t)
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"install", "--client", "emacs"}, &out, testStderr, nil); err == nil {
		t.Fatal("unknown client must be rejected")
	}
	if err := cmdHook(testCtx(t), []string{"install", "--hooks-file", "/x"}, &out, testStderr, nil); err == nil {
		t.Fatal("--hooks-file without --client antigravity must be rejected")
	}
}
