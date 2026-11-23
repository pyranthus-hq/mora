package search

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func ftsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{`CREATE TABLE memories(id TEXT PRIMARY KEY,scope TEXT,type TEXT,title TEXT,tags TEXT,source TEXT,created_at TEXT,path TEXT,text TEXT,provider TEXT,account TEXT,created_at_unix INTEGER)`, `CREATE VIRTUAL TABLE memories_fts USING fts5(id UNINDEXED,scope,title,tags,source,text,tokenize='porter unicode61')`} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func addFTS(t *testing.T, db *sql.DB, id, scope, title, provider, account string, at time.Time) {
	t.Helper()
	created := at.Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, scope, "note", title, "one,two", "local", created, "/"+id+".md", title, provider, account, at.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories_fts VALUES(?,?,?,?,?,?)`, id, scope, title, "one,two", "local", title); err != nil {
		t.Fatal(err)
	}
}
func TestExecuteFTSFilteringOrderingAndPool(t *testing.T) {
	db := ftsDB(t)
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	addFTS(t, db, "b", "project:x", "needle beta", "gmail", "work", now.Add(-time.Hour))
	addFTS(t, db, "a", "project:x", "needle alpha", "gmail", "personal", now.Add(-time.Hour))
	addFTS(t, db, "c", "project:y", "needle gamma", "gmail", "work", now.Add(-time.Hour))
	f := Filter{Source: "gmail:work", SourceFamily: "gmail", SourceInstance: "work", SinceHours: 2, Now: now}
	got, err := ExecuteFTS(context.Background(), db, "needle", "project:x", 2, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.PoolLimit != 50 || len(got.Memories) != 1 || got.Memories[0].ID != "b" || len(got.ParentIDs) != 1 || got.ParentIDs[0] != "b" {
		t.Fatalf("got=%+v", got)
	}
	if len(got.Memories[0].Tags) != 2 {
		t.Fatalf("tags=%v", got.Memories[0].Tags)
	}
}

func TestExecuteFTSAuthoredOnlyExcludesFilesystemBeforeRanking(t *testing.T) {
	db := ftsDB(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	insert := func(id, path, tags, text string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, "project:demo", "note", "note", tags, "", now.Format(time.RFC3339), path, text, "", "", now.Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO memories_fts VALUES(?,?,?,?,?,?)`, id, "project:demo", "note", tags, "", text); err != nil {
			t.Fatal(err)
		}
	}
	insert("src_raw", "/vault/sources/filesystem/docs/src_raw.md", "filesystem,docs", "isolationneedle isolationneedle isolationneedle")
	insert("gmail_thread/legacy", "/vault/sources/gmail/gmail_thread_legacy.md", "gmail", "isolationneedle isolationneedle")
	insert("mem_allowed", "/vault/memories/project/demo/mem_allowed.md", "", "isolationneedle")
	insert("src_mirror", "/vault/sources/filesystem/codex-memory/src_mirror.md", "filesystem,codex-memory", "isolationneedle")

	got, err := ExecuteFTS(context.Background(), db, "isolationneedle", "project:demo", 1, Filter{AuthoredOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Memories) != 2 || got.Memories[0].ID != "mem_allowed" || got.Memories[1].ID != "src_mirror" {
		t.Fatalf("AuthoredOnly must filter before ranking and retain agent-note mirrors, got %+v", got.ParentIDs)
	}
}
func TestExecuteFTSEmptyAndLimitEdges(t *testing.T) {
	db := ftsDB(t)
	got, err := ExecuteFTS(context.Background(), db, "!?", "", 10, Filter{})
	if err != nil || got.Memories != nil {
		t.Fatalf("empty=(%+v,%v)", got, err)
	}
	now := time.Now()
	addFTS(t, db, "a", "x", "needle", "gmail", "", now)
	got, err = ExecuteFTS(context.Background(), db, "needle", "", 0, Filter{})
	if err != nil || got.PoolLimit != 0 || len(got.Memories) != 0 {
		t.Fatalf("limit0=(%+v,%v)", got, err)
	}
}
func TestExecuteFTSBadSchemaSurfacesQueryError(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = ExecuteFTS(context.Background(), db, "needle", "", 1, Filter{}); err == nil {
		t.Fatal("missing schema must error")
	}
}

// FTSQuery ORs content terms, so a two-word query where only one word matches
// still returns the matching row. (Stemming is pinned against the real index
// creation path in internal/mora/index_porter_test.go.)
func TestExecuteFTSTwoTermOROneMatching(t *testing.T) {
	db := ftsDB(t)
	addFTS(t, db, "m1", "personal", "tentacle", "local", "", time.Now().UTC())
	// 2-term query where only one term matches still returns results via OR semantics.
	got, err := ExecuteFTS(context.Background(), db, "tentacle absenttermxyz", "personal", 10, Filter{})
	if err != nil {
		t.Fatalf("2-term ExecuteFTS: %v", err)
	}
	if len(got.Memories) == 0 {
		t.Fatal("expected 2-term query with one matching word to return results")
	}
}

func TestExecuteFTSTitleAndTagBM25Weights(t *testing.T) {
	db := ftsDB(t)
	now := time.Now().UTC()
	created := now.Format(time.RFC3339)
	insert := func(id, title, tags, text string) {
		if _, err := db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, "personal", "note", title, tags, "local", created, "/"+id+".md", text, "local", "", now.Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO memories_fts VALUES(?,?,?,?,?,?)`, id, "personal", title, tags, "local", text); err != nil {
			t.Fatal(err)
		}
	}
	insert("body_only", "General update", "misc", "zephyrstone mentioned once in body")
	insert("tag_hit", "General update", "zephyrstone", "unrelated body text here")
	insert("title_hit", "Zephyrstone summary", "misc", "unrelated body text here")

	got, err := ExecuteFTS(context.Background(), db, "zephyrstone", "personal", 10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Memories) == 3 {
		// The reported score stays the UNWEIGHTED bm25: confidence and the
		// recall threshold were calibrated on it. Weights only affect ordering.
		var plain float64
		if err := db.QueryRow(`SELECT bm25(memories_fts) FROM memories_fts WHERE memories_fts MATCH '"zephyrstone"' AND id = 'title_hit'`).Scan(&plain); err != nil {
			t.Fatal(err)
		}
		if got.Memories[0].ID != "title_hit" || got.Memories[0].Score != plain {
			t.Fatalf("title_hit score = %v, want unweighted bm25 %v", got.Memories[0].Score, plain)
		}
	}
	if len(got.ParentIDs) != 3 || got.ParentIDs[0] != "title_hit" || got.ParentIDs[1] != "tag_hit" || got.ParentIDs[2] != "body_only" {
		t.Fatalf("unexpected BM25 weight order: %v", got.ParentIDs)
	}
}
