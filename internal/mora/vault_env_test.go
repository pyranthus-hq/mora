package mora

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMoraVaultEnvOverridesConfigToml verifies that a set MORA_VAULT wins over
// config.toml's vault_dir at runtime. install.sh documents MORA_VAULT as the
// vault location, so a user who exports it expects the running binary to honor
// it — silently reading the config.toml vault instead points every command at
// the wrong memories (issue #66).
func TestMoraVaultEnvOverridesConfigToml(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.VaultDir = filepath.Join(t.TempDir(), "toml-vault")
	if err := writeConfig(cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	envVault := filepath.Join(t.TempDir(), "env-vault")
	t.Setenv("MORA_VAULT", envVault)

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.VaultDir != envVault {
		t.Fatalf("MORA_VAULT must override config.toml vault_dir: got %q, want %q", got.VaultDir, envVault)
	}
}

// TestMoraVaultEnvAppliesWithoutConfigToml covers the no-config.toml early
// return in loadConfig: MORA_VAULT must also beat the built-in default vault.
func TestMoraVaultEnvAppliesWithoutConfigToml(t *testing.T) {
	withTempHome(t)

	envVault := filepath.Join(t.TempDir(), "env-vault")
	t.Setenv("MORA_VAULT", envVault)

	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.VaultDir != envVault {
		t.Fatalf("MORA_VAULT must override the default vault_dir: got %q, want %q", got.VaultDir, envVault)
	}
}

// readRawConfig returns the raw config.toml bytes for assertions about what is
// PERSISTED, independent of the env override loadConfig applies.
func readRawConfig(t *testing.T, cfg Config) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.ConfigDir, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	return string(b)
}

// TestMoraVaultEnvNotPersistedByConfigWrite locks the runtime-only nature of the
// override: a read-modify-write of an UNRELATED key (`mora config context small`)
// with MORA_VAULT exported must not silently repoint config.toml's vault_dir —
// that would orphan the configured vault, the incident class the repoint
// confirmation in cmdInit exists to prevent. (Codex review P1 on issue #66.)
func TestMoraVaultEnvNotPersistedByConfigWrite(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	persisted := mustConfig(t).VaultDir

	envVault := filepath.Join(t.TempDir(), "env-vault")
	t.Setenv("MORA_VAULT", envVault)
	run(t, "config", "context", "small")

	raw := readRawConfig(t, mustConfig(t))
	if strings.Contains(raw, envVault) {
		t.Fatalf("MORA_VAULT leaked into config.toml via an unrelated config write:\n%s", raw)
	}
	if !strings.Contains(raw, persisted) {
		t.Fatalf("config.toml lost its vault_dir %q:\n%s", persisted, raw)
	}
}

// TestMoraVaultEnvNotPersistedByReInit: a scripted re-`init` with MORA_VAULT
// exported (install.sh's documented var) must not rewrite the durable vault_dir
// either — the env is effective for the process, config.toml stays canonical.
func TestMoraVaultEnvNotPersistedByReInit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	persisted := mustConfig(t).VaultDir

	envVault := filepath.Join(t.TempDir(), "env-vault")
	t.Setenv("MORA_VAULT", envVault)
	run(t, "init")

	raw := readRawConfig(t, mustConfig(t))
	if strings.Contains(raw, envVault) {
		t.Fatalf("MORA_VAULT leaked into config.toml via re-init:\n%s", raw)
	}
	if !strings.Contains(raw, persisted) {
		t.Fatalf("config.toml lost its vault_dir %q:\n%s", persisted, raw)
	}
}

