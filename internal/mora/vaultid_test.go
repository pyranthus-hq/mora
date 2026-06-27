package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func sandboxCfg(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := defaultConfig()
	for _, d := range []string{cfg.VaultDir, cfg.DataDir, cfg.StateDir, cfg.ConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestVaultMarkerWriteOnce(t *testing.T) {
	cfg := sandboxCfg(t)

	got, err := createVaultMarkerIfAbsent(cfg, "v_first")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v_first" {
		t.Fatalf("first create: got id %q, want v_first", got)
	}
	if _, err := os.Stat(markerPath(cfg)); err != nil {
		t.Fatalf("marker not written: %v", err)
	}

	// Second create must NOT overwrite — returns the existing id.
	got2, err := createVaultMarkerIfAbsent(cfg, "v_second")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "v_first" {
		t.Fatalf("second create returned %q, want existing v_first (write-once)", got2)
	}

	m, present, err := readVaultMarker(cfg)
	if err != nil || !present {
		t.Fatalf("read: present=%v err=%v", present, err)
	}
	if m.VaultID != "v_first" || m.Schema != 1 {
		t.Fatalf("marker = %+v", m)
	}
}

func TestVaultMarkerAbsent(t *testing.T) {
	cfg := sandboxCfg(t)
	_, present, err := readVaultMarker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("expected no marker in a fresh sandbox")
	}
}

func TestIndexMetaRoundTrip(t *testing.T) {
	cfg := sandboxCfg(t)
	// Seed one memory so the rebuild has content and binds an id.
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "x", Source: "manual", CreatedAt: nowRFC3339(), Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	id, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if id != "v_seed" {
		t.Fatalf("index vault_id = %q, want v_seed", id)
	}
}

func indexCount(t *testing.T, cfg Config) int {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRebuildEnforceBlocksEmptyVault(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "keep", Source: "manual", CreatedAt: nowRFC3339(), Text: "precious"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("seed index count = %d, want 1", got)
	}

	// Simulate the incident: the vault's memory files vanish (dir emptied).
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}

	_, err := rebuildIndex(context.Background(), cfg)
	if !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("expected errRebuildBlocked, got %v", err)
	}
	// CRITICAL: the old index must be intact (rolled back, not wiped).
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("index count after blocked rebuild = %d, want 1 (preserved)", got)
	}
}

func TestRebuildAllowCommitsEmpty(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "x", Source: "manual", CreatedAt: nowRFC3339(), Text: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}
	n, err := rebuildIndexWithPolicy(context.Background(), cfg, policyAllow)
	if err != nil {
		t.Fatalf("allow policy should commit, got %v", err)
	}
	if n != 0 || indexCount(t, cfg) != 0 {
		t.Fatalf("allow rebuild count = %d, index = %d, want 0/0", n, indexCount(t, cfg))
	}
}

func TestBlockRecordWrittenAndCleared(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("want blocked, got %v", err)
	}
	if _, present, _ := readBlockRecord(cfg); !present {
		t.Fatal("block record should exist after a blocked rebuild")
	}
	// Restore the vault; a good rebuild must clear the record.
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, present, _ := readBlockRecord(cfg); present {
		t.Fatal("block record should be cleared after a successful rebuild")
	}
}

func TestRebuildBlocksForeignVault(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("seed index count = %d, want 1", got)
	}
	// Overwrite the vault marker with a different id to simulate a foreign vault.
	m := vaultMarker{Schema: vaultMarkerSchema, VaultID: "v_b", CreatedAt: nowRFC3339(), CreatedBy: "test"}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(cfg), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("want errRebuildBlocked for foreign vault, got %v", err)
	}
	// Old corpus must be preserved.
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("index count after blocked foreign-vault rebuild = %d, want 1 (preserved)", got)
	}
}

func TestAssessRebuild(t *testing.T) {
	cases := []struct {
		name               string
		oldCount, newCount int
		markerID           string
		markerPresent      bool
		indexID            string
		want               rebuildDecision
	}{
		{"first build empty index", 0, 0, "", false, "", decProceed},
		{"first build populating", 0, 5, "", false, "", decProceed},
		{"the incident: populated->empty, no marker", 2823, 0, "", false, "", decBlockEmpty},
		{"populated->empty, marker matches index", 2823, 0, "v_a", true, "v_a", decBlockEmpty},
		{"adopt: legacy vault, no ids anywhere", 2823, 2820, "", false, "", decAdopt},
		{"adopt: marker present, index has no id yet", 10, 10, "v_a", true, "", decAdopt},
		{"block: index knows its id, marker vanished", 10, 10, "", false, "v_a", decBlockIdentity},
		{"block: different vault", 10, 9, "v_b", true, "v_a", decBlockIdentity},
		{"normal rebuild, ids match", 10, 11, "v_a", true, "v_a", decProceed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := assessRebuild(c.oldCount, c.newCount, c.markerID, c.markerPresent, c.indexID)
			if got != c.want {
				t.Fatalf("assessRebuild = %v, want %v", got, c.want)
			}
		})
	}
}
