package mora

import "testing"

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
