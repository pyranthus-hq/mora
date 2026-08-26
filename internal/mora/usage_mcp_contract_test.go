package mora

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Issue #245 — content-free MCP usage measurement.
//
// This file is the frozen contract for the usage event written by an MCP
// tools/call. It intentionally exercises the real handleMCP -> CallToolResult
// path: output_bytes is the compact JSON size of the FINAL result map, including
// both the text content block and structuredContent mirror. Read events expose
// only structural values. They never copy an id, body, match/evidence string,
// metadata value, or path into events.jsonl, even when query retention is opted
// in.

const (
	usageContractID             = "usage-contract-memory"
	usageContractBodySecret     = "BODY_SECRET_7f211d9d"
	usageContractMatchSecret    = "MATCH_SECRET_93a9c521"
	usageContractEvidenceSecret = "EVIDENCE_SECRET_4ed4c8a7"
	usageContractPathSecret     = "/private/attachments/PATH_SECRET_a161018a.pdf"
	usageContractFutureSecret   = "FUTURE_MODE_SECRET_e02d7608"
)

func seedUsageMCPContract(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	t.Setenv("DO_NOT_TRACK", "")
	// Read instrumentation must remain content-free even when the legacy query
	// retention opt-in is enabled. A read match is not a search query.
	t.Setenv("MORA_LOG_QUERIES", "1")
	run(t, "init")
	cfg := mustConfig(t)
	text := strings.Join([]string{
		usageContractBodySecret,
		strings.Repeat("padding ", 80),
		usageContractMatchSecret,
		strings.Repeat("padding ", 80),
	}, " ")
	if err := writeMemory(cfg, Memory{
		ID:        usageContractID,
		Scope:     "global",
		Type:      "note",
		Title:     "Usage contract",
		CreatedAt: "2026-07-31T12:00:00Z",
		Text:      text,
		Meta: map[string]any{
			"attachment_path": usageContractPathSecret,
			"evidence_text":   usageContractEvidenceSecret,
		},
	}); err != nil {
		t.Fatalf("seed usage contract memory: %v", err)
	}
	return cfg
}

func usageContractCall(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatalf("marshal MCP params: %v", err)
	}
	resp := handleMCP(testCtx(t), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "tools/call",
		Params:  params,
	})
	if resp.Error != nil {
		t.Fatalf("tools/call JSON-RPC error: %v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/call result = %T, want CallToolResult map", resp.Result)
	}
	return result
}

func usageContractEvents(t *testing.T, cfg Config) ([]map[string]any, []byte) {
	t.Helper()
	path := filepath.Join(cfg.StateDir, "usage", "events.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read usage events at %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	events := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("usage line %d is not independent valid JSON: %q", i+1, line)
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode usage line %d: %v", i+1, err)
		}
		events = append(events, event)
	}
	return events, raw
}

func usageContractInt(t *testing.T, event map[string]any, key string) int {
	t.Helper()
	n, ok := event[key].(float64)
	if !ok {
		t.Fatalf("usage event %q = %T, want JSON number: %v", key, event[key], event)
	}
	return int(n)
}

func usageContractSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func usageContractAssertReadShape(t *testing.T, event map[string]any) {
	t.Helper()
	want := []string{
		"budget_requested", "budget_used", "match_count", "millis", "mode",
		"output_bytes", "phases", "results", "tool", "truncated", "ts",
	}
	if got := usageContractSortedKeys(event); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("read usage keys = %v, want exact content-free schema %v; event=%v", got, want, event)
	}
	if event["tool"] != "read_memory" {
		t.Fatalf("read usage tool = %v, want read_memory", event["tool"])
	}
	if usageContractInt(t, event, "results") != 1 {
		t.Fatalf("read usage results = %v, want 1", event["results"])
	}
	if usageContractInt(t, event, "millis") < 0 || usageContractInt(t, event, "output_bytes") <= 0 {
		t.Fatalf("read usage duration/output invalid: %v", event)
	}
	phases, ok := event["phases"].(map[string]any)
	if !ok {
		t.Fatalf("read usage phases = %T, want object: %v", event["phases"], event)
	}
	wantPhases := []string{"assembly_ms", "config_ms", "envelope_ms", "retrieval_ms"}
	if got := usageContractSortedKeys(phases); fmt.Sprint(got) != fmt.Sprint(wantPhases) {
		t.Fatalf("read usage phase keys = %v, want %v", got, wantPhases)
	}
	for _, key := range wantPhases {
		if usageContractInt(t, phases, key) < 0 {
			t.Fatalf("read usage phase %s is negative: %v", key, phases)
		}
	}
}

