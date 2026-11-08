package rules

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	path := LogPath(t.TempDir())
	if err := Append(path, NewRow(time.Now(), HarnessClaude, Action{SessionID: "s1"})); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", fi.Mode(), err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", di.Mode(), err)
	}
	// An existing loose file and folder are tightened.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, NewRow(time.Now(), HarnessClaude, Action{SessionID: "s1"})); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	di, _ = os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes not tightened: %v %v", fi.Mode(), di.Mode())
	}
}

func TestAppendConcurrent(t *testing.T) {
	path := LogPath(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			row := NewRow(time.Now(), HarnessClaude, Action{SessionID: fmt.Sprintf("s%d", i), ToolUseID: "t"})
			row.Matched = []string{"compound-cd"}
			if err := Append(path, row); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		var row Row
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("line %d is not JSON: %q", n, sc.Text())
		}
		n++
	}
	if n != 100 {
		t.Fatalf("got %d lines, want 100", n)
	}
}

func TestAppendNeverLogsCommandText(t *testing.T) {
	const canary = "CANARY_7f3a"
	raw := `{"session_id":"s1","transcript_path":"/home/u/.claude/projects/x/s1.jsonl","cwd":"/tmp/` + canary + `","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_1","tool_input":{"command":"cd /tmp/` + canary + ` && ls","description":"` + canary + `"}}`
	a, harness, ok := ParsePreTool([]byte(raw))
	if !ok {
		t.Fatal("ParsePreTool rejected the canary input")
	}
	m := mustMatcher(t, "compound-cd", compoundCD(t).Spec)
	row := NewRow(time.Now(), harness, a)
	row.Matched = Match([]*Matcher{m}, a)
	row.Warned = row.Matched
	path := LogPath(t.TempDir())
	if err := Append(path, row); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	if strings.Contains(line, canary) {
		t.Fatalf("log contains command text: %s", line)
	}
	for _, key := range []string{`"command"`, `"cwd"`, `"prompt"`, `"transcript_path"`} {
		if strings.Contains(line, key) {
			t.Fatalf("log has %s: %s", key, line)
		}
	}
	if !strings.Contains(line, `"matched":["compound-cd"]`) {
		t.Fatalf("match not logged: %s", line)
	}
}

func TestRowShape(t *testing.T) {
	now := time.Date(2026, 11, 5, 14, 2, 11, 123456789, time.FixedZone("x", -8*3600))
	row := NewRow(now, HarnessClaude, Action{SessionID: "s1", ToolUseID: "toolu_1"})
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}Z$`).MatchString(row.TS) || row.TS != "2026-11-05T22:02:11.123456Z" {
		t.Fatalf("ts = %q", row.TS)
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"ts":"2026-11-05T22:02:11.123456Z","harness":"claude","session_id":"s1","agent_id":null,"tool_use_id":"toolu_1","group":"warned","matched":[],"warned":[],"holdout":[],"rules":[],"rule_version":"","mora":"","note":""}`
	if string(body) != want {
		t.Fatalf("row:\n got %s\nwant %s", body, want)
	}
	sub := NewRow(now, HarnessCodex, Action{SessionID: "abc", AgentID: "agent-1"})
	if sub.AgentID == nil || *sub.AgentID != "agent-1" || sub.ToolUseID != nil || sub.Group != GroupSilent {
		t.Fatalf("subagent row: %+v", sub)
	}
	// Append fills nil slices so score_c.py always sees lists.
	path := LogPath(t.TempDir())
	if err := Append(path, Row{V: 1}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"matched":[],"warned":[],"holdout":[],"rules":[]`) {
		t.Fatalf("nil slices logged as null: %s", data)
	}
}
