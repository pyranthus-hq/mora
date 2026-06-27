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
