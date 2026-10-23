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
}

func newEywaFake(t *testing.T) *eywaFake {
	f := &eywaFake{writes: map[string]string{}, versions: map[string]map[string]any{}}
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
	_ = json.NewDecoder(r.Body).Decode(&req)
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
		if _, ok := a["cursor"]; ok {
			return map[string]any{"items": []any{}, "next": "cur-2", "has_more": false}
		}
		return map[string]any{"items": f.changes, "next": "cur-1", "has_more": false}
	case "get_memory":
		if id, ok := a["write_id"].(string); ok {
			st := f.writes[id]
			if st == "received" {
				f.writes[id] = "in_review" // decided on the next read
			}
			return map[string]any{"write": map[string]any{"write_id": id, "status": st, "reason": nil}}
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
	st, err := loadEywaPushState(mustConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Records[id]
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
	st, _ := loadEywaPushState(mustConfig(t))
	if st.Records[id].Status != "received" {
		t.Fatalf("status after push = %+v", st.Records[id])
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
