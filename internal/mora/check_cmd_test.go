package mora

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/corrections"
)

const checkCanary = "CANARY_7f3a"

// checkDoc is the decoded --json receipt.
type checkDoc struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	corrections.Report
}

// copyCheckFixture puts the shared synthetic transcripts in the sandbox home.
// The repo holds them as claude and codex, because .gitignore ignores .claude.
func copyCheckFixture(t *testing.T, parts ...string) {
	t.Helper()
	home := mustConfig(t).HomeDir()
	for _, part := range parts {
		src := filepath.Join("..", "sessions", "testdata", "home", part)
		if err := os.CopyFS(filepath.Join(home, "."+part), os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
}

func decodeCheck(t *testing.T, stdout string) checkDoc {
	t.Helper()
	var doc checkDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if doc.Schema != "mora.check" || doc.SchemaVersion != 1 {
		t.Fatalf("envelope = %q v%d", doc.Schema, doc.SchemaVersion)
	}
	return doc
}

func TestCheckGolden(t *testing.T) {
	withTempHome(t)
	copyCheckFixture(t, "claude", "codex")
	want, err := os.ReadFile(filepath.Join("testdata", "check", "check_fixture.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, "check"); got != string(want) {
		t.Fatalf("check output differs from the golden:\n%s", got)
	}
}

func TestCheckJSON(t *testing.T) {
	withTempHome(t)
	copyCheckFixture(t, "claude", "codex")
	stdout, stderr, err := runSplit(t, "check", "--json")
	if err != nil || stderr != "" {
		t.Fatalf("err = %v, stderr = %q", err, stderr)
	}
	doc := decodeCheck(t, stdout)
	if want := (corrections.Window{First: "2026-10-01", Last: "2026-10-04"}); doc.Window != want {
		t.Errorf("window = %+v", doc.Window)
	}
	harnesses := []corrections.Harness{
		{Name: "claude", Sessions: 5, SubagentFiles: 1, Calls: 18, Signal: "typed"},
		{Name: "codex", Sessions: 1, Calls: 2, Signal: "no_rejection_signal"},
	}
	if !reflect.DeepEqual(doc.Harnesses, harnesses) {
		t.Errorf("harnesses = %+v", doc.Harnesses)
	}
	if want := (corrections.Counts{BashRejected: 4, BashInterrupted: 1, OtherRejected: 2, OtherInterrupted: 1, UserActions: 7}); doc.Corrections != want {
		t.Errorf("corrections = %+v", doc.Corrections)
	}
	rep := doc.Repeat
	if rep.Scored != 5 || rep.Eligible != 4 || rep.SameCommand != 2 || rep.SameShape != 3 || rep.BaselinePairs != 9 || rep.BaselineRepeated != 4 {
		t.Errorf("repeat = %+v", rep)
	}
	if rep.Rate == nil || *rep.Rate != 0.5 || rep.ShapeRate == nil || *rep.ShapeRate != 0.75 || rep.BaselineRate == nil || *rep.BaselineRate != 0.4444 {
		t.Errorf("rates = %+v", rep)
	}
	top := []corrections.Pattern{
		{Shape: "git push --force", SameCommand: 1, SameShape: 1},
		{Shape: "npm publish", SameCommand: 1, SameShape: 1},
		{Shape: "rm -rf", SameShape: 1},
	}
	if !reflect.DeepEqual(doc.Top, top) {
		t.Errorf("top_patterns = %+v", doc.Top)
	}
	if want := (corrections.NotCounted{BlockedByRule: 1, BlockedByAutoMode: 1, CancelledSiblings: 1, InterruptedInSubagent: 1}); doc.NotCounted != want {
		t.Errorf("not_counted = %+v", doc.NotCounted)
	}
	read := doc.Read
	if read.FilesRead != 7 || read.EmptyFiles != 1 || read.UnknownRecords != 1 || read.MalformedLines != 1 ||
		read.PartialLastLines != 1 || read.CopiedCalls != 4 || read.CodexCommandsUnreadable != 1 ||
		read.ClaudeSessions != 5 || read.ClaudeSubagentFiles != 1 || read.CodexSessions != 1 {
		t.Errorf("read = %+v", read)
	}
	walkCheckJSON(t, stdout, "fake-work", "sess-a", "example.invalid", "origin main")
}

func TestCheckJSONEmptyHomeHasNoNullArrays(t *testing.T) {
	withTempHome(t)
	stdout, _, err := runSplit(t, "check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"harnesses", "top_patterns"} {
		if _, ok := doc[name].([]any); !ok {
			t.Errorf("%s = %#v, want an array", name, doc[name])
		}
	}
	repeat := doc["repeat"].(map[string]any)
	for _, name := range []string{"rate", "shape_rate", "baseline_rate"} {
		if v, ok := repeat[name]; !ok || v != nil {
			t.Errorf("%s = %v, want null", name, v)
		}
	}
}

// walkCheckJSON decodes a receipt and checks every key and string value.
func walkCheckJSON(t *testing.T, stdout string, forbidden ...string) {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	privateKeys := map[string]bool{"command": true, "cwd": true, "path": true, "prompt": true, "session_id": true, "host": true, "tool_use_id": true}
	checkText := func(s string) {
		t.Helper()
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(s), strings.ToLower(bad)) {
				t.Errorf("%q holds %q", s, bad)
			}
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, value := range v {
				if privateKeys[key] {
					t.Errorf("key %q must not appear", key)
				}
				checkText(key)
				walk(value)
			}
		case []any:
			for _, value := range v {
				walk(value)
			}
		case string:
			checkText(v)
		}
	}
	walk(doc)
}

func canaryAssistant(sessionFields, id, name, command, ts string) string {
	input, _ := json.Marshal(map[string]string{"command": command, "description": checkCanary})
	return fmt.Sprintf(`{"message":{"id":"msg-%s-CANARY_7f3a","role":"assistant","content":[{"type":"tool_use","id":"%s","name":"%s","input":%s}]},"type":"assistant","timestamp":"%s",%s}`+"\n",
		id, id, name, input, ts, sessionFields)
}

func canaryRejection(sessionFields, id, kind, ts string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"CANARY_7f3a tool text","is_error":true,"tool_use_id":"%s"}]},"timestamp":"%s","toolUseResult":"CANARY_7f3a result","toolDenialKind":"%s","userFeedback":"CANARY_7f3a my words",%s}`+"\n",
		id, ts, kind, sessionFields)
}

func writeCanaryFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCanaryHome places the canary in every field a transcript can hold.
func writeCanaryHome(t *testing.T) {
	t.Helper()
	home := mustConfig(t).HomeDir()
	project := filepath.Join(home, ".claude", "projects", "-CANARY_7f3a-work")
	one := `"sessionId":"CANARY_7f3a-one","cwd":"/CANARY_7f3a/work","gitBranch":"CANARY_7f3a","agentId":"CANARY_7f3a","version":"CANARY_7f3a","entrypoint":"CANARY_7f3a"`
	two := strings.ReplaceAll(one, "CANARY_7f3a-one", "CANARY_7f3a-two")
	prompt := func(fields string) string {
		return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"CANARY_7f3a prompt"},"timestamp":"2026-10-01T10:00:00.000Z","turnOrigin":"human",%s}`+"\n", fields)
	}
	shapedCommand := "git push --force CANARY_7f3a"
	otherCommand := "CANARY_7f3a --flag=CANARY_7f3a"
	hostCommand := "curl https://CANARY_7f3a.example.invalid/x?token=CANARY_7f3a"
	writeCanaryFile(t, filepath.Join(project, "CANARY_7f3a-one.jsonl"),
		prompt(one),
		canaryAssistant(one, "c1", "Bash", shapedCommand, "2026-10-01T10:01:00.000Z"),
		canaryRejection(one, "c1", "user-rejected", "2026-10-01T10:01:05.000Z"),
		canaryAssistant(one, "c2", "Bash", otherCommand, "2026-10-01T10:02:00.000Z"),
		canaryRejection(one, "c2", "interrupted", "2026-10-01T10:02:05.000Z"),
		canaryAssistant(one, "c3", "Bash", hostCommand, "2026-10-01T10:03:00.000Z"),
		canaryRejection(one, "c3", "user-rejected", "2026-10-01T10:03:05.000Z"),
		canaryAssistant(one, "c4", "CANARY_7f3a", "x", "2026-10-01T10:04:00.000Z"),
		canaryRejection(one, "c4", "user-rejected", "2026-10-01T10:04:05.000Z"),
		canaryAssistant(one, "c5", "Bash", shapedCommand, "2026-10-01T10:05:00.000Z"),
		canaryRejection(one, "c5", "CANARY_7f3a", "2026-10-01T10:05:05.000Z"),
		`{"type":"CANARY_7f3a","CANARY_7f3a":"CANARY_7f3a"}`+"\n",
		"CANARY_7f3a not json\n",
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user CANARY_7f3a]"}]},"timestamp":"2026-10-01T10:06:00.000Z",`+one+"}\n",
		`{"type":"assistant","message":{"id":"CANARY_7f3a","content":[{"type":"tool_use","name":"Bash","input":{"command":"CANARY_7f3a no id"}}]},"timestamp":"CANARY_7f3a",`+one+"}\n",
		`{"type":"assistant","message":{"id":"CANARY_7f3a","content":[{"type":"tool_use","name":"Bash","input":{"command":"CANARY_7f3a no id"}}]},"timestamp":"2026-10-01T10:07:00.000Z",`+one+"}\n",
		`{"type":"user","CANARY_7f3a torn last line`,
	)
	writeCanaryFile(t, filepath.Join(project, "CANARY_7f3a-one", "subagents", "agent-CANARY_7f3a.jsonl"),
		canaryAssistant(one, "c6", "Bash", "CANARY_7f3a sub", "2026-10-01T10:08:00.000Z"),
		canaryRejection(one, "c6", "user-rejected", "2026-10-01T10:08:05.000Z"),
	)
	writeCanaryFile(t, filepath.Join(project, "CANARY_7f3a-two.jsonl"),
		prompt(two),
		canaryAssistant(two, "d1", "Bash", shapedCommand, "2026-10-02T10:00:00.000Z"),
		canaryAssistant(two, "d2", "Bash", otherCommand, "2026-10-02T10:01:00.000Z"),
		canaryAssistant(two, "d3", "Bash", hostCommand, "2026-10-02T10:02:00.000Z"),
		canaryAssistant(two, "d4", "Bash", "CANARY_7f3a baseline", "2026-10-02T10:03:00.000Z"),
	)
	writeCanaryFile(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "03", "rollout-CANARY_7f3a.jsonl"),
		`{"timestamp":"2026-10-03T12:00:00.000Z","type":"session_meta","payload":{"id":"CANARY_7f3a-thread","timestamp":"2026-10-03T12:00:00.000Z","cwd":"/CANARY_7f3a","originator":"CANARY_7f3a","cli_version":"CANARY_7f3a","instructions":"CANARY_7f3a","git":{"branch":"CANARY_7f3a","repository_url":"https://CANARY_7f3a.example.invalid"}}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:01.000Z","type":"turn_context","payload":{"cwd":"/CANARY_7f3a","model":"CANARY_7f3a","approval_policy":"CANARY_7f3a"}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"CANARY_7f3a prompt"}]}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:05.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"CANARY_7f3a.exec","call_id":"CANARY_7f3a-1","input":"await tools.exec_command({cmd: \"git push --force CANARY_7f3a\"});"}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:06.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"CANARY_7f3a-1","output":"CANARY_7f3a rejected by user"}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:07.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"CANARY_7f3a-2","input":"await tools.exec_command({cmd: CANARY_7f3a});"}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:08.000Z","type":"response_item","payload":{"type":"function_call","name":"CANARY_7f3a.shell","call_id":"CANARY_7f3a-3","arguments":"{\"command\":[\"bash\",\"-lc\",\"CANARY_7f3a --flag=CANARY_7f3a\"],\"workdir\":\"/CANARY_7f3a\"}"}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:09.000Z","type":"response_item","payload":{"type":"local_shell_call","call_id":"CANARY_7f3a-4","action":{"type":"exec","command":["CANARY_7f3a","local"],"working_directory":"/CANARY_7f3a"}}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:10.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"CANARY_7f3a","reason":"CANARY_7f3a"}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:11.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"CANARY_7f3a","status":"declined","stdout":"CANARY_7f3a"}}`+"\n",
		`{"timestamp":"2026-10-03T12:00:12.000Z","type":"CANARY_7f3a","payload":{"CANARY_7f3a":"CANARY_7f3a"}}`+"\n",
	)
}

