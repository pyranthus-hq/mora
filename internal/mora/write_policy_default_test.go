package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// The MCP write policy fails safe. With no mcp_write_policy line in
// config.toml, agent writes wait in the proposal queue and deletes are refused,
// however Mora was started: `mora mcp serve` on an empty HOME, `mora init`,
// `mora setup`, or an older install that never named the key. An explicit
// value, including `open`, is always kept.

// shippedUnsetMCPWritePolicy is captured by TestMain before it points the seam
// at open for the older open-mode tests.
var shippedUnsetMCPWritePolicy string

// useShippedWritePolicyDefault restores the production resolution of an unset
// policy for one test. Callers must not be parallel: the seam is package state.
func useShippedWritePolicyDefault(t *testing.T) {
	t.Helper()
	if shippedUnsetMCPWritePolicy != mcpWritePolicyPropose {
		t.Fatalf("shipped default for an unset policy = %q, want propose", shippedUnsetMCPWritePolicy)
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

func TestShippedUnsetWritePolicyIsPropose(t *testing.T) {
	if mcppkg.DefaultWritePolicy != mcpWritePolicyPropose {
		t.Fatalf("mcp.DefaultWritePolicy = %q, want propose", mcppkg.DefaultWritePolicy)
	}
	useShippedWritePolicyDefault(t)
	if got := configMCPWritePolicy(Config{}); got != mcpWritePolicyPropose {
		t.Fatalf("unset policy resolves to %q, want propose", got)
	}
}

// The reported bug: `mora mcp serve` on an empty HOME executed agent writes at
// once and created the vault and index, so a later `mora init` saw an existing
// install and never turned propose on.
func TestMCPServeOnFreshHomeStagesWritesAndInitKeepsPropose(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	cfg := mustConfig(t)
	if configFileExists(cfg) {
		t.Fatal("precondition: a fresh home must have no config.toml")
	}

	instructions, text, isErr := policyTestServe(t, "Fresh home candidate")
	if isErr {
		t.Fatalf("write_memory on a fresh home errored: %s", text)
	}
	if !strings.Contains(instructions, "pending proposal queue") || strings.Contains(instructions, mcppkg.OpenWriteInstruction) {
		t.Fatalf("fresh-home initialize advertises open writes: %s", instructions)
	}
	if n := vaultMemoryCount(t, cfg); n != 0 {
		t.Fatalf("mcp serve on a fresh home wrote %d memories to the vault before approval", n)
	}
	if n := pendingProposalCount(t, cfg); n != 1 {
		t.Fatalf("pending proposals after fresh-home write = %d, want 1", n)
	}

	// The later `mora init` must leave the install in propose.
	run(t, "init")
	loaded := mustConfig(t)
	if got := configMCPWritePolicy(loaded); got != mcpWritePolicyPropose {
		t.Fatalf("policy after mcp serve then init = %q, want propose", got)
	}
	_, text, isErr = policyTestServe(t, "After init candidate")
	if isErr {
		t.Fatalf("write_memory after init errored: %s", text)
	}
	if n := vaultMemoryCount(t, loaded); n != 0 {
		t.Fatalf("write after mcp serve then init reached the vault (%d memories)", n)
	}
	if n := pendingProposalCount(t, loaded); n != 2 {
		t.Fatalf("pending proposals = %d, want 2", n)
	}
	if _, err := mcppkg.MutationAction(configMCPWritePolicy(loaded), "delete_memory"); err == nil {
		t.Fatal("the default policy must refuse MCP deletes")
	}
}

func TestFreshInitAndSetupResolveToPropose(t *testing.T) {
	useShippedWritePolicyDefault(t)
	for _, args := range [][]string{{"init"}, {"setup", "--local-layout"}} {
		t.Run(args[0], func(t *testing.T) {
			withTempHome(t)
			run(t, args...)
			if got := configMCPWritePolicy(mustConfig(t)); got != mcpWritePolicyPropose {
				t.Fatalf("fresh %s policy = %q, want propose", args[0], got)
			}
			if out := run(t, "config"); !strings.Contains(out, "mcp_write_policy = propose") {
				t.Fatalf("mora config does not show the effective propose default:\n%s", out)
			}
		})
	}
}

// Existing installs that never wrote the key are not migrated to open: they
// resolve to propose after upgrading, the same as a new install.
func TestExistingConfigWithoutWritePolicyResolvesToPropose(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	cfg := mustConfig(t)
	writeRawConfig(t, cfg, "vault_dir = \""+cfg.VaultDir+"\"\n")
	run(t, "init")
	run(t, "setup", "--local-layout")
	if body := readTestConfigTOML(t, cfg); strings.Contains(body, "mcp_write_policy") {
		t.Fatalf("re-init must not write a policy line into an existing config:\n%s", body)
	}
	if got := configMCPWritePolicy(mustConfig(t)); got != mcpWritePolicyPropose {
		t.Fatalf("existing install without the key resolves to %q, want propose", got)
	}
}

// An explicit policy, including the open opt-out, survives init and setup and
// is what the MCP server enforces.
func TestExplicitWritePolicyIsKeptAndEnforced(t *testing.T) {
	useShippedWritePolicyDefault(t)
	for _, tc := range []struct {
		policy, action string
	}{
		{mcpWritePolicyOpen, mcppkg.ActionExecute},
		{mcpWritePolicyPropose, mcppkg.ActionPropose},
		{mcpWritePolicyReadonly, mcppkg.ActionRefuse},
	} {
		t.Run(tc.policy, func(t *testing.T) {
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

// The opt-out works end to end through the CLI and the MCP server.
func TestOptOutToOpenExecutesWritesOverMCP(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	run(t, "config", "mcp-write-policy", "open")
	cfg := mustConfig(t)
	instructions, text, isErr := policyTestServe(t, "Opted out write")
	if isErr {
		t.Fatalf("open write_memory errored: %s", text)
	}
	if !strings.Contains(instructions, mcppkg.OpenWriteInstruction) {
		t.Fatalf("open policy initialize lost the open instructions: %s", instructions)
	}
	if n := vaultMemoryCount(t, cfg); n != 1 {
		t.Fatalf("open policy wrote %d memories, want 1", n)
	}
	if n := pendingProposalCount(t, cfg); n != 0 {
		t.Fatalf("open policy staged %d proposals, want 0", n)
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
