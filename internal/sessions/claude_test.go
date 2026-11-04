package sessions

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDriftNeverCrashes(t *testing.T) {
	use := bash("t1", "git status")
	pad := strings.Repeat("x", 10<<20)
	over := `{"type":"user","pad":"` + strings.Repeat("x", maxLine) + `"}` + "\n"
	ok := Stats{FilesRead: 1, ClaudeSessions: 1}
	with := func(edit func(*Stats)) Stats {
		st := ok
		edit(&st)
		return st
	}
	cases := []struct {
		name    string
		body    string
		want    Stats
		calls   int
		denials int
	}{
		{"unknown type", `{"type":"brand-new"}` + "\n", with(func(s *Stats) { s.Lines, s.UnknownRecords = 1, 1 }), 0, 0},
		{"no type", `{"timestamp":"` + ts1 + `"}` + "\n", with(func(s *Stats) { s.Lines, s.UnknownRecords = 1, 1 }), 0, 0},
		{"unknown denial kind", use + toolResult("t1", `,"toolDenialKind":"brand-new"`), with(func(s *Stats) { s.Lines = 2 }), 1, 1},
		{"message as a string", `{"type":"user","message":"invented"}` + "\n" + `{"type":"assistant","message":"tool_use"}` + "\n",
			with(func(s *Stats) { s.Lines, s.MalformedLines = 2, 2 }), 0, 0},
		{"content as an object", `{"type":"assistant","message":{"id":"m","content":{"type":"tool_use"}}}` + "\n", with(func(s *Stats) { s.Lines = 1 }), 0, 0},
		{"content block as a number", `{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use"},5]}}` + "\n",
			with(func(s *Stats) { s.Lines, s.MalformedLines = 1, 1 }), 0, 0},
		{"tool input as a string", `{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use","id":"t","name":"Bash","input":"x"}]}}` + "\n",
			with(func(s *Stats) { s.Lines = 1 }), 0, 0},
		{"toolUseResult as a string", use + toolResult("t1", `,"toolUseResult":"\"interrupted\":true"`), with(func(s *Stats) { s.Lines = 2 }), 1, 0},
		{"toolUseResult as an array", use + toolResult("t1", `,"toolUseResult":[{"interrupted":true}]`), with(func(s *Stats) { s.Lines = 2 }), 1, 0},
		{"toolUseResult as null", use + toolResult("t1", `,"toolUseResult":null`), with(func(s *Stats) { s.Lines = 2 }), 1, 0},
		{"timestamp as a number", `{"type":"user","timestamp":5}` + "\n", Stats{FilesRead: 1, Lines: 1, MalformedLines: 1}, 0, 0},
		{"json array line", "[1,2]\n", Stats{FilesRead: 1, Lines: 1, MalformedLines: 1}, 0, 0},
		{"bare number line", "5\n", Stats{FilesRead: 1, Lines: 1, MalformedLines: 1}, 0, 0},
		{"line with a BOM", "\xef\xbb\xbf" + use, Stats{FilesRead: 1, Lines: 1, MalformedLines: 1}, 0, 0},
		{"crlf endings", strings.ReplaceAll(use, "\n", "\r\n"), with(func(s *Stats) { s.Lines = 1 }), 1, 0},
		{"invalid utf-8 in a string", strings.Replace(use, "git status", "git status \xff", 1), with(func(s *Stats) { s.Lines = 1 }), 1, 0},
		{"invalid utf-8 outside a string", "{\xff}\n", Stats{FilesRead: 1, Lines: 1, MalformedLines: 1}, 0, 0},
		{"10 MiB valid line", use + toolResult("t1", `,"toolDenialKind":"user-rejected","pad":"`+pad+`"`), with(func(s *Stats) { s.Lines = 2 }), 1, 1},
		{"line over maxLine", over + use, with(func(s *Stats) { s.Lines, s.OversizeLines = 1, 1 }), 1, 0},
		{"line over maxLine with no newline", use + strings.TrimSuffix(over, "\n"), with(func(s *Stats) { s.Lines, s.OversizeLines = 1, 1 }), 1, 0},
		{"truncated last line", use + `{"type":"assistant","mess`, with(func(s *Stats) { s.Lines, s.PartialLastLines = 1, 1 }), 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, claudeFile(home, "s1.jsonl"), tc.body)
			rec, st := scanHome(t, home)
			if st != tc.want {
				t.Fatalf("stats\n got %+v\nwant %+v", st, tc.want)
			}
			if len(rec.calls) != tc.calls || len(rec.denials) != tc.denials {
				t.Fatalf("got %d calls and %d denials", len(rec.calls), len(rec.denials))
			}
		})
	}
}

