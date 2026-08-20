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
	for _, q := range []string{`CREATE TABLE memories(id TEXT PRIMARY KEY,scope TEXT,type TEXT,title TEXT,tags TEXT,source TEXT,created_at TEXT,path TEXT,text TEXT,provider TEXT,account TEXT,created_at_unix INTEGER)`, `CREATE VIRTUAL TABLE memories_fts USING fts5(id UNINDEXED,scope,title,tags,source,text)`} {
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
