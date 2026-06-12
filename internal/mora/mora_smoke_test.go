package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// withTempHome points all XDG-derived dirs at a temp dir by setting HOME.
func withTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// Hermeticity: a developer's exported MORA_CONFIG_DIR must not leak a real
	// config into tests that assume the temp HOME's default location.
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