func FuzzClaudeLine(f *testing.F) {
	f.Add([]byte(bash("t1", "git status")))
	f.Add([]byte(toolResult("t1", `,"toolDenialKind":"interrupted","toolUseResult":{"interrupted":true}`)))
	f.Add([]byte(prompt(`,"turnOrigin":"peer","origin":{"kind":"peer"}`)))
	f.Add([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`))
	f.Add([]byte(`{"type":"api-request-blob"`))
	f.Fuzz(func(t *testing.T, line []byte) {
		r := newClaudeReader(transcript{path: "p", session: "s"}, &Stats{}, &recorder{})
		r.line([]byte(bash("t1", "git status")))
		r.line(line)
		r.finish()
	})
}

func TestLongLinesParse(t *testing.T) {
	home := t.TempDir()
	blob := `{"type":"api-request-blob","pad":"` + strings.Repeat("x", 2<<20) + `"}` + "\n"
	long := toolResult("t1", `,"toolDenialKind":"user-rejected","pad":"`+strings.Repeat("x", 3<<20)+`"`)
	writeFile(t, claudeFile(home, "s1.jsonl"), bash("t1", "git status")+blob+long+bash("t2", "ls"))
	rec, st := scanHome(t, home)
	if len(rec.calls) != 2 || len(rec.denials) != 1 || st.Lines != 4 || st.OversizeLines != 0 || st.MalformedLines != 0 {
		t.Fatalf("calls=%d denials=%d stats=%+v", len(rec.calls), len(rec.denials), st)
	}
}

func TestBlobNotCounted(t *testing.T) {
	inner := `"message":{"id":"m","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git status"}}]}`
	for name, line := range map[string]string{
		"stable prefix":   `{"type":"api-request-blob",` + inner + `}`,
		"type not first":  `{"timestamp":"` + ts1 + `",` + inner + `,"type":"api-request-blob"}`,
		"prefix not json": `{"type":"api-request-blob",` + inner,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, claudeFile(home, "s1.jsonl"), line+"\n")
			rec, st := scanHome(t, home)
			if len(rec.calls) != 0 || st.UnknownRecords != 0 || st.MalformedLines != 0 {
				t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
			}
		})
	}
}

func TestSubagentFiles(t *testing.T) {
	home := t.TempDir()
	writeFile(t, claudeFile(home, "sess-1.jsonl"), bash("m1", "git status"))
	writeFile(t, claudeFile(home, "sess-1/subagents/agent-x.jsonl"),
		bash("x1", "ls")+toolResult("x1", `,"toolDenialKind":"user-rejected"`))
	writeFile(t, claudeFile(home, "sess-1/subagents/workflows/wf/agent-y.jsonl"), bash("y1", "pwd"))
	writeFile(t, claudeFile(home, "sess-1/subagents/workflows/wf/journal.jsonl"), bash("j1", "date"))
	writeFile(t, claudeFile(home, "sess-1/tool-results/agent-z.jsonl"), bash("z1", "date"))
	writeFile(t, claudeFile(home, "sess-1/agent-z.jsonl"), bash("z2", "date"))
	writeFile(t, claudeFile(home, "sess-1/subagents/agent-garbage.jsonl"), "not json\n")
	rec, st := scanHome(t, home)
	if st.FilesRead != 4 || st.ClaudeSessions != 2 || st.ClaudeSubagentFiles != 2 {
		t.Fatalf("stats %+v", st)
	}
	agents := map[string]string{}
	for _, c := range rec.calls {
		agents[c.ToolUseID] = c.AgentID
		if c.ToolUseID != "m1" && c.SessionID != "sess-1" {
			t.Errorf("%s belongs to %q", c.ToolUseID, c.SessionID)
		}
	}
	if len(rec.calls) != 3 || agents["x1"] != "x" || agents["y1"] != "y" || agents["m1"] != "" {
		t.Fatalf("calls %+v", rec.calls)
	}
	if len(rec.denials) != 1 || !rec.denials[0].Subagent || rec.denials[0].AgentID != "x" || rec.denials[0].SessionID != "sess-1" {
		t.Fatalf("denials %+v", rec.denials)
	}
}

func TestSessionIDFallsBackToFileStem(t *testing.T) {
	home := t.TempDir()
	writeFile(t, claudeFile(home, "stem-1.jsonl"), strings.ReplaceAll(bash("t1", "ls"), `,"sessionId":"s1"`, ""))
	rec, st := scanHome(t, home)
	if len(rec.calls) != 1 || rec.calls[0].SessionID != "stem-1" || st.ClaudeSessions != 1 {
		t.Fatalf("calls=%+v stats=%+v", rec.calls, st)
	}
}

func TestDenialFields(t *testing.T) {
	home := t.TempDir()
	writeFile(t, claudeFile(home, "s1.jsonl"), toolUse("t1", "Edit", "x")+toolResult("t1", `,"toolDenialKind":"user-rejected"`))
	rec, _ := scanHome(t, home)
	if len(rec.denials) != 1 {
		t.Fatalf("denials %+v", rec.denials)
	}
	d := rec.denials[0]
	if d.Kind != "user-rejected" || d.Tool != "Edit" || d.MessageID != "m-t1" || d.ToolUseID != "t1" || d.SessionID != "s1" ||
		d.TS.Format("2006-01-02") != "2026-10-02" || d.FileFirst.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("denial %+v", d)
	}
}

func TestInterruptSignals(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		kind     string
		origin   string
		shutdown bool
		none     bool
	}{
		{name: "typed kind", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`), kind: "interrupted"},
		{name: "bash flag", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolUseResult":{"stdout":"","interrupted":true}`), kind: "interrupted"},
		{name: "flag false", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolUseResult":{"interrupted":false}`), none: true},
		{name: "flag on another tool", body: toolUse("t1", "Edit", "x") + toolResult("t1", `,"toolUseResult":{"interrupted":true}`), none: true},
		{name: "kind wins over flag", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"cancelled","toolUseResult":{"interrupted":true}`), kind: "cancelled"},
		{name: "shutdown", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted","interruptedByShutdown":true`), kind: "interrupted", shutdown: true},
		{name: "next turn origin", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`) + prompt(`,"turnOrigin":"peer"`), kind: "interrupted", origin: "peer"},
		{name: "next origin kind", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`) + prompt(`,"origin":{"kind":"task_notification"}`), kind: "interrupted", origin: "task_notification"},
		{name: "turn origin wins", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`) + prompt(`,"turnOrigin":"human","origin":{"kind":"peer"}`), kind: "interrupted", origin: "human"},
		{name: "first origin wins", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`) + prompt(`,"turnOrigin":"human"`) + prompt(`,"turnOrigin":"peer"`), kind: "interrupted", origin: "human"},
		{name: "prompt with no origin is passed over", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`) + prompt("") + prompt(`,"turnOrigin":"peer"`), kind: "interrupted", origin: "peer"},
		{name: "origin not an object", body: bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`) + prompt(`,"origin":"peer"`), kind: "interrupted"},
		{name: "origin before the denial does not count", body: prompt(`,"turnOrigin":"peer"`) + bash("t1", "sleep 1") + toolResult("t1", `,"toolDenialKind":"interrupted"`), kind: "interrupted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, claudeFile(home, "s1.jsonl"), tc.body)
			rec, _ := scanHome(t, home)
			if tc.none {
				if len(rec.denials) != 0 {
					t.Fatalf("denials %+v", rec.denials)
				}
				return
			}
			if len(rec.denials) != 1 {
				t.Fatalf("denials %+v", rec.denials)
			}
			d := rec.denials[0]
			if d.Kind != tc.kind || d.NextOrigin != tc.origin || d.Shutdown != tc.shutdown {
				t.Fatalf("denial %+v", d)
			}
		})
	}
}