func assertNoCanary(t *testing.T, name, s string) {
	t.Helper()
	if strings.Contains(strings.ToLower(s), strings.ToLower(checkCanary)) {
		t.Errorf("%s holds the canary:\n%s", name, s)
	}
}

func TestCheckCanary(t *testing.T) {
	withTempHome(t)
	writeCanaryHome(t)
	textOut, textErr, err := runSplit(t, "check")
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanary(t, "text stdout", textOut)
	assertNoCanary(t, "text stderr", textErr)

	stdout, stderr, err := runSplit(t, "check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanary(t, "json stdout", stdout)
	assertNoCanary(t, "json stderr", stderr)
	walkCheckJSON(t, stdout, checkCanary, "example.invalid")

	// The canary only proves something if these records reached the report.
	doc := decodeCheck(t, stdout)
	if doc.Corrections.BashRejected != 3 || doc.Corrections.BashInterrupted != 1 || doc.Corrections.OtherRejected != 1 {
		t.Errorf("corrections = %+v", doc.Corrections)
	}
	if doc.Repeat.SameCommand != 3 || doc.NotCounted.UnknownDenialKinds != 1 || doc.Read.CallsWithoutID != 2 {
		t.Errorf("repeat = %+v, not_counted = %+v, read = %+v", doc.Repeat, doc.NotCounted, doc.Read)
	}
	if doc.Read.CodexCommandsUnreadable != 1 || doc.Read.CodexTurnInterrupts != 1 || doc.Read.CodexPatchesDeclined != 1 {
		t.Errorf("read = %+v", doc.Read)
	}
	shapes := []string{}
	for _, p := range doc.Top {
		shapes = append(shapes, p.Shape)
	}
	if want := []string{"curl", "git push --force", "(other command)"}; !reflect.DeepEqual(shapes, want) {
		t.Errorf("top shapes = %q", shapes)
	}
}

func TestCheckWording(t *testing.T) {
	withTempHome(t)
	copyCheckFixture(t, "claude", "codex")
	for _, args := range [][]string{{"check"}, {"check", "--since", "2026-10-04"}} {
		out := run(t, args...)
		if !strings.Contains(out, "rejected or interrupted") {
			t.Errorf("%v: missing the wording:\n%s", args, out)
		}
		if strings.Contains(strings.ToLower(out), "mistake") {
			t.Errorf("%v: must not say mistake:\n%s", args, out)
		}
	}
	stdout, _, err := runSplit(t, "check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	walkCheckJSON(t, stdout, "mistake")
}

func TestCheckNoRateYet(t *testing.T) {
	withTempHome(t)
	copyCheckFixture(t, "claude", "codex")
	out := run(t, "check", "--since", "2026-10-04")
	for _, want := range []string{
		"Window: since 2026-10-04, 2026-10-04 to 2026-10-04\n",
		"  Commands rejected or interrupted: 1\n  With a later session: 0\n",
		"  No rate yet: no rejected or interrupted command has a later session.\n",
		"Top repeated patterns (same command / same shape)\n  none\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"Ran again", "Baseline", "Small sample"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q must not print:\n%s", gone, out)
		}
	}
}

