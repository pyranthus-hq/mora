package mora

import (
	"context"
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
