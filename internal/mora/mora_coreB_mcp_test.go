package mora

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// mora_coreB_mcp_test.go — white-box coverage for the MCP server layer:
// cmdMCP, serveMCP, handleMCP, toCallToolResult, callMCPTool, sourceFreshness.
// Every test drives a real behavior and asserts on the concrete result.

// coreBMcpFrames parses the newline-delimited JSON-RPC frames a `mcp serve` run
// wrote to stdout, returning one decoded frame per non-blank line.
func coreBMcpFrames(t *testing.T, out string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("non-JSON frame on the wire: %v\n%s", err, line)
		}
		frames = append(frames, f)
	}
	return frames
}

// coreBMcpServe runs `mcp serve` over the given stdin and returns combined
// stdout+stderr plus the run error (nil on clean exit).
func coreBMcpServe(t *testing.T, stdin string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), []string{"mcp", "serve"}, &out, &out, strings.NewReader(stdin))
	return out.String(), err
}

// --- cmdMCP -----------------------------------------------------------------

// TestCoreB_McpCmdMCPBadSubcommand locks the usage guard: anything other than a
// single "serve" arg is a usage error, not a silent no-op.
func TestCoreB_McpCmdMCPBadSubcommand(t *testing.T) {
	withTempHome(t)
	var out bytes.Buffer
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"serve", "extra"},
	} {
		err := cmdMCP(context.Background(), args, &out, &out, strings.NewReader(""))
		if err == nil {
			t.Fatalf("cmdMCP(%v) = nil, want usage error", args)
		}
		if err.Error() != "usage: mora mcp serve" {
			t.Fatalf("cmdMCP(%v) error = %q, want %q", args, err.Error(), "usage: mora mcp serve")
		}
	}
}

// TestCoreB_McpCmdMCPServeEmptyStdin drives the happy branch: `serve` with an
// empty stdin dispatches to serveMCP, which returns nil (EOF, no frames).
func TestCoreB_McpCmdMCPServeEmptyStdin(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	if err := cmdMCP(context.Background(), []string{"serve"}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("cmdMCP serve with empty stdin should exit clean, got %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("empty stdin must produce no frames, got: %q", out.String())
	}
}

// --- serveMCP ----------------------------------------------------------------

// TestCoreB_McpServeSkipsNotificationAndMalformed proves serveMCP's two continue
// branches: a JSON-parse failure and an id-less notification both produce NO
// frame, and a following valid request is still answered exactly once.
func TestCoreB_McpServeSkipsNotificationAndMalformed(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	in := strings.Join([]string{
		`{ this is not valid json`,                                    // json.Unmarshal fails -> continue
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,      // no id -> continue
		`{"jsonrpc":"2.0","id":42,"method":"initialize","params":{}}`, // answered
	}, "\n") + "\n"

	out, err := coreBMcpServe(t, in)
	if err != nil {
		t.Fatalf("serve: %v\n%s", err, out)
	}
	frames := coreBMcpFrames(t, out)
	if len(frames) != 1 {
		t.Fatalf("expected exactly 1 response frame (the initialize), got %d:\n%s", len(frames), out)
	}
	if frames[0]["id"] != float64(42) {
		t.Fatalf("the single frame must answer id 42, got id=%v", frames[0]["id"])
	}
	if _, ok := frames[0]["result"].(map[string]any); !ok {
		t.Fatalf("initialize must carry a result object, got: %v", frames[0])
	}
}

// TestCoreB_McpServeOverCapLine locks the request-size guard: a single line past
// mcpMaxRequestBytes makes serveMCP return the loud "MCP request line exceeded"
// error (bufio.ErrTooLong wrapped) rather than silently truncating.
func TestCoreB_McpServeOverCapLine(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	// One line strictly larger than the 4MiB cap, valid-ish JSON so the failure is
	// unambiguously the size guard (not a parse error, which is a silent continue).
	huge := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"x":"` +
		strings.Repeat("a", mcpMaxRequestBytes+16) + `"}}` + "\n"

	out, err := coreBMcpServe(t, huge)
	if err == nil {
		t.Fatalf("an over-cap line must return an error, got nil\n%s", out)
	}
	if !strings.Contains(err.Error(), "MCP request line exceeded") {
		t.Fatalf("over-cap error = %q, want it to mention the byte cap", err.Error())
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("over-cap error must wrap bufio.ErrTooLong, got %v", err)
	}
}

// coreBMcpErrReader is an io.Reader that always fails, used to drive serveMCP's
// scanner-error (non-ErrTooLong) return branch.
type coreBMcpErrReader struct{}

func (coreBMcpErrReader) Read(p []byte) (int, error) { return 0, errors.New("reader exploded") }

// TestCoreB_McpServeScannerError covers serveMCP's non-ErrTooLong error branch:
// a stdin reader that fails mid-scan surfaces the raw error, not nil.
func TestCoreB_McpServeScannerError(t *testing.T) {
	var out bytes.Buffer
	err := serveMCP(context.Background(), &out, coreBMcpErrReader{})
	if err == nil {
		t.Fatalf("a failing stdin reader must make serveMCP return an error")
	}
	if err.Error() != "reader exploded" {
		t.Fatalf("serveMCP scanner error = %q, want the raw reader error", err.Error())
	}
	if errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("a generic read error must NOT be reported as the byte-cap error")
	}
}

