package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSetupPlanIsReadOnly(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()

	out := run(t, "setup", "--plan")
	if !strings.Contains(out, "Mora setup foundation plan (read-only):") || !strings.Contains(out, "local_layout: pending") {
		t.Fatalf("plan output = %q", out)
	}
	for _, path := range []string{cfg.ConfigDir, cfg.StateDir, cfg.VaultDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("setup --plan wrote %s: stat error = %v", path, err)
		}
	}
	out = run(t, "setup")
	if !strings.Contains(out, "Non-interactive runs are read-only") {
		t.Fatalf("non-interactive setup output = %q", out)
	}
	for _, path := range []string{cfg.ConfigDir, cfg.StateDir, cfg.VaultDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-interactive setup wrote %s: stat error = %v", path, err)
		}
	}
}

func TestSetupInteractiveDefaultReconciles(t *testing.T) {
	withTempHome(t)
	orig := setupIsInteractive
	setupIsInteractive = func(io.Reader) bool { return true }
	t.Cleanup(func() { setupIsInteractive = orig })

	out := run(t, "setup")
	if !strings.Contains(out, "Foundation setup verified.") {
		t.Fatalf("interactive setup output = %q", out)
	}
}

func TestSetupPersistsVerifiedProgressAndResumes(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	orig := setupRebuildIndex
	setupRebuildIndex = func(context.Context, Config) (int, error) {
		return 0, errors.New("injected rebuild failure")
	}
	t.Cleanup(func() { setupRebuildIndex = orig })

	_, err := runErr(t, "setup", "--local-layout", "--committed-index", "--credential-storage")
	if err == nil || !strings.Contains(err.Error(), "injected rebuild failure") {
		t.Fatalf("first setup error = %v, want injected rebuild failure", err)
	}
	receipt, present, err := readSetupReceipt(cfg)
	if err != nil || !present {
		t.Fatalf("receipt after interrupted setup = %#v, present=%v, err=%v", receipt, present, err)
	}
	if receipt.Steps[0].State != "verified" || receipt.Steps[1].State != "pending" {
		t.Fatalf("interrupted receipt steps = %+v", receipt.Steps)
	}

	setupRebuildIndex = orig
	out := run(t, "setup", "--committed-index", "--credential-storage")
	if !strings.Contains(out, "Foundation setup verified.") {
		t.Fatalf("resumed setup output = %q", out)
	}
	receipt, present, err = readSetupReceipt(cfg)
	if err != nil || !present || !allSetupStepsVerified(receipt.Steps) {
		t.Fatalf("receipt after resume = %#v, present=%v, err=%v", receipt, present, err)
	}
	info, err := os.Stat(setupReceiptPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt mode = %o, want 0600", info.Mode().Perm())
	}
	before, err := os.ReadFile(setupReceiptPath(cfg))
	if strings.Contains(string(before), cfg.VaultDir) || strings.Contains(string(before), cfg.ConfigDir) {
		t.Fatalf("receipt leaked local paths: %s", before)
	}
	if err != nil {
		t.Fatal(err)
	}
	run(t, "setup")
	after, err := os.ReadFile(setupReceiptPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("second setup rewrote an already verified receipt")
	}
}

func TestSetupStatusReobservesInsteadOfTrustingReceipt(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	run(t, "setup", "--local-layout", "--committed-index", "--credential-storage")
	if err := os.Remove(dbPath(cfg)); err != nil {
		t.Fatal(err)
	}

	out := run(t, "setup", "status", "--json")
	var status setupStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, out)
	}
	if status.Complete {
		t.Fatal("first slice must not claim full onboarding complete")
	}
	if !status.ReceiptPresent {
		t.Fatal("status lost the persisted receipt")
	}
	if status.Steps[1].ID != "committed_index" || status.Steps[1].State != "pending" {
		t.Fatalf("status trusted receipt instead of re-observing index: %+v", status.Steps)
	}
}

func TestSetupStatusRequiresJSON(t *testing.T) {
	withTempHome(t)
	_, err := runErr(t, "setup", "status")
	if err == nil || err.Error() != "usage: mora setup status --json" {
		t.Fatalf("setup status error = %v", err)
	}
}

