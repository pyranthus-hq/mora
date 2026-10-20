package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// The MCP write policy default differs between NEW and EXISTING installs. A new
// install gets `propose` written explicitly to config.toml, so agent writes wait
// for local approval. An existing config.toml that never named the key keeps
// resolving to `open`, exactly as it did before this default changed.

// productionNewInstallMCPWritePolicy is captured by TestMain before it blanks
// the seam for legacy tests.
var productionNewInstallMCPWritePolicy string

// useProductionNewInstallPolicy restores the shipped new-install policy for one
// test. Callers must not be parallel: the seam is package state, and only
// sequential tests run while it is swapped.
func useProductionNewInstallPolicy(t *testing.T) {
	t.Helper()
	if productionNewInstallMCPWritePolicy != mcpWritePolicyPropose {
		t.Fatalf("shipped new-install policy = %q, want propose", productionNewInstallMCPWritePolicy)
	}
	prev := newInstallMCPWritePolicy
	newInstallMCPWritePolicy = productionNewInstallMCPWritePolicy
	t.Cleanup(func() { newInstallMCPWritePolicy = prev })
}

func readConfigTOML(t *testing.T, cfg Config) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.ConfigDir, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	return string(b)
}

func assertWriteAction(t *testing.T, cfg Config, want string) {
	t.Helper()
	got, err := mcppkg.MutationAction(configMCPWritePolicy(cfg), "write_memory")
	if err != nil {
		t.Fatalf("write_memory action under %q: %v", configMCPWritePolicy(cfg), err)
	}
	if got != want {
		t.Fatalf("write_memory action = %q, want %q (policy %q)", got, want, configMCPWritePolicy(cfg))
	}
}

func TestFreshInitWritesProposeWritePolicy(t *testing.T) {
	useProductionNewInstallPolicy(t)
	withTempHome(t)
	cfg := mustConfig(t)
	if configFileExists(cfg) {
		t.Fatal("precondition: a fresh home must have no config.toml")
	}
	run(t, "init")

	body := readConfigTOML(t, cfg)
	if !strings.Contains(body, `mcp_write_policy = "propose"`) {
		t.Fatalf("fresh init must persist propose explicitly; config.toml:\n%s", body)
	}
	loaded := mustConfig(t)
	if got := configMCPWritePolicy(loaded); got != mcpWritePolicyPropose {
		t.Fatalf("fresh install policy = %q, want propose", got)
	}
	assertWriteAction(t, loaded, mcppkg.ActionPropose)
	if _, err := mcppkg.MutationAction(configMCPWritePolicy(loaded), "delete_memory"); err == nil {
		t.Fatal("a fresh install must refuse MCP deletes")
	}

	// Re-running init on the now-existing install keeps the explicit value.
	run(t, "init")
	if got := configMCPWritePolicy(mustConfig(t)); got != mcpWritePolicyPropose {
		t.Fatalf("re-init changed the policy to %q", got)
	}
}

func TestFreshSetupLayoutWritesProposeWritePolicy(t *testing.T) {
	useProductionNewInstallPolicy(t)
	withTempHome(t)
	cfg := mustConfig(t)
	run(t, "setup", "--local-layout")
	if body := readConfigTOML(t, cfg); !strings.Contains(body, `mcp_write_policy = "propose"`) {
		t.Fatalf("fresh setup must persist propose explicitly; config.toml:\n%s", body)
	}
	assertWriteAction(t, mustConfig(t), mcppkg.ActionPropose)
}

func TestExistingConfigWithoutWritePolicyStaysOpen(t *testing.T) {
	useProductionNewInstallPolicy(t)
	withTempHome(t)
	cfg := mustConfig(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An install from before this change: config.toml exists, no policy key.
	legacy := "vault_dir = \"" + cfg.VaultDir + "\"\n"
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "config.toml"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := configMCPWritePolicy(mustConfig(t)); got != mcpWritePolicyOpen {
		t.Fatalf("legacy config resolves to %q, want open", got)
	}

	// Upgrading and re-running init or setup must not move it to propose.
	run(t, "init")
	run(t, "setup", "--local-layout")
	body := readConfigTOML(t, cfg)
	if strings.Contains(body, "mcp_write_policy") {
		t.Fatalf("an existing config must not gain a policy key on re-init; config.toml:\n%s", body)
	}
	loaded := mustConfig(t)
	if got := configMCPWritePolicy(loaded); got != mcpWritePolicyOpen {
		t.Fatalf("existing install policy = %q after re-init, want open", got)
	}
	assertWriteAction(t, loaded, mcppkg.ActionExecute)
}

func TestExistingVaultWithoutConfigIsNotTreatedAsNew(t *testing.T) {
	useProductionNewInstallPolicy(t)
	withTempHome(t)
	cfg := mustConfig(t)
	if err := os.MkdirAll(cfg.VaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_existing"); err != nil {
		t.Fatal(err)
	}
	if got := applyNewInstallDefaults(cfg).MCPWritePolicy; got != "" {
		t.Fatalf("a vault with a marker is an existing install; policy pinned to %q", got)
	}
}

func TestNewInstallDefaultsNeverOverrideAnExplicitPolicy(t *testing.T) {
	useProductionNewInstallPolicy(t)
	withTempHome(t)
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyReadonly
	if got := applyNewInstallDefaults(cfg).MCPWritePolicy; got != mcpWritePolicyReadonly {
		t.Fatalf("explicit readonly was replaced with %q", got)
	}
}
