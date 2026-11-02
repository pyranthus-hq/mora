package mora

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// agentsByID decodes any tool or CLI payload and maps each memory row's id to
// its agent field, wherever the rows sit ("memory" or "memories").
func agentsByID(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	switch {
	case doc["memories"] != nil:
		if err := json.Unmarshal(doc["memories"], &rows); err != nil {
			t.Fatal(err)
		}
	case doc["memory"] != nil:
		var row map[string]any
		if err := json.Unmarshal(doc["memory"], &row); err != nil {
			t.Fatal(err)
		}
		rows = []map[string]any{row}
	default:
		var row map[string]any
		if err := json.Unmarshal(b, &row); err != nil {
			t.Fatal(err)
		}
		rows = []map[string]any{row}
	}
	out := map[string]any{}
	for _, row := range rows {
		id, _ := row["id"].(string)
		out[id] = row["agent"]
	}
	return out
}

func writtenID(t *testing.T, v any) string {
	t.Helper()
	for id := range agentsByID(t, v) {
		if id != "" {
			return id
		}
	}
	t.Fatalf("no memory id in %#v", v)
	return ""
}

// A stdio MCP session signs its writes with the clientInfo.name the client
// sent in initialize, an explicit agent argument overrides it, and the
// signature is persisted in the memory file.
func TestMCPServeSignsWritesWithInitializeClientName(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for _, tc := range []struct {
		client, explicit, want string
	}{
		{"claude-code", "", "claude-code"},
		{"codex-mcp-client", "", "codex"},
		{"claude-code", "test-bot", "test-bot"},
	} {
		args := map[string]any{"title": "signed by " + tc.want, "text": "fixture body for " + tc.want}
		if tc.explicit != "" {
			args["agent"] = tc.explicit
		}
		call, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "write_memory", "arguments": args}})
		if err != nil {
			t.Fatal(err)
		}
		in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"` + tc.client + `","version":"1"}}}` + "\n" + string(call) + "\n"
		var out bytes.Buffer
		if err := serveMCP(testCtx(t), &out, strings.NewReader(in)); err != nil {
			t.Fatal(err)
		}
		var id string
		scanner := bufio.NewScanner(&out)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			var resp struct {
				ID     float64 `json:"id"`
				Result struct {
					IsError           bool `json:"isError"`
					StructuredContent struct {
						Memory struct {
							ID    string `json:"id"`
							Agent string `json:"agent"`
						} `json:"memory"`
					} `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.ID != 2 {
				continue
			}
			if resp.Result.IsError {
				t.Fatalf("write_memory failed: %s", scanner.Text())
			}
			if got := resp.Result.StructuredContent.Memory.Agent; got != tc.want {
				t.Fatalf("client %q explicit %q: response agent = %q, want %q", tc.client, tc.explicit, got, tc.want)
			}
			id = resp.Result.StructuredContent.Memory.ID
		}
		if id == "" {
			t.Fatalf("no write_memory response in %s", out.String())
		}
		m, err := findMemory(cfg, id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Agent != tc.want {
			t.Fatalf("persisted agent = %q, want %q", m.Agent, tc.want)
		}
	}
}

// Every read surface shows the signature, and an authored memory written with
// none (the shape of every pre-signature memory) reads as "unknown".
func TestAgentSignatureSurfacesOnReadListSearch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	ctx := testCtx(t)

	cliOut := run(t, "write", "--title", "zephyrine cli note", "--text", "zephyrine fixture from the cli", "--json")
	cliID := writtenID(t, json.RawMessage(cliOut))
	botOut := run(t, "write", "--agent", "Test-Bot", "--title", "zephyrine bot note", "--text", "zephyrine fixture from a bot", "--json")
	botID := writtenID(t, json.RawMessage(botOut))
	codex, err := callMCPTool(mcppkg.WithClientName(ctx, "codex-mcp-client"), "write_memory", map[string]any{"title": "zephyrine codex note", "text": "zephyrine fixture from codex"})
	if err != nil {
		t.Fatal(err)
	}
	codexID := writtenID(t, codex)
	legacy, err := callMCPTool(ctx, "write_memory", map[string]any{"title": "zephyrine unsigned note", "text": "zephyrine fixture with no client name"})
	if err != nil {
		t.Fatal(err)
	}
	legacyID := writtenID(t, legacy)
	if m, err := findMemory(mustConfig(t), legacyID); err != nil || m.Agent != "" {
		t.Fatalf("unsigned write persisted agent %q, %v", m.Agent, err)
	}

	want := map[string]any{cliID: "cli", botID: "test-bot", codexID: "codex", legacyID: "unknown"}
	check := func(surface string, got map[string]any) {
		t.Helper()
		for id, agent := range want {
			if a, ok := got[id]; ok && a != agent {
				t.Fatalf("%s: %s agent = %#v, want %q", surface, id, a, agent)
			}
		}
	}
	for id, agent := range want {
		got := agentsByID(t, json.RawMessage(run(t, "read", id, "--json")))
		if got[id] != agent {
			t.Fatalf("cli read %s agent = %#v, want %q", id, got[id], agent)
		}
		res, err := callMCPTool(ctx, "read_memory", map[string]any{"id": id})
		if err != nil {
			t.Fatal(err)
		}
		if got := agentsByID(t, res)[id]; got != agent {
			t.Fatalf("read_memory %s agent = %#v, want %q", id, got, agent)
		}
	}
	listed := agentsByID(t, json.RawMessage(run(t, "list", "--json")))
	if len(listed) < len(want) {
		t.Fatalf("cli list returned %d rows, want %d", len(listed), len(want))
	}
	check("cli list", listed)
	searched := agentsByID(t, json.RawMessage(run(t, "search", "zephyrine", "--json")))
	if len(searched) == 0 {
		t.Fatal("cli search found nothing")
	}
	check("cli search", searched)
	for _, tool := range []string{"list_memory", "search_memory"} {
		res, err := callMCPTool(ctx, tool, map[string]any{"query": "zephyrine", "limit": float64(10)})
		if err != nil {
			t.Fatal(err)
		}
		got := agentsByID(t, res)
		if len(got) == 0 {
			t.Fatalf("%s returned no rows", tool)
		}
		check(tool, got)
	}
	human := run(t, "list")
	for _, label := range []string{"by cli", "by test-bot", "by codex", "by unknown"} {
		if !strings.Contains(human, label) {
			t.Fatalf("human list lacks %q:\n%s", label, human)
		}
	}
	if _, err := runErr(t, "write", "--agent", "two words", "--title", "t", "--text", "x"); err == nil {
		t.Fatal("cli accepted an invalid --agent")
	}
}

