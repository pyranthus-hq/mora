package mora

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// The MCP write policy defaults to tentative: tentative_test.go pins that path
// end to end. TestMain points an unset policy at open for the older open-mode
// tests, so the tests here that say "unset" exercise the audit log and undo that
// open and tentative share. propose and readonly remain explicit choices.

// shippedUnsetMCPWritePolicy is captured by TestMain before it points the seam
// at open for the older open-mode tests.
var shippedUnsetMCPWritePolicy string

// useShippedWritePolicyDefault restores the production resolution of an unset
// policy for one test. Callers must not be parallel: the seam is package state.
func useShippedWritePolicyDefault(t *testing.T) {
	t.Helper()
	if shippedUnsetMCPWritePolicy != mcppkg.WritePolicyTentative {
		t.Fatalf("shipped default for an unset policy = %q, want tentative", shippedUnsetMCPWritePolicy)
	}
	prev := unsetMCPWritePolicy
	unsetMCPWritePolicy = shippedUnsetMCPWritePolicy
	t.Cleanup(func() { unsetMCPWritePolicy = prev })
}

// policyTestServe runs one `mcp serve` session (initialize, then one
// write_memory call) and returns the initialize instructions and the
// write_memory CallToolResult text.
func policyTestServe(t *testing.T, title string) (instructions, writeText string, writeIsError bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"title": title, "text": "body for " + title, "type": "fact"})
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_memory","arguments":` + string(args) + `}}` + "\n"
	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"mcp", "serve"}, &out, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("mcp serve: %v\n%s", err, out.String())
	}
	for _, f := range coreBMcpFrames(t, out.String()) {
		res, _ := f["result"].(map[string]any)
		switch f["id"] {
		case float64(1):
			instructions, _ = res["instructions"].(string)
		case float64(2):
			content, _ := res["content"].([]any)
			if len(content) > 0 {
				first, _ := content[0].(map[string]any)
				writeText, _ = first["text"].(string)
			}
			writeIsError, _ = res["isError"].(bool)
		}
	}
	if instructions == "" || writeText == "" {
		t.Fatalf("missing initialize or write_memory frame:\n%s", out.String())
	}
	return instructions, writeText, writeIsError
}

func vaultMemoryCount(t *testing.T, cfg Config) int {
	t.Helper()
	files, err := allMemoryFiles(cfg)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(files)
}