// TestInitExplicitVaultRepointGateUnderEnv: the explicit `init --vault` flag is
// the one sanctioned repoint path — it must persist even when MORA_VAULT is set,
// AND the confirmation gate must still fire, comparing against the PERSISTED
// vault. The adversarial case is persisted != env == --vault: the pre-fix
// comparison against the env-effective VaultDir would see want == VaultDir and
// silently skip the confirmation. The stub COUNTS invocations and asserts the
// 'from' side, so a regression to the effective-value comparison goes red
// (gate skipped ⇒ calls == 0) instead of silently passing. (Fizz review F2.)
func TestInitExplicitVaultRepointGateUnderEnv(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	persisted := mustConfig(t).VaultDir

	var calls int
	var gotFrom, gotTo string
	orig := confirmVaultRepointFn
	confirmVaultRepointFn = func(_ io.Reader, _ io.Writer, from, to string) error {
		calls++
		gotFrom, gotTo = from, to
		return nil
	}
	t.Cleanup(func() { confirmVaultRepointFn = orig })

	want := filepath.Join(t.TempDir(), "flag-vault")
	t.Setenv("MORA_VAULT", want)
	run(t, "init", "--vault", want)

	if calls != 1 {
		t.Fatalf("repoint confirmation must fire exactly once (persisted %q != target %q), got %d calls", persisted, want, calls)
	}
	if gotFrom != persisted {
		t.Fatalf("gate 'from' must be the PERSISTED vault %q, got %q", persisted, gotFrom)
	}
	if gotTo != want {
		t.Fatalf("gate 'to' must be the --vault target %q, got %q", want, gotTo)
	}
	raw := readRawConfig(t, mustConfig(t))
	if !strings.Contains(raw, want) {
		t.Fatalf("explicit init --vault %q must persist even with MORA_VAULT set:\n%s", want, raw)
	}
}

// TestMoraVaultEnvNotPersistedWhenConfigVaultDirEmpty pins the override-tracking
// representation: a malformed-but-preserved `vault_dir = ""` in config.toml must
// not defeat the runtime-only contract. An empty-string "no override" sentinel
// does exactly that — the stashed persisted value IS empty, so an unrelated
// config write persists the env vault. The override marker must be an explicit
// flag/pointer, not an overloaded empty string. (Fizz review F1.)
func TestMoraVaultEnvNotPersistedWhenConfigVaultDirEmpty(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfgPath := filepath.Join(mustConfig(t).ConfigDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("vault_dir = \"\"\n"), 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	envVault := filepath.Join(t.TempDir(), "env-vault")
	t.Setenv("MORA_VAULT", envVault)
	run(t, "config", "context", "small")

	raw := readRawConfig(t, mustConfig(t))
	if strings.Contains(raw, envVault) {
		t.Fatalf("MORA_VAULT leaked into config.toml when persisted vault_dir was empty:\n%s", raw)
	}
}

// TestMoraVaultEnvRejectsBlank: a set-but-blank (whitespace-only) MORA_VAULT is
// a misconfiguration, not a vault selection — it must fail loudly rather than
// silently resolve to a garbage path. (Empty string means unset, as usual for
// env vars.) (Fizz review F4.)
func TestMoraVaultEnvRejectsBlank(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_VAULT", "   ")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "MORA_VAULT") {
		t.Fatalf("blank MORA_VAULT must fail loudly naming the variable, got err=%v", err)
	}
}

// TestMoraVaultEnvRejectsRelativePath: a relative MORA_VAULT would resolve
// against the process CWD — installed services and schedules run from / (or an
// arbitrary dir), so the same value would silently select different vaults per
// process. Refuse it loudly. (Fizz review F4.)
func TestMoraVaultEnvRejectsRelativePath(t *testing.T) {
	withTempHome(t)
	for _, v := range []string{"relative/vault", "~"} {
		t.Setenv("MORA_VAULT", v)
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "MORA_VAULT") {
			t.Fatalf("relative MORA_VAULT %q must fail loudly naming the variable, got err=%v", v, err)
		}
	}
}

// TestSchedulePlistCarriesVaultEnv locks the launchd-env contract for
// MORA_VAULT, mirroring TestSchedulePlistCarriesConfigDirEnv: launchd jobs run
// with a bare environment, so an install whose vault is selected by MORA_VAULT
// must have the var snapshotted into the plist or the job silently operates on
// config.toml's vault.
func TestSchedulePlistCarriesVaultEnv(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	t.Setenv("MORA_VAULT", "/Users/x/alt/vault")
	plist := schedulePlist(t, cfg, "ingest-hourly")
	if !strings.Contains(plist, "<key>MORA_VAULT</key><string>/Users/x/alt/vault</string>") {
		t.Fatalf("plist missing the snapshotted MORA_VAULT — the scheduled job would run against config.toml's vault:\n%s", plist)
	}
}

// TestServeHTTPEnvVarsCarryVault: same contract for the HTTP daemon installer.
func TestServeHTTPEnvVarsCarryVault(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_VAULT", "/Users/x/alt/vault")
	keys, vals := serveHTTPEnvVars()
	for i, k := range keys {
		if k == "MORA_VAULT" && vals[i] == "/Users/x/alt/vault" {
			return
		}
	}
	t.Fatalf("serveHTTPEnvVars must snapshot MORA_VAULT: keys=%v vals=%v", keys, vals)
}
