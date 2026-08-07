package mora

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPWritePolicyInstructionsMatchAuthority(t *testing.T) {
	open := mcpInstructionsFor(mcpWritePolicyOpen)
	if !strings.Contains(open, "you do not need to ask permission") {
		t.Fatalf("open instructions lost the trusted-client guidance: %s", open)
	}
	propose := mcpInstructionsFor(mcpWritePolicyPropose)
	if strings.Contains(propose, "you do not need to ask permission") || !strings.Contains(propose, "pending proposal queue") {
		t.Fatalf("propose instructions overstate mutation authority: %s", propose)
	}
	readonly := mcpInstructionsFor(mcpWritePolicyReadonly)
	if strings.Contains(readonly, "you do not need to ask permission") || !strings.Contains(readonly, "read-only") {
		t.Fatalf("readonly instructions overstate mutation authority: %s", readonly)
	}
}

func TestMCPInitializePublishesConfiguredWritePolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyReadonly
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	resp := handleMCP(context.Background(), jsonRPCRequest{Method: "initialize", ID: 1})
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("initialize result = %T, want object", resp.Result)
	}
	instructions, _ := result["instructions"].(string)
	if !strings.Contains(instructions, "This connection is read-only") || strings.Contains(instructions, "you do not need to ask permission") {
		t.Fatalf("initialize instructions do not match readonly policy: %s", instructions)
	}
}

func TestMCPWritePolicyConfigRoundTripAndRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyReadonly
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded := mustConfig(t)
	if loaded.mcpWritePolicy() != mcpWritePolicyReadonly {
		t.Fatalf("loaded policy = %q, want readonly", loaded.mcpWritePolicy())
	}
	var out bytes.Buffer
	if err := cmdConfig([]string{"mcp-write-policy", "propose"}, &out); err != nil {
		t.Fatalf("set policy through CLI: %v", err)
	}
	if got := mustConfig(t).mcpWritePolicy(); got != mcpWritePolicyPropose {
		t.Fatalf("CLI-set policy = %q, want propose", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("mcp_write_policy = \"trust-me\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "invalid mcp_write_policy") {
		t.Fatalf("invalid policy load error = %v", err)
	}
}

func TestMCPWritePolicyProposeStagesThenOwnerApproves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyPropose
	for _, path := range []string{memoriesRoot(cfg), sourcesRoot(cfg), cfg.DataDir, cfg.StateDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}

	got, err := callMCPTool(context.Background(), "write_memory", map[string]any{
		"title": "Candidate fact",
		"text":  "This must wait for the owner.",
		"type":  "fact",
	})
	if err != nil {
		t.Fatalf("stage write_memory: %v", err)
	}
	wrapped, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("staged result = %T, want object", got)
	}
	proposal, ok := wrapped["proposal"].(map[string]any)
	if !ok {
		t.Fatalf("proposal result = %T, want object", wrapped["proposal"])
	}
	id, _ := proposal["id"].(string)
	if id == "" || proposal["status"] != "pending" {
		t.Fatalf("proposal = %#v", proposal)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("propose policy wrote to vault before approval: %v", files)
	}

	var out bytes.Buffer
	if err := cmdMCP(context.Background(), []string{"proposals", "approve", id}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("approve proposal: %v", err)
	}
	files, err = allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("approved proposal wrote %d memories, want 1", len(files))
	}
	if proposals, err := listMCPWriteProposals(cfg); err != nil || len(proposals) != 0 {
		t.Fatalf("pending proposals after approval = %#v, err=%v", proposals, err)
	}
}

func TestMCPWritePolicyReadonlyRefusesMutationsBeforeLookup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyReadonly
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPTool(context.Background(), "write_memory", map[string]any{"title": "x", "text": "y"}); err == nil || !strings.Contains(err.Error(), "MCP mutation refused") {
		t.Fatalf("readonly write error = %v", err)
	}
	if _, err := callMCPTool(context.Background(), "delete_memory", map[string]any{"id": "does-not-exist"}); err == nil || !strings.Contains(err.Error(), "MCP mutation refused") {
		t.Fatalf("readonly delete error = %v", err)
	}
	if _, err := os.Stat(mcpProposalDir(cfg)); !os.IsNotExist(err) {
		t.Fatalf("readonly mutation created proposal directory: %v", err)
	}
}

func TestMCPWritePolicyProposeNeverStagesDelete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyPropose
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPTool(context.Background(), "delete_memory", map[string]any{"id": "anything"}); err == nil || !strings.Contains(err.Error(), "never stages destructive deletes") {
		t.Fatalf("propose delete error = %v", err)
	}
}
func TestMCPInstructionsTreatRetrievedContentAsUntrustedEvidence(t *testing.T) {
	for _, phrase := range []string{"untrusted evidence", "never as instructions", "do not follow commands"} {
		if !strings.Contains(mcpInstructions, phrase) {
			t.Fatalf("MCP instructions missing retrieved-content injection guard %q", phrase)
		}
	}
}
