package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	t.Setenv("MORA_VAULT", "")
	return dir
}
func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func TestCoreA_ParseConfigValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"/home/x/vault"`, "/home/x/vault"}, // plain quoted
		{`"/x" # inline comment`, "/x"},      // inline comment after close quote ignored
		{`"/tmp/a\tb"`, "/tmp/a\tb"},         // escape honored via Unquote
		{`"a\qb"`, `a\qb`},                   // invalid escape: Unquote fails => lenient Trim
		{`"unterminated`, "unterminated"},    // unterminated quote: legacy lenient read
		{`/plain/path`, "/plain/path"},       // unquoted
		{`/plain # note`, "/plain"},          // unquoted cut at '#'
		{`  spaced  `, "spaced"},             // trimmed
	}
	for _, tc := range cases {
		if got := ParseValue(tc.in); got != tc.want {
			t.Errorf("ParseValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCoreA_LoadConfigAllKeys(t *testing.T) {
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		"# a comment line",
		"", // blank line
		"vault_dir = \"/v/dir\"",
		"data_dir = \"/d/dir\"",
		"state_dir = \"/s/dir\"",
		"embedder = \"ollama\"",
		"context = \"large\"",
		"mmr = true",
		"unknown_key = \"ignored\"",
		"no_equals_line", // len(parts)!=2 => skipped
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VaultDir != "/v/dir" || cfg.DataDir != "/d/dir" || cfg.StateDir != "/s/dir" {
		t.Fatalf("dirs not loaded: %+v", cfg)
	}
	if cfg.Embedder != "ollama" || cfg.ContextProfile != "large" || !cfg.MMR {
		t.Fatalf("embedder/context/mmr not loaded: %+v", cfg)
	}
}

func TestCoreA_LoadConfigReadError(t *testing.T) {
	dir := isolate(t)
	// Make config.toml a DIRECTORY so os.ReadFile returns a non-ErrNotExist error.
	if err := os.MkdirAll(filepath.Join(dir, "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load should surface a non-ErrNotExist read error")
	}
}

func TestDefaultConfigHonorsMoraConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := Default()
	if cfg.ConfigDir != dir {
		t.Fatalf("Default().ConfigDir = %q, want MORA_CONFIG_DIR %q", cfg.ConfigDir, dir)
	}
	// The override must isolate the ENTIRE install, not just the config: with
	// global defaults for DataDir/StateDir/VaultDir, a scratch `init` rebuilt
	// (wiped) the LIVE index.db and shared the live watermark state — the
	// exact incident class the env var was added to prevent.
	for name, got := range map[string]string{
		"VaultDir": cfg.VaultDir, "DataDir": cfg.DataDir, "StateDir": cfg.StateDir,
	} {
		if !strings.HasPrefix(got, dir+string(os.PathSeparator)) {
			t.Errorf("%s = %q escapes MORA_CONFIG_DIR %q — scratch runs would touch the live install", name, got, dir)
		}
	}
	t.Setenv("MORA_CONFIG_DIR", "")
	if got := Default().ConfigDir; !strings.HasSuffix(got, filepath.Join(".config", "mora")) {
		t.Fatalf("with MORA_CONFIG_DIR unset, ConfigDir = %q, want the ~/.config/mora default", got)
	}
}

// TestInitVaultRefusesRepointNonTTY: `init --vault` against an EXISTING config
// pointing elsewhere is a destructive repoint (the old vault is orphaned from
// Mora's view). Non-interactively it must refuse with an actionable error; the
// config must be untouched.
func TestWriteConfigPreservesUnknownKeysAndComments(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
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

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Embedder = "ollama"
	if err := Write(cfg); err != nil {
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
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.VaultDir != filepath.Join(home, "v1") || cfg2.Embedder != "ollama" {
		t.Fatalf("round-trip lost known keys: %+v", cfg2)
	}

	// Reset-by-dropping semantics survive the rewrite path: clearing the
	// embedder removes the line (the static floor is the default).
	cfg2.Embedder = ""
	if err := Write(cfg2); err != nil {
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

// TestLoadConfigParsesInlineCommentsAndQuotedValues: Load used to take
// everything after '=' and strip outer quotes, so a hand-written
// `vault_dir = "/x" # note` loaded as the garbage path `/x" # note` — and the
// read-modify-write Write then persisted the corruption back via %q,
// orphaning the real vault. Hand-editing is a path our own refusal messages
// recommend, so quoted values must parse exactly and inline comments must be
// ignored.
func TestLoadConfigParsesInlineCommentsAndQuotedValues(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("MORA_CONFIG_DIR", "")

	cfgDir := filepath.Join(home, ".config", "mora")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(home, "my vault") // space: quoted-value parsing must survive it
	body := fmt.Sprintf("vault_dir = %q # weekly backup is on the NAS\n", vault) +
		fmt.Sprintf("data_dir = %q\n", filepath.Join(home, "d")) +
		fmt.Sprintf("state_dir = %q\n", filepath.Join(home, "s")) +
		"embedder = \"ollama\"   # trailing comment\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VaultDir != vault {
		t.Fatalf("inline comment corrupted vault_dir: got %q, want %q", cfg.VaultDir, vault)
	}
	if cfg.Embedder != "ollama" {
		t.Fatalf("inline comment corrupted embedder: got %q", cfg.Embedder)
	}

	// The load→rewrite round-trip must not amplify anything: after a rewrite,
	// a second load still yields the exact same values.
	if err := Write(cfg); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.VaultDir != vault || cfg2.Embedder != "ollama" {
		t.Fatalf("round-trip corrupted values: %+v", cfg2)
	}
}

// TestInitVaultTrailingSlashIsNotARepoint: `--vault /same/path/` (shell tab
// completion, install.sh's MORA_VAULT knob) must stay idempotent — the guard
// compares cleaned paths, not raw strings.
func TestWriteConfigPreservesEmptyDirValues(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("MORA_CONFIG_DIR", "")

	cfgDir := filepath.Join(home, ".config", "mora")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "vault_dir = \"\"\n" +
		fmt.Sprintf("data_dir = %q\n", filepath.Join(home, "d")) +
		fmt.Sprintf("state_dir = %q\n", filepath.Join(home, "s"))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ContextProfile = "small"
	if err := Write(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(cfgDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `vault_dir = ""`) {
		t.Fatalf("empty vault_dir was dropped by an unrelated rewrite (silent repoint to the default):\n%s", got)
	}
}
func TestMoraVaultEnvRejectsBlank(t *testing.T) {
	isolate(t)
	t.Setenv("MORA_VAULT", "   ")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MORA_VAULT") {
		t.Fatalf("blank MORA_VAULT must fail loudly naming the variable, got err=%v", err)
	}
}

// TestMoraVaultEnvRejectsRelativePath: a relative MORA_VAULT would resolve
// against the process CWD — installed services and schedules run from / (or an
// arbitrary dir), so the same value would silently select different vaults per
// process. Refuse it loudly. (Fizz review F4.)
func TestMoraVaultEnvRejectsRelativePath(t *testing.T) {
	isolate(t)
	for _, v := range []string{"relative/vault", "~"} {
		t.Setenv("MORA_VAULT", v)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MORA_VAULT") {
			t.Fatalf("relative MORA_VAULT %q must fail loudly naming the variable, got err=%v", v, err)
		}
	}
}

// TestSchedulePlistCarriesVaultEnv locks the launchd-env contract for
// MORA_VAULT, mirroring TestSchedulePlistCarriesConfigDirEnv: launchd jobs run
// with a bare environment, so an install whose vault is selected by MORA_VAULT
// must have the var snapshotted into the plist or the job silently operates on
// config.toml's vault.