func usageContractResultText(t *testing.T, result map[string]any) string {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal CallToolResult: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("decode CallToolResult: %v", err)
	}
	structured, ok := decoded["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("CallToolResult lacks object structuredContent mirror: %v", decoded)
	}
	memory, ok := structured["memory"].(map[string]any)
	if !ok {
		t.Fatalf("read_memory structuredContent lacks memory object: %v", structured)
	}
	text, _ := memory["text"].(string)
	return text
}

func TestUsageMCPReadFullAndBoundedAreStructuralAndFinalEnvelopeSized(t *testing.T) {
	cfg := seedUsageMCPContract(t)

	full := usageContractCall(t, "read_memory", map[string]any{"id": usageContractID})
	bounded := usageContractCall(t, "read_memory", map[string]any{
		"id": usageContractID, "match": usageContractMatchSecret, "max_tokens": float64(8),
	})
	events, raw := usageContractEvents(t, cfg)
	if len(events) != 2 {
		t.Fatalf("read usage event count = %d, want one full + one bounded event; log=%s", len(events), raw)
	}

	for _, secret := range []string{
		usageContractID,
		usageContractBodySecret,
		usageContractMatchSecret,
		usageContractEvidenceSecret,
		usageContractPathSecret,
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("usage log leaked read content %q: %s", secret, raw)
		}
	}

	usageContractAssertReadShape(t, events[0])
	if events[0]["mode"] != "full" || events[0]["truncated"] != false {
		t.Fatalf("full read structural fields incorrect: %v", events[0])
	}
	if usageContractInt(t, events[0], "match_count") != 0 || usageContractInt(t, events[0], "budget_requested") != 0 {
		t.Fatalf("full read match/budget fields incorrect: %v", events[0])
	}
	fullBytes, _ := json.Marshal(full)
	if got := usageContractInt(t, events[0], "output_bytes"); got != len(fullBytes) {
		t.Fatalf("full output_bytes = %d, want final CallToolResult size %d", got, len(fullBytes))
	}
	if got, want := usageContractInt(t, events[0], "budget_used"), estimateTokensUsed(len(usageContractResultText(t, full))); got != want {
		t.Fatalf("full budget_used = %d, want returned body estimate %d", got, want)
	}

	usageContractAssertReadShape(t, events[1])
	if events[1]["mode"] != "match" || events[1]["truncated"] != true {
		t.Fatalf("bounded read structural fields incorrect: %v", events[1])
	}
	if usageContractInt(t, events[1], "match_count") != 1 || usageContractInt(t, events[1], "budget_requested") != 8 {
		t.Fatalf("bounded read match/budget fields incorrect: %v", events[1])
	}
	boundedBytes, _ := json.Marshal(bounded)
	if got := usageContractInt(t, events[1], "output_bytes"); got != len(boundedBytes) {
		t.Fatalf("bounded output_bytes = %d, want final CallToolResult size %d", got, len(boundedBytes))
	}
	if got, want := usageContractInt(t, events[1], "budget_used"), estimateTokensUsed(len(usageContractResultText(t, bounded))); got != want {
		t.Fatalf("bounded budget_used = %d, want returned excerpt estimate %d", got, want)
	}
}

func TestUsageMCPReadModeLabelsAreAllowlisted(t *testing.T) {
	cfg := seedUsageMCPContract(t)
	usageContractCall(t, "read_memory", map[string]any{
		"id": usageContractID, "evidence_ref": usageContractEvidenceSecret,
	})
	usageContractCall(t, "read_memory", map[string]any{
		"id": usageContractID, "mode": usageContractFutureSecret,
	})
	events, raw := usageContractEvents(t, cfg)
	if len(events) != 2 {
		t.Fatalf("read mode event count = %d, want 2; log=%s", len(events), raw)
	}
	if events[0]["mode"] != "evidence_ref" {
		t.Fatalf("evidence_ref mode label = %v, want allowlisted label: %v", events[0]["mode"], events[0])
	}
	if events[1]["mode"] != "other" {
		t.Fatalf("unknown/future read mode label = %v, want generic other: %v", events[1]["mode"], events[1])
	}
	for _, secret := range []string{usageContractEvidenceSecret, usageContractFutureSecret} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("usage mode copied sensitive/raw value %q: %s", secret, raw)
		}
	}
}