func TestUntypedRejections(t *testing.T) {
	rejected := func(id, isError string) string {
		return `{"type":"user","message":{"content":[{"type":"tool_result","content":"The user doesn't want to proceed with this tool use. STOP.","is_error":` +
			isError + `,"tool_use_id":"` + id + `"}]},"timestamp":"` + ts2 + `"}` + "\n"
	}
	arrayForm := `{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"The user doesn't want to proceed with this tool use."}],"is_error":true,"tool_use_id":"t1"}]},"timestamp":"` + ts2 + `"}` + "\n"
	cases := map[string]struct {
		body string
		want int
	}{
		"bash":          {bash("t1", "ls") + rejected("t1", "true"), 1},
		"array content": {bash("t1", "ls") + arrayForm, 1},
		"not an error":  {bash("t1", "ls") + rejected("t1", "false"), 0},
		"mcp tool":      {toolUse("t1", "mcp__fake__run", "ls") + rejected("t1", "true"), 0},
		"edit":          {toolUse("t1", "Edit", "ls") + rejected("t1", "true"), 0},
		"no tool_use":   {rejected("t1", "true"), 0},
		"other text":    {bash("t1", "ls") + toolResult("t1", ""), 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, claudeFile(home, "s1.jsonl"), tc.body)
			rec, st := scanHome(t, home)
			if st.UntypedRejections != tc.want || len(rec.denials) != tc.want {
				t.Fatalf("denials=%d stats=%+v", len(rec.denials), st)
			}
			for _, d := range rec.denials {
				if d.Kind != KindUntypedRejection || d.Tool != "Bash" || d.ToolUseID != "t1" {
					t.Fatalf("denial %+v", d)
				}
			}
		})
	}
}

