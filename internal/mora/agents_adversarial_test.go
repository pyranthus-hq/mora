package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// Adversarial fixtures for #539: every test here asks one question, "can this
// profile see or do more than its grant?", and answers it by scanning the whole
// JSON the client would receive for canary strings the profile must never see.

func crowdProfiles() map[string]agentProfile {
	tools := func() []string { return append([]string(nil), agentDefaultTools...) }
	return map[string]agentProfile{
		"multi-scope":  {Name: "duo", ReadScopes: []string{"project:widget", "personal"}, Write: mcpWritePolicyPropose, Tools: tools()},
		"single-scope": {Name: "muse", ReadScopes: []string{"project:widget"}, Write: mcpWritePolicyPropose, Tools: tools()},
	}
}

// seedCrowdFixture buries one allowed record under twelve stronger records the
// profile may not read (a foreign scope) and twelve stronger connector rows in
// its own scope. Authorization after ranking would return nothing.
func seedCrowdFixture(t *testing.T) Config {
	t.Helper()
	cfg := coreBIngestInitCfg(t)
	mems := []Memory{
		{ID: "mem_allowed_low", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Low ranked allowed", Text: "crowdword once among many other plain words about gardening", CreatedAt: "2026-09-01T00:00:00Z"},
	}
	for i := 0; i < 12; i++ {
		mems = append(mems,
			Memory{ID: fmt.Sprintf("mem_denied_%02d", i), Scope: "project:secret", Type: "fact", Source: "mcp", Title: "crowdword crowdword", Text: "crowdword crowdword crowdword crowdword deniedsecret", CreatedAt: "2026-09-02T00:00:00Z"},
			Memory{ID: fmt.Sprintf("gmail_thread/raw%02d", i), Scope: "project:widget", Type: "email", Source: fmt.Sprintf("raw%02d", i), Provider: "gmail", ProviderID: fmt.Sprintf("raw%02d", i), Title: "crowdword crowdword", Text: "crowdword crowdword crowdword crowdword rawsecret", CreatedAt: "2026-09-02T00:00:00Z"},
		)
	}
	for _, m := range mems {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAgentAllowedLowRankedRecordSurvivesDeniedCrowding(t *testing.T) {
	seedCrowdFixture(t)
	for name, p := range crowdProfiles() {
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"search_memory", map[string]any{"query": "crowdword", "limit": 5}},
			{"list_memory", map[string]any{"limit": 3}},
		} {
			subRun(t, name+"/"+call.tool, func(t *testing.T) {
				got, err := callMCPTool(withAgentProfile(testCtx(t), p), call.tool, call.args)
				if err != nil {
					t.Fatal(err)
				}
				body := mustJSON(t, got)
				for _, leak := range []string{"deniedsecret", "mem_denied_", "rawsecret", "gmail_thread/"} {
					if strings.Contains(body, leak) {
						t.Fatalf("leaked %q: %s", leak, body)
					}
				}
				if !strings.Contains(body, "mem_allowed_low") {
					t.Fatalf("the allowed record was crowded out by rows the profile cannot see: %s", body)
				}
			})
		}
	}
}

// seedRichFixture is a vault with one granted authored memory and, around it,
// everything a profile must never learn about: a raw mail thread that shares
// its scope, authored notes in another scope, a correction aimed at the granted
// memory, and a subscribed corpus in another scope.
func seedRichFixture(t *testing.T) Config {
	t.Helper()
	cfg := coreBIngestInitCfg(t)
	for _, m := range []Memory{
		{ID: "mem_granted", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget plan", Text: "richword granted alpha", CreatedAt: "2026-09-01T00:00:00Z"},
		{ID: "gmail_thread/rawA", Scope: "project:widget", Type: "email", Source: "rawA", Provider: "gmail", ProviderID: "rawA", Title: "Raw Mail Title", Text: "richword rawcanary", CreatedAt: "2026-09-02T00:00:00Z"},
		{ID: "mem_personal", Scope: "personal", Type: "fact", Source: "mcp", Title: "Personal Title", Text: "richword personalcanary", CreatedAt: "2026-09-03T00:00:00Z"},
		{ID: "mem_hidden_fix", Scope: "personal", Type: "correction", Source: "mcp", Title: "Hidden fix title", Text: "richword hiddenfixcanary", CreatedAt: "2026-09-04T00:00:00Z", Meta: map[string]any{"target": "mem_granted", "disposition": "outdated"}},
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	subscribeAcme(t, cfg)
	return cfg
}

var richCanaries = []string{"rawcanary", "gmail_thread/rawA", "Raw Mail Title", "personalcanary", "mem_personal", "Personal Title", "hiddenfixcanary", "mem_hidden_fix", "Hidden fix title", "Acme widgets", sharedAcmeID, "acme ships widgets", "project:acme"}

func TestAgentResultsNeverCarryDeniedCanaries(t *testing.T) {
	seedRichFixture(t)
	p := museProfile()
	ctx := withAgentProfile(testCtx(t), p)
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"search_memory", map[string]any{"query": "richword"}},
		{"search_memory", map[string]any{"query": "richword", "confidence": true, "limit": 50}},
		{"search_memory", map[string]any{"query": "acme widgets"}},
		{"search_memory", map[string]any{"query": "richword", "since_hours": 1000000}},
		{"search_memory", map[string]any{"query": "richword", "exclude_dispositions": []any{"outdated"}}},
		{"list_memory", map[string]any{}},
		{"list_memory", map[string]any{"limit": 100}},
		{"list_memory", map[string]any{"event_since_hours": 1000000, "include_authored_writes": true}},
		{"read_memory", map[string]any{"id": "mem_granted"}},
		{"read_memory", map[string]any{"id": "mem_granted", "match": "richword", "max_tokens": 50}},
	}
	for _, c := range calls {
		got, err := callMCPTool(ctx, c.tool, c.args)
		if err != nil {
			// A refusal is fine; a leak is not. The error text is scanned too.
			got = err.Error()
		}
		body := mustJSON(t, got)
		for _, leak := range richCanaries {
			if strings.Contains(body, leak) {
				t.Fatalf("%s %v leaked %q to profile %s: %s", c.tool, c.args, leak, p.Name, body)
			}
		}
	}
	// The granted record is still reachable, so the scan above is not vacuous.
	got, err := callMCPTool(ctx, "search_memory", map[string]any{"query": "richword"})
	if err != nil || !strings.Contains(mustJSON(t, got), "mem_granted") {
		t.Fatalf("granted record missing from search: %v %v", got, err)
	}
	// A subscribed corpus in a granted scope is visible; one outside it is not.
	acme := agentProfile{Name: "acme", ReadScopes: []string{"project:acme"}, Write: mcpWritePolicyReadonly, Tools: []string{"search_memory", "read_memory"}}
	actx := withAgentProfile(testCtx(t), acme)
	got, err = callMCPTool(actx, "search_memory", map[string]any{"query": "acme widgets"})
	if err != nil || !strings.Contains(mustJSON(t, got), sharedAcmeID) {
		t.Fatalf("granted shared corpus not visible: %v %v", got, err)
	}
	got, err = callMCPTool(actx, "search_memory", map[string]any{"query": "richword"})
	if err != nil || strings.Contains(mustJSON(t, got), "mem_granted") {
		t.Fatalf("project:acme profile saw project:widget: %v %v", got, err)
	}
}

// Read by id: a record the profile may not read must answer exactly like an id
// that does not exist, whatever other argument rides along.
func TestAgentReadByIDDenialHasNoDistinguishingContent(t *testing.T) {
	cfg := seedRichFixture(t)
	p := museProfile()
	ctx := withAgentProfile(testCtx(t), p)
	missing := map[string]string{
		"mem_personal":      "mem_20260101_000000_deadbeef",
		"gmail_thread/rawA": "gmail_thread/nope",
		sharedAcmeID:        "mem_20260601_000000_bbbbbbbb",
	}
	extras := []map[string]any{
		{},
		{"evidence_ref": "gmail:seg:1"},
		{"match": "richword", "max_tokens": 20},
		{"max_tokens": 5},
		{"mode": "bounded", "occurrence": 2},
	}
	normalize := func(s, id string) string { return strings.ReplaceAll(s, id, "<ID>") }
	for denied, absent := range missing {
		for _, extra := range extras {
			run := func(id string) (string, string) {
				args := map[string]any{"id": id}
				for k, v := range extra {
					args[k] = v
				}
				got, err := callMCPTool(ctx, "read_memory", args)
				if err == nil {
					return mustJSON(t, got), ""
				}
				return "", err.Error()
			}
			dOK, dErr := run(denied)
			aOK, aErr := run(absent)
			if dOK != "" || aOK != "" {
				t.Fatalf("read_memory(%s, %v) returned content: denied=%s absent=%s", denied, extra, dOK, aOK)
			}
			if normalize(dErr, denied) != normalize(aErr, absent) {
				t.Fatalf("read_memory(%s, %v) is distinguishable from a missing id:\n denied: %s\n absent: %s", denied, extra, dErr, aErr)
			}
		}
	}
	// The receipts must not tell the two cases apart either.
	b, err := os.ReadFile(agentReceiptsPath(cfg, p.Name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "personalcanary") || strings.Contains(string(b), "rawcanary") {
		t.Fatalf("receipts carry denied text: %s", b)
	}
}

// A profile can only reach tools its allowlist names, and a tool added to the
// catalog later reaches no existing profile and no new default.
func TestAgentToolSurfaceNeverWidensByDefault(t *testing.T) {
	if got := strings.Join(agentDefaultTools, ","); got != "search_memory,read_memory,list_memory,write_memory" {
		t.Fatalf("default agent tools changed to %s; a widened default needs an explicit decision", got)
	}
	p := museProfile()
	ctx := withAgentProfile(seedAgentFixtureCtx(t), p)
	allowed := map[string]bool{}
	for _, name := range p.Tools {
		allowed[name] = true
	}
	for _, def := range mcppkg.ToolCatalog() {
		if allowed[def.Name] {
			continue
		}
		if _, err := callMCPTool(ctx, def.Name, map[string]any{"query": "x", "id": "mem_granted", "name": "x"}); err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("unlisted tool %s was not refused: %v", def.Name, err)
		}
		if def.Name != "delete_memory" {
			probe := museProfile()
			probe.Tools = []string{def.Name}
			if err := probe.validate(); err == nil {
				t.Fatalf("a scoped profile may be granted %s, which cannot be filtered per row", def.Name)
			}
		}
	}
	for _, def := range agentToolDefs(p) {
		if !allowed[def.Name] {
			t.Fatalf("tools/list shows %s, which is not on the allowlist", def.Name)
		}
	}
	if _, err := callMCPTool(ctx, "no_such_tool", nil); err == nil {
		t.Fatal("an unknown tool name was accepted")
	}
	// A stored profile that names a tool the binary does not know fails the load.
	bad := museProfile()
	bad.Tools = []string{"search_memory", "future_tool"}
	if err := bad.validate(); err == nil {
		t.Fatal("an unknown tool name passed profile validation")
	}
}

func seedAgentFixtureCtx(t *testing.T) context.Context {
	seedAgentFixture(t)
	return testCtx(t)
}

// Omitted, forged or broader arguments must not widen the boundary.
func TestAgentArgumentsCannotWidenTheProfile(t *testing.T) {
	seedRichFixture(t)
	p := museProfile()
	ctx := withAgentProfile(testCtx(t), p)
	forged := []map[string]any{
		{"query": "richword", "scope": "all"},
		{"query": "richword", "scope": "personal"},
		{"query": "richword", "scope": []any{"personal"}},
		{"query": "richword", "scope": 7},
		{"query": "richword", "source": "gmail"},
		{"query": "richword", "source": []any{"gmail"}},
		{"query": "richword", "source": "filesystem"},
		{"query": "richword", "profile": "owner", "agent": "owner", "scopes": []any{"all"}, "raw_sources": true, "read_scopes": []any{"all"}, "include_raw": true, "project": "personal", "project_dir": "/"},
		{"query": "richword", "limit": 1000000},
		{"query": "richword", "limit": -1},
	}
	for _, args := range forged {
		for _, tool := range []string{"search_memory", "list_memory"} {
			got, err := callMCPTool(ctx, tool, args)
			body := ""
			if err != nil {
				body = err.Error()
			} else {
				body = mustJSON(t, got)
			}
			for _, leak := range richCanaries {
				if strings.Contains(body, leak) {
					t.Fatalf("%s %v leaked %q: %s", tool, args, leak, body)
				}
			}
		}
	}
	// A write cannot name another scope or another author.
	for _, args := range []map[string]any{
		{"title": "t", "text": "x", "scope": "personal"},
		{"title": "t", "text": "x", "scope": "all"},
		{"title": "t", "text": "x", "scope": "project:acme"},
		{"title": "t", "text": "x", "scope": []any{"personal"}},
	} {
		if got, err := callMCPTool(ctx, "write_memory", args); err == nil && proposalIDOf(got) != "" {
			proposal, _, rerr := readMCPWriteProposal(mustConfig(t), proposalIDOf(got))
			if rerr != nil || proposal.Arguments["scope"] != "project:widget" {
				t.Fatalf("write %v staged scope %v (%v), want the profile's own scope", args, proposal.Arguments["scope"], rerr)
			}
		}
	}
	for _, author := range []any{"owner", "mcp-stdio", "agent:owner", nil, 3} {
		got, err := callMCPTool(ctx, "write_memory", map[string]any{"title": "t", "text": "authored by someone else", "source": author})
		if err != nil {
			continue
		}
		proposal, _, rerr := readMCPWriteProposal(mustConfig(t), proposalIDOf(got))
		if rerr != nil || proposal.Arguments["source"] != "agent:muse" {
			t.Fatalf("source %v became %v (%v), want agent:muse", author, proposal.Arguments["source"], rerr)
		}
	}
}

// The transport, not the caller, picks the profile.
func TestAgentHTTPCallerCannotSelectAnotherProfile(t *testing.T) {
	cfg := seedRichFixture(t)
	var out bytes.Buffer
	for _, args := range [][]string{
		{"add", "narrow", "--read-scopes", "project:widget"},
		{"add", "owner", "--read-scopes", "all", "--raw-sources"},
	} {
		if err := cmdAgents(testCtx(t), args, &out, &out); err != nil {
			t.Fatal(err)
		}
	}
	tokens := map[string]string{}
	name := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Created agent profile") {
			name = strings.Fields(line)[3]
			name = strings.Trim(name, `":`)
		}
		if strings.HasPrefix(line, agentTokenPrefix) {
			tokens[name] = strings.TrimSpace(line)
		}
	}
	ctx := testCtx(t)
	srv := newMCPHTTPServer(7780, nil, func() (agentsStore, error) { return loadAgentsStore(cfg) })
	h := srv.handler(ctx)
	call := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"richword","profile":"owner","agent":"owner","scope":"personal"},"_meta":{"profile":"owner"}}}`
	rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", tokens["narrow"], call, "X-Mora-Profile", "owner", "X-Agent-Profile", "owner")
	body := rec.Body.String()
	for _, leak := range richCanaries {
		if strings.Contains(body, leak) {
			t.Fatalf("narrow profile saw %q by naming another profile: %s", leak, body)
		}
	}
	plain := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"richword"}}}`
	if rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", tokens["owner"], plain); !strings.Contains(rec.Body.String(), "personalcanary") || !strings.Contains(rec.Body.String(), "rawcanary") {
		t.Fatalf("control: the owner profile should see the personal note and the raw mail: %s", rec.Body.String())
	}
	// Receipts land under the token's profile only.
	if _, err := os.Stat(agentReceiptsPath(cfg, "owner")); err == nil {
		b, _ := os.ReadFile(agentReceiptsPath(cfg, "owner"))
		if strings.Contains(string(b), `"error"`) || strings.Count(string(b), "\n") != 1 {
			t.Fatalf("narrow's call was logged under owner: %s", b)
		}
	}
}