// A propose-mode write stores the client's signature in the proposal, the
// queue shows it, and approval publishes the memory under that signature.
func TestProposeModeKeepsSignatureThroughApproval(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "config", "mcp-write-policy", "propose")
	staged, err := callMCPTool(mcppkg.WithClientName(testCtx(t), "claude-code"), "write_memory", map[string]any{"title": "pending signed fact", "text": "waits for the owner"})
	if err != nil {
		t.Fatal(err)
	}
	proposalID, _ := staged.(map[string]any)["proposal"].(map[string]any)["id"].(string)
	if proposalID == "" {
		t.Fatalf("no proposal: %#v", staged)
	}
	var queue mcpProposalsPayload
	if err := json.Unmarshal([]byte(run(t, "mcp", "proposals", "list", "--json")), &queue); err != nil {
		t.Fatal(err)
	}
	if len(queue.Proposals) != 1 || queue.Proposals[0].Agent != "claude-code" {
		t.Fatalf("proposal queue = %+v", queue.Proposals)
	}
	if text := run(t, "mcp", "proposals", "list"); !strings.Contains(text, "(by claude-code)") {
		t.Fatalf("human proposal list lacks the signature:\n%s", text)
	}
	var approved mcpProposalsPayload
	if err := json.Unmarshal([]byte(run(t, "mcp", "proposals", "approve", proposalID, "--json")), &approved); err != nil {
		t.Fatal(err)
	}
	m, err := findMemory(mustConfig(t), approved.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Agent != "claude-code" {
		t.Fatalf("approved memory agent = %q, want claude-code", m.Agent)
	}
}

// Undo keeps working on signed memories: a Teach correction is signed by the
// CLI, undoing it restores the original writer's signed memory, and deleting
// a signed memory removes it from every read surface.
func TestSignedMemoryUndoAndDelete(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	ctx := mcppkg.WithClientName(testCtx(t), "codex-mcp-client")
	cfg := mustConfig(t)
	res, err := callMCPTool(ctx, "write_memory", map[string]any{"title": "Revision A", "text": "quillwort revision a"})
	if err != nil {
		t.Fatal(err)
	}
	originalID := writtenID(t, res)

	run(t, "teach", "memory", "correct", "--id", originalID, "--title", "Revision B", "--text", "quillwort revision b", "--yes")
	governance, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := governance.Entries[len(governance.Entries)-1]
	replacement, err := findMemory(cfg, entry.ReplacementID)
	if err != nil || replacement.Agent != "cli" {
		t.Fatalf("correction agent = %q, %v; want cli", replacement.Agent, err)
	}
	run(t, "teach", "undo", entry.ID)
	restored, err := findMemory(cfg, originalID)
	if err != nil || restored.Text != "quillwort revision a" || restored.Agent != "codex" {
		t.Fatalf("undo restored %+v, %v; want revision a signed codex", restored, err)
	}

	if _, err := callMCPTool(ctx, "delete_memory", map[string]any{"id": originalID}); err != nil {
		t.Fatal(err)
	}
	if _, err := findMemory(cfg, originalID); err == nil {
		t.Fatal("deleted signed memory is still readable")
	}
	if got := agentsByID(t, json.RawMessage(run(t, "search", "quillwort", "--json"))); got[originalID] != nil {
		t.Fatalf("deleted signed memory still searchable: %#v", got)
	}
}
