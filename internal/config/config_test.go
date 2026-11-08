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

func TestOperationSourceBindingSharesPublishCountAcrossCopies(t *testing.T) {
	var unbound Config
	unbound.NoteOperationPublish() // must not panic outside a bound ingest
	if unbound.OperationSourceKey() != "" || unbound.OperationPublishes() != 0 {
		t.Fatalf("unbound config = key %q publishes %d", unbound.OperationSourceKey(), unbound.OperationPublishes())
	}
	var cfg Config
	cfg.BindOperationSource("applecalendar")
	handedDown := cfg // the run passes config by value into connector code
	handedDown.NoteOperationPublish()
	handedDown.NoteOperationPublish()
	if cfg.OperationSourceKey() != "applecalendar" || cfg.OperationPublishes() != 2 {
		t.Fatalf("binding = key %q publishes %d, want applecalendar/2", cfg.OperationSourceKey(), cfg.OperationPublishes())
	}
	cfg.BindOperationSource("gmail")
	if cfg.OperationPublishes() != 0 || handedDown.OperationPublishes() != 2 {
		t.Fatalf("rebinding must start a fresh count: new %d, old copy %d", cfg.OperationPublishes(), handedDown.OperationPublishes())
	}
}