// Revocation and rotation take effect on the very next request, on both transports.
func TestAgentRevocationAndRotationAreImmediate(t *testing.T) {
	cfg := seedRichFixture(t)
	var out bytes.Buffer
	if err := cmdAgents(testCtx(t), []string{"add", "muse", "--read-scopes", "project:widget"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	token := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, agentTokenPrefix) {
			token = strings.TrimSpace(line)
		}
	}
	ctx := testCtx(t)
	h := newMCPHTTPServer(7780, nil, func() (agentsStore, error) { return loadAgentsStore(cfg) }).handler(ctx)
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"richword"}}}`
	if rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", token, call); rec.Code != 200 {
		t.Fatalf("control call status %d", rec.Code)
	}
	out.Reset()
	if err := cmdAgents(ctx, []string{"rotate", "muse"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", token, call); rec.Code != 401 {
		t.Fatalf("rotated-out token status %d, want 401", rec.Code)
	}
	fresh := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, agentTokenPrefix) {
			fresh = strings.TrimSpace(line)
		}
	}
	if rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", fresh, call); rec.Code != 200 {
		t.Fatalf("new token status %d", rec.Code)
	}
	// A narrowing update applies on the next request too.
	if err := cmdAgents(ctx, []string{"update", "muse", "--tools", "read_memory"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", fresh, call); !strings.Contains(rec.Body.String(), "not available") {
		t.Fatalf("a tool removed from the profile still answered: %s", rec.Body.String())
	}
	if err := cmdAgents(ctx, []string{"revoke", "muse"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", fresh, call); rec.Code != 401 {
		t.Fatalf("revoked token status %d, want 401", rec.Code)
	}
}

// stdioSession drives serveMCPProfile over pipes so a test can change the
// store between two requests of one live session.
type stdioSession struct {
	t   *testing.T
	in  *io.PipeWriter
	out *json.Decoder
}

func startStdioProfile(t *testing.T, name string) (*stdioSession, func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	ctx := testCtx(t)
	go func() {
		done <- serveMCPProfile(ctx, name, outW, inR)
		_ = outW.Close()
	}()
	s := &stdioSession{t: t, in: inW, out: json.NewDecoder(outR)}
	return s, func() {
		_ = inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("stdio session did not stop")
		}
	}
}

func (s *stdioSession) call(id int, method, params string) map[string]any {
	s.t.Helper()
	if _, err := fmt.Fprintf(s.in, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", id, method, params); err != nil {
		s.t.Fatal(err)
	}
	var resp map[string]any
	if err := s.out.Decode(&resp); err != nil {
		s.t.Fatal(err)
	}
	return resp
}

func TestAgentStdioSessionHonorsRevocationAndUpdateMidSession(t *testing.T) {
	seedRichFixture(t)
	var out bytes.Buffer
	if err := cmdAgents(testCtx(t), []string{"add", "muse", "--read-scopes", "project:widget"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	s, stop := startStdioProfile(t, "muse")
	defer stop()
	search := `{"name":"search_memory","arguments":{"query":"richword"}}`
	if body := mustJSON(t, s.call(1, "tools/call", search)); !strings.Contains(body, "mem_granted") {
		t.Fatalf("control call: %s", body)
	}
	if err := cmdAgents(testCtx(t), []string{"update", "muse", "--tools", "read_memory"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if body := mustJSON(t, s.call(2, "tools/call", search)); !strings.Contains(body, "not available") {
		t.Fatalf("`agents update` did not reach the live stdio session: %s", body)
	}
	if err := cmdAgents(testCtx(t), []string{"revoke", "muse"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	read := `{"name":"read_memory","arguments":{"id":"mem_granted"}}`
	body := mustJSON(t, s.call(3, "tools/call", read))
	if strings.Contains(body, "mem_granted") && !strings.Contains(body, "revoked") {
		t.Fatalf("`agents revoke` did not stop a live stdio session: %s", body)
	}
	if strings.Contains(body, "richword granted alpha") {
		t.Fatalf("a revoked profile still read memory text: %s", body)
	}
}

// The text a client is told at initialize must match what dispatch enforces.
func TestAgentInitializeTextMatchesDispatchedWritePolicy(t *testing.T) {
	cfg := seedAgentFixture(t)
	for _, vault := range []string{mcpWritePolicyOpen, mcpWritePolicyPropose, mcpWritePolicyReadonly} {
		for _, write := range []string{mcpWritePolicyOpen, mcpWritePolicyPropose, mcpWritePolicyReadonly} {
			subRun(t, vault+"/"+write, func(t *testing.T) {
				cfg.MCPWritePolicy = vault
				if err := writeConfig(cfg); err != nil { // dispatch and initialize read the stored config
					t.Fatal(err)
				}
				p := museProfile()
				p.Write = write
				ctx := withAgentProfile(testCtx(t), p)
				resp := handleAgentMCP(ctx, p, jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: json.RawMessage(`{}`)})
				instr, _ := resp.Result.(map[string]any)["instructions"].(string)
				before, _ := allMemoryFiles(cfg)
				got, err := callMCPTool(ctx, "write_memory", map[string]any{"title": "policy probe " + vault + write, "text": "probe", "type": "fact"})
				after, _ := allMemoryFiles(cfg)
				proposed := err == nil && proposalIDOf(got) != ""
				wrote := len(after) > len(before)
				// Owner decision: the stricter of vault and profile wins
				// (readonly < propose < open); the expectation is stated
				// here independently of agentWritePolicy.
				effective := write
				if vault == mcpWritePolicyReadonly || (vault == mcpWritePolicyPropose && write == mcpWritePolicyOpen) {
					effective = vault
				}
				switch effective {
				case mcpWritePolicyOpen:
					if !strings.Contains(instr, "you do not need to ask permission") || !wrote || proposed {
						t.Fatalf("effective open: text says open=%v, wrote=%v proposed=%v err=%v", strings.Contains(instr, "you do not need to ask permission"), wrote, proposed, err)
					}
				case mcpWritePolicyPropose:
					if !strings.Contains(instr, "pending proposal queue") || wrote || !proposed {
						t.Fatalf("effective propose: wrote=%v proposed=%v err=%v", wrote, proposed, err)
					}
				case mcpWritePolicyReadonly:
					if !strings.Contains(instr, "read-only") || wrote || proposed || err == nil {
						t.Fatalf("effective readonly: wrote=%v proposed=%v err=%v", wrote, proposed, err)
					}
				}
				for _, other := range []string{"tentative", "tool outside"} {
					if strings.Contains(instr, "saves at once as tentative") && other == "tentative" {
						t.Fatalf("a profile was told it writes tentatively: %s", instr)
					}
				}
			})
		}
	}
}

// Writes keep the writer's name from the call, through approval, to the vault.
func TestAgentWriteAttributionSurvivesOpenAndApproval(t *testing.T) {
	cfg := seedAgentFixture(t)
	open := museProfile()
	open.Write = mcpWritePolicyOpen
	got, err := callMCPTool(withAgentProfile(testCtx(t), open), "write_memory", map[string]any{"title": "Open write", "text": "written directly", "type": "fact", "source": "owner"})
	if err != nil {
		t.Fatal(err)
	}
	mem, _ := got.(map[string]any)["memory"].(map[string]any)
	id, _ := mem["id"].(string)
	if id == "" {
		t.Fatalf("open write returned no memory: %s", mustJSON(t, got))
	}
	stored, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Source != "agent:muse" || stored.Scope != "project:widget" {
		t.Fatalf("open write stored source %q scope %q, want agent:muse in project:widget", stored.Source, stored.Scope)
	}
	// A committed write is visible to the same profile at once, on every read tool.
	for _, probe := range []struct {
		tool string
		args map[string]any
	}{
		{"search_memory", map[string]any{"query": "written directly"}},
		{"list_memory", map[string]any{}},
		{"read_memory", map[string]any{"id": id}},
	} {
		got, err := callMCPTool(withAgentProfile(testCtx(t), open), probe.tool, probe.args)
		if err != nil || !strings.Contains(mustJSON(t, got), id) {
			t.Fatalf("%s does not show the committed write %s: %v %v", probe.tool, id, got, err)
		}
	}
	// The audit line names the writer too.
	logged, _ := os.ReadFile(agentWriteLogPath(cfg))
	if !strings.Contains(string(logged), id) {
		t.Fatalf("agent write log has no line for %s: %s", id, logged)
	}
	if !strings.Contains(string(logged), "muse") {
		t.Fatalf("agent write log does not name the profile: %s", logged)
	}
}

// A receipt records which profile asked for what and which ids it got, never the text.
func TestAgentReceiptsAreContentFree(t *testing.T) {
	cfg := seedRichFixture(t)
	p := museProfile()
	ctx := withAgentProfile(testCtx(t), p)
	_, _ = callMCPTool(ctx, "search_memory", map[string]any{"query": "richword"})
	_, _ = callMCPTool(ctx, "list_memory", map[string]any{})
	_, _ = callMCPTool(ctx, "read_memory", map[string]any{"id": "mem_granted"})
	_, _ = callMCPTool(ctx, "read_memory", map[string]any{"id": "mem_personal"})
	_, _ = callMCPTool(ctx, "write_memory", map[string]any{"title": "SECRETTITLE", "text": "SECRETBODY", "type": "fact"})
	b, err := os.ReadFile(agentReceiptsPath(cfg, p.Name))
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, leak := range append([]string{"richword granted alpha", "Widget plan", "SECRETBODY", "SECRETTITLE", agentTokenPrefix}, richCanaries...) {
		// The call above asked for mem_personal by name, so the receipt may echo that id.
		if leak != "mem_personal" && strings.Contains(log, leak) {
			t.Fatalf("receipt carries %q: %s", leak, log)
		}
	}
	if !strings.Contains(log, "mem_granted") {
		t.Fatalf("receipt lost the returned id: %s", log)
	}
}

// HTTP limits: a body over the cap is refused before it is parsed.
func TestMCPHTTPRefusesOversizedBodies(t *testing.T) {
	h, token := mcpHTTPTestServer(t)
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"` + strings.Repeat("a", mcpMaxRequestBytes+1) + `"}}}`
	if rec := doMCPHTTP(t, h, "POST", "127.0.0.1:7780", token, big); rec.Code != 413 {
		t.Fatalf("oversized body status %d, want 413", rec.Code)
	}
}

