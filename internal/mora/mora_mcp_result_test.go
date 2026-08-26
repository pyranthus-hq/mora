package mora

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mcpResult runs `mcp serve` against a single JSON-RPC line and returns the
// decoded `result` object, failing the test on a transport error or a JSON-RPC
// error envelope. Shared by every MCP roundtrip test.
func mcpResult(t *testing.T, line string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"mcp", "serve"}, &out, &out, strings.NewReader(line+"\n")); err != nil {
		t.Fatalf("mcp serve: %v\noutput:\n%s", err, out.String())
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		Error   json.RawMessage `json:"error"`
		Result  map[string]any  `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("mcp response json: %v\n%s", err, out.String())
	}
	if len(resp.Error) != 0 {
		t.Fatalf("unexpected JSON-RPC error: %s", string(resp.Error))
	}
	return resp.Result
}

// mcpToolText extracts the first text content block and the isError flag from a
// CallToolResult. It asserts the response is a spec-compliant CallToolResult —
// the exact shape strict clients (Codex) require and the bare-result bug broke.
func mcpToolText(t *testing.T, line string) (text string, isError bool) {
	t.Helper()
	res := mcpResult(t, line)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("CallToolResult has no content blocks (not a valid tools/call result): %v", res)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Fatalf("expected first content block type=text, got: %v", res)
	}
	text, _ = first["text"].(string)
	isError, _ = res["isError"].(bool)
	return text, isError
}

// TestMCPToolsCallReturnsCallToolResult locks the MCP contract: every tools/call
// must return a CallToolResult ({content:[{type:"text",text:...}], isError,
// structuredContent}) — not the bare tool value. Strict clients (Codex desktop)
// reject the bare shape with "unexpected response type"; this is the regression
// guard for that bug.
func TestMCPToolsCallReturnsCallToolResult(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")

	// SUCCESS (object-returning tool via the C4 envelope break): the payload
	// must ride inside the text content block, parseable as the tool's native
	// JSON. list_memory moved to {memories,health} alongside search_memory's
	// pre-existing {results,freshness}.
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_memory","arguments":{}}}`)
	if isErr {
		t.Fatalf("list_memory unexpectedly isError; text=%s", text)
	}
	var wrapped struct {
		Memories []Memory      `json:"memories"`
		Health   compactHealth `json:"health"`
	}
	if err := json.Unmarshal([]byte(text), &wrapped); err != nil {
		t.Fatalf("content[0].text is not the tool's JSON payload: %v\n%s", err, text)
	}
	hits := wrapped.Memories
	if len(hits) != 1 || hits[0].Title != "Alpha" {
		t.Fatalf("expected Alpha via the text content block, got: %s", text)
	}

	// SUCCESS (object-returning tool): also carries machine-readable structuredContent.
	res := mcpResult(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_entity","arguments":{"name":"Alpha"}}}`)
	if _, ok := res["structuredContent"]; !ok {
		t.Fatalf("object-returning tool result is missing structuredContent: %v", res)
	}

	// ERROR path: a tool error surfaces as isError content, NOT a JSON-RPC error,
	// so the agent's tool loop stays alive and can react to the message.
	etext, eIsErr := mcpToolText(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_memory","arguments":{"id":"does-not-exist"}}}`)
	if !eIsErr {
		t.Fatalf("expected isError:true for a missing id, got text=%s", etext)
	}
	if strings.TrimSpace(etext) == "" {
		t.Fatalf("expected a non-empty error message in the content block")
	}
}

// TestMCPInitializeServesInstructions locks the auto-adoption contract: the
// initialize response must carry a non-empty `instructions` string so clients
// (Claude Code, Codex) inject Mora's usage guidance into the model's context and
// the agent actually reaches for the tools.
func TestMCPInitializeServesInstructions(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	res := mcpResult(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	instr, _ := res["instructions"].(string)
	if strings.TrimSpace(instr) == "" {
		t.Fatalf("initialize must return a non-empty instructions field, got: %v", res)
	}
	for _, want := range []string{"search_memory", "context_memory", "write_memory"} {
		if !strings.Contains(instr, want) {
			t.Fatalf("instructions should mention %q so the agent knows to call it; got: %s", want, instr)
		}
	}
}