// --- handleMCP ---------------------------------------------------------------

// TestCoreB_McpHandleUnknownMethod covers the default arm: an unknown method with
// an id returns a JSON-RPC error frame (-32601 method not found), no result.
func TestCoreB_McpHandleUnknownMethod(t *testing.T) {
	resp := handleMCP(context.Background(), jsonRPCRequest{JSONRPC: "2.0", ID: float64(7), Method: "does/not/exist"})
	if resp.Result != nil {
		t.Fatalf("unknown method must not carry a result, got: %v", resp.Result)
	}
	if resp.ID != float64(7) {
		t.Fatalf("response must echo the request id 7, got: %v", resp.ID)
	}
	e, ok := resp.Error.(map[string]any)
	if !ok {
		t.Fatalf("unknown method must set an error object, got %T: %v", resp.Error, resp.Error)
	}
	if e["code"] != -32601 {
		t.Fatalf("error code = %v, want -32601", e["code"])
	}
	if e["message"] != "method not found" {
		t.Fatalf("error message = %v, want %q", e["message"], "method not found")
	}
}

// TestCoreB_McpHandleInitialize covers the initialize arm directly: protocol
// version + the auto-adoption instructions the clients inject.
func TestCoreB_McpHandleInitialize(t *testing.T) {
	resp := handleMCP(context.Background(), jsonRPCRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	if resp.Error != nil {
		t.Fatalf("initialize must not error, got: %v", resp.Error)
	}
	res, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("initialize result must be a map, got %T", resp.Result)
	}
	if res["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v, want 2024-11-05", res["protocolVersion"])
	}
	instr, _ := res["instructions"].(string)
	if !strings.Contains(instr, "search_memory") {
		t.Fatalf("instructions must mention search_memory, got: %q", instr)
	}
}

// TestCoreB_McpHandleToolsList covers the tools/list arm: exactly the 12 tools we
// publish, each a JSON-Schema object with the required properties.
func TestCoreB_McpHandleToolsList(t *testing.T) {
	resp := handleMCP(context.Background(), jsonRPCRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/list"})
	res, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/list result must be a map, got %T", resp.Result)
	}
	tools, ok := res["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools must be a slice of maps, got %T", res["tools"])
	}
	if len(tools) != 12 {
		t.Fatalf("expected 12 published tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl["name"].(string)] = true
	}
	for _, want := range []string{"write_memory", "read_memory", "search_memory", "list_memory",
		"delete_memory", "context_memory", "think", "list_entities", "get_entity", "digest", "brief", "meeting_prep"} {
		if !names[want] {
			t.Fatalf("tools/list is missing %q; got %v", want, names)
		}
	}
}

// TestCoreB_McpHandleToolsCall covers the tools/call arm: params are unmarshaled
// and dispatched through callMCPTool, wrapped in a CallToolResult.
func TestCoreB_McpHandleToolsCall(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")

	params := json.RawMessage(`{"name":"list_memory","arguments":{}}`)
	resp := handleMCP(context.Background(), jsonRPCRequest{JSONRPC: "2.0", ID: float64(9), Method: "tools/call", Params: params})
	if resp.Error != nil {
		t.Fatalf("tools/call must not set a JSON-RPC error, got: %v", resp.Error)
	}
	res, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/call result must be a CallToolResult map, got %T", resp.Result)
	}
	if res["isError"] != false {
		t.Fatalf("a successful list_memory must not be isError, got: %v", res)
	}
	content := res["content"].([]map[string]any)
	text := content[0]["text"].(string)
	if !strings.Contains(text, "Alpha") {
		t.Fatalf("tools/call text block should carry the memory payload, got: %q", text)
	}
}

// --- toCallToolResult --------------------------------------------------------

// TestCoreB_McpToCallToolResultError: a tool error becomes isError content, not a
// JSON-RPC error, so the agent's tool loop survives.
func TestCoreB_McpToCallToolResultError(t *testing.T) {
	res := toCallToolResult(nil, errors.New("boom"))
	if res["isError"] != true {
		t.Fatalf("error result must set isError:true, got: %v", res)
	}
	if _, ok := res["structuredContent"]; ok {
		t.Fatalf("error result must not carry structuredContent, got: %v", res)
	}
	content := res["content"].([]map[string]any)
	if content[0]["text"] != "boom" {
		t.Fatalf("error text = %v, want %q", content[0]["text"], "boom")
	}
}