func TestInterruptedBetweenCalls(t *testing.T) {
	marker := func(text string) string {
		return `{"type":"user","message":{"content":[{"type":"text","text":"` + text + `"}]},"timestamp":"` + ts1 + `"}` + "\n"
	}
	nested := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"[Request interrupted by user]"}]}]},"timestamp":"` + ts1 + `"}` + "\n"
	home := t.TempDir()
	writeFile(t, claudeFile(home, "s1.jsonl"), marker("[Request interrupted by user]")+
		marker("[Request interrupted by user for tool use]")+marker("See [Request interrupted by user]")+nested+
		`{"type":"user","message":{"content":[{"type":"note","text":"[Request interrupted by user]"}]}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`+"\n")
	_, st := scanHome(t, home)
	if st.InterruptedBetweenCalls != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestDenialWithoutCall(t *testing.T) {
	home := t.TempDir()
	writeFile(t, claudeFile(home, "s1.jsonl"), toolResult("gone", `,"toolDenialKind":"user-rejected"`)+
		toolResult("gone2", `,"toolUseResult":{"interrupted":true}`)+toolResult("gone3", ""))
	rec, st := scanHome(t, home)
	if len(rec.denials) != 0 || st.DenialsWithoutCall != 2 {
		t.Fatalf("denials=%d stats=%+v", len(rec.denials), st)
	}
}

func TestBadTimestampDropsRecord(t *testing.T) {
	home := t.TempDir()
	body := strings.ReplaceAll(bash("t1", "ls"), ts1, "yesterday") + toolResult("t1", `,"toolDenialKind":"user-rejected"`) +
		bash("t2", "pwd") + strings.ReplaceAll(toolResult("t2", `,"toolDenialKind":"user-rejected"`), ts2, "soon") + `{"type":"system","timestamp":"never"}` + "\n"
	writeFile(t, claudeFile(home, "s1.jsonl"), body)
	rec, st := scanHome(t, home)
	if st.BadTimestamps != 2 || len(rec.calls) != 1 || rec.calls[0].ToolUseID != "t2" {
		t.Fatalf("calls=%+v stats=%+v", rec.calls, st)
	}
	// The call with the bad time still joins, so its denial survives.
	if len(rec.denials) != 1 || rec.denials[0].ToolUseID != "t1" {
		t.Fatalf("denials %+v", rec.denials)
	}
}

func TestCallWithoutID(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"assistant","timestamp":"` + ts1 + `","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}},{"type":"tool_use","name":"Bash","input":{"command":"pwd"}}]}}` + "\n"
	writeFile(t, claudeFile(home, "s1.jsonl"), line)
	rec, st := scanHome(t, home)
	if st.CallsWithoutID != 2 || len(rec.calls) != 2 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
	if a, b := rec.calls[0].ToolUseID, rec.calls[1].ToolUseID; a == b || !strings.HasSuffix(a, ":1:0") || !strings.HasSuffix(b, ":1:1") {
		t.Fatalf("keys %q %q", a, b)
	}
}

