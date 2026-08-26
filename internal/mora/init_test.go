package mora

import (
	"bytes"
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
	setTestHome(t, home)
	t.Setenv("MORA_CONFIG_DIR", "")

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
	if err := Run(testCtx(t), []string{"init"}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}

	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VaultDir != custom {
		t.Fatalf("init clobbered vault_dir: got %q want %q", cfg.VaultDir, custom)
	}

	// The pre-existing memory must still be reachable (not orphaned).
	var sout bytes.Buffer
	if err := Run(testCtx(t), []string{"search", "precious", "--json"}, &sout, &sout, strings.NewReader("")); err != nil {
		t.Fatalf("search: %v\n%s", err, sout.String())
	}
	// Plan 01-07: `search --json` carries its array under `memories`.
	got, err := decodeMemoriesJSON(t, sout.String())
	if err != nil {
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
	setTestHome(t, home)
	t.Setenv("MORA_CONFIG_DIR", "")
	want := filepath.Join(home, "elsewhere")

	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"init", "--vault", want}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("init --vault: %v\n%s", err, out.String())
	}
	cfg, err := loadConfigFor(testCtx(t))
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
func TestInitVaultRefusesRepointNonTTY(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
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
	err := Run(testCtx(t), []string{"init", "--vault", filepath.Join(home, "elsewhere")}, &out, &out, strings.NewReader(""))
	if err == nil {
		t.Fatal("init --vault repointed an existing config on a non-TTY; want refusal")
	}
	if !strings.Contains(err.Error(), custom) {
		t.Fatalf("refusal should name the currently-configured vault, got: %v", err)
	}
	cfg, lerr := loadConfigFor(testCtx(t))
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
	setTestHome(t, home)
	t.Setenv("MORA_CONFIG_DIR", "")

	custom := filepath.Join(home, "custom-vault")
	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"init", "--vault", custom}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("first init --vault: %v", err)
	}
	if err := Run(testCtx(t), []string{"init", "--vault", custom}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("re-running init with the same --vault must be idempotent, got: %v", err)
	}
}

// TestWriteConfigPreservesUnknownKeysAndComments pins the read-modify-write
// contract: writeConfig owns five keys and must not eat anything else — a
// comment or a key written by a NEWER mora (or by hand) survived loadConfig
// (which skips unknowns) only to be silently dropped on the next rewrite.
func TestInitVaultTrailingSlashIsNotARepoint(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("MORA_CONFIG_DIR", "")

	custom := filepath.Join(home, "custom-vault")
	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"init", "--vault", custom}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("first init --vault: %v", err)
	}
	if err := Run(testCtx(t), []string{"init", "--vault", custom + string(os.PathSeparator)}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("trailing-slash re-init of the same vault must be idempotent, got: %v", err)
	}
}

// TestWriteConfigPreservesEmptyDirValues: drop-on-empty is reset-to-default
// semantics for embedder/context ONLY. An empty dir value (`vault_dir = ""`,
// hand-written) is broken either way, but silently DROPPING it on an
// unrelated rewrite repoints the vault to the default — the exact side-effect
// class the repoint guard exists to prevent. The line must survive verbatim.