// TestCoreB_McpToCallToolResultObject: an object-shaped value carries the JSON in
// the text block AND mirrors into structuredContent.
func TestCoreB_McpToCallToolResultObject(t *testing.T) {
	v := map[string]any{"deleted": "id-1"}
	res := toCallToolResult(v, nil)
	if res["isError"] != false {
		t.Fatalf("success result must set isError:false, got: %v", res)
	}
	sc, ok := res["structuredContent"]
	if !ok {
		t.Fatalf("object result must attach structuredContent, got: %v", res)
	}
	if _, isMap := sc.(map[string]any); !isMap {
		t.Fatalf("structuredContent must mirror the object value, got %T", sc)
	}
	content := res["content"].([]map[string]any)
	text := content[0]["text"].(string)
	var back map[string]any
	if err := json.Unmarshal([]byte(text), &back); err != nil {
		t.Fatalf("text block must be the object's JSON: %v\n%s", err, text)
	}
	if back["deleted"] != "id-1" {
		t.Fatalf("round-tripped payload = %v, want deleted=id-1", back)
	}
}

// TestCoreB_McpToCallToolResultArray: an array (non-object) value ships in the
// text block but must NOT set structuredContent (spec: object-only).
func TestCoreB_McpToCallToolResultArray(t *testing.T) {
	res := toCallToolResult([]int{1, 2, 3}, nil)
	if res["isError"] != false {
		t.Fatalf("success result must set isError:false, got: %v", res)
	}
	if _, ok := res["structuredContent"]; ok {
		t.Fatalf("array result must NOT carry structuredContent, got: %v", res)
	}
	content := res["content"].([]map[string]any)
	text := content[0]["text"].(string)
	if !strings.HasPrefix(strings.TrimSpace(text), "[") {
		t.Fatalf("array payload text must be a JSON array, got: %q", text)
	}
}

// TestCoreB_McpToCallToolResultMarshalFallback: a value json.MarshalIndent cannot
// encode (a map holding a func) falls back to fmt.Sprintf without panicking, and
// the non-'{' fallback text means no structuredContent is attached.
func TestCoreB_McpToCallToolResultMarshalFallback(t *testing.T) {
	v := map[string]any{"fn": func() {}}
	res := toCallToolResult(v, nil)
	if res["isError"] != false {
		t.Fatalf("fallback result must still be a success, got: %v", res)
	}
	if _, ok := res["structuredContent"]; ok {
		t.Fatalf("Sprintf fallback text is not '{'-prefixed, so no structuredContent; got: %v", res)
	}
	content := res["content"].([]map[string]any)
	text := content[0]["text"].(string)
	if !strings.HasPrefix(text, "map[") {
		t.Fatalf("expected the fmt.Sprintf form of the map, got: %q", text)
	}
}

// --- callMCPTool -------------------------------------------------------------

// coreBMcpInit sets up a temp home + initialized vault so callMCPTool's loadConfig
// resolves a real vault + control files + index.
func coreBMcpInit(t *testing.T) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
}

// TestCoreB_McpCallWriteMemoryHappy: write_memory returns the saved Memory with a
// minted id; the body is really on disk (read_memory resolves it).
func TestCoreB_McpCallWriteMemoryHappy(t *testing.T) {
	coreBMcpInit(t)
	got, err := callMCPTool(context.Background(), "write_memory", map[string]any{
		"title": "Coverage Note", "text": "coreB body", "type": "decision", "scope": "project:acme",
	})
	if err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	m, ok := got.(Memory)
	if !ok {
		t.Fatalf("write_memory returned %T, want Memory", got)
	}
	if m.ID == "" {
		t.Fatalf("write_memory must mint an id, got empty")
	}
	if m.Title != "Coverage Note" || m.Text != "coreB body" || m.Type != "decision" || m.Scope != "project:acme" {
		t.Fatalf("write_memory echoed wrong memory: %+v", m)
	}
	if m.Source != "mcp" {
		t.Fatalf("write_memory default source = %q, want mcp", m.Source)
	}
	// The write really persisted: read it straight back by id.
	back, err := callMCPTool(context.Background(), "read_memory", map[string]any{"id": m.ID})
	if err != nil {
		t.Fatalf("read_memory after write: %v", err)
	}
	if back.(Memory).Text != "coreB body" {
		t.Fatalf("read_memory returned wrong body: %+v", back)
	}
}

// TestCoreB_McpCallWriteMemoryMissingFields: an absent title or text is a loud
// validation error, never a silent empty write.
func TestCoreB_McpCallWriteMemoryMissingFields(t *testing.T) {
	coreBMcpInit(t)
	for _, args := range []map[string]any{
		{"text": "no title here"},
		{"title": "no text here"},
		{},
	} {
		_, err := callMCPTool(context.Background(), "write_memory", args)
		if err == nil {
			t.Fatalf("write_memory(%v) = nil error, want validation error", args)
		}
		if err.Error() != "title and text required" {
			t.Fatalf("write_memory(%v) error = %q, want %q", args, err.Error(), "title and text required")
		}
	}
}

