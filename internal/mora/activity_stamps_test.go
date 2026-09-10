package mora

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestActivityStampsRebuildAndUpsertPersistFutureEvent(t *testing.T) {
	m := coreBIdxmem("gmail_thread/future", "global", "note", "Future", "body")
	m.Provider, m.Source, m.Account = "gmail", "gmail", "work"
	m.Meta = map[string]any{"occurred_at": "2099-01-02T03:04:05.123456789Z"}
	cfg := gate2Vault(t, m)
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var at string
	var sec, nano int64
	if err := db.QueryRow(`SELECT event_at,event_at_unix,event_at_nanos FROM activity_stamps WHERE memory_id=?`, m.ID).Scan(&at, &sec, &nano); err != nil {
		t.Fatal(err)
	}
	if at != "2099-01-02T03:04:05.123456789Z" || nano != 123456789 {
		t.Fatalf("future stamp = %q/%d/%d", at, sec, nano)
	}
	m.Meta["occurred_at"] = "2099-01-02T03:04:06Z"
	if err := indexUpsert(context.Background(), cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT event_at FROM activity_stamps WHERE memory_id=?`, m.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at != "2099-01-02T03:04:06Z" {
		t.Fatal(at)
	}
}

func TestWikiIndexIsStateCacheAndDoesNotRewriteVaultMarkdown(t *testing.T) {
	cfg := gate2Vault(t)
	legacy := []byte("# User owned index\nkeep me\n")
	path := filepath.Join(cfg.VaultDir, "index.md")
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(legacy) {
		t.Fatalf("vault index was rewritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "index.md")); err != nil {
		t.Fatalf("state cache missing: %v", err)
	}
}
