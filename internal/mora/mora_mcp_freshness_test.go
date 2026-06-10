package mora

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// TestMCPSearchMemoryCarriesFreshness locks the honest-snapshot contract onto
// the PRIMARY query surface: every search_memory call must carry the per-source
// last_synced timestamps (same sourceFreshness map context_memory already
// ships), so an agent can tell the user "this answer is from data synced N
// hours ago" instead of presenting a stale vault as live. The payload becomes
// {results, freshness} — object-shaped, so structuredContent attaches too.
func TestMCPSearchMemoryCarriesFreshness(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	stamp := "2026-06-10T08:00:00Z"
	if err := memory.SaveStatus(filepath.Join(cfg.StateDir, "sync", "google-gmail.json"),
		&memory.SyncStatus{Source: "gmail", LastSynced: stamp, ItemCount: 3}); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}

	res, err := callMCPTool(context.Background(), "search_memory", map[string]any{"query": "alpha"})
	if err != nil {
		t.Fatalf("search_memory: %v", err)
	}
	obj, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("search_memory returned %T, want map with results+freshness", res)
	}
	results, ok := obj["results"].([]Memory)
	if !ok || len(results) != 1 || results[0].Title != "Alpha" {
		t.Fatalf("results = %#v, want the single Alpha hit", obj["results"])
	}
	fresh, ok := obj["freshness"].(map[string]string)
	if !ok {
		t.Fatalf("freshness missing or wrong type: %#v", obj["freshness"])
	}
	if fresh["gmail"] != stamp {
		t.Fatalf("freshness[gmail] = %q, want %q", fresh["gmail"], stamp)
	}
}

// TestMCPSearchMemoryFreshnessOverWire locks the same contract at the JSON-RPC
// layer: the text content block parses as {results, freshness} and the result
// carries structuredContent (object-shaped payloads must — the Codex contract).
func TestMCPSearchMemoryFreshnessOverWire(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")

	res := mcpResult(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"alpha"}}}`)
	if _, ok := res["structuredContent"]; !ok {
		t.Fatalf("search_memory result is missing structuredContent: %v", res)
	}
	content, _ := res["content"].([]any)
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var payload struct {
		Results   []Memory          `json:"results"`
		Freshness map[string]string `json:"freshness"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("text block is not {results,freshness} JSON: %v\n%s", err, text)
	}
	if len(payload.Results) != 1 || payload.Results[0].Title != "Alpha" {
		t.Fatalf("expected the Alpha hit in results, got: %s", text)
	}
	if payload.Freshness == nil {
		t.Fatalf("freshness key absent from wire payload: %s", text)
	}
}
