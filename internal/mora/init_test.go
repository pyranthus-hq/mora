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

// TestDefaultConfigHonorsMoraConfigDir pins the MORA_CONFIG_DIR override: an
// isolated config location for scripts, launchd jobs, demos, and tests — the
// structural fix for the ephemeral-vault incidents where a scratch `init`
// clobbered the user's one global config.
func TestDefaultConfigHonorsMoraConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	if got := defaultConfig().ConfigDir; got != dir {
		t.Fatalf("defaultConfig().ConfigDir = %q, want MORA_CONFIG_DIR %q", got, dir)
	}
	t.Setenv("MORA_CONFIG_DIR", "")
	if got := defaultConfig().ConfigDir; !strings.HasSuffix(got, filepath.Join(".config", "mora")) {
		t.Fatalf("with MORA_CONFIG_DIR unset, ConfigDir = %q, want the ~/.config/mora default", got)
	}
}

// TestInitVaultRefusesRepointNonTTY: `init --vault` against an EXISTING config
// pointing elsewhere is a destructive repoint (the old vault is orphaned from
// Mora's view). Non-interactively it must refuse with an actionable error; the
// config must be untouched.
func TestInitVaultRefusesRepointNonTTY(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MORA_CONFIG_DIR", "")

	cfgDir := filepath.Join(home, ".config", "mora")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(home, "custom-vault")
	cfgBody := fmt.Sprintf("vault_dir = %q\ndata_dir = %q\nstate_dir = %q\n",
		custom, filepath.Join(home, "data"), filepath.Join(home, "state"))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Run(context.Background(), []string{"init", "--vault", filepath.Join(home, "elsewhere")}, &out, &out, strings.NewReader(""))
	if err == nil {
		t.Fatal("init --vault repointed an existing config on a non-TTY; want refusal")
	}
	if !strings.Contains(err.Error(), custom) {
		t.Fatalf("refusal should name the currently-configured vault, got: %v", err)
	}
	cfg, lerr := loadConfig()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if cfg.VaultDir != custom {
		t.Fatalf("config was modified by the refused init: vault_dir = %q, want %q", cfg.VaultDir, custom)
	}
}

// TestInitVaultSameDirIsNotARepoint: --vault naming the ALREADY-configured
// vault is idempotent, not a repoint — no refusal, no prompt.
func TestInitVaultSameDirIsNotARepoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MORA_CONFIG_DIR", "")

	custom := filepath.Join(home, "custom-vault")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"init", "--vault", custom}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("first init --vault: %v", err)
	}
	if err := Run(context.Background(), []string{"init", "--vault", custom}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("re-running init with the same --vault must be idempotent, got: %v", err)
	}
}

// TestWriteConfigPreservesUnknownKeysAndComments pins the read-modify-write
// contract: writeConfig owns five keys and must not eat anything else — a
// comment or a key written by a NEWER mora (or by hand) survived loadConfig
// (which skips unknowns) only to be silently dropped on the next rewrite.
func TestWriteConfigPreservesUnknownKeysAndComments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MORA_CONFIG_DIR", "")

	cfgDir := filepath.Join(home, ".config", "mora")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# hand-written note: do not lose\n" +
		fmt.Sprintf("vault_dir = %q\n", filepath.Join(home, "v1")) +
		"future_knob = \"keep-me\"\n" +
		fmt.Sprintf("data_dir = %q\n", filepath.Join(home, "d")) +
		fmt.Sprintf("state_dir = %q\n", filepath.Join(home, "s"))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Embedder = "ollama"
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(cfgDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# hand-written note: do not lose", "future_knob = \"keep-me\"", "embedder = \"ollama\""} {
		if !strings.Contains(string(got), want) {
			t.Errorf("rewritten config lost %q:\n%s", want, got)
		}
	}
	// Round-trip: the known keys still load correctly.
	cfg2, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.VaultDir != filepath.Join(home, "v1") || cfg2.Embedder != "ollama" {
		t.Fatalf("round-trip lost known keys: %+v", cfg2)
	}

	// Reset-by-dropping semantics survive the rewrite path: clearing the
	// embedder removes the line (the static floor is the default).
	cfg2.Embedder = ""
	if err := writeConfig(cfg2); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(cfgDir, "config.toml"))
	if strings.Contains(string(got), "embedder") {
		t.Fatalf("cleared embedder must be dropped, config still has it:\n%s", got)
	}
	if !strings.Contains(string(got), "future_knob = \"keep-me\"") {
		t.Fatalf("unknown key lost on second rewrite:\n%s", got)
	}
}
