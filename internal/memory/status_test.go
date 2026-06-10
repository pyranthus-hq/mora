package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStatusRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens", "gmail.json")

	s := &SyncStatus{
		Source:       "gmail",
		LastSynced:   time.Now().UTC().Format(time.RFC3339),
		ItemCount:    42,
		ErrorCount:   3,
		LastError:    "rate limited",
		Checkpoint:   "page-token-7",
		GmailHistory: "history-123",
		CalSyncToken: "calendar-token-456",
	}
	if err := SaveStatus(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", got, s)
	}
}

func TestLoadStatusMissingIsEmpty(t *testing.T) {
	got, err := LoadStatus(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got == nil {
		t.Fatal("missing file should return a non-nil empty status")
	}
	if got.Checkpoint != "" || got.ItemCount != 0 {
		t.Fatalf("expected empty status, got %+v", got)
	}
}

func TestSaveStatusCreatesParentsAndUsesPrivateFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "tokens", "gmail.json")

	if err := SaveStatus(path, &SyncStatus{Source: "gmail"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("status file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSaveStatusUsesExpectedJSONFieldNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens", "calendar.json")
	s := &SyncStatus{
		Source:       "calendar",
		LastSynced:   "2026-05-31T12:00:00Z",
		ItemCount:    12,
		ErrorCount:   1,
		LastError:    "sync token expired",
		Checkpoint:   "calendar-page-token",
		GmailHistory: "unused-for-calendar",
		CalSyncToken: "cal-sync-token",
	}

	if err := SaveStatus(path, s); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status file should contain valid JSON: %v", err)
	}
	for _, key := range []string{
		"source",
		"last_synced",
		"item_count",
		"error_count",
		"last_error",
		"checkpoint",
		"gmail_history",
		"cal_sync_token",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("status JSON missing key %q in %s", key, string(body))
		}
	}
	if _, ok := got["Source"]; ok {
		t.Fatalf("status JSON should use tagged field names, got %s", string(body))
	}
}

func TestLoadStatusInvalidJSONReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens", "gmail.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadStatus(path)
	if err == nil {
		t.Fatalf("invalid JSON should error, got status %+v", got)
	}
}