// TestCoreB_McpCallReadMemoryMissingID: read_memory with no id fails with the
// not-found error (findMemory never matches an empty id to a real memory).
func TestCoreB_McpCallReadMemoryMissingID(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--title", "Present", "--text", "present body")
	_, err := callMCPTool(context.Background(), "read_memory", map[string]any{})
	if err == nil {
		t.Fatalf("read_memory with no id must error")
	}
	if !strings.Contains(err.Error(), "memory not found") {
		t.Fatalf("read_memory error = %q, want a not-found message", err.Error())
	}
}

// TestCoreB_McpCallSearchMemory: search_memory returns {results, freshness}, with
// the matching memory in results and the per-source freshness snapshot alongside.
func TestCoreB_McpCallSearchMemory(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--scope", "global", "--title", "Vega", "--text", "vega alignment notes")
	got, err := callMCPTool(context.Background(), "search_memory", map[string]any{"query": "vega"})
	if err != nil {
		t.Fatalf("search_memory: %v", err)
	}
	obj := got.(map[string]any)
	results, ok := obj["results"].([]Memory)
	if !ok || len(results) != 1 || results[0].Title != "Vega" {
		t.Fatalf("search_memory results = %#v, want the single Vega hit", obj["results"])
	}
	if _, ok := obj["freshness"].(map[string]string); !ok {
		t.Fatalf("search_memory must ship a freshness map, got %T", obj["freshness"])
	}
}

// TestCoreB_McpCallSearchMemoryTruncates: a large limit over many big matches
// overruns searchMemoryResultsBudgetBytes, so the payload is trimmed on whole-
// memory boundaries and the cut is reported honestly via results_truncated.
func TestCoreB_McpCallSearchMemoryTruncates(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	body := strings.Repeat("budget alignment context ", 20) // ~500 chars so each snippet fills its cap
	for i := 0; i < 60; i++ {
		m := Memory{
			ID:        "big-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Scope:     "global",
			Type:      "insight",
			Title:     "Budget note " + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Text:      body,
			Source:    "test",
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
		}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	got, err := callMCPTool(context.Background(), "search_memory", map[string]any{"query": "budget", "limit": float64(60)})
	if err != nil {
		t.Fatalf("search_memory: %v", err)
	}
	obj := got.(map[string]any)
	dropped, ok := obj["results_truncated"].(int)
	if !ok || dropped <= 0 {
		t.Fatalf("expected results_truncated > 0 when the byte budget is exceeded, got %v", obj["results_truncated"])
	}
	results := obj["results"].([]Memory)
	if len(results) == 0 {
		t.Fatalf("truncation must still keep some results, got none")
	}
	// The kept set must respect the byte budget (trimmed on whole-memory boundaries).
	if b, _ := json.Marshal(results); len(b) > searchMemoryResultsBudgetBytes+2000 {
		t.Fatalf("kept results (%d B) far exceed the budget %d B — not trimmed", len(b), searchMemoryResultsBudgetBytes)
	}
}

// TestCoreB_McpCallGetEntity: get_entity resolves a [[link]] entity and returns
// the memories that reference it plus a count.
func TestCoreB_McpCallGetEntity(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--scope", "global", "--title", "Linkholder", "--text", "notes about [[Nebula Project]]")
	// write now upserts only the memory + FTS row (O(1)); the entity graph is
	// materialized by a full rebuild, so build it before querying entities.
	run(t, "index", "rebuild")
	got, err := callMCPTool(context.Background(), "get_entity", map[string]any{"name": "Nebula Project"})
	if err != nil {
		t.Fatalf("get_entity: %v", err)
	}
	res := got.(map[string]any)
	mems, ok := res["memories"].([]Memory)
	if !ok || len(mems) == 0 {
		t.Fatalf("get_entity must return the referencing memories, got %#v", res["memories"])
	}
	found := false
	for _, m := range mems {
		if m.Title == "Linkholder" {
			found = true
		}
	}
	if !found {
		t.Fatalf("get_entity memories should include Linkholder, got %+v", mems)
	}
}

// TestCoreB_McpCallContextMemoryWithQuery: a query drives the hybridSearch path
// and returns a map carrying both context and freshness.
func TestCoreB_McpCallContextMemoryWithQuery(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--title", "Pyranthus roadmap", "--text", "Ship the coreB coverage milestone")
	got, err := callMCPTool(context.Background(), "context_memory", map[string]any{"query": "coreB coverage"})
	if err != nil {
		t.Fatalf("context_memory(query): %v", err)
	}
	obj := got.(map[string]any)
	ctx, ok := obj["context"].(string)
	if !ok {
		t.Fatalf("context_memory must return a context string, got %T", obj["context"])
	}
	if !strings.Contains(ctx, "coreB coverage milestone") {
		t.Fatalf("context should surface the matching memory body, got: %q", ctx)
	}
	if _, ok := obj["freshness"].(map[string]string); !ok {
		t.Fatalf("context_memory must return a freshness map, got %T", obj["freshness"])
	}
}

// TestCoreB_McpCallContextMemoryNoQuery: with no query, context_memory falls back
// to a recency briefing (listMemories path) and still returns context+freshness.
func TestCoreB_McpCallContextMemoryNoQuery(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--title", "Recent thing", "--text", "a recent recency-briefing body")
	got, err := callMCPTool(context.Background(), "context_memory", map[string]any{})
	if err != nil {
		t.Fatalf("context_memory(no query): %v", err)
	}
	obj := got.(map[string]any)
	ctx, ok := obj["context"].(string)
	if !ok {
		t.Fatalf("context_memory must return a context string, got %T", obj["context"])
	}
	if !strings.Contains(ctx, "recency-briefing body") {
		t.Fatalf("recency briefing should include the recent memory, got: %q", ctx)
	}
	if _, ok := obj["freshness"].(map[string]string); !ok {
		t.Fatalf("context_memory must return a freshness map, got %T", obj["freshness"])
	}
}

// TestCoreB_McpCallThink: think returns a synthesis envelope with cited evidence
// for the queried memory and a non-empty synthesis prompt.
func TestCoreB_McpCallThink(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--title", "Zephyr launch", "--text", "The Zephyr launch shipped on schedule")
	got, err := callMCPTool(context.Background(), "think", map[string]any{"query": "Zephyr launch"})
	if err != nil {
		t.Fatalf("think: %v", err)
	}
	res, ok := got.(ThinkResult)
	if !ok {
		t.Fatalf("think returned %T, want ThinkResult", got)
	}
	if res.Query != "Zephyr launch" {
		t.Fatalf("think echoed query %q", res.Query)
	}
	if len(res.Evidence) == 0 {
		t.Fatalf("think must cite the matching memory as evidence, got none")
	}
	found := false
	for _, e := range res.Evidence {
		if e.Title == "Zephyr launch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence should include the Zephyr memory, got %+v", res.Evidence)
	}
	if strings.TrimSpace(res.SynthesisPrompt) == "" {
		t.Fatalf("think must carry a synthesis prompt")
	}
}