func TestSetupRefusesCorruptReceipt(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	if err := os.MkdirAll(filepath.Dir(setupReceiptPath(cfg)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupReceiptPath(cfg), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runErr(t, "setup")
	if err == nil || !strings.Contains(err.Error(), "read setup receipt") {
		t.Fatalf("corrupt receipt error = %v", err)
	}
}


func TestSetupIdentityRejectsMismatchedMarker(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	run(t, "setup", "--local-layout", "--committed-index", "--credential-storage")

	// Read the original marker's vault_id so we can confirm the new one differs.
	origMarker, origPresent, err := readVaultMarker(cfg)
	if err != nil || !origPresent || origMarker.VaultID == "" {
		t.Fatalf("precondition: original marker missing or empty: %v, present=%v, id=%q", err, origPresent, origMarker.VaultID)
	}

	// Simulate losing the marker.
	if err := os.Remove(markerPath(cfg)); err != nil {
		t.Fatal(err)
	}

	// Layout reconciliation creates a fresh marker with a new random vault_id.
	run(t, "setup", "--local-layout")

	// The new marker must have a different id from the original.
	newMarker, newPresent, err := readVaultMarker(cfg)
	if err != nil || !newPresent {
		t.Fatalf("new marker should exist after layout reconciliation: %v, present=%v", err, newPresent)
	}
	if newMarker.VaultID == origMarker.VaultID {
		t.Fatal("layout reconciliation must generate a fresh vault_id, not reuse the old one")
	}

	// Verify committed_index is pending due to identity mismatch, not verified.
	orig := setupRebuildIndex
	setupRebuildIndex = func(context.Context, Config) (int, error) {
		return 0, errors.New("should not be called")
	}
	t.Cleanup(func() { setupRebuildIndex = orig })

	out := run(t, "setup", "--plan")
	if !strings.Contains(out, "committed_index: pending") {
		t.Fatalf("committed_index must be pending after identity mismatch:\n%s", out)
	}
	if strings.Contains(out, "committed_index: verified") {
		t.Fatal("committed_index must NOT report verified when the vault marker identity doesn't match the index")
	}
}

func TestSetupOverlapRejectsStateDirInsideVault(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	cfg.StateDir = filepath.Join(cfg.VaultDir, "state")
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, err := runErr(t, "setup", "--local-layout")
	if err == nil || !strings.Contains(err.Error(), "overlaps the vault directory") {
		t.Fatalf("setup with StateDir inside VaultDir must fail closed: %v", err)
	}
	// Assert no file was created inside the vault before the error.
	if _, err := os.Stat(cfg.VaultDir); err == nil {
		entries, _ := os.ReadDir(cfg.VaultDir)
		if len(entries) > 0 {
			t.Fatalf("setup created %d file(s) inside the vault before failing", len(entries))
		}
	}
}

func TestSetupOverlapRejectsDataDirInsideVault(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	cfg.DataDir = filepath.Join(cfg.VaultDir, "data")
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, err := runErr(t, "setup", "--local-layout")
	if err == nil || !strings.Contains(err.Error(), "overlaps the vault directory") {
		t.Fatalf("setup with DataDir inside VaultDir must fail closed: %v", err)
	}
}

func TestSetupOverlapRejectsConfigDirInsideVault(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	cfg.VaultDir = filepath.Join(cfg.ConfigDir, "vault")
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, err := runErr(t, "setup", "--local-layout")
	if err == nil || !strings.Contains(err.Error(), "overlaps the vault directory") {
		t.Fatalf("setup with VaultDir inside ConfigDir must fail closed: %v", err)
	}
}

func TestSetupOverlapRejectsReceiptPathInsideVault(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	cfg.StateDir = filepath.Join(cfg.VaultDir, "state")
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, err := runErr(t, "setup", "--local-layout")
	if err == nil || !strings.Contains(err.Error(), "overlaps the vault directory") {
		t.Fatalf("setup with receipt path inside VaultDir must fail closed: %v", err)
	}
}

func TestSetupOverlapSymlinkResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	withTempHome(t)
	cfg := defaultConfig()
	// Create the vault dir and a symlink that resolves inside it.
	if err := os.MkdirAll(cfg.VaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(cfg.VaultDir), "config-link")
	if err := os.Symlink(cfg.VaultDir, link); err != nil {
		t.Fatal(err)
	}
	// StateDir via the symlink resolves inside the vault.
	cfg.StateDir = filepath.Join(link, "state")
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, err := runErr(t, "setup", "--local-layout")
	if err == nil || !strings.Contains(err.Error(), "overlaps the vault directory") {
		t.Fatalf("setup with symlink-resolved StateDir inside VaultDir must fail closed: %v", err)
	}
}

// TestSetupUnboundIndexStaysPending pins the identity rule for indexes that
// carry no vault_id binding: an unbound index proves nothing, so
// committed_index must stay pending until a rebuild establishes the binding.
func TestSetupUnboundIndexStaysPending(t *testing.T) {
	withTempHome(t)
	cfg := defaultConfig()
	run(t, "setup", "--local-layout", "--committed-index", "--credential-storage")

	// Strip the index's vault_id binding to simulate a legacy/foreign index.
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM index_meta WHERE key='vault_id'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	steps := setupFoundationStatus(cfg)
	for _, step := range steps {
		if step.ID == "committed_index" && step.State == "verified" {
			t.Fatal("an index without a vault_id binding must not report committed_index verified")
		}
	}
}
