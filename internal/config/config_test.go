package config

import "testing"

func TestRuntimeOverridesAreProcessLocal(t *testing.T) {
	cfg := Config{VaultDir: "/durable"}
	cfg.ApplyVaultOverride("/runtime")
	if cfg.VaultDir != "/runtime" || cfg.PersistVaultDir() != "/durable" {
		t.Fatalf("vault override = runtime %q durable %q", cfg.VaultDir, cfg.PersistVaultDir())
	}
	cfg.ClearVaultOverride()
	if cfg.PersistVaultDir() != "/runtime" {
		t.Fatalf("cleared durable = %q", cfg.PersistVaultDir())
	}

	cfg.SetOperationRunID("run-1")
	if cfg.OperationRunID() != "run-1" {
		t.Fatal("operation run id lost")
	}
	fusion, mmr := &struct{}{}, &struct{}{}
	cfg.SetFusionOverride(fusion)
	cfg.SetMMROverride(mmr)
	if cfg.FusionOverride() != fusion || cfg.MMROverride() != mmr {
		t.Fatal("evaluation overrides lost")
	}
}
