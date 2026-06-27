package mora

import (
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