// TestCoreB_McpCallListEntities: list_entities returns the entity graph rows —
// here the scope entity created by a scoped write.
func TestCoreB_McpCallListEntities(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--scope", "project:zeta", "--title", "Scoped", "--text", "scoped body about [[Zeta Widget]]")
	// write upserts only memory + FTS (O(1)); rebuild materializes the entity graph.
	run(t, "index", "rebuild")
	got, err := callMCPTool(context.Background(), "list_entities", map[string]any{})
	if err != nil {
		t.Fatalf("list_entities: %v", err)
	}
	ents, ok := got.([]Entity)
	if !ok {
		t.Fatalf("list_entities returned %T, want []Entity", got)
	}
	var haveScope, haveLink bool
	for _, e := range ents {
		if e.Kind == "scope" && e.Name == "project:zeta" {
			haveScope = true
		}
		if e.Kind == "link" && e.Name == "Zeta Widget" {
			haveLink = true
		}
	}
	if !haveScope {
		t.Fatalf("expected a scope entity project:zeta, got %+v", ents)
	}
	if !haveLink {
		t.Fatalf("expected a link entity 'Zeta Widget', got %+v", ents)
	}
}

// TestCoreB_McpCallListEntitiesKindFilter: a kind filter narrows the result to
// just that kind.
func TestCoreB_McpCallListEntitiesKindFilter(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--scope", "project:zeta", "--title", "Scoped", "--text", "body with [[Zeta Widget]]")
	// write upserts only memory + FTS (O(1)); rebuild materializes the entity graph.
	run(t, "index", "rebuild")
	got, err := callMCPTool(context.Background(), "list_entities", map[string]any{"kind": "link"})
	if err != nil {
		t.Fatalf("list_entities(kind=link): %v", err)
	}
	ents := got.([]Entity)
	if len(ents) == 0 {
		t.Fatalf("expected at least one link entity")
	}
	for _, e := range ents {
		if !strings.EqualFold(e.Kind, "link") {
			t.Fatalf("kind filter leaked a non-link entity: %+v", e)
		}
	}
}

// TestCoreB_McpCallDigestSinceHours: an explicit since_hours selects the plain
// window path and the payload echoes that window plus the structured sections.
func TestCoreB_McpCallDigestSinceHours(t *testing.T) {
	coreBMcpInit(t)
	got, err := callMCPTool(context.Background(), "digest", map[string]any{"since_hours": float64(24)})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	p := got.(map[string]any)
	if p["since_hours"] != 24 {
		t.Fatalf("digest since_hours = %v, want 24", p["since_hours"])
	}
	if _, ok := p["sections"]; !ok {
		t.Fatalf("digest payload must ship sections; keys=%v", coreBMcpKeys(p))
	}
	if _, ok := p["source_states"]; !ok {
		t.Fatalf("digest payload must ship source_states; keys=%v", coreBMcpKeys(p))
	}
	// The digest payload must NOT double a render string beside the typed sections.
	if _, ok := p["digest"]; ok {
		t.Fatalf("digest payload must not carry a render string; keys=%v", coreBMcpKeys(p))
	}
}

