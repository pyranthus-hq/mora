package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/eywa"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// eywaFake is a minimal in-process agent MCP server for the Mora-level tests:
// a client_credentials token endpoint and a JSON-only streamable HTTP endpoint.
type eywaFake struct {
	srv        *httptest.Server
	mu         sync.Mutex
	tokenCode  int
	calls      []string
	remembered []map[string]any
	writes     map[string]string // write_id -> status
	changes    []map[string]any
	versions   map[string]map[string]any // version_id -> version
	pages      map[string]map[string]any // cursor -> changes_since page (overrides the default)
	failFact   map[string]int            // fact_id -> get_memory lookups that fail before one succeeds
	remHook    func()                    // runs (unlocked) before each remember call is answered
	polled     []string                  // write ids read with get_memory(write_id)
}

func newEywaFake(t *testing.T) *eywaFake {
	f := &eywaFake{writes: map[string]string{}, versions: map[string]map[string]any{},
		pages: map[string]map[string]any{}, failFact: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.tokenCode != 0 {
			w.WriteHeader(f.tokenCode)
			return
		}
		if id, secret, ok := r.BasicAuth(); !ok || id != "client-a" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`)
	})
	mux.HandleFunc("/mcp", f.mcp)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	orig := eywaHTTPClientFn
	origSleep := eywaSleepFn
	eywaHTTPClientFn = func() *http.Client { return f.srv.Client() }
	eywaSleepFn = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { eywaHTTPClientFn, eywaSleepFn = orig, origSleep })
	t.Setenv(eywa.SecretEnv, "s3cret")
	return f
}

func (f *eywaFake) mcp(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hook := f.remHook
	f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if hook != nil && strings.Contains(string(body), "___remember") {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	switch req.Method {
	case "initialize":
		reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		reply(map[string]any{"tools": []map[string]any{{"name": "t___remember"}, {"name": "t___get_memory"}, {"name": "t___history"}, {"name": "t___changes_since"}}})
	case "tools/call":
		tool := strings.TrimPrefix(req.Params.Name, "t___")
		f.calls = append(f.calls, tool)
		out := f.tool(tool, req.Params.Arguments)
		b, _ := json.Marshal(out)
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}})
	}
}

func (f *eywaFake) tool(name string, a map[string]any) any {
	switch name {
	case "changes_since":
		if c, ok := a["cursor"].(string); ok && f.pages[c] != nil {
			return f.pages[c]
		}
		if _, ok := a["cursor"]; ok {
			return map[string]any{"items": []any{}, "next": "cur-2", "has_more": false}
		}
		return map[string]any{"items": f.changes, "next": "cur-1", "has_more": false}
	case "get_memory":
		if id, ok := a["write_id"].(string); ok {
			f.polled = append(f.polled, id)
			st := f.writes[id]
			if st == "received" {
				f.writes[id] = "in_review" // decided on the next read
			}
			return map[string]any{"write": map[string]any{"write_id": id, "status": st, "reason": nil}}
		}
		if id, ok := a["fact_id"].(string); ok && f.failFact[id] > 0 {
			f.failFact[id]--
			return map[string]any{"eywa_tool_error": map[string]any{"code": "internal", "message": "lookup failed"}}
		}
		for _, v := range f.versions {
			if v["status"] == "current" && (v["fact_id"] == a["fact_id"] || (v["entity"] == a["entity"] && v["attribute"] == a["attribute"])) {
				return map[string]any{"version": v}
			}
		}
		return map[string]any{"eywa_error": map[string]any{"code": -32001, "message": "Forbidden"}}
	case "history":
		var vs []map[string]any
		for _, v := range f.versions {
			if v["entity"] == a["entity"] {
				vs = append(vs, map[string]any{"version_id": v["version_id"], "attribute": v["attribute"], "value": v["value"], "status": v["status"],
					"valid_from": v["valid_from"], "valid_to": nil, "recorded_at": v["recorded_at"], "retired_at": v["retired_at"], "source_uri": "chat://export-1/msg-9"})
			}
		}
		return map[string]any{"entity": a["entity"], "versions": vs}
	case "remember":
		f.remembered = append(f.remembered, a)
		id := fmt.Sprintf("wrt_%d", len(f.remembered))
		f.writes[id] = "received"
		return map[string]any{"write_id": id, "status": "received"}
	}
	return map[string]any{}
}

func (f *eywaFake) count(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == tool {
			n++
		}
	}
	return n
}

func (f *eywaFake) connectArgs(extra ...string) []string {
	return append([]string{"connect", "eywa", "--endpoint", f.srv.URL + "/mcp", "--token-url", f.srv.URL + "/token", "--client-id", "client-a"}, extra...)
}

func TestEywaCatalogStatusPathAndHelp(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	if got, want := syncStatusPathFor(cfg, Source{Name: "eywa", Type: "eywa"}), filepath.Join(cfg.StateDir, "sync", "eywa-eywa.json"); got != want {
		t.Fatalf("status path = %q, want %q", got, want)
	}
	c, ok := lookupCatalog("eywa")
	if !ok || !c.Ingesting || c.NeedsAuth {
		t.Fatalf("eywa catalog entry = %+v, %v", c, ok)
	}
	if got := memory.DeriveProvenance(memory.Memory{ID: "eywa_fact/ver_1", Provider: "eywa"}); got != memory.ProvenanceEvidence {
		t.Fatalf("eywa provenance = %q, want evidence", got)
	}
	var out bytes.Buffer
	printUsage(&out)
	for _, want := range []string{"mora connect eywa", "mora sync eywa", "mora eywa push"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("top-level help omits %q", want)
		}
	}
}

func TestEywaEnableNeedsConfiguration(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	out := run(t, "connectors", "enable", "eywa")
	if !strings.Contains(out, "mora connect eywa --endpoint") {
		t.Fatalf("enable without config should point at connect:\n%s", out)
	}
	sources, err := loadSources(mustConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		if s.Type == "eywa" {
			t.Fatalf("enable without config minted a source row: %+v", s)
		}
	}
}

func TestEywaConnectRejectsPlainHTTPAndNeverStoresSecret(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if _, err := runErr(t, "connect", "eywa", "--endpoint", "http://eywa.example.com/mcp", "--token-url", "https://auth.example.com/token", "--client-id", "c"); err == nil {
		t.Fatal("a non-loopback http endpoint must be refused")
	}
	f := newEywaFake(t)
	run(t, f.connectArgs("--no-sync")...)
	cfg := mustConfig(t)
	b, err := os.ReadFile(filepath.Join(cfg.ConfigDir, "sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cret") {
		t.Fatal("sources.json contains the client secret")
	}
	if !strings.Contains(string(b), `"client_id": "client-a"`) && !strings.Contains(string(b), `"client_id":"client-a"`) {
		t.Fatalf("sources.json lacks the eywa settings:\n%s", b)
	}
}

func TestEywaConnectThenSyncWritesReadOnlyFacts(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	f := newEywaFake(t)
	f.versions["ver_1"] = map[string]any{"version_id": "ver_1", "fact_id": "fct_1", "entity": "Acme Corp", "attribute": "payment terms",
		"value": "net 45", "topic": "payments", "visibility": "tenant", "status": "current", "valid_from": "2026-08-01T00:00:00Z",
		"recorded_at": "2026-08-01T00:00:00Z", "retired_at": nil,
		"lineage": map[string]any{"source_uri": "chat://export-1/msg-9", "content_hash": strings.Repeat("b", 64), "signer": "usr_1", "excerpt": "Acme moves to net 45"}}
	f.changes = []map[string]any{{"type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_1", "version_id": "ver_1"}}}
	out := run(t, f.connectArgs()...)
	if !strings.Contains(out, "synced 1 fact record(s)") {
		t.Fatalf("connect output:\n%s", out)
	}
	cfg := mustConfig(t)
	path := filepath.Join(sourcesRoot(cfg), "eywa", memory.SafeFilename("eywa_fact/ver_1")+".md")
	m, err := parseMemory(path)
	if err != nil {
		t.Fatalf("pulled record missing at %s: %v", path, err)
	}
	if m.ID != "eywa_fact/ver_1" || m.Provider != "eywa" || m.Meta["status"] != "current" || !strings.Contains(m.Text, "Citation: chat://export-1/msg-9 @ ver_1") {
		t.Fatalf("record = %+v", m)
	}
	st, err := memory.LoadStatus(syncStatusPathFor(cfg, Source{Name: "eywa", Type: "eywa"}))
	if err != nil || st.IncrementalCursor != "cur-1" || st.LastSuccessAt == "" {
		t.Fatalf("sync status = %+v, %v; want cursor cur-1 in the state dir", st, err)
	}
	if strings.HasPrefix(syncStatusPathFor(cfg, Source{Name: "eywa", Type: "eywa"}), cfg.VaultDir) {
		t.Fatal("the cursor must live in the state dir, never the vault")
	}
	// The next sync resumes from the cursor and writes nothing new.
	out = run(t, "sync", "eywa")
	if !strings.Contains(out, "synced 0 fact record(s)") {
		t.Fatalf("resync output:\n%s", out)
	}
}

func TestEywaSyncSurfacesAuthFailureAsUnauthorized(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	f := newEywaFake(t)
	run(t, f.connectArgs("--no-sync")...)
	f.tokenCode = http.StatusUnauthorized
	out, err := runErr(t, "sync", "eywa")
	if err == nil {
		t.Fatalf("sync with refused credentials succeeded:\n%s", out)
	}
	if strings.Contains(out+err.Error(), "s3cret") {
		t.Fatal("the secret leaked into output")
	}
	st, _ := memory.LoadStatus(syncStatusPathFor(mustConfig(t), Source{Name: "eywa", Type: "eywa"}))
	if st == nil || st.ErrorCode != errCodeConnectorUnauthorized || !strings.Contains(st.LastError, "token endpoint refused") {
		t.Fatalf("status = %+v, want connector.unauthorized with an honest message", st)
	}
}

func writeAuthored(t *testing.T, scope, title, text string) string {
	t.Helper()
	out := run(t, "write", "--scope", scope, "--type", "note", "--title", title, "--text", text, "--json")
	var env struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	id := env.ID
	if id == "" {
		t.Fatalf("could not read the written id from:\n%s", out)
	}
	return id
}

// eywaDestRecords returns the ledger records of the configured destination.
func eywaDestRecords(t *testing.T) map[string]eywaPushRecord {
	t.Helper()
	cfg := mustConfig(t)
	st, err := loadEywaPushState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadEywaSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d := st.Destinations[eywaDestinationKey(s.Eywa)]; d != nil {
		return d.Records
	}
	return map[string]eywaPushRecord{}
}

func setupEywaPush(t *testing.T) *eywaFake {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	f := newEywaFake(t)
	run(t, f.connectArgs("--no-sync", "--agent-id", "agt_TEST", "--topic", "payments")...)
	return f
}

func TestEywaPushEnableIsExplicit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	f := newEywaFake(t)
	run(t, f.connectArgs("--no-sync")...)
	if _, err := runErr(t, "eywa", "push", "enable"); err == nil {
		t.Fatal("push enable without agent id and topic must be refused")
	}
	run(t, f.connectArgs("--no-sync", "--agent-id", "agt_TEST", "--topic", "payments")...)
	s, err := loadEywaSource(mustConfig(t))
	if err != nil || s.Eywa.PushEnabled {
		t.Fatalf("push must be off by default: %+v %v", s.Eywa, err)
	}
	run(t, "eywa", "push", "enable")
	run(t, f.connectArgs("--no-sync", "--agent-id", "agt_TEST", "--topic", "payments")...)
	if s, _ := loadEywaSource(mustConfig(t)); !s.Eywa.PushEnabled {
		t.Fatal("re-running connect must not silently turn push off")
	}
	run(t, "eywa", "push", "disable")
	if s, _ := loadEywaSource(mustConfig(t)); s.Eywa.PushEnabled {
		t.Fatal("push disable did not stick")
	}
}

func TestEywaPushRefusesWhenDisabledOrUnconfirmed(t *testing.T) {
	f := setupEywaPush(t)
	writeAuthored(t, "project:acme", "Acme terms", "Acme Corp pays on net 45.")
	if _, err := runErr(t, "eywa", "push", "--scope", "project:acme", "--yes"); err == nil || !strings.Contains(err.Error(), "opt-in") {
		t.Fatalf("push while disabled = %v, want opt-in refusal", err)
	}
	run(t, "eywa", "push", "enable")
	out, err := runErr(t, "eywa", "push", "--scope", "project:acme")
	if err == nil || !strings.Contains(err.Error(), "without --yes") {
		t.Fatalf("non-interactive push without --yes = %v, want refusal", err)
	}
	if !strings.Contains(out, "1 memory update(s) would leave this machine") {
		t.Fatalf("the refusal must come after the preview:\n%s", out)
	}
	if f.count("remember") != 0 {
		t.Fatal("a refused push sent data")
	}
}

func TestEywaPreviewSendsNothingAndExcludesConnectorRecords(t *testing.T) {
	f := setupEywaPush(t)
	id := writeAuthored(t, "global", "Acme terms", "Acme Corp pays on net 45.")
	// A pulled fact in the same scope must never echo back.
	f.versions["ver_1"] = map[string]any{"version_id": "ver_1", "fact_id": "fct_1", "entity": "Acme Corp", "attribute": "owner",
		"value": "Riya", "status": "current", "valid_from": "2026-08-01T00:00:00Z", "recorded_at": "2026-08-01T00:00:00Z", "retired_at": nil,
		"lineage": map[string]any{"source_uri": "chat://export-1/msg-9"}}
	f.changes = []map[string]any{{"type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_1", "version_id": "ver_1"}}}
	run(t, "sync", "eywa")
	out := run(t, "eywa", "preview", "--scope", "global")
	if !strings.Contains(out, id) || !strings.Contains(out, "| Acme terms") || !strings.Contains(out, "eywa-run://agt_TEST/mora/"+id) {
		t.Fatalf("preview lacks the exact update:\n%s", out)
	}
	if strings.Contains(out, "eywa_fact") {
		t.Fatalf("preview offers a pulled connector record for push:\n%s", out)
	}
	if !strings.Contains(out, "push is disabled") || !strings.Contains(out, "curator review") {
		t.Fatalf("preview must say push is off and that the service governs updates:\n%s", out)
	}
	if f.count("remember") != 0 {
		t.Fatal("preview sent data")
	}
}

func TestEywaPushPreviewConfirmSendsLineageAndRecordsInReview(t *testing.T) {
	f := setupEywaPush(t)
	id := writeAuthored(t, "project:acme", "Acme terms", "Acme Corp pays on net 45.")
	writeAuthored(t, "personal", "Private", "not in the pushed scope")
	run(t, "eywa", "push", "enable")
	confirmed := 0
	orig := confirmEywaPushFn
	confirmEywaPushFn = func(io.Reader, io.Writer, string, int) error { confirmed++; return nil }
	t.Cleanup(func() { confirmEywaPushFn = orig })

	var stdout, stderr bytes.Buffer
	if err := Run(testCtx(t), []string{"eywa", "push", "--scope", "project:acme", "--json"}, &stdout, &stderr, strings.NewReader("")); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr.String())
	}
	if confirmed != 1 {
		t.Fatalf("confirm gate ran %d times, want 1", confirmed)
	}
	if !strings.Contains(stderr.String(), "would leave this machine") || !strings.Contains(stderr.String(), "curator review") {
		t.Fatalf("human preview missing on stderr:\n%s", stderr.String())
	}
	var env struct {
		Schema string `json:"schema"`
		eywaPushReceipt
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout.String())
	}
	if env.Schema != "mora.eywa.push" || env.Sent != 1 || len(env.Updates) != 1 || env.Updates[0].Status != "in_review" {
		t.Fatalf("receipt = %+v", env)
	}
	if len(f.remembered) != 1 {
		t.Fatalf("remember calls = %d, want only the scoped memory", len(f.remembered))
	}
	sent := f.remembered[0]
	lin, _ := sent["lineage"].(map[string]any)
	text, _ := sent["text"].(string)
	if sent["topic"] != "payments" || !strings.Contains(text, "net 45") || lin["source_uri"] != "eywa-run://agt_TEST/mora/"+id ||
		lin["excerpt"] != text || lin["content_hash"] != eywa.ContentHash(text) {
		t.Fatalf("remember arguments = %+v", sent)
	}
	rec := eywaDestRecords(t)[id]
	if rec.WriteID != "wrt_1" || rec.Status != "in_review" || rec.ContentHash != eywa.ContentHash(text) {
		t.Fatalf("push state = %+v", rec)
	}
	raw, _ := os.ReadFile(eywaPushStatePath(mustConfig(t)))
	if strings.Contains(string(raw), "net 45") {
		t.Fatal("push state must hold ids and hashes, never memory text")
	}
	// Unchanged memories are not re-sent.
	out := run(t, "eywa", "push", "--scope", "project:acme", "--yes")
	if !strings.Contains(out, "nothing to push") || len(f.remembered) != 1 {
		t.Fatalf("an unchanged memory was re-sent:\n%s", out)
	}
}

func TestEywaStatusRepollsPendingUpdates(t *testing.T) {
	f := setupEywaPush(t)
	id := writeAuthored(t, "global", "Acme terms", "Acme Corp pays on net 45.")
	run(t, "eywa", "push", "enable")
	pushText := run(t, "eywa", "push", "--scope", "global", "--yes", "--wait", "0s")
	if !strings.Contains(pushText, "received; not decided yet") || !strings.Contains(pushText, "mora eywa status") {
		t.Fatalf("a push that outlives --wait must say the outcome is pending:\n%s", pushText)
	}
	if recs := eywaDestRecords(t); recs[id].Status != "received" {
		t.Fatalf("status after push = %+v", recs[id])
	}
	out := run(t, "eywa", "status", "--json")
	var env struct {
		Schema string `json:"schema"`
		eywaPushReceipt
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if env.Schema != "mora.eywa.status" || len(env.Updates) != 1 || env.Updates[0].Status != "in_review" || env.Pending != 0 {
		t.Fatalf("status receipt = %+v", env)
	}
	if f.count("get_memory") != 2 {
		t.Fatalf("get_memory(write_id) calls = %d, want one per read", f.count("get_memory"))
	}
	// A decided update is not polled again.
	run(t, "eywa", "status")
	if f.count("get_memory") != 2 {
		t.Fatal("status re-polled an update that already had a final status")
	}
}

func TestEywaPushStopsOnAuthFailureAndKeepsState(t *testing.T) {
	f := setupEywaPush(t)
	writeAuthored(t, "global", "One", "Acme Corp pays on net 45.")
	run(t, "eywa", "push", "enable")
	f.tokenCode = http.StatusUnauthorized
	_, err := runErr(t, "eywa", "push", "--scope", "global", "--yes")
	if err == nil || !strings.Contains(err.Error(), "token endpoint refused") {
		t.Fatalf("push with refused credentials = %v", err)
	}
	if len(f.remembered) != 0 {
		t.Fatal("nothing should have been sent")
	}
	f.tokenCode = 0
	out := run(t, "eywa", "push", "--scope", "global", "--yes")
	if !strings.Contains(out, "sent to curator review") || len(f.remembered) != 1 {
		t.Fatalf("the retry after an auth failure must send the pending update:\n%s", out)
	}
}

func eywaVersion(id, fact, value, status string) map[string]any {
	return map[string]any{"version_id": id, "fact_id": fact, "entity": "Acme Corp", "attribute": "payment terms",
		"value": value, "topic": "payments", "visibility": "tenant", "status": status, "valid_from": "2026-08-01T00:00:00Z",
		"recorded_at": "2026-08-01T00:00:00Z", "retired_at": nil,
		"lineage": map[string]any{"source_uri": "chat://export-1/msg-9", "content_hash": strings.Repeat("b", 64), "signer": "usr_1", "excerpt": "Acme terms"}}
}

// A fact lookup that fails once must not let the feed position move past the
// failed change: the next sync retries it and the fact ends present and current.
func TestEywaSyncKeepsCursorWhenARecordFailsAndRetriesIt(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	f := newEywaFake(t)
	f.versions["ver_1"] = eywaVersion("ver_1", "fct_1", "net 45", "current")
	f.changes = []map[string]any{{"type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_1", "version_id": "ver_1"}}}
	run(t, f.connectArgs()...)
	cfg := mustConfig(t)
	statusPath := syncStatusPathFor(cfg, Source{Name: "eywa", Type: "eywa"})
	if st, _ := memory.LoadStatus(statusPath); st.IncrementalCursor != "cur-1" {
		t.Fatalf("cursor after the first clean sync = %q, want cur-1", st.IncrementalCursor)
	}

	// The fact changes value; the next page's lookup fails once.
	f.mu.Lock()
	f.versions["ver_1"]["status"] = "retired"
	f.versions["ver_2"] = eywaVersion("ver_2", "fct_1", "net 60", "current")
	f.pages["cur-1"] = map[string]any{"items": []map[string]any{{"type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_1", "version_id": "ver_2"}}},
		"next": "cur-2", "has_more": false}
	f.failFact["fct_1"] = 1
	f.mu.Unlock()

	out, err := runErr(t, "sync", "eywa")
	if err == nil {
		t.Fatalf("a sync with a failed record reported success:\n%s", out)
	}
	st, _ := memory.LoadStatus(statusPath)
	if st.IncrementalCursor != "cur-1" || st.Checkpoint != "" || st.LastError == "" {
		t.Fatalf("status after the failed record = %+v; want cursor held at cur-1 and the error surfaced", st)
	}
	ver2 := filepath.Join(sourcesRoot(cfg), "eywa", memory.SafeFilename("eywa_fact/ver_2")+".md")
	if _, err := os.Stat(ver2); err == nil {
		t.Fatal("the failed version was written")
	}

	run(t, "sync", "eywa")
	st, _ = memory.LoadStatus(statusPath)
	if st.IncrementalCursor != "cur-2" || st.LastError != "" {
		t.Fatalf("status after the retry = %+v; want cursor cur-2 and no error", st)
	}
	m, err := parseMemory(ver2)
	if err != nil || m.Meta["status"] != "current" || m.Meta["value"] != "net 60" {
		t.Fatalf("retried fact = %+v, %v; want present and current", m, err)
	}
	old, err := parseMemory(filepath.Join(sourcesRoot(cfg), "eywa", memory.SafeFilename("eywa_fact/ver_1")+".md"))
	if err != nil || old.Meta["status"] != "replaced" {
		t.Fatalf("earlier version = %+v, %v; want replaced", old, err)
	}
}

// A failure on one page pins a mid-walk checkpoint to that page's start, so a
// later page cannot publish a position past the failed change.
func TestEywaPageTrackerHoldsCheckpointAtFirstFailedPage(t *testing.T) {
	tr := &eywaPageTracker{pageStart: "cur-1"}
	tr.recordFailure()
	tr.pageStart = "cur-5"
	tr.recordFailure()
	mid := &memory.SyncStatus{Checkpoint: "cur-6", CursorMode: memory.CursorModeHistory, IncrementalCursor: "cur-1", PendingSyncCursor: "cur-9"}
	tr.hold(mid)
	if mid.Checkpoint != "cur-1" || mid.PendingSyncCursor != "" || mid.CursorMode != memory.CursorModeHistory {
		t.Fatalf("mid-walk status = %+v; want checkpoint held at cur-1", mid)
	}
	done := &memory.SyncStatus{IncrementalCursor: "cur-9"}
	tr.hold(done)
	if done.IncrementalCursor != "cur-1" {
		t.Fatalf("completed status = %+v; want cursor held at cur-1", done)
	}
	clean := &eywaPageTracker{pageStart: "cur-1"}
	ok := &memory.SyncStatus{IncrementalCursor: "cur-9"}
	clean.hold(ok)
	if ok.IncrementalCursor != "cur-9" {
		t.Fatal("a clean run must advance the cursor")
	}
}

func (f *eywaFake) remembers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.remembered)
}

func (f *eywaFake) polledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.polled...)
}

func TestEywaDestinationKeyNormalizesEndpoint(t *testing.T) {
	a := &memory.EywaSettings{Endpoint: "HTTPS://Eywa.Example.com:443/mcp/", ClientID: "c", AgentID: "a", Topic: "t"}
	b := &memory.EywaSettings{Endpoint: "https://eywa.example.com/mcp", ClientID: "c", AgentID: "a", Topic: "t"}
	if eywaDestinationKey(a) != eywaDestinationKey(b) {
		t.Fatalf("equivalent endpoints got different keys: %q vs %q", normalizeEywaEndpoint(a.Endpoint), normalizeEywaEndpoint(b.Endpoint))
	}
	for _, change := range []func(*memory.EywaSettings){
		func(e *memory.EywaSettings) { e.Endpoint = "https://other.example.com/mcp" },
		func(e *memory.EywaSettings) { e.ClientID = "c2" },
		func(e *memory.EywaSettings) { e.AgentID = "a2" },
		func(e *memory.EywaSettings) { e.Topic = "t2" },
	} {
		c := *b
		change(&c)
		if eywaDestinationKey(&c) == eywaDestinationKey(b) {
			t.Fatalf("destination %+v shares a ledger with %+v", c, *b)
		}
	}
}

// After a successful push, a new topic or a new endpoint is a new destination:
// the memory is sent again, and the old destination's pending write ids are
// never polled against the new service.
func TestEywaPushReconfiguredDestinationSendsAgainAndNeverPollsOldIDs(t *testing.T) {
	a := setupEywaPush(t)
	id := writeAuthored(t, "global", "Acme terms", "Acme Corp pays on net 45.")
	run(t, "eywa", "push", "enable")
	run(t, "eywa", "push", "--scope", "global", "--yes", "--wait", "0s") // wrt_1 stays "received" at a
	if a.remembers() != 1 {
		t.Fatalf("first push sent %d update(s)", a.remembers())
	}
	if out := run(t, "eywa", "push", "--scope", "global", "--yes"); !strings.Contains(out, "nothing to push") {
		t.Fatalf("same destination must deduplicate:\n%s", out)
	}

	// New topic, same service: the update goes again.
	run(t, a.connectArgs("--no-sync", "--agent-id", "agt_TEST", "--topic", "billing")...)
	run(t, "eywa", "push", "--scope", "global", "--yes", "--wait", "0s")
	if a.remembers() != 2 || a.remembered[1]["topic"] != "billing" {
		t.Fatalf("a new topic must receive the update; remember calls = %d", a.remembers())
	}

	// New service: nothing pending at a may be polled against b.
	polledAtA := len(a.polledIDs())
	b := newEywaFake(t)
	run(t, b.connectArgs("--no-sync", "--agent-id", "agt_TEST", "--topic", "payments")...)
	out := run(t, "eywa", "status")
	if len(b.polledIDs()) != 0 {
		t.Fatalf("status polled old write ids against the new service: %v", b.polledIDs())
	}
	if !strings.Contains(out, "No updates have been pushed to the configured destination") || !strings.Contains(out, "2 update(s) were pushed to a different") {
		t.Fatalf("status must say the old updates are not re-checked:\n%s", out)
	}
	run(t, "eywa", "push", "--scope", "global", "--yes", "--wait", "0s")
	if b.remembers() != 1 {
		t.Fatalf("the new service must receive the update; remember calls = %d", b.remembers())
	}
	run(t, "eywa", "status")
	for _, w := range b.polledIDs() {
		if w != "wrt_1" { // b's own first write id
			t.Fatalf("b was polled for %q", w)
		}
	}
	if got := eywaDestRecords(t)[id]; got.WriteID != "wrt_1" || got.Status != "in_review" {
		t.Fatalf("new destination record = %+v", got)
	}
	if got := a.polledIDs(); len(got) != polledAtA {
		t.Fatalf("a was polled after it stopped being the destination: %v", got[polledAtA:])
	}
}

// A ledger written before destinations were tracked never suppresses a send
// and its write ids are never polled: their destination is unknown.
func TestEywaLegacyPushLedgerNeverSuppressesOrPolls(t *testing.T) {
	f := setupEywaPush(t)
	id := writeAuthored(t, "global", "Acme terms", "Acme Corp pays on net 45.")
	run(t, "eywa", "push", "enable")
	cfg := mustConfig(t)
	u, err := eywa.BuildUpdate("agt_TEST", id, "Acme terms", "Acme Corp pays on net 45.")
	if err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf(`{"records":{%q:{"content_hash":%q,"source_uri":%q,"write_id":"wrt_legacy","status":"received"}}}`, id, u.ContentHash, u.SourceURI)
	if err := os.MkdirAll(filepath.Dir(eywaPushStatePath(cfg)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eywaPushStatePath(cfg), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, "eywa", "status")
	if !strings.Contains(out, "1 update(s) were pushed to a different") || len(f.polledIDs()) != 0 {
		t.Fatalf("legacy write ids must be reported, not polled (polled %v):\n%s", f.polledIDs(), out)
	}
	run(t, "eywa", "push", "--scope", "global", "--yes")
	if f.remembers() != 1 {
		t.Fatalf("a legacy record suppressed the send; remember calls = %d", f.remembers())
	}
	for _, w := range f.polledIDs() {
		if w == "wrt_legacy" {
			t.Fatal("a legacy write id was polled")
		}
	}
	st, err := loadEywaPushState(cfg)
	if err != nil || st.Legacy[id].WriteID != "wrt_legacy" {
		t.Fatalf("legacy record must be kept for the record: %+v, %v", st.Legacy, err)
	}
}

// Overlapping pushes share one ledger lock: no write id is lost and no update
// is sent twice.
func TestEywaConcurrentPushesLoseNoWriteIDsAndSendNoDuplicates(t *testing.T) {
	f := setupEywaPush(t)
	idA := writeAuthored(t, "project:a", "A terms", "A pays on net 30.")
	idB := writeAuthored(t, "project:b", "B terms", "B pays on net 60.")
	run(t, "eywa", "push", "enable")
	// Widen the window in which two unserialized pushes would interleave.
	f.mu.Lock()
	f.remHook = func() { time.Sleep(30 * time.Millisecond) }
	f.mu.Unlock()
	ctx := testCtx(t)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		scope := "project:a"
		if i%2 == 1 {
			scope = "project:b"
		}
		wg.Add(1)
		go func(i int, scope string) {
			defer wg.Done()
			var out, errOut bytes.Buffer
			args := []string{"eywa", "push", "--scope", scope, "--yes"}
			if i >= 4 {
				args = []string{"eywa", "status"}
			}
			errs[i] = Run(ctx, args, &out, &errOut, strings.NewReader(""))
		}(i, scope)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	if n := f.remembers(); n != 2 {
		t.Fatalf("remember calls = %d, want exactly one per memory", n)
	}
	recs := eywaDestRecords(t)
	if recs[idA].WriteID == "" || recs[idB].WriteID == "" || recs[idA].WriteID == recs[idB].WriteID {
		t.Fatalf("ledger lost a write id: %+v", recs)
	}
}

func TestEywaPushLockTimesOutClearlyAndSurvivesALeftoverGuard(t *testing.T) {
	setupEywaPush(t)
	cfg := mustConfig(t)
	orig := eywaPushLockTimeout
	eywaPushLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { eywaPushLockTimeout = orig })
	release, err := acquireEywaPushLock(testCtx(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireEywaPushLock(testCtx(t), cfg); err == nil || !strings.Contains(err.Error(), "another `mora eywa push` or `mora eywa status`") {
		t.Fatalf("a held lock = %v, want a clear timeout", err)
	}
	if _, err := runErr(t, "eywa", "status"); err == nil || !strings.Contains(err.Error(), "push ledger") {
		t.Fatalf("status under a held lock = %v, want the same timeout", err)
	}
	release()
	// The guard file stays behind, as it does after a crash; it is not a lock.
	if _, err := os.Stat(leaseGuardPath(eywaPushLockPath(cfg))); err != nil {
		t.Fatalf("guard file: %v", err)
	}
	again, err := acquireEywaPushLock(testCtx(t), cfg)
	if err != nil {
		t.Fatalf("a leftover guard file blocked the lock: %v", err)
	}
	again()
}

func eywaPushJSON(t *testing.T, scope string) (eywaPushReceipt, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(testCtx(t), []string{"eywa", "push", "--scope", scope, "--yes", "--json"}, &stdout, &stderr, strings.NewReader(""))
	var env struct {
		Schema string `json:"schema"`
		eywaPushReceipt
	}
	if uerr := json.Unmarshal(stdout.Bytes(), &env); uerr != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s\n%s", uerr, stdout.String(), stderr.String())
	}
	return env.eywaPushReceipt, stdout.String(), err
}

func TestEywaPushReportsLocallyRefusedUpdatesAsFailures(t *testing.T) {
	f := setupEywaPush(t)
	long := strings.Repeat("Acme overflow clause. ", eywa.MaxUpdateChars/20)
	bigID := writeAuthored(t, "global", "Too long", long)
	run(t, "eywa", "push", "enable")

	// All refused: nothing is sent and the run fails.
	r, raw, err := eywaPushJSON(t, "global")
	if err == nil || !strings.Contains(err.Error(), "refused locally") || !strings.Contains(err.Error(), bigID) {
		t.Fatalf("an all-refused push = %v, want a nonzero result naming the id", err)
	}
	if r.Sent != 0 || r.Refused != 1 || r.Failed != 1 || len(r.Updates) != 1 ||
		r.Updates[0].MemoryID != bigID || r.Updates[0].Status != "refused" || r.Updates[0].Reason != "too_long" {
		t.Fatalf("all-refused receipt = %+v", r)
	}
	if strings.Contains(raw, "overflow clause") || f.remembers() != 0 {
		t.Fatalf("a refused update leaked content or was sent:\n%s", raw)
	}

	// Mixed: the valid update is sent, the refused one is still a failure.
	okID := writeAuthored(t, "global", "Acme terms", "Acme Corp pays on net 45.")
	r, raw, err = eywaPushJSON(t, "global")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 selected update(s) were refused locally") {
		t.Fatalf("a mixed push = %v, want a nonzero result", err)
	}
	if r.Sent != 1 || r.Refused != 1 || r.Failed != 1 || len(r.Updates) != 2 {
		t.Fatalf("mixed receipt = %+v", r)
	}
	byID := map[string]eywaPushRow{}
	for _, u := range r.Updates {
		byID[u.MemoryID] = u
	}
	if byID[okID].Status != "in_review" || byID[bigID].Status != "refused" || byID[bigID].Reason != "too_long" || byID[bigID].Error != "" {
		t.Fatalf("mixed rows = %+v", r.Updates)
	}
	if strings.Contains(raw, "overflow clause") || f.remembers() != 1 {
		t.Fatalf("mixed push leaked content or sent the refused update:\n%s", raw)
	}
}