// The vector and graph arms rank before the profile's post-filter sees a row,
// so each must apply the grant inside its own query. Fifty-one stronger,
// denied rows fill the 50-row pool of each arm.
func TestAgentGrantAppliesInsideVectorAndGraphArms(t *testing.T) {
	srv := fakeOllama(t, []float64{1, 0, 0, 0})
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	cfg := coreBIngestInitCfg(t)
	if emb, err := chooseEmbedderFor(cfg); err != nil || !embedderIsSemantic(emb) {
		t.Fatalf("fakeOllama should yield a semantic embedder (err=%v)", err)
	}
	person := map[string]any{
		"from":  []string{"zqvethran@example.com"},
		"to":    []string{"adit@x.com"},
		"names": map[string]string{"zqvethran@example.com": "Zqvethran Bolgo"},
	}
	for i := 0; i < 51; i++ {
		for _, m := range []Memory{
			{ID: fmt.Sprintf("mem_aaa_denied_%02d", i), Scope: "project:secret", Type: "fact", Source: "mcp", Title: "unrelated filler", Text: "vector filler deniedsecret", CreatedAt: "2026-09-02T00:00:00Z"},
			{ID: fmt.Sprintf("mem_graph_denied_%02d", i), Scope: "project:secret", Type: "fact", Source: "mcp", Title: "graph filler", Text: "no name here deniedsecret", CreatedAt: fmt.Sprintf("2026-09-%02dT00:00:00Z", (i%27)+1), Meta: person},
		} {
			if err := writeMemory(cfg, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, m := range []Memory{
		{ID: "mem_zzz_allowed", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "vector target", Text: "the vector arm target body", CreatedAt: "2026-07-05T00:00:00Z"},
		{ID: "mem_graph_allowed", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "graph target", Text: "the graph arm target body, also no name here", CreatedAt: "2026-05-01T00:00:00Z", Meta: person},
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// Two scopes, so the transport cannot pin a single scope for the arms.
	ctx := withAgentProfile(testCtx(t), crowdProfiles()["multi-scope"])
	for query, want := range map[string]string{"qzxvecarmprobe": "mem_zzz_allowed", "Zqvethran Bolgo": "mem_graph_allowed"} {
		got, err := callMCPTool(ctx, "search_memory", map[string]any{"query": query, "limit": 10})
		if err != nil {
			t.Fatal(err)
		}
		body := mustJSON(t, got)
		if strings.Contains(body, "deniedsecret") || strings.Contains(body, "denied_") {
			t.Fatalf("%q leaked a denied row: %s", query, body)
		}
		if !strings.Contains(body, want) {
			t.Fatalf("%q: %s was crowded out of the arm's pool by denied rows: %s", query, want, body)
		}
	}
}

// Owner decision: rotate never un-revokes. A revoked profile stays revoked and
// the user is told to add a new one.
func TestAgentsRotateRefusesRevokedProfile(t *testing.T) {
	cfg := seedRichFixture(t)
	var out bytes.Buffer
	for _, args := range [][]string{{"add", "muse", "--read-scopes", "project:widget"}, {"revoke", "muse"}} {
		if err := cmdAgents(testCtx(t), args, &out, &out); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := loadAgentsStore(cfg)
	out.Reset()
	err := cmdAgents(testCtx(t), []string{"rotate", "muse"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "agents add") {
		t.Fatalf("rotate of a revoked profile error = %v", err)
	}
	if strings.Contains(out.String(), agentTokenPrefix) {
		t.Fatalf("rotate printed a token for a revoked profile: %s", out.String())
	}
	after, _ := loadAgentsStore(cfg)
	if after.Profiles[0].RevokedAt == "" || after.Profiles[0].TokenSHA256 != before.Profiles[0].TokenSHA256 {
		t.Fatalf("rotate changed a revoked profile: %+v", after.Profiles[0])
	}
}
