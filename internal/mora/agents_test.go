package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// seedAgentFixture writes one authored memory in the granted scope, one
// connector record in the SAME scope (the case a scope check alone would leak),
// and one authored memory in a scope the profile is not granted.
func seedAgentFixture(t *testing.T) Config {
	t.Helper()
	cfg := coreBIngestInitCfg(t)
	for _, m := range []Memory{
		{ID: "mem_granted", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget plan", Text: "widgetrun granted alpha", CreatedAt: "2026-09-01T00:00:00Z"},
		{ID: "gmail_thread/raw1", Scope: "project:widget", Type: "email", Source: "raw1", Provider: "gmail", ProviderID: "raw1", Title: "Widget mail", Text: "widgetrun rawsecret mail body", CreatedAt: "2026-09-02T00:00:00Z"},
		{ID: "mem_personal", Scope: "personal", Type: "fact", Source: "mcp", Title: "Personal note", Text: "widgetrun personalsecret", CreatedAt: "2026-09-03T00:00:00Z"},
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func museProfile() agentProfile {
	return agentProfile{Name: "muse", ReadScopes: []string{"project:widget"}, Write: mcpWritePolicyPropose, Tools: append([]string(nil), agentDefaultTools...)}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAgentProfileNeverReturnsRawSourcesOrForeignScopes(t *testing.T) {
	seedAgentFixture(t)
	p := museProfile()
	ctx := withAgentProfile(testCtx(t), p)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"search_memory", map[string]any{"query": "widgetrun"}},
		{"list_memory", map[string]any{"limit": 50}},
	} {
		got, err := callMCPTool(ctx, tc.tool, tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		body := mustJSON(t, got)
		for _, leak := range []string{"rawsecret", "personalsecret", "gmail_thread/raw1", "mem_personal", "Widget mail", "Personal note", "/vault/"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s leaked %q to the profile: %s", tc.tool, leak, body)
			}
		}
		if tc.tool == "search_memory" && !strings.Contains(body, "mem_granted") {
			t.Fatalf("search_memory lost the granted memory: %s", body)
		}
	}

	for _, id := range []string{"gmail_thread/raw1", "mem_personal"} {
		if got, err := callMCPTool(ctx, "read_memory", map[string]any{"id": id}); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("read_memory(%s) = %v, %v; want not found", id, got, err)
		}
	}
	if _, err := callMCPTool(ctx, "read_memory", map[string]any{"id": "mem_granted"}); err != nil {
		t.Fatalf("read_memory(granted): %v", err)
	}
	if _, err := callMCPTool(ctx, "search_memory", map[string]any{"query": "widgetrun", "scope": "personal"}); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("foreign scope search error = %v", err)
	}
	if _, err := callMCPTool(ctx, "search_memory", map[string]any{"query": "widgetrun", "source": "gmail"}); err == nil || !strings.Contains(err.Error(), "raw sources") {
		t.Fatalf("connector source filter error = %v", err)
	}
	for _, tool := range []string{"digest", "delete_memory", "context_memory", "get_entity"} {
		if _, err := callMCPTool(ctx, tool, map[string]any{"id": "mem_granted", "name": "x"}); err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("%s error = %v, want not available", tool, err)
		}
	}
}

