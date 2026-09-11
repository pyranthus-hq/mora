package search

import (
	"context"
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"reflect"
	"testing"
	"time"
)

func TestActivityFilterStrictParsingAndReceipt(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 123456789, time.UTC)
	for _, v := range []any{0, -1, 8785, 1.5, "24", true, nil, json.Number("999999999999999999999999")} {
		if _, err := ParseFilter(map[string]any{"event_since_hours": v}, now, Catalog{}); err == nil {
			t.Fatalf("accepted event window %#v", v)
		}
	}
	for _, v := range []any{"not-context", nil, []any{true}, []any{"unknown"}} {
		if _, err := ParseFilter(map[string]any{"exclude_dispositions": v}, now, Catalog{}); err == nil {
			t.Fatalf("accepted exclusions %#v", v)
		}
	}
	f, err := ParseFilter(map[string]any{"event_since_hours": float64(24), "since_hours": float64(48), "exclude_dispositions": []any{"not-context", "done", "not-context"}}, now, Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"event_since_hours": 24, "since_hours": 48, "exclude_dispositions": []string{"done", "not-context"}}
	if !reflect.DeepEqual(f.Receipt(), want) {
		t.Fatalf("receipt: %#v", f.Receipt())
	}
}

func TestActivitySQLPredicateExactBoundsAndPreLimitExclusion(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE memories(id TEXT PRIMARY KEY,scope TEXT,created_at_unix INTEGER)`,
		`CREATE TABLE activity_stamps(memory_id TEXT PRIMARY KEY,scope TEXT,event_at_unix INTEGER,event_at_nanos INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 9, 10, 12, 0, 0, 123456789, time.UTC)
	cutoff := now.Add(-24 * time.Hour)
	stamps := map[string]time.Time{"lower": cutoff, "upper": now, "before": cutoff.Add(-time.Nanosecond), "future": now.Add(time.Nanosecond), "excluded": now.Add(-time.Hour)}
	for id, at := range stamps {
		if _, err := db.Exec(`INSERT INTO memories VALUES (?, 'global', ?)`, id, now.Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO activity_stamps VALUES (?, 'global', ?, ?)`, id, at.Unix(), at.Nanosecond()); err != nil {
			t.Fatal(err)
		}
	}
	f := Filter{EventSinceHours: 24, Now: now, ExcludeDispositions: []string{"not-context"}, ExcludedMemoryIDs: []string{"excluded"}}
	predicate, args := f.SQLPredicate()
	rows, err := db.QueryContext(context.Background(), `SELECT m.id FROM memories m WHERE 1=1`+predicate+` ORDER BY m.id LIMIT 2`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"lower", "upper"}) {
		t.Fatalf("bounds/exclusion failed before limit: %v", ids)
	}
}