func TestCheckSince(t *testing.T) {
	withTempHome(t)
	copyCheckFixture(t, "claude", "codex")
	stdout, _, err := runSplit(t, "check", "--since", "2026-10-03", "--json")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeCheck(t, stdout)
	if want := (corrections.Window{Since: "2026-10-03", First: "2026-10-03", Last: "2026-10-04"}); doc.Window != want {
		t.Errorf("window = %+v", doc.Window)
	}
	if want := (corrections.Counts{BashRejected: 1, UserActions: 1}); doc.Corrections != want {
		t.Errorf("corrections = %+v", doc.Corrections)
	}
	if doc.Harnesses[0].Calls != 7 || doc.Harnesses[1].Calls != 2 {
		t.Errorf("harnesses = %+v", doc.Harnesses)
	}
}

func TestCheckOldFilesAreNotEmptyHome(t *testing.T) {
	withTempHome(t)
	copyCheckFixture(t, "claude")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	err := filepath.WalkDir(mustConfig(t).HomeDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(path, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, "check", "--since", "2026-10-01")
	if strings.Contains(out, "No Claude Code or Codex transcripts found.") || !strings.Contains(out, "Window: since 2026-10-01\n") {
		t.Errorf("output:\n%s", out)
	}
}

func TestCheckCodexNoSignal(t *testing.T) {
	cases := []struct {
		name   string
		parts  []string
		claude int
		codex  int
	}{
		{"zero Claude data", []string{"codex"}, 0, 2},
		{"with Claude data", []string{"claude", "codex"}, 18, 2},
	}
	for _, c := range cases {
		subRun(t, c.name, func(t *testing.T) {
			withTempHome(t)
			copyCheckFixture(t, c.parts...)
			if out := run(t, "check"); !strings.Contains(out, "no rejection signal") {
				t.Errorf("text:\n%s", out)
			}
			stdout, _, err := runSplit(t, "check", "--json")
			if err != nil {
				t.Fatal(err)
			}
			doc := decodeCheck(t, stdout)
			if doc.Harnesses[1].Signal != "no_rejection_signal" || doc.Harnesses[1].Calls != c.codex || doc.Harnesses[0].Calls != c.claude {
				t.Errorf("harnesses = %+v", doc.Harnesses)
			}
		})
	}
}

func TestCheckEmptyHome(t *testing.T) {
	withTempHome(t)
	want := "mora check (read-only, counts only, safe to paste)\nNo Claude Code or Codex transcripts found.\n"
	if got := run(t, "check"); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestCheckBadFlags(t *testing.T) {
	cases := []struct {
		args []string
		code string
	}{
		{[]string{"check", "--since", "nope"}, errCodeUsageUnknownValue},
		{[]string{"check", "--since", "0d"}, errCodeUsageUnknownValue},
		{[]string{"check", "--since", "-1d"}, errCodeUsageUnknownValue},
		{[]string{"check", "--since", ""}, errCodeUsageUnknownValue},
		{[]string{"check", "--since="}, errCodeUsageUnknownValue},
		{[]string{"check", "--since", "0001-01-01"}, errCodeUsageUnknownValue},
		{[]string{"check", "--since", "999999999d"}, errCodeUsageUnknownValue},
		{[]string{"check", "extra"}, errCodeUsageUnknownValue},
		{[]string{"check", "--bogus"}, errCodeUsageUnknownFlag},
	}
	for _, c := range cases {
		withTempHome(t)
		_, _, err := runSplit(t, c.args...)
		var typed moraError
		if !errors.As(err, &typed) || typed.Code != c.code {
			t.Errorf("%v: err = %v, want code %s", c.args, err, c.code)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		value string
		want  time.Time
	}{
		{"30d", time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)},
		{"24h", time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)},
		{"2026-11-01", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseSince(c.value, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: got %v, %v", c.value, got, err)
		}
	}
	for _, bad := range []string{"", "d", "h", "5", "5w", "5.5d", "2026-13-01", "2026-11", "9999999999h", "0001-01-01"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestCheckUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not stop this user from reading")
	}
	withTempHome(t)
	copyCheckFixture(t, "claude")
	path := filepath.Join(mustConfig(t).HomeDir(), ".claude", "projects", "-fake-work", "sess-a.jsonl")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	stdout, stderr, err := runSplit(t, "check")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "  Unreadable files: 1\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if stderr != "" || strings.Contains(stdout, "sess-a") || strings.Contains(stdout, "fake-work") {
		t.Errorf("a path leaked: stderr = %q", stderr)
	}
}

// Only the home folder matters to check, and a config error can hold a path or
// a value. So none of these may stop the scan or reach stderr.
func TestCheckBrokenConfig(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"bad value": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`mcp_write_policy = "`+checkCanary+`"`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a folder": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(t *testing.T, path string) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("file modes do not stop this user from reading")
			}
			if err := os.WriteFile(path, nil, 0); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			withTempHome(t)
			copyCheckFixture(t, "claude")
			cfg := mustConfig(t)
			if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
				t.Fatal(err)
			}
			plant(t, filepath.Join(cfg.ConfigDir, "config.toml"))
			stdout, stderr, err := runSplit(t, "check")
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if stderr != "" || !strings.Contains(stdout, "  Files read: 6\n") {
				t.Errorf("stderr = %q, stdout:\n%s", stderr, stdout)
			}
			assertNoCanary(t, "stdout", stdout)
			assertNoCanary(t, "stderr", stderr)
			for _, s := range []string{cfg.ConfigDir, cfg.HomeDir(), "config.toml"} {
				if strings.Contains(stdout, s) {
					t.Errorf("stdout holds %q", s)
				}
			}
		})
	}
}

