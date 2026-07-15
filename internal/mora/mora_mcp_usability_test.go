package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestMCPToolSchemasAreSpecific locks the discoverability contract: every MCP
// tool must advertise a real inputSchema (typed properties + required fields),
// not the catch-all {type:object, additionalProperties:true}. Neil's pilot
// reported the tools were "not useful directly" because the agent had no idea
// what args to pass; this is the regression guard for that.
func TestMCPToolSchemasAreSpecific(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	res := mcpResult(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	toolsRaw, _ := res["tools"].([]any)
	if len(toolsRaw) == 0 {
		t.Fatalf("tools/list returned no tools: %v", res)
	}
	schemas := map[string]map[string]any{}
	for _, tr := range toolsRaw {
		tm, _ := tr.(map[string]any)
		name, _ := tm["name"].(string)
		sc, _ := tm["inputSchema"].(map[string]any)
		schemas[name] = sc
	}

	props := func(tool string) map[string]any {
		sc := schemas[tool]
		if sc == nil {
			t.Fatalf("tool %q missing from tools/list", tool)
		}
		p, ok := sc["properties"].(map[string]any)
		if !ok || len(p) == 0 {
			t.Fatalf("tool %q has no inputSchema.properties (still the catch-all schema): %v", tool, sc)
		}
		return p
	}
	required := func(tool string) map[string]bool {
		out := map[string]bool{}
		if rs, ok := schemas[tool]["required"].([]any); ok {
			for _, r := range rs {
				if s, ok := r.(string); ok {
					out[s] = true
				}
			}
		}
		return out
	}
	hasProp := func(tool, prop string) {
		if _, ok := props(tool)[prop]; !ok {
			t.Errorf("tool %q should declare property %q; got %v", tool, prop, props(tool))
		}
	}
	mustRequire := func(tool string, fields ...string) {
		req := required(tool)
		for _, f := range fields {
			if !req[f] {
				t.Errorf("tool %q must mark %q required; got required=%v", tool, f, req)
			}
		}
	}

	hasProp("search_memory", "query")
	hasProp("search_memory", "scope")
	hasProp("search_memory", "limit")
	mustRequire("search_memory", "query")

	hasProp("write_memory", "title")
	hasProp("write_memory", "text")
	mustRequire("write_memory", "title", "text")

	mustRequire("read_memory", "id")
	mustRequire("delete_memory", "id")
	mustRequire("get_entity", "name")

	hasProp("context_memory", "max_tokens")
	hasProp("get_entity", "max_tokens")
	hasProp("meeting_prep", "event_id")
	hasProp("meeting_prep", "at")

	hasProp("list_memory", "limit")

	hasProp("think", "query")
	mustRequire("think", "query")
}

// TestResolveContextBudget locks the token-budget contract: agents speak tokens
// (Neil asked for a ~20k-token per-call ceiling), the engine stays char-based
// (~charsPerToken chars/token, no tokenizer dependency). The token count is
// clamped BEFORE the char conversion so a huge max_tokens can't overflow.
func TestResolveContextBudget(t *testing.T) {
	maxChars := maxContextTokens * charsPerToken
	defChars := defaultContextTokens * charsPerToken

	cases := []struct {
		name      string
		maxTokens int
		want      int
	}{
		{"default when unset", 0, defChars},
		{"negative falls back to default", -5, defChars},
		{"tokens to chars", 50, 200},
		{"at the ceiling", maxContextTokens, maxChars},
		{"clamped above ceiling (no overflow)", 1_000_000_000, maxChars},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveContextBudget(Config{}, c.maxTokens); got != c.want {
				t.Fatalf("resolveContextBudget(Config{}, %d) = %d, want %d", c.maxTokens, got, c.want)
			}
		})
	}
}

// TestContextMemoryHonorsMaxTokens proves the token budget controls response
// size end-to-end through MCP: a tiny max_tokens truncates hard, a larger one
// returns more of the same content.
func TestContextMemoryHonorsMaxTokens(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	big := ""
	for i := 0; i < 4000; i++ {
		big += "x"
	}
	run(t, "write", "--scope", "global", "--title", "Bulk", "--text", big)

	small, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_memory","arguments":{"query":"Bulk","max_tokens":50}}}`)
	if isErr {
		t.Fatalf("context_memory errored: %s", small)
	}
	large, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"context_memory","arguments":{"query":"Bulk","max_tokens":2000}}}`)
	if isErr {
		t.Fatalf("context_memory errored: %s", large)
	}
	if len(small) == 0 {
		t.Fatalf("small budget produced empty context")
	}
	if len(small) >= len(large) {
		t.Fatalf("max_tokens=50 (%d bytes) should yield less than max_tokens=2000 (%d bytes)", len(small), len(large))
	}
	// max_tokens=50 ≈ 200 chars of body; allow slack for the JSON envelope and
	// freshness map, but it must stay far under the 4000-char body.
	if len(small) > 1500 {
		t.Fatalf("max_tokens=50 should truncate hard (~200 chars of body); got %d bytes: %s", len(small), small)
	}
}

// TestContextMemoryBudgetUnit stamps budget_unit on the MCP envelope (#69).
func TestContextMemoryBudgetUnit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_memory","arguments":{"query":"lorem","max_tokens":500}}}`)
	if isErr {
		t.Fatalf("context_memory errored: %s", text)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if env["budget_unit"] != budgetUnitTokens {
		t.Fatalf("budget_unit = %v, want %q", env["budget_unit"], budgetUnitTokens)
	}
	if budget, ok := env["budget"].(float64); !ok || int(budget) != 500 {
		t.Fatalf("budget = %v, want 500 tokens", env["budget"])
	}
	if _, ok := env["used"]; !ok {
		t.Fatalf("missing used field: %v", env)
	}
}

