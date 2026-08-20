package previewfilter

import (
	"context"
	"database/sql"
	_ "modernc.org/sqlite"
	"reflect"
	"testing"
)

func resolverDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(`CREATE TABLE entities(id TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return db
}
func addEntity(t *testing.T, db *sql.DB, id, display, aliases string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO entities VALUES(?,?,?,1)`, id, display, aliases); err != nil {
		t.Fatal(err)
	}
}
func TestResolveEntityByDisplayAndAlias(t *testing.T) {
	db := resolverDB(t)
	addEntity(t, db, "person:neil@example.com", "Neil Patel", `["neil@example.com","Neil Patel"]`)
	addEntity(t, db, "memory:ignore", "Neil Patel", `["ignored@example.com"]`)
	for _, query := range []string{"Neil Patel", "neil@example.com"} {
		got, err := ResolveEntity(context.Background(), db, query)
		if err != nil || !got.OK || got.Canonical != "person:neil@example.com" || len(got.Ambiguous) != 0 || !got.IDSet[got.Canonical] {
			t.Fatalf("query=%q got=%+v err=%v", query, got, err)
		}
	}
}
func TestResolveEntityNoMatchAndEmpty(t *testing.T) {
	db := resolverDB(t)
	addEntity(t, db, "person:neil@example.com", "Neil Patel", `["neil@example.com"]`)
	for _, query := range []string{"Nobody Here", "  "} {
		got, err := ResolveEntity(context.Background(), db, query)
		if err != nil || got.OK || len(got.Ambiguous) != 0 {
			t.Fatalf("query=%q got=%+v err=%v", query, got, err)
		}
	}
}
func TestResolveEntityAmbiguousDeterministic(t *testing.T) {
	db := resolverDB(t)
	addEntity(t, db, "person:riya.s@beta.com", "Riya", `["riya.s@beta.com"]`)
	addEntity(t, db, "person:riya.k@alpha.com", "Riya", `["riya.k@alpha.com"]`)
	got, err := ResolveEntity(context.Background(), db, "Riya")
	want := []string{"Riya <riya.k@alpha.com>", "Riya <riya.s@beta.com>"}
	if err != nil || got.OK || !reflect.DeepEqual(got.Ambiguous, want) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestResolveEntityAmbiguousFallbackDisplay(t *testing.T) {
	db := resolverDB(t)
	addEntity(t, db, "person:a@example.com", "", `["Shared"]`)
	addEntity(t, db, "person:b@example.com", "", `["Shared"]`)
	got, err := ResolveEntity(context.Background(), db, "Shared")
	want := []string{"a@example.com <a@example.com>", "b@example.com <b@example.com>"}
	if err != nil || !reflect.DeepEqual(got.Ambiguous, want) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