// TestCoreB_McpCallDigestEntityNoMatch: an entity that resolves to nothing is a
// loud error, not an empty digest.
func TestCoreB_McpCallDigestEntityNoMatch(t *testing.T) {
	coreBMcpInit(t)
	_, err := callMCPTool(context.Background(), "digest", map[string]any{"entity": "Nonexistent Person"})
	if err == nil {
		t.Fatalf("digest with an unresolvable entity must error")
	}
	if !strings.Contains(err.Error(), "no entity matches") {
		t.Fatalf("digest entity error = %q, want a no-entity-matches message", err.Error())
	}
}

// TestCoreB_McpCallDigestEntityResolves: a resolvable entity (by address) threads
// the entity filter into the digest without error — the success arm of the
// entity block.
func TestCoreB_McpCallDigestEntityResolves(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, personMemNamed("p-riya", "gmail", "riya@a.com", "Riya Karode", time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	got, err := callMCPTool(context.Background(), "digest", map[string]any{"entity": "riya@a.com", "since_hours": float64(24)})
	if err != nil {
		t.Fatalf("digest with a resolvable entity must not error: %v", err)
	}
	if _, ok := got.(map[string]any)["sections"]; !ok {
		t.Fatalf("filtered digest must still ship a sections key, got: %v", got)
	}
}

// TestCoreB_McpCallBriefEntityResolves: a resolvable entity threads the filter
// into the brief through the filter-aware factory without error.
func TestCoreB_McpCallBriefEntityResolves(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, personMemNamed("p-riya", "gmail", "riya@a.com", "Riya Karode", time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	got, err := callMCPTool(context.Background(), "brief", map[string]any{"entity": "riya@a.com"})
	if err != nil {
		t.Fatalf("brief with a resolvable entity must not error: %v", err)
	}
	if _, ok := got.(map[string]any)["sections"]; !ok {
		t.Fatalf("filtered brief must still ship a sections key, got: %v", got)
	}
}

// TestCoreB_McpCallMeetingPrepWithEvent: a seeded next-meeting event + a named,
// resolvable attendee drives the full prep pack — the success arm where the
// attendee filter resolves and buildMeetingPrep returns a non-nil event.
func TestCoreB_McpCallMeetingPrepWithEvent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	pinPrepClock(t, now)
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "riya@a.com", "Riya Karode", now.Add(-48*time.Hour))); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Acme sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com")); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	got, err := callMCPTool(context.Background(), "meeting_prep", map[string]any{"name": "riya@a.com"})
	if err != nil {
		t.Fatalf("meeting_prep with a resolvable attendee: %v", err)
	}
	mp := got.(MeetingPrepResult)
	if mp.Event == nil || mp.Event.Title != "Acme sync" {
		t.Fatalf("meeting_prep must resolve the Acme sync event, got %+v", mp.Event)
	}
	if strings.TrimSpace(mp.SynthesisPrompt) == "" {
		t.Fatalf("meeting_prep must carry a synthesis prompt")
	}
}

// TestCoreB_McpCallDigestEnvelope: opting into envelope returns the DigestEnvelope
// (a synthesis_prompt beside the same budgeted sections).
func TestCoreB_McpCallDigestEnvelope(t *testing.T) {
	coreBMcpInit(t)
	got, err := callMCPTool(context.Background(), "digest", map[string]any{"envelope": true})
	if err != nil {
		t.Fatalf("digest envelope: %v", err)
	}
	env, ok := got.(DigestEnvelope)
	if !ok {
		t.Fatalf("digest envelope returned %T, want DigestEnvelope", got)
	}
	if strings.TrimSpace(env.SynthesisPrompt) == "" {
		t.Fatalf("envelope must carry a synthesis_prompt")
	}
}

// TestCoreB_McpCallBriefUnfiltered: an unfiltered brief returns the digestMCPPayload
// map (source_states + sections), the session-start briefing.
func TestCoreB_McpCallBriefUnfiltered(t *testing.T) {
	coreBMcpInit(t)
	got, err := callMCPTool(context.Background(), "brief", map[string]any{})
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	p := got.(map[string]any)
	if _, ok := p["sections"]; !ok {
		t.Fatalf("brief payload must ship sections; keys=%v", coreBMcpKeys(p))
	}
	if _, ok := p["source_states"]; !ok {
		t.Fatalf("brief payload must ship source_states; keys=%v", coreBMcpKeys(p))
	}
}