// TestAgentFilterHidesPositionsAndCountsOfHiddenRows pins the two indirect
// signals a hidden row could leave behind: a gap in the kept rows' rank
// positions, and a truncation count that includes it.
func TestAgentFilterHidesPositionsAndCountsOfHiddenRows(t *testing.T) {
	value := map[string]any{
		"results": []any{
			map[string]any{"id": "gmail_thread/raw1", "scope": "project:widget", "provider": "gmail", "title": "Widget mail"},
			map[string]any{"id": "mem_granted", "scope": "project:widget", "provenance": "authored", "title": "Widget plan"},
		},
		"ranking": []any{
			map[string]any{"evidence_id": "gmail_thread/raw1", "position": 1},
			map[string]any{"evidence_id": "mem_granted", "position": 2},
		},
		"results_truncated": 3,
	}
	got, kept, err := filterAgentResult(museProfile(), "search_memory", map[string]any{"query": "widgetrun"}, value)
	if err != nil {
		t.Fatal(err)
	}
	body := mustJSON(t, got)
	if strings.Contains(body, "raw1") || strings.Contains(body, "results_truncated") {
		t.Fatalf("filtered result still shows the hidden row: %s", body)
	}
	if ranking := mustJSON(t, got.(map[string]any)["ranking"]); ranking != `[{"evidence_id":"mem_granted","position":1}]` {
		t.Fatalf("ranking = %s, want the kept row at position 1", ranking)
	}
	if len(kept) != 1 || kept[0].ID != "mem_granted" {
		t.Fatalf("kept = %+v", kept)
	}
}

