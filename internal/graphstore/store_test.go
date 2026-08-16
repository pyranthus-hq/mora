package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/memory"
	_ "modernc.org/sqlite"
	"reflect"
	"testing"
)

func graphDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := []string{`CREATE TABLE entities(id TEXT PRIMARY KEY,kind TEXT,display_name TEXT,aliases TEXT,mention_count INTEGER,first_seen TEXT,last_seen TEXT,salience_micros INTEGER)`, `CREATE TABLE edges(src TEXT,rel TEXT,dst TEXT,evidence_id TEXT,valid_from TEXT,valid_to TEXT,observed_at TEXT,invalidated_at TEXT,PRIMARY KEY(src,rel,dst,evidence_id))`, `CREATE TABLE person_merges(member_a TEXT,member_b TEXT,signal TEXT,detail TEXT,PRIMARY KEY(member_a,member_b,signal))`, `CREATE TABLE memories(id TEXT PRIMARY KEY,scope TEXT,type TEXT,title TEXT,tags TEXT,source TEXT,created_at TEXT,path TEXT,text TEXT)`}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func TestWritePersistsGraphRowsAndNulls(t *testing.T) {
	db := graphDB(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := graph.Result{Entities: []graph.EntityRow{{ID: "person:a", Kind: "person", DisplayName: "A", Aliases: nil, MentionCount: 2, Salience: 17}}, Edges: []graph.EdgeRow{{Src: "memory:m", Rel: "MENTIONS", Dst: "person:a", EvidenceID: "m"}, {Src: "memory:m", Rel: "MENTIONS", Dst: "person:a", EvidenceID: "m"}}, Merges: []graph.MergeLink{{A: "a", B: "b", Signal: "confirmed", Detail: "g"}}}
	if err := Write(context.Background(), tx, r); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var aliases string
	var first, valid any
	var sal int64
	if err := db.QueryRow(`SELECT aliases,first_seen,salience_micros FROM entities WHERE id='person:a'`).Scan(&aliases, &first, &sal); err != nil {
		t.Fatal(err)
	}
	if aliases != "[]" || first != nil || sal != 17 {
		t.Fatalf("entity=%q %#v %d", aliases, first, sal)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*),valid_to FROM edges`).Scan(&n, &valid); err != nil {
		t.Fatal(err)
	}
	if n != 1 || valid != nil {
		t.Fatalf("edges=%d valid=%#v", n, valid)
	}
	if err := db.QueryRow(`SELECT count(*) FROM person_merges`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("merges=%d err=%v", n, err)
	}
}
func TestGraphReadQueriesLiveDistinctAndTieBreak(t *testing.T) {
	db := graphDB(t)
	for _, q := range []string{`INSERT INTO entities VALUES('person:b','person','Alex','["b@example.com"]',2,NULL,NULL,9)`, `INSERT INTO entities VALUES('person:a','person','Alex','bad',2,NULL,NULL,NULL)`, `INSERT INTO entities VALUES('memory:m','memory','hub','[]',1,NULL,NULL,0)`, `INSERT INTO edges VALUES('memory:m','MENTIONS','person:a','m',NULL,NULL,'2026-01-01',NULL)`, `INSERT OR IGNORE INTO edges VALUES('memory:m','MENTIONS','person:a','m',NULL,NULL,'2026-01-01',NULL)`, `INSERT INTO edges VALUES('memory:x','MENTIONS','person:a','x',NULL,NULL,NULL,'gone')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := LiveEvidenceByEntity(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ev["person:a"], []string{"m"}) {
		t.Fatalf("ev=%v", ev)
	}
	rows, err := ListEntityRows(context.Background(), db)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	m, err := FindEntity(context.Background(), db, "Alex")
	if err != nil || m.ID != "person:a" {
		t.Fatalf("match=%+v err=%v", m, err)
	}
	m, err = FindEntity(context.Background(), db, "b@example.com")
	if err != nil || m.ID != "person:b" {
		t.Fatalf("alias=%+v err=%v", m, err)
	}
	edges, ids, err := IncomingEdges(context.Background(), db, "person:a")
	if err != nil || len(edges) != 1 || !reflect.DeepEqual(ids, []string{"m"}) {
		t.Fatalf("edges=%v ids=%v err=%v", edges, ids, err)
	}
}
func TestCoOccurringPeopleFiltersAndOrders(t *testing.T) {
	db := graphDB(t)
	for _, q := range []string{`INSERT INTO entities VALUES('person:a','person','A','[]',1,NULL,NULL,0)`, `INSERT INTO entities VALUES('person:b','person','B','[]',1,NULL,NULL,0)`, `INSERT INTO entities VALUES('service:s','service','S','[]',1,NULL,NULL,0)`, `INSERT INTO edges VALUES('memory:m','ATTENDED','person:a','m',NULL,NULL,NULL,NULL)`, `INSERT INTO edges VALUES('memory:m','ATTENDED','person:b','m',NULL,NULL,NULL,NULL)`, `INSERT INTO edges VALUES('memory:m','ATTENDED','service:s','m',NULL,NULL,NULL,NULL)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := CoOccurringPeople(context.Background(), db, "person:a")
	if err != nil || !reflect.DeepEqual(got, []string{"person:b"}) {
		t.Fatalf("got=%v err=%v", got, err)
	}
}
func TestLoadMemoriesSortsHydratesAndFallsBack(t *testing.T) {
	db := graphDB(t)
	for _, q := range []string{`INSERT INTO memories VALUES('b','global','note','B','z,a','mcp','2026-01-01','bad','row-b')`, `INSERT INTO memories VALUES('a','global','note','A','','mcp','2026-02-01','good','row-a')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadMemoriesByID(context.Background(), db, []string{"a", "b"}, LoadOptions{Hydrate: func(p string) (memory.Memory, error) {
		if p == "good" {
			return memory.Memory{ID: "a", Title: "hydrated", CreatedAt: "2026-02-01"}, nil
		}
		return memory.Memory{}, errors.New("read")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "hydrated" || got[1].Text != "row-b" || !reflect.DeepEqual(got[1].Tags, []string{"z", "a"}) {
		t.Fatalf("got=%+v", got)
	}
	empty, err := LoadMemoriesByID(context.Background(), db, nil, LoadOptions{})
	if err != nil || empty != nil {
		t.Fatalf("empty=%v err=%v", empty, err)
	}
}
func TestGraphStoreErrorsAreLoud(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := LiveEvidenceByEntity(context.Background(), db); err == nil {
		t.Fatal("missing edges accepted")
	}
	tx, _ := db.Begin()
	if err := Write(context.Background(), tx, graph.Result{Entities: []graph.EntityRow{{ID: "x"}}}); err == nil {
		t.Fatal("missing entities accepted")
	}
	_ = tx.Rollback()
	if AliasMatches("bad", "x") {
		t.Fatal("malformed alias matched")
	}
}
