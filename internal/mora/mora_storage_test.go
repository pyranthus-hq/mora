package mora

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStorageStatus locks Neil's storage thresholds (a 2-3 GB target, a 10-15 GB
// hard ceiling): ok up to the soft target, warn between target and ceiling, over
// past the ceiling.
func TestStorageStatus(t *testing.T) {
	cases := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"empty", 0, "ok"},
		{"at target boundary", storageTargetBytes, "ok"},
		{"just over target", storageTargetBytes + 1, "warn"},
		{"at ceiling boundary", storageCeilingBytes, "warn"},
		{"over ceiling", storageCeilingBytes + 1, "over"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := storageStatus(c.bytes); got != c.want {
				t.Fatalf("storageStatus(%d) = %q, want %q", c.bytes, got, c.want)
			}
		})
	}
}

// TestDirBytes sums regular-file sizes recursively; a missing root is 0 (best-effort).
func TestDirBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.md"), make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dirBytes(dir); got != 350 {
		t.Fatalf("dirBytes = %d, want 350", got)
	}
	if got := dirBytes(filepath.Join(dir, "does-not-exist")); got != 0 {
		t.Fatalf("dirBytes(missing) = %d, want 0", got)
	}
}

// TestVaultStorageNoDoubleCount: when the index DB lives INSIDE the vault, the
// recursive walk already counts it — vaultStorageBytes must not add it again.
// Disjoint layouts (the default) count it via the stat-add path. Both arrive at
// the same total.
func TestVaultStorageNoDoubleCount(t *testing.T) {
	// DB inside the vault (data_dir under vault_dir).
	vault := t.TempDir()
	data := filepath.Join(vault, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "note.md"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "index.db"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vaultStorageBytes(Config{VaultDir: vault, DataDir: data}); got != 600 {
		t.Fatalf("DB-inside-vault double-counted: got %d, want 600", got)
	}

	// Disjoint layout: vault note + external DB still totals 600.
	vault2, data2 := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(vault2, "note.md"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data2, "index.db"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vaultStorageBytes(Config{VaultDir: vault2, DataDir: data2}); got != 600 {
		t.Fatalf("disjoint layout: got %d, want 600", got)
	}
}

// TestDoctorReportsStorage: `mora doctor` surfaces a storage line with the
// footprint and Neil's target/ceiling — the visibility he asked for.
func TestDoctorReportsStorage(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"doctor"}, &out, &out, nil); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "storage") {
		t.Fatalf("doctor output should include a storage line; got:\n%s", s)
	}
}
