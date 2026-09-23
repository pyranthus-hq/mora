package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
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

func TestSyncStatusWhitespaceSourceUsesNamedLegacyIdentity(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	path := filepath.Join(cfg.StateDir, "sync", "google-legacy.json")
	if err := memory.SaveStatus(path, &memory.SyncStatus{Source: " \t ", ItemCount: 3}); err != nil {
		t.Fatal(err)
	}
	human := run(t, "sync", "status")
	if !strings.Contains(human, "google-legacy: 3 items") || strings.Contains(human, " \t : 3 items") {
		t.Fatalf("human status has blank or missing identity: %q", human)
	}
	var receipt syncStatusReceipt
	if err := json.Unmarshal([]byte(run(t, "sync", "status", "--json")), &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Sources) != 1 || receipt.Sources[0].Source != "google-legacy" || receipt.Sources[0].InstanceID != "google-legacy" {
		t.Fatalf("JSON status has blank or missing identity: %+v", receipt.Sources)
	}
	if len(receipt.Diagnostics) != 1 || receipt.Diagnostics[0].Reason != "legacy_missing_source" {
		t.Fatalf("whitespace identity was not diagnosed: %+v", receipt.Diagnostics)
	}
	for key := range sourceFreshness(cfg) {
		if strings.TrimSpace(key) == "" {
			t.Fatalf("freshness contains an unnamed source: %q", key)
		}
	}
}