// TestContextCLIBudgetHonorsTokens proves mora context --budget counts tokens,
// bounds the items array, and stamps budget_unit in --json output (#69).
func TestContextCLIBudgetHonorsTokens(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	big := strings.Repeat("x", 4000)
	run(t, "write", "--scope", "global", "--title", "Bulk", "--text", big)

	out := run(t, "context", "--query", "Bulk", "--budget", "500", "--json")
	var env struct {
		Context    string            `json:"context"`
		Items      []contextItemJSON `json:"items"`
		BudgetUnit string            `json:"budget_unit"`
		Budget     int               `json:"budget"`
		Used       int               `json:"used"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if env.BudgetUnit != budgetUnitTokens {
		t.Fatalf("budget_unit = %q, want %q", env.BudgetUnit, budgetUnitTokens)
	}
	if env.Budget != 500 {
		t.Fatalf("budget = %d, want 500 tokens", env.Budget)
	}
	// ~500 tokens ≈ 2000 chars of context; must not ship the full 4000-char body.
	if len(env.Context) > 2500 {
		t.Fatalf("context length %d exceeds ~500-token budget (~2000 chars)", len(env.Context))
	}
	itemsJSON, _ := json.Marshal(env.Items)
	if len(itemsJSON) > 2500 {
		t.Fatalf("items JSON %d bytes should honor the same token budget", len(itemsJSON))
	}
}

// TestGetEntityDossierShape pins the budgeted, fully-cited dossier contract (Track B).
// Neighbor type values are stubbed until Track A merges — this test locks shape only.
func TestGetEntityDossierShape(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, Memory{
		ID: "m1", Scope: "project:demo", Title: "Kickoff with Neil",
		Text: "[[Neil]] discussed the pilot", CreatedAt: "2026-05-30T10:00:00Z",
		Provider: "gmail", Source: "gmail",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, Memory{
		ID: "m2", Scope: "project:demo", Title: "Follow-up",
		Text: "[[Neil]] ok", CreatedAt: "2026-05-31T10:00:00Z",
		Provider: "gmail", Source: "gmail",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_entity","arguments":{"name":"Neil","max_tokens":6000}}}`)
	if isErr {
		t.Fatalf("get_entity errored: %s", text)
	}
	var wrapped struct {
		Entity map[string]any `json:"entity"`
		Health compactHealth  `json:"health"`
	}
	if err := json.Unmarshal([]byte(text), &wrapped); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	dossier := wrapped.Entity
	for _, key := range []string{"budget_unit", "budget", "used", "evidence", "aliases", "display_name", "found"} {
		if _, ok := dossier[key]; !ok {
			t.Fatalf("dossier missing %q: %v", key, dossier)
		}
	}
	if dossier["budget_unit"] != budgetUnitTokens {
		t.Fatalf("budget_unit = %v", dossier["budget_unit"])
	}
	if dossier["found"] != true {
		t.Fatalf("found = %v", dossier["found"])
	}
	evidence, ok := dossier["evidence"].([]any)
	if !ok || len(evidence) == 0 {
		t.Fatalf("evidence = %v", dossier["evidence"])
	}
	row, ok := evidence[0].(map[string]any)
	if !ok {
		t.Fatalf("evidence row type: %T", evidence[0])
	}
	for _, field := range []string{"id", "title", "source", "created_at", "snippet"} {
		if row[field] == nil || row[field] == "" {
			t.Fatalf("evidence row missing cited field %q: %v", field, row)
		}
	}
	if _, hasMemories := dossier["memories"]; hasMemories {
		t.Fatalf("dossier must not ship raw memories — use evidence + read_memory")
	}
	if nbrs, ok := dossier["neighbors"].([]any); ok && len(nbrs) > 0 {
		n := nbrs[0].(map[string]any)
		if n["type"] != neighborTypeStub {
			t.Fatalf("neighbor type = %v, want stub %q until Track A", n["type"], neighborTypeStub)
		}
	}
}

