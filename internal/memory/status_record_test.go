package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectStatusRecordPreservesLegacyAndRejectsManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("filesystem-notes.manifest.json", `{"source":"notes.manifest","item_count":2}`)
	write("filesystem-notes.manifest.manifest.json", `{"/notes/a.md":{"size":4}}`)
	write("google-old.json", `{"last_synced":"2026-09-01T00:00:00Z","item_count":4}`)
	write("google-broken.json", `{"source":`)
	write("google-old.json.tmp", `{"source":"old"}`)
	if st, diagnostic := InspectStatusRecord(dir, "filesystem-notes.manifest.json"); st == nil || diagnostic != "" || st.Source != "notes.manifest" {
		t.Fatalf("named source vanished: %+v, %q", st, diagnostic)
	}
	if st, diagnostic := InspectStatusRecord(dir, "filesystem-notes.manifest.manifest.json"); st != nil || diagnostic != "" {
		t.Fatalf("walk manifest classified as sync status: %+v, %q", st, diagnostic)
	}
	if st, diagnostic := InspectStatusRecord(dir, "google-old.json"); st == nil || diagnostic != "legacy_missing_source" || st.ItemCount != 4 {
		t.Fatalf("legacy record lost: %+v, %q", st, diagnostic)
	}
	if st, diagnostic := InspectStatusRecord(dir, "google-broken.json"); st != nil || diagnostic != "invalid_status_json" {
		t.Fatalf("malformed state must be diagnosed: %+v, %q", st, diagnostic)
	}
	if _, err := os.Stat(filepath.Join(dir, "google-broken.json")); err != nil {
		t.Fatalf("diagnosis must preserve the file: %v", err)
	}
	if st, diagnostic := InspectStatusRecord(dir, "google-old.json.tmp"); st != nil || diagnostic != "" {
		t.Fatalf("temporary write is not status: %+v, %q", st, diagnostic)
	}
}