// TestCoreB_McpCallBriefScopeFilter: a scope arg routes through the filter-aware
// factory (filteredBriefDigest) and still returns a structured payload.
func TestCoreB_McpCallBriefScopeFilter(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--scope", "project:acme", "--title", "Acme", "--text", "acme scoped body")
	got, err := callMCPTool(context.Background(), "brief", map[string]any{"scope": "project:acme", "since_days": float64(7)})
	if err != nil {
		t.Fatalf("brief(scope): %v", err)
	}
	p := got.(map[string]any)
	if _, ok := p["sections"]; !ok {
		t.Fatalf("filtered brief payload must ship sections; keys=%v", coreBMcpKeys(p))
	}
}

// TestCoreB_McpCallBriefEntityNoMatch: a brief entity filter that resolves to
// nothing errors, mirroring digest.
func TestCoreB_McpCallBriefEntityNoMatch(t *testing.T) {
	coreBMcpInit(t)
	_, err := callMCPTool(context.Background(), "brief", map[string]any{"entity": "Nobody Here"})
	if err == nil {
		t.Fatalf("brief with an unresolvable entity must error")
	}
	if !strings.Contains(err.Error(), "no entity matches") {
		t.Fatalf("brief entity error = %q, want a no-entity-matches message", err.Error())
	}
}

// TestCoreB_McpCallBriefEnvelope: brief with envelope returns a DigestEnvelope
// carrying a synthesis_prompt (the additive Phase-15 machinery).
func TestCoreB_McpCallBriefEnvelope(t *testing.T) {
	coreBMcpInit(t)
	got, err := callMCPTool(context.Background(), "brief", map[string]any{"envelope": true})
	if err != nil {
		t.Fatalf("brief envelope: %v", err)
	}
	env, ok := got.(DigestEnvelope)
	if !ok {
		t.Fatalf("brief envelope returned %T, want DigestEnvelope", got)
	}
	if strings.TrimSpace(env.SynthesisPrompt) == "" {
		t.Fatalf("brief envelope must carry a synthesis_prompt")
	}
}

// TestCoreB_McpCallMeetingPrep: with no calendar connected the next-meeting prep
// resolves to a nil-event pack (returned as-is by meetingPrepMCPPayload).
func TestCoreB_McpCallMeetingPrep(t *testing.T) {
	coreBMcpInit(t)
	got, err := callMCPTool(context.Background(), "meeting_prep", map[string]any{})
	if err != nil {
		t.Fatalf("meeting_prep: %v", err)
	}
	mp, ok := got.(MeetingPrepResult)
	if !ok {
		t.Fatalf("meeting_prep returned %T, want MeetingPrepResult", got)
	}
	// No calendar source => no event; the payload passes through untouched.
	if mp.Event != nil {
		t.Fatalf("expected a nil event with no calendar connected, got %+v", mp.Event)
	}
}

// TestCoreB_McpCallMeetingPrepEntityNoMatch: a name that resolves to no entity is
// a loud error before any prep is built.
func TestCoreB_McpCallMeetingPrepEntityNoMatch(t *testing.T) {
	coreBMcpInit(t)
	_, err := callMCPTool(context.Background(), "meeting_prep", map[string]any{"name": "Ghost Attendee"})
	if err == nil {
		t.Fatalf("meeting_prep with an unresolvable name must error")
	}
	if !strings.Contains(err.Error(), "no entity matches") {
		t.Fatalf("meeting_prep name error = %q, want a no-entity-matches message", err.Error())
	}
}

// TestCoreB_McpCallDeleteMemoryHappy: write then delete returns {deleted:id} and
// the memory is really gone (read_memory no longer resolves it).
func TestCoreB_McpCallDeleteMemoryHappy(t *testing.T) {
	coreBMcpInit(t)
	written, err := callMCPTool(context.Background(), "write_memory", map[string]any{"title": "Doomed", "text": "delete me"})
	if err != nil {
		t.Fatalf("seed write_memory: %v", err)
	}
	id := written.(Memory).ID
	got, err := callMCPTool(context.Background(), "delete_memory", map[string]any{"id": id})
	if err != nil {
		t.Fatalf("delete_memory: %v", err)
	}
	res := got.(map[string]any)
	if res["deleted"] != id {
		t.Fatalf("delete_memory returned %v, want deleted=%s", res, id)
	}
	// The file is really removed: a by-id read now fails.
	if _, rerr := callMCPTool(context.Background(), "read_memory", map[string]any{"id": id}); rerr == nil {
		t.Fatalf("read_memory should fail after delete of %s", id)
	}
}

// TestCoreB_McpCallDeleteMemoryMissingID: delete_memory with no id fails at the
// findMemory lookup rather than removing anything.
func TestCoreB_McpCallDeleteMemoryMissingID(t *testing.T) {
	coreBMcpInit(t)
	_, err := callMCPTool(context.Background(), "delete_memory", map[string]any{})
	if err == nil {
		t.Fatalf("delete_memory with no id must error")
	}
	if !strings.Contains(err.Error(), "memory not found") {
		t.Fatalf("delete_memory error = %q, want a not-found message", err.Error())
	}
}