// Links are not followed, but that must not read as "nothing here".
func TestCheckSymlinkedRootIsNotEmptyHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	withTempHome(t)
	home := mustConfig(t).HomeDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, ".claude", "projects")); err != nil {
		t.Fatal(err)
	}
	out := run(t, "check")
	if strings.Contains(out, "No Claude Code or Codex transcripts found.") || !strings.Contains(out, "  Symlinks skipped: 1\n") {
		t.Errorf("output:\n%s", out)
	}
}

// snapshotTree records path, mode, size, mtime and content for everything below root.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		modTime := info.ModTime().UnixNano()
		if d.IsDir() && runtime.GOOS == "windows" {
			modTime = 0 // NTFS updates a directory's time lazily, after its children appear
		}
		entry := fmt.Sprintf("%v %d %d", info.Mode(), info.Size(), modTime)
		if info.Mode().IsRegular() && info.Mode().Perm()&0o400 != 0 {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += fmt.Sprintf(" %x", sha256.Sum256(body))
		}
		tree[path] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestCheckWritesNothing(t *testing.T) {
	withTempHome(t)
	copyCheckFixture(t, "claude", "codex")
	cfg := mustConfig(t)
	checkLeavesTree := func(roots ...string) {
		t.Helper()
		before := map[string]map[string]string{}
		for _, root := range roots {
			before[root] = snapshotTree(t, root)
		}
		run(t, "check")
		run(t, "check", "--json")
		run(t, "check", "--since", "30d")
		for _, root := range roots {
			if after := snapshotTree(t, root); !reflect.DeepEqual(before[root], after) {
				t.Errorf("%s changed:\nbefore %v\nafter  %v", root, before[root], after)
			}
		}
	}
	// Before init the state directory does not exist, and check must not make it.
	checkLeavesTree(cfg.HomeDir())
	if _, err := os.Stat(cfg.StateDir); !os.IsNotExist(err) {
		t.Fatalf("state dir: %v", err)
	}
	run(t, "init")
	if len(snapshotTree(t, cfg.StateDir)) == 0 {
		t.Fatal("init should have made the state dir")
	}
	checkLeavesTree(cfg.HomeDir(), cfg.StateDir)
}
