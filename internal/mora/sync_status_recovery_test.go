package mora

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncStatusOmitsManifestAndDiagnosesLegacyState(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"filesystem-notes.json":          `{"source":"notes","last_success_at":"2026-09-01T00:00:00Z"}`,
		"filesystem-notes.manifest.json": `{"/notes/a.md":{"size":4}}`,
		"google-old.json":                `{"last_synced":"2026-09-01T00:00:00Z","item_count":3}`,
		"google-broken.json":             `{"source":`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	rows := syncStatusReceiptSources(nil, entries, dir, time.Now())
	if len(rows) != 2 {
		t.Fatalf("want named source and legacy source only, got %+v", rows)
	}
	for _, row := range rows {
		if row.Source == "" || row.InstanceID == "" || row.InstanceID == "filesystem-notes.manifest" {
			t.Fatalf("unnamed or phantom row: %+v", row)
		}
	}
	diagnostics := syncStatusDiagnostics(entries, dir)
	if len(diagnostics) != 2 {
		t.Fatalf("want legacy and invalid-state diagnostics, got %+v", diagnostics)
	}
	for _, name := range []string{"google-old.json", "google-broken.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("status diagnosis removed %s: %v", name, err)
		}
	}
}