func TestOnlyExactBashIsACall(t *testing.T) {
	home := t.TempDir()
	writeFile(t, claudeFile(home, "s1.jsonl"), toolUse("t1", "bash", "ls")+toolUse("t2", "Edit", "ls")+bash("t3", "")+bash("t4", "ls"))
	rec, _ := scanHome(t, home)
	if len(rec.calls) != 1 || rec.calls[0].ToolUseID != "t4" {
		t.Fatalf("calls %+v", rec.calls)
	}
}

// TestRealRejectionForms reads records from a live Claude Code 2.1.318 session,
// with ids and paths replaced. Cases 2 to 5 are No, No with a typed reason, Esc
// while the command ran, and No for a subagent.
func TestRealRejectionForms(t *testing.T) {
	forms, err := os.ReadFile(filepath.Join("testdata", "real-forms.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var calls strings.Builder
	for _, id := range []string{"toolu_FAKE0002", "toolu_FAKE0012", "toolu_FAKE0019", "toolu_FAKE0023", "toolu_FAKE0032"} {
		calls.WriteString(bash(id, "echo invented"))
	}
	home := t.TempDir()
	writeFile(t, claudeFile(home, "s1.jsonl"), calls.String()+string(forms))
	rec, st := scanHome(t, home)
	got := map[string]string{}
	for _, d := range rec.denials {
		got[d.ToolUseID] = d.Kind
	}
	want := map[string]string{"toolu_FAKE0012": "user-rejected", "toolu_FAKE0019": "user-rejected",
		"toolu_FAKE0023": "interrupted", "toolu_FAKE0032": "user-rejected"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("denials %v, want %v", got, want)
	}
	if st.InterruptedBetweenCalls != 0 || st.UnknownRecords != 0 || st.MalformedLines != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestToolMarkerSettlesOnlyItsPrompt(t *testing.T) {
	rejected := func(id, promptID string) string {
		return toolResult(id, `,"toolDenialKind":"user-rejected","promptId":"`+promptID+`"`)
	}
	marker := func(promptID, extra string) string {
		return `{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"timestamp":"` + ts2 + `","sessionId":"s1","promptId":"` + promptID + `"` + extra + `}` + "\n"
	}
	home := t.TempDir()
	writeFile(t, claudeFile(home, "s1.jsonl"), bash("a", "echo a")+bash("b", "echo b")+bash("c", "echo c")+
		rejected("a", "p1")+rejected("b", "p2")+marker("p2", `,"interruptedMessageId":"m"`)+
		marker("p3", `,"interruptedMessageId":"m"`)+rejected("c", "p3"))
	rec, st := scanHome(t, home)
	got := map[string]string{}
	for _, d := range rec.denials {
		if d.ToolUseID != "" {
			got[d.ToolUseID] = d.Kind
		}
	}
	if want := map[string]string{"a": "user-rejected", "b": "interrupted", "c": "user-rejected"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("denials %v, want %v", got, want)
	}
	if st.InterruptedBetweenCalls != 1 {
		t.Fatalf("a marker with no rejection before it still counts, stats %+v", st)
	}
}
