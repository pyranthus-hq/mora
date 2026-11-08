package sessions

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func customExec(callID, source string) string {
	quoted, _ := json.Marshal(source)
	return codexResponse(`{"type":"custom_tool_call","name":"exec","call_id":"` + callID + `","input":` + string(quoted) + `}`)
}

func TestCodexDrift(t *testing.T) {
	ok := Stats{FilesRead: 1, CodexSessions: 1}
	with := func(edit func(*Stats)) Stats {
		st := ok
		edit(&st)
		return st
	}
	cases := []struct {
		name string
		body string
		want Stats
	}{
		{"unknown type", `{"type":"brand-new","payload":{}}` + "\n", with(func(s *Stats) { s.Lines, s.UnknownRecords = 1, 1 })},
		{"known types", `{"type":"turn_context"}` + "\n" + `{"type":"world_state"}` + "\n" + `{"type":"compacted"}` + "\n" + `{"type":"token_usage_record"}` + "\n", with(func(s *Stats) { s.Lines = 4 })},
		{"payload as a string", `{"type":"response_item","payload":"x"}` + "\n", with(func(s *Stats) { s.Lines, s.MalformedLines = 1, 1 })},
		{"no payload", `{"type":"event_msg"}` + "\n", with(func(s *Stats) { s.Lines, s.MalformedLines = 1, 1 })},
		{"not json", "not json\n", Stats{FilesRead: 1, Lines: 1, MalformedLines: 1}},
		{"event with an object status", `{"type":"event_msg","payload":{"type":"other","status":{"done":true}}}` + "\n", with(func(s *Stats) { s.Lines = 1 })},
		{"turn aborted", `{"timestamp":"` + ts1 + `","type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}` + "\n", with(func(s *Stats) { s.Lines, s.CodexTurnInterrupts = 1, 1 })},
		{"patch declined", `{"timestamp":"` + ts1 + `","type":"event_msg","payload":{"type":"patch_apply_end","status":"declined"}}` + "\n", with(func(s *Stats) { s.Lines, s.CodexPatchesDeclined = 1, 1 })},
		{"patch applied", `{"type":"event_msg","payload":{"type":"patch_apply_end","status":"completed"}}` + "\n", with(func(s *Stats) { s.Lines = 1 })},
		{"arguments not json", codexResponse(`{"type":"function_call","name":"shell","call_id":"c","arguments":"{oops"}`), with(func(s *Stats) { s.Lines, s.CodexCommandsUnreadable = 1, 1 })},
		{"event with no timestamp", `{"type":"event_msg","payload":{"type":"turn_aborted"}}` + "\n", with(func(s *Stats) { s.Lines, s.CodexTurnInterrupts, s.BadTimestamps = 1, 1, 1 })},
		{"event with a number id", `{"timestamp":"` + ts1 + `","type":"event_msg","payload":{"type":"turn_aborted","turn_id":7}}` + "\n", with(func(s *Stats) { s.Lines, s.CodexTurnInterrupts = 1, 1 })},
		{"event with a null id", `{"timestamp":"` + ts1 + `","type":"event_msg","payload":{"type":"turn_aborted","turn_id":null}}` + "\n", with(func(s *Stats) { s.Lines, s.CodexTurnInterrupts = 1, 1 })},
		{"bad timestamp", strings.ReplaceAll(customExec("c", `tools.exec_command({cmd: "ls"})`), "2026-10-03T12:00:05.000Z", "soon"), with(func(s *Stats) { s.Lines, s.BadTimestamps = 1, 1 })},
		{"partial last line", codexMetaLine + `{"type":"resp`, with(func(s *Stats) { s.Lines, s.PartialLastLines = 1, 1 })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, codexFile(home, "rollout-x.jsonl"), tc.body)
			rec, st := scanHome(t, home)
			if st != tc.want {
				t.Fatalf("stats\n got %+v\nwant %+v", st, tc.want)
			}
			if len(rec.calls) != 0 {
				t.Fatalf("calls %+v", rec.calls)
			}
		})
	}
}

