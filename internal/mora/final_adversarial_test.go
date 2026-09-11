package mora

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func vaultMarkdownSHA(t *testing.T, cfg Config) []string {
	t.Helper()
	var lines []string
	if err := filepath.WalkDir(cfg.VaultDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(cfg.VaultDir, path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s:%x", filepath.ToSlash(rel), sha256.Sum256(b)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return lines
}

func TestFinalRebuildNeverChangesAnyVaultMarkdown(t *testing.T) {
	m := coreBIdxmem("gmail_thread/sha", "global", "note", "SHA", "body")
	m.Provider, m.Source = "gmail", "gmail"
	m.Meta = map[string]any{"occurred_at": "2026-09-10T10:00:00Z"}
	cfg := gate2Vault(t, m)
	// Include a top-level legacy generated page: it is Markdown too, but is now user-owned.
	if err := os.WriteFile(filepath.Join(cfg.VaultDir, "index.md"), []byte("# preserved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := vaultMarkdownSHA(t, cfg)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	after := vaultMarkdownSHA(t, cfg)
	if len(before) != len(after) {
		t.Fatalf("vault markdown set changed: %v -> %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("vault markdown changed: %q -> %q", before[i], after[i])
		}
	}
}

func TestFinalFailedRebuildRetainsActivityStampRows(t *testing.T) {
	m := coreBIdxmem("gmail_thread/keep", "global", "note", "Keep", "body")
	m.Provider, m.Source = "gmail", "gmail"
	m.Meta = map[string]any{"occurred_at": "2026-09-10T10:00:00.123456789Z"}
	cfg := gate2Vault(t, m)
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.QueryRow(`SELECT event_at FROM activity_stamps WHERE memory_id=?`, m.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, coreBIdxmem("zz-final-fail", "global", "note", "Fail", "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER final_fail_stamp BEFORE INSERT ON memories WHEN NEW.id='zz-final-fail' BEGIN SELECT RAISE(FAIL,'forced'); END;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := rebuildIndex(context.Background(), cfg); err == nil {
		t.Fatal("rebuild unexpectedly succeeded")
	}
	db, err = sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var after string
	if err := db.QueryRow(`SELECT event_at FROM activity_stamps WHERE memory_id=?`, m.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("stamp changed across rollback: %q -> %q", before, after)
	}
}

func TestFinalPartialActivityStampSchemaHealsOnUpsertAndShareReadRejects(t *testing.T) {
	m := coreBIdxmem("gmail_thread/heal", "global", "note", "Heal", "body")
	m.Provider, m.Source = "gmail", "gmail"
	m.Meta = map[string]any{"occurred_at": "2026-09-10T10:00:00Z"}
	cfg := gate2Vault(t, m)
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE activity_stamps; CREATE TABLE activity_stamps(memory_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := indexUpsert(context.Background(), cfg, m); err != nil {
		t.Fatalf("upsert did not autoheal partial schema: %v", err)
	}
	db, err = sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := indexSchemaPhysicalComplete(context.Background(), db)
	db.Close()
	if err != nil || !ok {
		t.Fatalf("healed schema incomplete: %v %v", ok, err)
	}
	// A share read cannot silently run SQL against an equally-versioned partial table.
	path := filepath.Join(t.TempDir(), "partial-share.db")
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE activity_stamps(memory_id TEXT PRIMARY KEY); PRAGMA user_version = %d`, indexSchemaVersion)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err := openShareIndexRO(context.Background(), path, ""); err == nil {
		db.Close()
		t.Fatal("partial share schema was accepted")
	}
}

func TestFinalSharedEventSurvivesLocalSameIDDisposition(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	const id = "shared-same-id"
	local := Memory{ID: id, Scope: "project:acme", Type: "insight", Provider: "gmail", Source: "gmail", Title: "local", Text: "shared-event-probe", CreatedAt: "2026-09-10T10:00:00Z", Meta: map[string]any{"occurred_at": "2026-09-10T11:00:00Z"}}
	correction := Memory{ID: "local-correction", Scope: local.Scope, Type: "correction", Source: "manual", Title: "hide local", Text: "hide", CreatedAt: "2026-09-10T10:30:00Z", Meta: map[string]any{"target": id, "disposition": "not-context"}}
	if err := writeMemory(cfg, local); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, correction); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	shared := local
	shared.Title, shared.Text = "shared", "shared-event-probe from neil"
	setupSubscription(t, cfg, "neil", []Memory{shared})
	out, err := defaultSearchForMCP(context.Background(), cfg, "shared-event-probe", local.Scope, 10, searchFilters{Now: now, EventSinceHours: 24, ExcludeDispositions: []string{"not-context"}})
	if err != nil {
		t.Fatal(err)
	}
	var row *Memory
	for i := range out.Results {
		if out.Results[i].Owner == "neil" && out.Results[i].ID == id {
			row = &out.Results[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("local disposition excluded same-ID shared row: %+v", out.Results)
	}
	if row.EventAt != "2026-09-10T11:00:00Z" {
		t.Fatalf("shared event annotation lost: %+v", row)
	}
}