func TestUsageMCPOutputBytesCoversPreviouslyUninstrumentedTools(t *testing.T) {
	cfg := seedUsageMCPContract(t)
	const writeSecret = "WRITE_BODY_SECRET_2908381e"
	result := usageContractCall(t, "write_memory", map[string]any{
		"title": "Usage write contract", "text": writeSecret,
	})
	events, raw := usageContractEvents(t, cfg)
	if len(events) != 1 {
		t.Fatalf("write_memory usage event count = %d, want 1; log=%s", len(events), raw)
	}
	if events[0]["tool"] != "write_memory" {
		t.Fatalf("usage tool = %v, want write_memory", events[0]["tool"])
	}
	b, _ := json.Marshal(result)
	if got := usageContractInt(t, events[0], "output_bytes"); got != len(b) {
		t.Fatalf("write_memory output_bytes = %d, want final CallToolResult size %d", got, len(b))
	}
	if strings.Contains(string(raw), writeSecret) {
		t.Fatalf("generic MCP usage event copied raw arguments/result content: %s", raw)
	}
}

func TestUsageMCPTrackingGatesSuppressReadEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate func(t *testing.T, cfg Config)
	}{
		{
			name: "DO_NOT_TRACK",
			gate: func(t *testing.T, _ Config) { t.Setenv("DO_NOT_TRACK", "1") },
		},
		{
			name: "OFF_sentinel",
			gate: func(t *testing.T, cfg Config) {
				dir := filepath.Join(cfg.StateDir, "usage")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "OFF"), []byte("off\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		subRun(t, tc.name, func(t *testing.T) {
			cfg := seedUsageMCPContract(t)
			tc.gate(t, cfg)
			usageContractCall(t, "read_memory", map[string]any{"id": usageContractID})
			path := filepath.Join(cfg.StateDir, "usage", "events.jsonl")
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("tracking gate wrote new usage event at %s (err=%v)", path, err)
			}
		})
	}
}

func TestUsageMCPEventsStayInStateNotVault(t *testing.T) {
	cfg := seedUsageMCPContract(t)
	usageContractCall(t, "read_memory", map[string]any{"id": usageContractID})
	statePath := filepath.Join(cfg.StateDir, "usage", "events.jsonl")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state-dir usage event missing: %v", err)
	}
	if err := filepath.WalkDir(cfg.VaultDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Base(path) == "events.jsonl" {
			return fmt.Errorf("usage event written into vault: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageMCPConcurrentCallsAppendIndependentJSONL(t *testing.T) {
	cfg := seedUsageMCPContract(t)
	params, err := json.Marshal(map[string]any{
		"name": "read_memory", "arguments": map[string]any{"id": usageContractID},
	})
	if err != nil {
		t.Fatal(err)
	}
	const calls = 64
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp := handleMCP(testCtx(t), jsonRPCRequest{
				JSONRPC: "2.0", ID: float64(id + 1), Method: "tools/call", Params: params,
			})
			if _, ok := resp.Result.(map[string]any); !ok {
				errs <- fmt.Errorf("call %d result = %T", id, resp.Result)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	events, raw := usageContractEvents(t, cfg)
	if len(events) != calls {
		t.Fatalf("concurrent usage line count = %d, want %d independent records; log=%s", len(events), calls, raw)
	}
	for i, event := range events {
		if event["tool"] != "read_memory" {
			t.Fatalf("concurrent event %d tool = %v, want read_memory", i, event["tool"])
		}
	}
}

func TestUsageMCPLegacyReaderRemainsBackwardCompatible(t *testing.T) {
	cfg := seedUsageMCPContract(t)
	dir := filepath.Join(cfg.StateDir, "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"ts":"2026-07-30T00:00:00Z","tool":"search_memory","results":2,"millis":7}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	usageContractCall(t, "read_memory", map[string]any{"id": usageContractID})
	var out strings.Builder
	if err := usageReport(cfg, &out); err != nil {
		t.Fatalf("usage report: %v", err)
	}
	if !strings.Contains(out.String(), "total calls: 2") ||
		!strings.Contains(out.String(), "search_memory: 1") ||
		!strings.Contains(out.String(), "read_memory: 1") {
		t.Fatalf("usage report did not combine legacy + current schema:\n%s", out.String())
	}
}