func TestMCPServeHTTPRefusesToStartWithoutProfiles(t *testing.T) {
	coreBIngestInitCfg(t)
	var out bytes.Buffer
	err := cmdMCPServeHTTP(testCtx(t), []string{"--port", "65533"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "no active agent profiles") {
		t.Fatalf("serve-http with no profiles: err = %v, want a refusal", err)
	}
}

func TestAgentProfileProposesEvenWhenVaultPolicyIsOpen(t *testing.T) {
	cfg := seedAgentFixture(t)
	if configMCPWritePolicy(cfg) != mcpWritePolicyOpen {
		t.Fatalf("fixture policy = %q, want open", configMCPWritePolicy(cfg))
	}
	before, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withAgentProfile(testCtx(t), museProfile())
	got, err := callMCPTool(ctx, "write_memory", map[string]any{"title": "Muse fact", "text": "proposed by muse", "type": "fact", "source": "manual"})
	if err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	id := proposalIDOf(got)
	if id == "" {
		t.Fatalf("write_memory did not stage a proposal: %s", mustJSON(t, got))
	}
	after, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("profile write reached the vault before approval: %d -> %d files", len(before), len(after))
	}
	proposal, _, err := readMCPWriteProposal(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.ProposedBy != "muse" || proposal.Arguments["source"] != "agent:muse" || proposal.Arguments["scope"] != "project:widget" {
		t.Fatalf("proposal attribution = by %q source %v scope %v", proposal.ProposedBy, proposal.Arguments["source"], proposal.Arguments["scope"])
	}
	var out bytes.Buffer
	if err := cmdMCP(testCtx(t), []string{"proposals", "approve", id}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("approve: %v", err)
	}
	approved, err := callMCPTool(ctx, "search_memory", map[string]any{"query": "proposed by muse"})
	if err != nil {
		t.Fatal(err)
	}
	if body := mustJSON(t, approved); !strings.Contains(body, "agent:muse") {
		t.Fatalf("approved memory is not attributed to the agent: %s", body)
	}
	if _, err := callMCPTool(ctx, "write_memory", map[string]any{"title": "x", "text": "y", "scope": "personal"}); err == nil || !strings.Contains(err.Error(), "may not write") {
		t.Fatalf("foreign-scope write error = %v", err)
	}
	readonly := museProfile()
	readonly.Write = mcpWritePolicyReadonly
	if _, err := callMCPTool(withAgentProfile(testCtx(t), readonly), "write_memory", map[string]any{"title": "x", "text": "y"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("readonly profile write error = %v", err)
	}
}

func TestAgentProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    agentProfile
		want string
	}{
		{"digest needs unrestricted", agentProfile{Name: "a", ReadScopes: []string{"project:x"}, Write: "propose", Tools: []string{"digest"}}, "cannot be filtered"},
		{"delete needs open", agentProfile{Name: "a", ReadScopes: []string{"all"}, Write: "propose", Tools: []string{"delete_memory"}}, "requires write=open"},
		{"delete needs unrestricted", agentProfile{Name: "a", ReadScopes: []string{"project:x"}, Write: "open", Tools: []string{"delete_memory"}}, "cannot be filtered"},
		{"bad scope", agentProfile{Name: "a", ReadScopes: []string{"sources/gmail"}, Write: "propose", Tools: []string{"search_memory"}}, "invalid scope"},
		{"all is exclusive", agentProfile{Name: "a", ReadScopes: []string{"all", "personal"}, Write: "propose", Tools: []string{"search_memory"}}, "cannot be combined"},
		{"bad name", agentProfile{Name: "../x", ReadScopes: []string{"personal"}, Write: "propose", Tools: []string{"search_memory"}}, "must match"},
	} {
		if err := tc.p.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	ok := agentProfile{Name: "owner", ReadScopes: []string{"all"}, RawSources: true, Write: "open", Tools: []string{"digest", "delete_memory"}}
	if err := ok.validate(); err != nil {
		t.Fatalf("unrestricted profile rejected: %v", err)
	}
}

func TestAgentsCLIStoresOnlyTokenHashAndRevokes(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var out bytes.Buffer
	if err := cmdAgents(testCtx(t), []string{"add", "muse", "--read-scopes", "project:widget"}, &out, &out); err != nil {
		t.Fatalf("agents add: %v", err)
	}
	token := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, agentTokenPrefix) {
			token = strings.TrimSpace(line)
		}
	}
	if token == "" {
		t.Fatalf("no token printed: %s", out.String())
	}
	raw, err := os.ReadFile(agentsStorePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("agents.json stores the plaintext token")
	}
	if info, _ := os.Stat(agentsStorePath(cfg)); info.Mode().Perm() != 0o600 {
		t.Fatalf("agents.json mode = %v, want 0600", info.Mode().Perm())
	}
	store, _ := loadAgentsStore(cfg)
	if p, ok := store.matchToken(token); !ok || p.Name != "muse" || p.Write != mcpWritePolicyPropose {
		t.Fatalf("token did not map to the propose-mode muse profile: %#v %v", p, ok)
	}
	if err := cmdAgents(testCtx(t), []string{"update", "muse", "--read-scopes", "project:widget,global"}, &out, &out); err != nil {
		t.Fatalf("agents update: %v", err)
	}
	store, _ = loadAgentsStore(cfg)
	if p, ok := store.matchToken(token); !ok || len(p.ReadScopes) != 2 || p.Write != mcpWritePolicyPropose {
		t.Fatalf("update lost the token or changed unrelated fields: %#v %v", p, ok)
	}
	if err := cmdAgents(testCtx(t), []string{"update", "muse", "--tools", "digest"}, &out, &out); err == nil {
		t.Fatal("update accepted a tool the profile cannot filter")
	}
	if err := cmdAgents(testCtx(t), []string{"revoke", "muse"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	store, _ = loadAgentsStore(cfg)
	if _, ok := store.matchToken(token); ok {
		t.Fatal("revoked token still maps to a profile")
	}
}

func mcpHTTPTestServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	coreBIngestInitCfg(t)
	var out bytes.Buffer
	if err := cmdAgents(testCtx(t), []string{"add", "grok", "--read-scopes", "project:widget"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	token := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, agentTokenPrefix) {
			token = strings.TrimSpace(line)
		}
	}
	ctx := testCtx(t)
	srv := newMCPHTTPServer(7780, []string{"box.example.ts.net"}, func() (agentsStore, error) {
		cfg, err := loadConfigFor(ctx)
		if err != nil {
			return agentsStore{}, err
		}
		return loadAgentsStore(cfg)
	})
	return srv.handler(ctx), token
}