// TestGetEntityHonorsMaxTokens proves max_tokens scales the cited evidence payload.
func TestGetEntityHonorsMaxTokens(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	body := strings.Repeat("lorem ipsum dolor sit amet ", 80)
	for i := 0; i < 30; i++ {
		if err := writeMemory(cfg, Memory{
			ID: fmt.Sprintf("gmail_thread/t%04d", i), Scope: "personal", Type: "email",
			Title: fmt.Sprintf("Thread %04d", i), Text: body + " [[Neil Patel]]",
			CreatedAt: fmt.Sprintf("2026-05-%02dT00:00:00Z", (i%28)+1),
			Meta: map[string]any{
				"from":  []string{"neil@example.com"},
				"names": map[string]string{"neil@example.com": "Neil Patel"},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	small, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_entity","arguments":{"name":"Neil Patel","max_tokens":200}}}`)
	if isErr {
		t.Fatalf("get_entity small: %s", small)
	}
	large, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_entity","arguments":{"name":"Neil Patel","max_tokens":8000}}}`)
	if isErr {
		t.Fatalf("get_entity large: %s", large)
	}
	if len(large) <= len(small) {
		t.Fatalf("max_tokens=8000 (%d bytes) should exceed max_tokens=200 (%d bytes)", len(large), len(small))
	}
}

// TestBudgetHardBoundContextCLI asserts a tiny --budget cannot be exceeded by the
// items array even when a single memory body alone is larger (#69 hard bound).
func TestBudgetHardBoundContextCLI(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	huge := strings.Repeat("z", 8000)
	run(t, "write", "--scope", "global", "--title", "Huge", "--text", huge)

	tokenBudget := 50
	charBudget := tokenBudget * charsPerToken
	out := run(t, "context", "--query", "Huge", "--budget", fmt.Sprint(tokenBudget), "--json")
	var env struct {
		Context string   `json:"context"`
		Items   []Memory `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	itemsJSON, _ := json.Marshal(env.Items)
	if len(env.Context) > charBudget {
		t.Fatalf("context %d chars exceeds %d-char budget for %d tokens", len(env.Context), charBudget, tokenBudget)
	}
	remaining := charBudget - len(env.Context)
	if remaining < 0 {
		remaining = 0
	}
	if remaining == 0 {
		if string(itemsJSON) != "[]" {
			t.Fatalf("with no remaining budget, items must be []; got %s", itemsJSON)
		}
	} else if len(itemsJSON) > remaining {
		t.Fatalf("items JSON %d chars exceeds remaining budget %d (context=%d, total=%d)",
			len(itemsJSON), remaining, len(env.Context), charBudget)
	}
}

// TestBudgetHardBoundGetEntity asserts get_entity dossier evidence respects max_tokens.
func TestBudgetHardBoundGetEntity(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	body := strings.Repeat("alpha beta gamma ", 200)
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/huge", Scope: "personal", Type: "email",
		Title: "Oversized thread", Text: body + " [[Neil Patel]]",
		CreatedAt: "2026-05-01T00:00:00Z",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	tokenBudget := 30
	text, isErr := mcpToolText(t, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_entity","arguments":{"name":"Neil Patel","max_tokens":%d}}}`,
		tokenBudget))
	if isErr {
		t.Fatalf("get_entity errored: %s", text)
	}
	charBudget := mcpEntityBudgetChars(Config{}, tokenBudget) / 2
	var dossier map[string]any
	if err := json.Unmarshal([]byte(text), &dossier); err != nil {
		t.Fatalf("decode: %v", err)
	}
	evidenceJSON, _ := json.Marshal(dossier["evidence"])
	if len(evidenceJSON) > charBudget {
		t.Fatalf("evidence JSON %d chars exceeds dossier char budget %d", len(evidenceJSON), charBudget)
	}
}

// TestBudgetEntityEvidenceHardBound unit-tests the greedy evidence capper.
func TestBudgetEntityEvidenceHardBound(t *testing.T) {
	big := EntityEvidence{
		ID: "x", Title: "T", Source: "gmail", CreatedAt: "2026-01-01T00:00:00Z",
		Snippet: strings.Repeat("s", 5000),
	}
	got, _ := budgetEntityEvidence([]EntityEvidence{big}, 100)
	if len(got) == 0 {
		t.Fatal("expected a truncated evidence row")
	}
	if jsonLen(got)+2 > 100 {
		t.Fatalf("evidence payload %d bytes exceeds 100-byte budget", jsonLen(got)+2)
	}
}

// TestBudgetContextItemsHardBound unit-tests the items capper.
func TestBudgetContextItemsHardBound(t *testing.T) {
	m := Memory{ID: "m1", Title: "Big", Text: strings.Repeat("t", 5000), CreatedAt: "2026-01-01T00:00:00Z"}
	row := contextItemJSON{ID: m.ID, Title: m.Title, CreatedAt: m.CreatedAt, Text: m.Text}
	fitted, ok := fitContextItemJSON(row, 116)
	if !ok {
		t.Fatalf("fitContextItemJSON failed for compact projection")
	}
	if jsonLen(fitted) > 116 {
		t.Fatalf("fitted item %d bytes exceeds 116-byte cap", jsonLen(fitted))
	}
	got := budgetContextItemsJSON([]Memory{m}, 0, 120, "Big")
	if len(got) == 0 {
		t.Fatal("expected a truncated item")
	}
	payload, _ := json.Marshal(got)
	if len(payload) > 120 {
		t.Fatalf("items JSON %d bytes exceeds 120-byte budget", len(payload))
	}
}
