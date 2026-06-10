package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitPreservesExistingConfig pins the data-safety invariant for `mora init`:
// re-running it on an existing install must NOT repoint the vault. The original
// bug used defaultConfig()+unconditional writeConfig(), which clobbered a custom
// vault_dir back to the default — orphaning the real vault ("init clears vault").
func TestInitPreservesExistingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgDir := filepath.Join(home, ".config", "mora")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(home, "custom-vault")
	memDir := filepath.Join(custom, "memories", "global")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgBody := fmt.Sprintf("vault_dir = %q\ndata_dir = %q\nstate_dir = %q\n",
		custom, filepath.Join(home, "data"), filepath.Join(home, "state"))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	mem := "---\nid: precious-001\nscope: global\ntype: insight\ntitle: Precious memory\ncreated_at: 2026-06-06T00:00:00Z\n---\nDo not lose me.\n"
	if err := os.WriteFile(filepath.Join(memDir, "precious.md"), []byte(mem), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-run init on the existing install.
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"init"}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VaultDir != custom {
		t.Fatalf("init clobbered vault_dir: got %q want %q", cfg.VaultDir, custom)
	}

	// The pre-existing memory must still be reachable (not orphaned).
	var sout bytes.Buffer
	if err := Run(context.Background(), []string{"search", "precious", "--json"}, &sout, &sout, strings.NewReader("")); err != nil {
		t.Fatalf("search: %v\n%s", err, sout.String())
	}
	var got []Memory
	if err := json.Unmarshal(sout.Bytes(), &got); err != nil {
		t.Fatalf("search json: %v\n%s", err, sout.String())
	}
	if len(got) != 1 || got[0].ID != "precious-001" {
		t.Fatalf("expected to still find precious-001 after init, got %+v", got)
	}
}

// TestInitVaultFlagStillOverrides confirms an explicit --vault still repoints —
// the fix preserves *existing* config but must not break deliberate override.
func TestInitVaultFlagStillOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, "elsewhere")

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"init", "--vault", want}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("init --vault: %v\n%s", err, out.String())
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VaultDir != want {
		t.Fatalf("--vault override lost: got %q want %q", cfg.VaultDir, want)
	}
}