func doMCPHTTP(t *testing.T, h http.Handler, method, host, token, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://"+host+"/mcp", strings.NewReader(body)).WithContext(testCtx(t))
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMCPHTTPRequiresProfileTokenAndAllowedHost(t *testing.T) {
	h, token := mcpHTTPTestServer(t)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	for _, tc := range []struct {
		name, method, host, token, body string
		headers                         []string
		want                            int
	}{
		{"no token", "POST", "127.0.0.1:7780", "", init, nil, 401},
		{"wrong token", "POST", "127.0.0.1:7780", agentTokenPrefix + "nope", init, nil, 401},
		{"lowercase scheme", "POST", "127.0.0.1:7780", "", init, []string{"Authorization", "bearer " + token}, 200},
		{"bare agent token", "POST", "127.0.0.1:7780", "", init, []string{"Authorization", token}, 200},
		{"other scheme", "POST", "127.0.0.1:7780", "", init, []string{"Authorization", "Basic " + token}, 401},
		{"foreign host", "POST", "evil.example.com", token, init, nil, 403},
		{"browser origin", "POST", "127.0.0.1:7780", token, init, []string{"Origin", "https://evil.example.com"}, 403},
		{"get stream", "GET", "127.0.0.1:7780", token, "", nil, 405},
		{"batch", "POST", "127.0.0.1:7780", token, "[" + init + "]", nil, 400},
		{"notification", "POST", "box.example.ts.net", token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil, 202},
		{"tunnel host", "POST", "box.example.ts.net", token, init, nil, 200},
		{"tunnel host :443", "POST", "box.example.ts.net:443", token, init, nil, 200},
	} {
		if rec := doMCPHTTP(t, h, tc.method, tc.host, tc.token, tc.body, tc.headers...); rec.Code != tc.want {
			t.Fatalf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	rec := doMCPHTTP(t, h, "POST", "box.example.ts.net", token, init)
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", resp.Result["protocolVersion"])
	}
	instr, _ := resp.Result["instructions"].(string)
	if !strings.Contains(instr, `"grok" agent profile`) || !strings.Contains(instr, "pending proposal queue") || strings.Contains(instr, "you do not need to ask permission") {
		t.Fatalf("instructions do not describe the profile boundary: %s", instr)
	}
	rec = doMCPHTTP(t, h, "POST", "box.example.ts.net", token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	body := rec.Body.String()
	for _, want := range agentDefaultTools {
		if !strings.Contains(body, `"`+want+`"`) {
			t.Fatalf("tools/list missing %s: %s", want, body)
		}
	}
	for _, hidden := range []string{"delete_memory", "digest", "calendar_events", "meeting_prep"} {
		if strings.Contains(body, `"`+hidden+`"`) {
			t.Fatalf("tools/list exposes %s to the profile: %s", hidden, body)
		}
	}
	var out bytes.Buffer
	if err := cmdAgents(testCtx(t), []string{"revoke", "grok"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if rec := doMCPHTTP(t, h, "POST", "box.example.ts.net", token, init); rec.Code != 401 {
		t.Fatalf("revoked token status %d, want 401", rec.Code)
	}
}

func TestAgentReceiptsRecordRowsNeverText(t *testing.T) {
	cfg := seedAgentFixture(t)
	ctx := withAgentProfile(testCtx(t), museProfile())
	if _, err := callMCPTool(ctx, "search_memory", map[string]any{"query": "widgetrun"}); err != nil {
		t.Fatal(err)
	}
	_, _ = callMCPTool(ctx, "digest", map[string]any{})
	b, err := os.ReadFile(agentReceiptsPath(cfg, "muse"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "mem_granted") || !strings.Contains(log, `"action":"refused"`) {
		t.Fatalf("receipts missing rows or refusal: %s", log)
	}
	for _, leak := range []string{"granted alpha", "rawsecret", "personalsecret"} {
		if strings.Contains(log, leak) {
			t.Fatalf("receipt log contains memory text %q: %s", leak, log)
		}
	}
}