func FuzzCodexLine(f *testing.F) {
	f.Add([]byte(codexMetaLine))
	f.Add([]byte(customExec("c1", `await tools.exec_command({cmd: "ls"}); tools.exec_command({cmd: 'a\'b`)))
	f.Add([]byte(codexResponse(`{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"ls\"]}"}`)))
	f.Add([]byte(codexResponse(`{"type":"local_shell_call","action":{"type":"exec","command":["ls"]}}`)))
	f.Add([]byte(`{"type":"event_msg","payload":{"type":"patch_apply_end","status":"declined"}}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		r := newCodexReader(transcript{path: "p", session: "s", harness: HarnessCodex}, &Stats{}, &recorder{})
		r.line(line)
		r.finish()
	})
}

func TestCustomExecLiteralsNoEval(t *testing.T) {
	source := `await tools.exec_command({cmd: "echo first"}); await tools.exec_command({cmd: 'echo second'}); await tools.exec_command({cmd: dynamic});`
	home := t.TempDir()
	writeFile(t, codexFile(home, "rollout-x.jsonl"), codexMetaLine+customExec("wrapper", source))
	rec, st := scanHome(t, home)
	if !reflect.DeepEqual(rec.commands(), []string{"echo first", "echo second"}) {
		t.Fatalf("commands %q", rec.commands())
	}
	if rec.calls[0].ToolUseID != "wrapper:0" || rec.calls[1].ToolUseID != "wrapper:1" || st.CodexCommandsUnreadable != 1 {
		t.Fatalf("calls=%+v stats=%+v", rec.calls, st)
	}
	for _, source := range []string{
		"tools.exec_command({cmd: `echo ${secret}`})",
		`tools.exec_command({cmd: "echo " + value})`,
		`tools.exec_command({cmd: "unterminated`,
		`tools.exec_command({cmd: `,
	} {
		if got, unreadable := literalExecCommands(source); len(got) != 0 || unreadable != 1 {
			t.Errorf("%s: got %q, unreadable %d", source, got, unreadable)
		}
	}
	cases := map[string]string{
		`tools.exec_command({workdir: "/fake", cmd: "echo fake"})`: "echo fake",
		`exec_command({"cmd": "echo fake"})`:                       "echo fake",
		`exec_command({'cmd': 'it\'s'})`:                           "it's",
		"exec_command({cmd: `echo \\`x\\``})":                      "echo `x`",
		`exec_command({cmd: "a\nb"})`:                              "a\nb",
	}
	for source, want := range cases {
		if got, unreadable := literalExecCommands(source); len(got) != 1 || got[0] != want || unreadable != 0 {
			t.Errorf("%s: got %q, unreadable %d", source, got, unreadable)
		}
	}
}

func TestSingleCustomExecKeepsCallID(t *testing.T) {
	home := t.TempDir()
	writeFile(t, codexFile(home, "rollout-x.jsonl"), codexMetaLine+customExec("call_1", `tools.exec_command({cmd: "ls"});`))
	rec, st := scanHome(t, home)
	if len(rec.calls) != 1 || rec.calls[0].ToolUseID != "call_1" || rec.calls[0].SessionID != "thread-1" || rec.calls[0].Harness != HarnessCodex || st.CodexSessions != 1 {
		t.Fatalf("calls=%+v stats=%+v", rec.calls, st)
	}
}

func TestShellCommandFormsAndLegacyLocalShell(t *testing.T) {
	cases := []struct{ payload, want string }{
		{`{"type":"function_call","name":"functions.exec_command","call_id":"c","arguments":"{\"cmd\":\"echo fake\"}"}`, "echo fake"},
		{`{"type":"function_call","name":"shell_command","call_id":"c","arguments":"{\"command\":\"echo fake\"}"}`, "echo fake"},
		{`{"type":"function_call","name":"exec","call_id":"c","arguments":{"cmd":"echo fake"}}`, "echo fake"},
		{`{"type":"function_call","name":"shell","call_id":"c","arguments":"{\"command\":[\"printf\",\"%s\",\"fake space\"]}"}`, "printf %s 'fake space'"},
		{`{"type":"function_call","name":"shell","call_id":"c","arguments":"{\"command\":[\"/bin/zsh\",\"-c\",\"echo fake\"]}"}`, "echo fake"},
		{`{"type":"function_call","name":"shell","call_id":"c","arguments":"{\"command\":[\"bash\",\"-x\",\"echo\"]}"}`, "bash -x echo"},
		{`{"type":"local_shell_call","id":"c","action":{"type":"exec","command":["/bin/bash","-lc","echo fake"]}}`, "echo fake"},
		{`{"type":"local_shell_call","call_id":"c","action":{"type":"exec","command":["echo","it's"]}}`, `echo 'it'"'"'s'`},
	}
	for _, tc := range cases {
		home := t.TempDir()
		writeFile(t, codexFile(home, "rollout-x.jsonl"), codexMetaLine+codexResponse(tc.payload))
		rec, _ := scanHome(t, home)
		if len(rec.calls) != 1 || rec.calls[0].Command != tc.want || rec.calls[0].ToolUseID != "c" {
			t.Errorf("%s: calls %+v", tc.payload, rec.calls)
		}
	}
	for _, payload := range []string{
		`{"type":"function_call","name":"apply_patch","call_id":"c","arguments":"{\"cmd\":\"echo fake\"}"}`,
		`{"type":"local_shell_call","call_id":"c","action":{"type":"other","command":["ls"]}}`,
		`{"type":"custom_tool_call","name":"apply_patch","call_id":"c","input":"tools.exec_command({cmd: \"ls\"})"}`,
	} {
		home := t.TempDir()
		writeFile(t, codexFile(home, "rollout-x.jsonl"), codexMetaLine+codexResponse(payload))
		if rec, _ := scanHome(t, home); len(rec.calls) != 0 {
			t.Errorf("%s: calls %+v", payload, rec.calls)
		}
	}
}

func TestCodexSessionIDOrder(t *testing.T) {
	line := func(payload string) string { return `{"type":"session_meta","payload":` + payload + "}\n" }
	cases := map[string]struct{ body, want string }{
		"id":          {line(`{"id":"a","thread_id":"b","session_id":"c"}`), "a"},
		"thread_id":   {line(`{"thread_id":"b","session_id":"c"}`), "b"},
		"session_id":  {line(`{"session_id":"c"}`), "c"},
		"first meta":  {line(`{"id":"a"}`) + line(`{"id":"parent"}`), "a"},
		"no id":       {line(`{}`), "rollout-stem"},
		"no metadata": {"", "rollout-stem"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, codexFile(home, "rollout-stem.jsonl"), tc.body+customExec("c", `tools.exec_command({cmd: "ls"})`))
			rec, _ := scanHome(t, home)
			if len(rec.calls) != 1 || rec.calls[0].SessionID != tc.want {
				t.Fatalf("calls %+v", rec.calls)
			}
		})
	}
}

func TestCodexFileFirst(t *testing.T) {
	home := t.TempDir()
	meta := `{"timestamp":"2026-10-03T12:00:00.000Z","type":"session_meta","payload":{"id":"t","timestamp":"2026-10-01T08:00:00.000Z"}}` + "\n"
	writeFile(t, codexFile(home, "rollout-a.jsonl"), meta+customExec("c", `tools.exec_command({cmd: "ls"})`))
	writeFile(t, codexFile(home, "rollout-b.jsonl"), customExec("d", `tools.exec_command({cmd: "ls"})`))
	rec, _ := scanHome(t, home)
	first := map[string]string{}
	for _, c := range rec.calls {
		first[c.ToolUseID] = c.FileFirst.Format("2006-01-02")
	}
	if first["c"] != "2026-10-01" || first["d"] != "2026-10-03" {
		t.Fatalf("first %v", first)
	}
}

func TestCodexCallWithoutID(t *testing.T) {
	home := t.TempDir()
	writeFile(t, codexFile(home, "rollout-x.jsonl"), codexMetaLine+codexResponse(`{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}"}`))
	rec, st := scanHome(t, home)
	if len(rec.calls) != 1 || st.CallsWithoutID != 1 || !strings.HasSuffix(rec.calls[0].ToolUseID, ":2:0") {
		t.Fatalf("calls=%+v stats=%+v", rec.calls, st)
	}
}

func TestCodexRejectedTextHasNoEffect(t *testing.T) {
	body := func(text string) string {
		quoted, _ := json.Marshal(text)
		return codexMetaLine + customExec("c1", `tools.exec_command({cmd: "npm publish"});`) +
			codexResponse(`{"type":"custom_tool_call_output","call_id":"c1","output":`+string(quoted)+`}`) +
			codexResponse(`{"type":"function_call_output","call_id":"c1","output":`+string(quoted)+`}`) +
			`{"type":"event_msg","payload":{"type":"exec_command_end","aggregated_output":` + string(quoted) + `}}` + "\n"
	}
	scan := func(text string) (*recorder, Stats) {
		home := t.TempDir()
		writeFile(t, codexFile(home, "rollout-x.jsonl"), body(text))
		return scanHome(t, home)
	}
	baseRec, baseStats := scan("exit code 0")
	for _, text := range []string{"rejected by user", "exec command rejected by user", "rejected by user approval settings"} {
		rec, st := scan(text)
		if st != baseStats || len(rec.calls) != len(baseRec.calls) || len(rec.denials) != 0 {
			t.Errorf("%s changed a count: %+v", text, st)
		}
	}
}

func TestCompressedRollouts(t *testing.T) {
	pack := func(body string) string {
		var b bytes.Buffer
		w, err := zstd.NewWriter(&b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	body := codexMetaLine + codexResponse(`{"type":"function_call","name":"shell_command","call_id":"c1","arguments":"{\"command\":\"go test ./...\"}"}`)
	home := t.TempDir()
	writeFile(t, codexFile(home, "rollout-x.jsonl.zst"), pack(body))
	writeFile(t, codexFile(home, "rollout-cut.jsonl.zst"), pack(body)[:9])
	writeFile(t, codexFile(home, "notes.zst"), pack(body))
	rec, st := scanHome(t, home)
	if got := rec.commands(); !reflect.DeepEqual(got, []string{"go test ./..."}) {
		t.Fatalf("commands %v", got)
	}
	if st.FilesRead != 1 || st.UnreadableFiles != 1 || st.CodexSessions != 1 {
		t.Fatalf("stats %+v", st)
	}
}
