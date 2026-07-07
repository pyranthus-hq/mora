package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// withTempHome points all XDG-derived dirs at a fresh temp dir (via setTestHome).
func withTempHome(t *testing.T) {
	t.Helper()
	setTestHome(t, t.TempDir())
}

// setTestHome isolates a test at a caller-chosen home dir. It sets HOME (Unix)
// AND USERPROFILE — on Windows os.UserHomeDir() reads %USERPROFILE% and ignores
// $HOME, so setting only HOME leaves the suite resolving the developer's REAL
// vault and scribbling into live data. Clearing MORA_CONFIG_DIR keeps an
// exported dev config from leaking in. Use this (never a bare
// t.Setenv("HOME", …)) anywhere a test plants a home path it then references
// (issue #56).
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("MORA_CONFIG_DIR", "")
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Run(context.Background(), args, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run(%v) error: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestSmokeInitWriteSearch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:wink", "--type", "decision",
		"--title", "OAuth auth path", "--text", "Use OAuth 2.0 for Wink API auth")

	out := run(t, "search", "OAuth", "--scope", "project:wink", "--json")
	var got []Memory
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("search json: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Title != "OAuth auth path" {
		t.Fatalf("expected 1 hit titled 'OAuth auth path', got %+v", got)
	}
	if got[0].Scope != "project:wink" || got[0].Type != "decision" {
		t.Fatalf("expected scoped decision memory, got %+v", got[0])
	}
	if !strings.Contains(got[0].Text, "OAuth 2.0") {
		t.Fatalf("expected search result text to include written body, got %+v", got[0])
	}
}

func TestSmokeMCPSearchRoundtrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")

	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"alpha"}}}`)
	if isErr {
		t.Fatalf("search_memory unexpectedly isError; text=%s", text)
	}
	var payload struct {
		Results []Memory `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode CallToolResult text: %v\n%s", err, text)
	}
	hits := payload.Results
	if len(hits) != 1 || hits[0].Title != "Alpha" {
		t.Fatalf("expected MCP search to return Alpha, got: %s", text)
	}
	if hits[0].Scope != "global" || hits[0].Text != "alpha body" {
		t.Fatalf("expected MCP search to return written memory, got %+v", hits[0])
	}
}

func TestDoctorReportsInjectedWindowsPlatform(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	origGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = origGOOS })
	runtimeGOOS = func() string { return "windows" }

	var js bytes.Buffer
	if err := cmdDoctor(context.Background(), []string{"--json"}, &js); err != nil {
		t.Fatal(err)
	}
	var rep doctorReport
	if err := json.Unmarshal(js.Bytes(), &rep); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, js.String())
	}
	if rep.Platform != "windows" {
		t.Fatalf("doctor platform = %q, want windows", rep.Platform)
	}

	var text bytes.Buffer
	if err := cmdDoctor(context.Background(), nil, &text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "skipping chat.db checks on windows") {
		t.Fatalf("doctor should report iMessage as macOS-only on windows; got:\n%s", text.String())
	}
}