func pendingProposalCount(t *testing.T, cfg Config) int {
	t.Helper()
	ps, err := listMCPWriteProposals(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return len(ps)
}

func writeRawConfig(t *testing.T, cfg Config, body string) {
	t.Helper()
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUnsetWritePolicyIsTentative(t *testing.T) {
	useShippedWritePolicyDefault(t)
	if mcppkg.DefaultWritePolicy != mcppkg.WritePolicyTentative {
		t.Fatalf("mcp.DefaultWritePolicy = %q, want tentative", mcppkg.DefaultWritePolicy)
	}
	if got := configMCPWritePolicy(Config{}); got != mcppkg.WritePolicyTentative {
		t.Fatalf("unset policy resolves to %q, want tentative", got)
	}
}

// writeMemoryID pulls the memory id out of a write_memory result text.
func writeMemoryID(t *testing.T, text string) string {
	t.Helper()
	var res struct {
		Memory struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"memory"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil || res.Memory.ID == "" {
		t.Fatalf("write_memory result has no memory id (%v):\n%s", err, text)
	}
	return res.Memory.ID
}

func readAgentWriteLog(t *testing.T, cfg Config) string {
	t.Helper()
	b, err := os.ReadFile(agentWriteLogPath(cfg))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// An unset policy writes directly over MCP stdio, on a fresh HOME and after
// init, and every write is logged.
func TestUnsetPolicyWritesDirectlyAndLogs(t *testing.T) {
	withTempHome(t)
	instructions, text, isErr := policyTestServe(t, "Fresh home write")
	if isErr {
		t.Fatalf("write_memory on a fresh home errored: %s", text)
	}
	if !strings.Contains(instructions, mcppkg.OpenWriteInstruction) {
		t.Fatalf("unset policy initialize lost the open instructions: %s", instructions)
	}
	run(t, "init")
	cfg := mustConfig(t)
	if body := readTestConfigTOML(t, cfg); strings.Contains(body, "mcp_write_policy") {
		t.Fatalf("init must not write a policy line:\n%s", body)
	}
	_, text2, isErr := policyTestServe(t, "After init write")
	if isErr {
		t.Fatalf("write_memory after init errored: %s", text2)
	}
	if n := vaultMemoryCount(t, cfg); n != 2 {
		t.Fatalf("vault memories = %d, want 2", n)
	}
	if n := pendingProposalCount(t, cfg); n != 0 {
		t.Fatalf("unset policy staged %d proposals, want 0", n)
	}
	log := readAgentWriteLog(t, cfg)
	for _, tc := range []struct{ id, title string }{{writeMemoryID(t, text), "Fresh home write"}, {writeMemoryID(t, text2), "After init write"}} {
		want := " write " + tc.id + " scope=global source=mcp caller=mcp-stdio/t title=\"" + tc.title + "\"\n"
		if !strings.Contains(log, want) {
			t.Fatalf("agent-writes.log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "body for") {
		t.Fatalf("agent-writes.log must not copy memory bodies:\n%s", log)
	}
}

func TestFreshInitAndSetupResolveToTentative(t *testing.T) {
	useShippedWritePolicyDefault(t)
	for _, args := range [][]string{{"init"}, {"setup", "--local-layout"}} {
		subRun(t, args[0], func(t *testing.T) {
			withTempHome(t)
			run(t, args...)
			if got := configMCPWritePolicy(mustConfig(t)); got != mcppkg.WritePolicyTentative {
				t.Fatalf("fresh %s policy = %q, want tentative", args[0], got)
			}
			if out := run(t, "config"); !strings.Contains(out, "mcp_write_policy = tentative") {
				t.Fatalf("mora config does not show the effective default:\n%s", out)
			}
		})
	}
}

// An install upgraded without the key moves to the tentative default.
func TestExistingConfigWithoutWritePolicyResolvesToTentative(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	cfg := mustConfig(t)
	writeRawConfig(t, cfg, "vault_dir = \""+cfg.VaultDir+"\"\n")
	run(t, "init")
	run(t, "setup", "--local-layout")
	if body := readTestConfigTOML(t, cfg); strings.Contains(body, "mcp_write_policy") {
		t.Fatalf("re-init must not write a policy line into an existing config:\n%s", body)
	}
	if got := configMCPWritePolicy(mustConfig(t)); got != mcppkg.WritePolicyTentative {
		t.Fatalf("existing install without the key resolves to %q, want tentative", got)
	}
}

// An explicit policy survives init and setup and is what the server enforces.
func TestExplicitWritePolicyIsKeptAndEnforced(t *testing.T) {
	for _, tc := range []struct {
		policy, action string
	}{
		{mcpWritePolicyOpen, mcppkg.ActionExecute},
		{mcpWritePolicyPropose, mcppkg.ActionPropose},
		{mcpWritePolicyReadonly, mcppkg.ActionRefuse},
	} {
		subRun(t, tc.policy, func(t *testing.T) {
			withTempHome(t)
			cfg := mustConfig(t)
			writeRawConfig(t, cfg, "vault_dir = \""+cfg.VaultDir+"\"\nmcp_write_policy = \""+tc.policy+"\"\n")
			run(t, "init")
			run(t, "setup", "--local-layout")
			loaded := mustConfig(t)
			if got := configMCPWritePolicy(loaded); got != tc.policy {
				t.Fatalf("explicit %s became %q after init and setup", tc.policy, got)
			}
			if got, _ := mcppkg.MutationAction(configMCPWritePolicy(loaded), "write_memory"); got != tc.action {
				t.Fatalf("write_memory action under explicit %s = %q, want %q", tc.policy, got, tc.action)
			}
		})
	}
}

// propose still queues, refuses deletes, logs nothing until approval, and doctor
// counts what is waiting.
func TestExplicitProposeStillQueuesAndDoctorCountsPending(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "config", "mcp-write-policy", "propose")
	cfg := mustConfig(t)
	instructions, text, isErr := policyTestServe(t, "Staged one")
	if isErr {
		t.Fatalf("propose write_memory errored: %s", text)
	}
	if !strings.Contains(instructions, "pending proposal queue") {
		t.Fatalf("propose initialize lost the queue instructions: %s", instructions)
	}
	policyTestServe(t, "Staged two")
	if n := vaultMemoryCount(t, cfg); n != 0 {
		t.Fatalf("propose wrote %d memories before approval", n)
	}
	if n := pendingProposalCount(t, cfg); n != 2 {
		t.Fatalf("pending proposals = %d, want 2", n)
	}
	if _, err := mcppkg.MutationAction(configMCPWritePolicy(cfg), "delete_memory"); err == nil {
		t.Fatal("propose must refuse MCP deletes")
	}
	if log := readAgentWriteLog(t, cfg); log != "" {
		t.Fatalf("a staged proposal must not be logged as a write:\n%s", log)
	}

	out, _ := runErr(t, "doctor")
	if !strings.Contains(out, "2 agent write(s) pending in the MCP proposal queue") || !strings.Contains(out, "mora mcp proposals approve <id>") {
		t.Fatalf("doctor does not report the pending proposals:\n%s", out)
	}
	jsonOut, _ := runErr(t, "doctor", "--json")
	if !strings.Contains(jsonOut, `"pending_mcp_proposals": 2`) {
		t.Fatalf("doctor --json lacks pending_mcp_proposals=2:\n%s", jsonOut)
	}

	ps, err := listMCPWriteProposals(cfg)
	if err != nil {
		t.Fatal(err)
	}
	approved := run(t, "mcp", "proposals", "approve", ps[0].ID)
	memID := strings.TrimSuffix(strings.TrimSpace(approved[strings.LastIndex(approved, "memory ")+len("memory "):]), ".")
	if log := readAgentWriteLog(t, cfg); !strings.Contains(log, " write "+memID+" ") || !strings.Contains(log, "caller=approved-proposal") {
		t.Fatalf("approved proposal %s not logged:\n%s", memID, log)
	}
	out, _ = runErr(t, "doctor")
	if !strings.Contains(out, "1 agent write(s) pending") {
		t.Fatalf("doctor pending count did not drop to 1:\n%s", out)
	}
	run(t, "mcp", "proposals", "reject", ps[1].ID)
	out, _ = runErr(t, "doctor")
	if strings.Contains(out, "pending in the MCP proposal queue") {
		t.Fatalf("doctor still reports pending proposals after the queue emptied:\n%s", out)
	}
}

func TestExplicitReadonlyStillRefuses(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "config", "mcp-write-policy", "readonly")
	cfg := mustConfig(t)
	_, text, isErr := policyTestServe(t, "Refused write")
	if !isErr || !strings.Contains(text, "mcp_write_policy=readonly") {
		t.Fatalf("readonly write_memory = (%v) %s, want a policy refusal", isErr, text)
	}
	if n := vaultMemoryCount(t, cfg); n != 0 {
		t.Fatalf("readonly wrote %d memories", n)
	}
	if n := pendingProposalCount(t, cfg); n != 0 {
		t.Fatalf("readonly staged %d proposals", n)
	}
	if log := readAgentWriteLog(t, cfg); log != "" {
		t.Fatalf("readonly logged a write:\n%s", log)
	}
}

// The Aside loopback bridge writes under the same default and is logged as http.
func TestHTTPCallWriteIsDirectAndLogged(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	h := (&httpServer{token: "tok", port: 7777}).handler(testCtx(t))
	req := httptest.NewRequest(http.MethodPost, "/call",
		strings.NewReader(`{"name":"write_memory","arguments":{"title":"From Aside","text":"bridge body","type":"fact"}}`))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /call write_memory: HTTP %d\n%s", rec.Code, rec.Body.String())
	}
	if n := vaultMemoryCount(t, cfg); n != 1 {
		t.Fatalf("HTTP write landed %d memories, want 1", n)
	}
	if log := readAgentWriteLog(t, cfg); !strings.Contains(log, "caller=http title=\"From Aside\"") {
		t.Fatalf("HTTP write not logged:\n%s", log)
	}
}

func TestUndoRemovesAgentMemoryAndLogs(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	_, text, isErr := policyTestServe(t, "Agent guess to undo")
	if isErr {
		t.Fatal(text)
	}
	id := writeMemoryID(t, text)
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, "undo", id)
	if !strings.Contains(out, "undid "+id) {
		t.Fatalf("undo output = %q", out)
	}
	if _, err := os.Stat(m.Path); !os.IsNotExist(err) {
		t.Fatalf("memory file still present after undo: %v", err)
	}
	if _, err := findMemory(cfg, id); err == nil {
		t.Fatal("undone memory is still readable")
	}
	if out := run(t, "search", "undo"); strings.Contains(out, id) {
		t.Fatalf("undone memory still searchable:\n%s", out)
	}
	log := readAgentWriteLog(t, cfg)
	if !strings.Contains(log, " write "+id+" ") || !strings.Contains(log, " undo "+id+" ") || !strings.Contains(log, "caller=cli") {
		t.Fatalf("agent-writes.log does not record the write and the undo:\n%s", log)
	}
	if _, err := runErr(t, "undo", id); err == nil {
		t.Fatal("a second undo of the same id must fail")
	}
}

func TestUndoRefusesNonAgentMemoryWithoutForce(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	run(t, "write", "--title", "Human note", "--text", "typed by the owner")
	ms, err := allMemoryFiles(cfg)
	if err != nil || len(ms) != 1 {
		t.Fatalf("want one memory, got %v (%v)", ms, err)
	}
	id := strings.TrimSuffix(filepath.Base(ms[0]), ".md")
	out, err := runErr(t, "undo", id)
	if err == nil || !strings.Contains(err.Error(), "not written by an agent") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("undo of a CLI memory = %v (%s), want a refusal naming --force", err, out)
	}
	if _, err := os.Stat(ms[0]); err != nil {
		t.Fatalf("refused undo touched the file: %v", err)
	}
	run(t, "undo", "--force", id)
	if _, err := os.Stat(ms[0]); !os.IsNotExist(err) {
		t.Fatalf("forced undo left the file: %v", err)
	}
	if log := readAgentWriteLog(t, cfg); !strings.Contains(log, " undo "+id+" scope=global source=manual") {
		t.Fatalf("forced undo not logged:\n%s", log)
	}
}

// An agent can choose its own source label; the audit log still marks the
// memory as agent-written, so undo needs no --force.
func TestUndoTrustsTheLogOverASpoofedSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	args, _ := json.Marshal(map[string]any{"title": "Claims to be manual", "text": "x", "type": "fact", "source": "manual"})
	in := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_memory","arguments":` + string(args) + `}}` + "\n"
	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"mcp", "serve"}, &out, &out, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	frames := coreBMcpFrames(t, out.String())
	res, _ := frames[0]["result"].(map[string]any)
	content, _ := res["content"].([]any)
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	id := writeMemoryID(t, text)
	run(t, "undo", id)
}

func TestUndoJSONReceipt(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	_, text, isErr := policyTestServe(t, "Undo me as JSON")
	if isErr {
		t.Fatal(text)
	}
	id := writeMemoryID(t, text)
	out := run(t, "undo", "--json", id)
	var env struct {
		Schema string `json:"schema"`
		undoReceipt
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("undo --json is not one JSON document: %v\n%s", err, out)
	}
	if env.Schema != "mora.undo" || env.ID != id || !env.Deleted || !env.AgentWrite || !env.AuditLogged {
		t.Fatalf("undo receipt = %+v\n%s", env, out)
	}
}

func readTestConfigTOML(t *testing.T, cfg Config) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.ConfigDir, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	return string(b)
}