// TestCoreB_McpCallUnknownTool: an unrecognized tool name is a loud error naming
// the tool.
func TestCoreB_McpCallUnknownTool(t *testing.T) {
	coreBMcpInit(t)
	_, err := callMCPTool(context.Background(), "not_a_tool", map[string]any{})
	if err == nil {
		t.Fatalf("unknown tool must error")
	}
	if err.Error() != `unknown tool "not_a_tool"` {
		t.Fatalf("unknown tool error = %q, want %q", err.Error(), `unknown tool "not_a_tool"`)
	}
}

// TestCoreB_McpCallListMemory: list_memory returns the recent memories newest
// first, honoring a scope filter.
func TestCoreB_McpCallListMemory(t *testing.T) {
	coreBMcpInit(t)
	run(t, "write", "--scope", "project:one", "--title", "One", "--text", "body one")
	run(t, "write", "--scope", "project:two", "--title", "Two", "--text", "body two")
	got, err := callMCPTool(context.Background(), "list_memory", map[string]any{"scope": "project:one"})
	if err != nil {
		t.Fatalf("list_memory: %v", err)
	}
	mems := got.([]Memory)
	if len(mems) != 1 || mems[0].Title != "One" {
		t.Fatalf("scoped list_memory should return only 'One', got %+v", mems)
	}
}

// --- sourceFreshness ---------------------------------------------------------

// TestCoreB_McpSourceFreshness exercises every branch: Source-keyed rows (present
// and never-synced), the empty-Source filename-stem fallback (google-/imessage-
// prefix strip), and a corrupt status file that is skipped rather than mis-keyed.
func TestCoreB_McpSourceFreshness(t *testing.T) {
	cfg := testCfg(t)
	dir := filepath.Join(cfg.StateDir, "sync")

	// 1. Source set -> keyed by Source, non-empty timestamp.
	gmailStamp := "2026-06-10T08:00:00Z"
	coreBMcpSaveStatus(t, dir, "google-gmail.json", &memory.SyncStatus{Source: "gmail", LastSynced: gmailStamp})
	// 2. Source set but never synced -> keyed by Source, EMPTY value (surfaced, not dropped).
	coreBMcpSaveStatus(t, dir, "imessage-me.json", &memory.SyncStatus{Source: "imessage", LastSynced: ""})
	// 3. Empty Source -> fall back to filename stem, google- prefix stripped.
	calStamp := "2026-06-11T09:00:00Z"
	coreBMcpSaveStatus(t, dir, "google-calendar.json", &memory.SyncStatus{Source: "", LastSynced: calStamp})
	// 4. Empty Source -> fall back to filename stem, imessage- prefix stripped.
	buddyStamp := "2026-01-01T00:00:00Z"
	coreBMcpSaveStatus(t, dir, "imessage-buddy.json", &memory.SyncStatus{Source: "", LastSynced: buddyStamp})
	// 5. Corrupt status file -> LoadStatus errors -> skipped entirely.
	coreBMcpWriteRaw(t, filepath.Join(dir, "broken.json"), "{ not valid json")

	got := sourceFreshness(cfg)

	if got["gmail"] != gmailStamp {
		t.Fatalf("freshness[gmail] = %q, want %q", got["gmail"], gmailStamp)
	}
	if v, ok := got["imessage"]; !ok || v != "" {
		t.Fatalf("never-synced imessage must be present with an empty value, got (%q, present=%v)", v, ok)
	}
	if got["calendar"] != calStamp {
		t.Fatalf("empty-Source google-calendar.json must key as 'calendar'=%q, got %q", calStamp, got["calendar"])
	}
	if got["buddy"] != buddyStamp {
		t.Fatalf("empty-Source imessage-buddy.json must key as 'buddy'=%q, got %q", buddyStamp, got["buddy"])
	}
	if _, ok := got["broken"]; ok {
		t.Fatalf("a corrupt status file must be skipped, not keyed; got: %v", got)
	}
	if len(got) != 4 {
		t.Fatalf("expected exactly 4 keys (gmail, imessage, calendar, buddy), got %v", got)
	}
}

// TestCoreB_McpSourceFreshnessNoDir: a missing sync/ dir yields an empty map (the
// ReadDir error is swallowed by design).
func TestCoreB_McpSourceFreshnessNoDir(t *testing.T) {
	cfg := testCfg(t)
	got := sourceFreshness(cfg)
	if len(got) != 0 {
		t.Fatalf("no sync dir must yield an empty freshness map, got: %v", got)
	}
}

func coreBMcpSaveStatus(t *testing.T, dir, name string, st *memory.SyncStatus) {
	t.Helper()
	if err := memory.SaveStatus(filepath.Join(dir, name), st); err != nil {
		t.Fatalf("SaveStatus(%s): %v", name, err)
	}
}

func coreBMcpWriteRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func coreBMcpKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
