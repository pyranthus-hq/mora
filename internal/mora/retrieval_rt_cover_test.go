package mora

// AREA=rt test-coverage worker. These tests close the remaining branches in the
// retrieval/ranking files (hybrid.go, mmr.go) with precise, controlled fixtures:
// hand-built SQLite DBs for the low-level id/vector queries (so ranking order,
// scope filtering, and error paths are deterministic) plus a fakeOllama-backed
// integration run for the semantic hybrid path (vecOK/useVec/deep-trace/MMR).
//
// Every test is TestRt_*, every helper/type is rt-prefixed (merge-safety: a
// sibling worker edits other files in THIS package in parallel).

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---- rt shared DB helpers ----

// rtOpenDB opens a fresh temp-file SQLite DB (the "sqlite" driver is already
// registered by the production import) and closes it at test end.
func rtOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rt.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open rt db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// rtExec runs a statement, failing the test on error.
func rtExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// rtVecTables creates the memories + mem_vectors tables (the two joined by
// vectorSearchIDs / loadVectorsByID / vectorsAvailable).
func rtVecTables(t *testing.T, db *sql.DB) {
	t.Helper()
	rtExec(t, db, `CREATE TABLE memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT)`)
	rtExec(t, db, `CREATE TABLE mem_vectors (memory_id TEXT PRIMARY KEY, dim INT, model TEXT, vec BLOB)`)
}

// rtInsertMem inserts a bare memory row (id + scope are all the retrieval joins need).
func rtInsertMem(t *testing.T, db *sql.DB, id, scope string) {
	t.Helper()
	rtExec(t, db, `INSERT INTO memories (id, scope) VALUES (?, ?)`, id, scope)
}

// rtInsertVec inserts a mem_vectors row, serializing the vector exactly as production does.
func rtInsertVec(t *testing.T, db *sql.DB, memID, model string, vec []float32) {
	t.Helper()
	rtExec(t, db, `INSERT INTO mem_vectors (memory_id, dim, model, vec) VALUES (?, ?, ?, ?)`,
		memID, len(vec), model, encodeVec(vec))
}

// rtEmbedder is a fixed-output Embedder: Embed always returns vec, so a test
// controls the query vector exactly and asserts the resulting cosine ranking.
type rtEmbedder struct {
	vec   []float32
	model string
}

func (e rtEmbedder) Embed(string) ([]float32, error) { return e.vec, nil }
func (e rtEmbedder) Dim() int                         { return len(e.vec) }
func (e rtEmbedder) ModelID() string                  { return e.model }

// ---- vectorSearchIDs ----

// TestRt_VectorSearchIDsRankingAndFilters pins the cosine ranking, the drop of
// zero/negative-similarity rows, the scope filter, the pool cap, and the
// deterministic id tie-break — the core of the vector arm (previously 0% covered).
func TestRt_VectorSearchIDsRanking(t *testing.T) {
	ctx := context.Background()
	db := rtOpenDB(t)
	rtVecTables(t, db)
	const model = "rt-model-v1"
	for _, r := range []struct{ id, scope string }{
		{"m1", "personal"}, {"m2", "personal"}, {"m3", "personal"},
		{"m4", "work"}, {"m5", "personal"},
	} {
		rtInsertMem(t, db, r.id, r.scope)
	}
	// Query vector is {1,0}; stored cosines: m1=1, m2=0.6, m3=0 (dropped),
	// m4=1 (work scope), m5=-1 (dropped).
	rtInsertVec(t, db, "m1", model, []float32{1, 0})
	rtInsertVec(t, db, "m2", model, []float32{0.6, 0.8})
	rtInsertVec(t, db, "m3", model, []float32{0, 1})
	rtInsertVec(t, db, "m4", model, []float32{1, 0})
	rtInsertVec(t, db, "m5", model, []float32{-1, 0})
	emb := rtEmbedder{vec: []float32{1, 0}, model: model}

	// Unscoped: m1,m4 tie at sim=1 (id asc ⇒ m1 before m4), then m2; m3/m5 dropped.
	got, err := vectorSearchIDs(ctx, db, emb, "q", "", 10)
	if err != nil {
		t.Fatalf("vectorSearchIDs: %v", err)
	}
	if want := []string{"m1", "m4", "m2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unscoped ranking = %v, want %v (zero/neg sim dropped, id tie-break)", got, want)
	}

	// Scoped to personal: m4 (work) excluded; m3/m5 still dropped ⇒ [m1, m2].
	got, err = vectorSearchIDs(ctx, db, emb, "q", "personal", 10)
	if err != nil {
		t.Fatalf("vectorSearchIDs scoped: %v", err)
	}
	if want := []string{"m1", "m2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped ranking = %v, want %v", got, want)
	}

	// Pool cap truncates to the top-1.
	got, err = vectorSearchIDs(ctx, db, emb, "q", "", 1)
	if err != nil {
		t.Fatalf("vectorSearchIDs pool=1: %v", err)
	}
	if want := []string{"m1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pool cap = %v, want %v", got, want)
	}

	// A model with no stored vectors ⇒ empty arm (the mismatch-empties contract).
	got, err = vectorSearchIDs(ctx, db, rtEmbedder{vec: []float32{1, 0}, model: "rt-other"}, "q", "", 10)
	if err != nil {
		t.Fatalf("vectorSearchIDs wrong model: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("wrong model must yield empty arm, got %v", got)
	}
}

// TestRt_VectorSearchIDsQueryError: a missing mem_vectors table surfaces the
// QueryContext error rather than silently returning no ids.
func TestRt_VectorSearchIDsQueryError(t *testing.T) {
	ctx := context.Background()
	db := rtOpenDB(t)
	// Only the memories table exists; mem_vectors is absent.
	rtExec(t, db, `CREATE TABLE memories (id TEXT PRIMARY KEY, scope TEXT)`)
	emb := rtEmbedder{vec: []float32{1, 0}, model: "m"}
	_, err := vectorSearchIDs(ctx, db, emb, "q", "", 10)
	if err == nil || !strings.Contains(err.Error(), "mem_vectors") {
		t.Fatalf("missing mem_vectors table must error, got %v", err)
	}
}

// ---- vectorsAvailable ----

// TestRt_VectorsAvailable pins all three reachable outcomes: table absent ⇒ false,
// table present but empty ⇒ false, table populated ⇒ true.
func TestRt_VectorsAvailable(t *testing.T) {
	ctx := context.Background()

	// (a) No mem_vectors table at all ⇒ false (sqlite_master count == 0).
	noTable := rtOpenDB(t)
	rtExec(t, noTable, `CREATE TABLE memories (id TEXT PRIMARY KEY, scope TEXT)`)
	if vectorsAvailable(ctx, noTable) {
		t.Fatal("missing mem_vectors table must report unavailable")
	}

	// (b) Table exists but empty ⇒ false (count(*) == 0).
	empty := rtOpenDB(t)
	rtVecTables(t, empty)
	if vectorsAvailable(ctx, empty) {
		t.Fatal("empty mem_vectors table must report unavailable")
	}

	// (c) Table populated ⇒ true.
	full := rtOpenDB(t)
	rtVecTables(t, full)
	rtInsertMem(t, full, "m1", "personal")
	rtInsertVec(t, full, "m1", "rt-model", []float32{1, 0})
	if !vectorsAvailable(ctx, full) {
		t.Fatal("populated mem_vectors table must report available")
	}
}

// ---- loadVectorsByID ----

// TestRt_LoadVectorsByIDEmptyAndError closes the two branches the existing
// TestLoadVectorsByID leaves open: an empty id list short-circuits to (nil,nil),
// and a query against a missing table surfaces the error.
func TestRt_LoadVectorsByIDEmptyAndError(t *testing.T) {
	ctx := context.Background()

	db := rtOpenDB(t)
	rtVecTables(t, db)
	got, err := loadVectorsByID(ctx, db, "rt-model", nil)
	if err != nil || got != nil {
		t.Fatalf("empty ids must short-circuit to (nil,nil), got (%v,%v)", got, err)
	}

	// Populated lookup returns the exact decoded vector for a present id and omits absent ids.
	rtInsertMem(t, db, "m1", "personal")
	rtInsertVec(t, db, "m1", "rt-model", []float32{0.6, 0.8})
	out, err := loadVectorsByID(ctx, db, "rt-model", []string{"m1", "absent"})
	if err != nil {
		t.Fatalf("loadVectorsByID: %v", err)
	}
	if !reflect.DeepEqual(out["m1"], []float32{0.6, 0.8}) {
		t.Fatalf("decoded vector = %v, want [0.6 0.8]", out["m1"])
	}
	if _, ok := out["absent"]; ok {
		t.Fatal("absent id must be missing from the map")
	}

	// Missing table ⇒ error surfaced (not swallowed).
	broken := rtOpenDB(t)
	rtExec(t, broken, `CREATE TABLE memories (id TEXT PRIMARY KEY)`)
	if _, err := loadVectorsByID(ctx, broken, "rt-model", []string{"m1"}); err == nil {
		t.Fatal("missing mem_vectors table must surface a query error")
	}
}

// ---- queryIDs ----

// TestRt_QueryIDs pins the shared id-collector: ordered rows come back in order, a
// bad query surfaces the QueryContext error, and a two-column projection surfaces
// the row-scan error (Scan into a single dest fails on a 2-column row).
func TestRt_QueryIDs(t *testing.T) {
	ctx := context.Background()
	db := rtOpenDB(t)
	rtExec(t, db, `CREATE TABLE memories (id TEXT PRIMARY KEY, scope TEXT)`)
	rtInsertMem := func(id, scope string) { rtExec(t, db, `INSERT INTO memories (id, scope) VALUES (?, ?)`, id, scope) }
	rtInsertMem("b", "s")
	rtInsertMem("a", "s")

	got, err := queryIDs(ctx, db, `SELECT id FROM memories ORDER BY id`)
	if err != nil {
		t.Fatalf("queryIDs: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("queryIDs order = %v, want %v", got, want)
	}

	if _, err := queryIDs(ctx, db, `SELECT id FROM does_not_exist`); err == nil {
		t.Fatal("bad table must surface a query error")
	}

	if _, err := queryIDs(ctx, db, `SELECT id, scope FROM memories`); err == nil {
		t.Fatal("two-column projection must surface a scan error (single dest)")
	}
}

// ---- ftsSearchIDs (direct, hand-built FTS index) ----

// rtFtsTables creates memories + the fts5 mirror ftsSearchIDs queries.
func rtFtsTables(t *testing.T, db *sql.DB) {
	t.Helper()
	rtExec(t, db, `CREATE TABLE memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT)`)
	rtExec(t, db, `CREATE VIRTUAL TABLE memories_fts USING fts5(id, scope, title, tags, source, text)`)
}

// rtInsertFts inserts a memory into both the base table and the fts mirror.
func rtInsertFts(t *testing.T, db *sql.DB, id, scope, text string) {
	t.Helper()
	rtExec(t, db, `INSERT INTO memories (id, scope, text) VALUES (?, ?, ?)`, id, scope, text)
	rtExec(t, db, `INSERT INTO memories_fts (id, scope, title, tags, source, text) VALUES (?, ?, '', '', '', ?)`, id, scope, text)
}

// TestRt_FtsSearchIDs covers the scope filter (the previously-open branch), the
// empty/punctuation-only short-circuit, a real BM25 match, and the query error.
func TestRt_FtsSearchIDs(t *testing.T) {
	ctx := context.Background()
	db := rtOpenDB(t)
	rtFtsTables(t, db)
	rtInsertFts(t, db, "m1", "personal", "alpha shared")
	rtInsertFts(t, db, "m2", "work", "alpha shared")
	rtInsertFts(t, db, "m3", "personal", "beta only")

	// Unscoped "alpha" matches m1 and m2 (equal bm25 ⇒ id tie-break m1,m2).
	got, err := ftsSearchIDs(ctx, db, "alpha", "", 10)
	if err != nil {
		t.Fatalf("ftsSearchIDs: %v", err)
	}
	if want := []string{"m1", "m2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unscoped match = %v, want %v", got, want)
	}

	// Scoped to personal drops m2 (work).
	got, err = ftsSearchIDs(ctx, db, "alpha", "personal", 10)
	if err != nil {
		t.Fatalf("ftsSearchIDs scoped: %v", err)
	}
	if want := []string{"m1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped match = %v, want %v", got, want)
	}

	// Empty and punctuation-only queries yield no match expression ⇒ nil.
	for _, q := range []string{"", "   ", "!!! ???"} {
		got, err := ftsSearchIDs(ctx, db, q, "", 10)
		if err != nil {
			t.Fatalf("ftsSearchIDs(%q): %v", q, err)
		}
		if got != nil {
			t.Fatalf("ftsSearchIDs(%q) = %v, want nil (no content tokens)", q, got)
		}
	}

	// A dropped fts table surfaces the query error.
	rtExec(t, db, `DROP TABLE memories_fts`)
	if _, err := ftsSearchIDs(ctx, db, "alpha", "", 10); err == nil {
		t.Fatal("missing fts table must surface a query error")
	}
}

// ---- graphExpandIDs + loadPersonGazetteer (direct, hand-built graph) ----

// rtGraphTables creates memories + entities + edges (the graph arm's inputs).
func rtGraphTables(t *testing.T, db *sql.DB) {
	t.Helper()
	rtExec(t, db, `CREATE TABLE memories (id TEXT PRIMARY KEY, scope TEXT, created_at TEXT)`)
	rtExec(t, db, `CREATE TABLE entities (id TEXT PRIMARY KEY, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, first_seen TEXT, last_seen TEXT, salience_micros INTEGER)`)
	rtExec(t, db, `CREATE TABLE edges (src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, valid_from TEXT, valid_to TEXT, observed_at TEXT, invalidated_at TEXT, PRIMARY KEY (src, rel, dst, evidence_id))`)
}

// rtInsertEntity inserts a person entity with a display name and JSON aliases.
func rtInsertEntity(t *testing.T, db *sql.DB, id, display, aliasesJSON string) {
	t.Helper()
	rtExec(t, db, `INSERT INTO entities (id, kind, display_name, aliases, mention_count) VALUES (?, 'person', ?, ?, 1)`,
		id, display, aliasesJSON)
}

// rtInsertEvidence inserts a memory + a live edge pointing a person at it.
func rtInsertEvidence(t *testing.T, db *sql.DB, personID, evID, scope, createdAt string) {
	t.Helper()
	rtExec(t, db, `INSERT INTO memories (id, scope, created_at) VALUES (?, ?, ?)`, evID, scope, createdAt)
	rtExec(t, db, `INSERT INTO edges (src, rel, dst, evidence_id, invalidated_at) VALUES (?, 'PARTICIPATED_IN', ?, ?, NULL)`,
		"memory:"+evID, personID, evID)
}

// TestRt_GraphExpandIDsScopeAndDedup pins the graph arm: it resolves the named
// person (multi-token gazetteer name), pulls their live evidence newest-first,
// dedups across people, and honors the scope filter (a previously-open branch).
func TestRt_GraphExpandIDsScopeAndDedup(t *testing.T) {
	ctx := context.Background()
	db := rtOpenDB(t)
	rtGraphTables(t, db)
	rtInsertEntity(t, db, "person:neil@x.com", "Neil Patel", `["neil@x.com"]`)
	// Two evidence memories in different scopes; newest first within scope.
	rtInsertEvidence(t, db, "person:neil@x.com", "ev_personal_new", "personal", "2026-05-02T00:00:00Z")
	rtInsertEvidence(t, db, "person:neil@x.com", "ev_personal_old", "personal", "2026-05-01T00:00:00Z")
	rtInsertEvidence(t, db, "person:neil@x.com", "ev_work", "work", "2026-05-03T00:00:00Z")

	// Unscoped: all three evidence ids for Neil (newest-first ⇒ work is newest).
	got, err := graphExpandIDs(ctx, db, "note about Neil Patel", "", 10)
	if err != nil {
		t.Fatalf("graphExpandIDs: %v", err)
	}
	if want := []string{"ev_work", "ev_personal_new", "ev_personal_old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unscoped graph expand = %v, want %v", got, want)
	}

	// Scoped to personal drops the work evidence (covers the scope branch).
	got, err = graphExpandIDs(ctx, db, "Neil Patel", "personal", 10)
	if err != nil {
		t.Fatalf("graphExpandIDs scoped: %v", err)
	}
	if want := []string{"ev_personal_new", "ev_personal_old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped graph expand = %v, want %v", got, want)
	}

	// A query naming nobody known ⇒ no matches ⇒ nil.
	got, err = graphExpandIDs(ctx, db, "nobody in particular", "", 10)
	if err != nil {
		t.Fatalf("graphExpandIDs no-match: %v", err)
	}
	if got != nil {
		t.Fatalf("no-name query must expand to nil, got %v", got)
	}
}

// TestRt_GraphExpandIDsAliasTokenAndErrors pins the exact alias-token match path
// (a precise email/handle token, not a gazetteer name) and the two error seams:
// a missing entities table (loadPersonGazetteer) and a missing edges table (queryIDs).
func TestRt_GraphExpandIDsAliasTokenAndErrors(t *testing.T) {
	ctx := context.Background()
	db := rtOpenDB(t)
	rtGraphTables(t, db)
	// Single-token display name "Bob" is NOT gazetteer-eligible, so this person is
	// reachable ONLY via the exact alias token "bobbytables".
	rtInsertEntity(t, db, "person:bobbytables@x.com", "Bob", `["bobbytables@x.com"]`)
	rtInsertEvidence(t, db, "person:bobbytables@x.com", "ev_bob", "personal", "2026-05-01T00:00:00Z")

	got, err := graphExpandIDs(ctx, db, "ping bobbytables about it", "", 10)
	if err != nil {
		t.Fatalf("graphExpandIDs alias token: %v", err)
	}
	if want := []string{"ev_bob"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("alias-token expand = %v, want %v", got, want)
	}

	// loadPersonGazetteer error: entities table absent.
	noEnt := rtOpenDB(t)
	rtExec(t, noEnt, `CREATE TABLE edges (src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, invalidated_at TEXT)`)
	if _, err := graphExpandIDs(ctx, noEnt, "Neil Patel", "", 10); err == nil {
		t.Fatal("missing entities table must surface an error")
	}

	// queryIDs (edge lookup) error: entities present + a match, but edges table absent.
	noEdge := rtOpenDB(t)
	rtExec(t, noEdge, `CREATE TABLE entities (id TEXT PRIMARY KEY, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, first_seen TEXT, last_seen TEXT, salience_micros INTEGER)`)
	rtInsertEntity(t, noEdge, "person:neil@x.com", "Neil Patel", `["neil@x.com"]`)
	if _, err := graphExpandIDs(ctx, noEdge, "Neil Patel", "", 10); err == nil {
		t.Fatal("missing edges table must surface an error")
	}
}

// TestRt_LoadPersonGazetteerAmbiguityAndScan pins the deterministic smallest-id
// tie-break for a shared display name (the id<cur branch), the aliases-JSON
// unmarshal, and the row-scan error when display_name is NULL.
func TestRt_LoadPersonGazetteer(t *testing.T) {
	ctx := context.Background()
	db := rtOpenDB(t)
	rtGraphTables(t, db)
	// Insert the LARGER id first, then the SMALLER id, both sharing "Sam Jones".
	// loadPersonGazetteer must keep the smaller id (id < cur branch).
	rtInsertEntity(t, db, "person:zed@x.com", "Sam Jones", `["zed@x.com"]`)
	rtInsertEntity(t, db, "person:abe@x.com", "Sam Jones", `["abe@x.com","+15551230000"]`)

	gaz, aliasToID, err := loadPersonGazetteer(ctx, db)
	if err != nil {
		t.Fatalf("loadPersonGazetteer: %v", err)
	}
	if gaz["sam jones"] != "person:abe@x.com" {
		t.Fatalf("ambiguous name tie-break = %q, want the smaller id person:abe@x.com", gaz["sam jones"])
	}
	// Aliases JSON was parsed into the exact-token map.
	if aliasToID["zed"] != "person:zed@x.com" {
		t.Fatalf("alias token 'zed' = %q, want person:zed@x.com", aliasToID["zed"])
	}

	// A NULL display_name forces the row-scan error path.
	bad := rtOpenDB(t)
	rtGraphTables(t, bad)
	rtExec(t, bad, `INSERT INTO entities (id, kind, display_name, aliases, mention_count) VALUES ('person:n@x.com','person',NULL,'[]',1)`)
	if _, _, err := loadPersonGazetteer(ctx, bad); err == nil {
		t.Fatal("NULL display_name must surface a scan error")
	}
}

// ---- hybridSearchTrace / defaultSearch (integration) ----

// TestRt_HybridAutoRebuildsMissingIndex covers the os.Stat(dbPath) miss branch:
// a memory written (via the library writer, which does NOT index) but never
// indexed triggers an on-demand rebuild inside hybridSearchTrace, and the
// just-written doc is retrievable.
func TestRt_HybridAutoRebuildsMissingIndex(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, Memory{
		ID: "note/zephyr", Scope: "global", Type: "note", Title: "Zephyr",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "zephyr breeze notes",
	}); err != nil {
		t.Fatal(err)
	}
	// The memory lives in the vault markdown; remove the derived index so the
	// os.Stat(dbPath) miss branch fires and hybridSearchTrace rebuilds on demand.
	if err := os.Remove(dbPath(cfg)); err != nil {
		t.Fatalf("remove index.db: %v", err)
	}
	if _, err := os.Stat(dbPath(cfg)); err == nil {
		t.Fatal("precondition failed: index.db still exists, cannot exercise the auto-rebuild branch")
	}
	got, err := hybridSearch(ctx, cfg, "zephyr", "", 5)
	if err != nil {
		t.Fatalf("hybridSearch (auto-rebuild): %v", err)
	}
	if len(got) == 0 {
		t.Fatal("auto-rebuild path returned nothing for a written memory")
	}
	found := false
	for _, m := range got {
		if strings.Contains(strings.ToLower(m.Text), "zephyr") {
			found = true
		}
	}
	if !found {
		t.Fatalf("auto-rebuild did not surface the written memory; got %v", idList(got))
	}
}

// TestRt_HybridEmptyPoolReturnsNil covers the all-arms-empty short-circuit: a query
// that matches nothing lexically and names no known person yields a nil result.
func TestRt_HybridEmptyPoolReturnsNil(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	run(t, "write", "--scope", "global", "--type", "note", "--title", "Groceries", "--text", "milk eggs bread")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := hybridSearch(ctx, cfg, "quuxzzznomatchtoken", "", 5)
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("no-match query must return empty, got %v", idList(got))
	}
}

// TestRt_DefaultSearchSemanticRoutesToHybrid pins that with an ACTIVELY-CHOSEN
// semantic embedder (Ollama opted in + a reachable fake daemon), defaultSearch
// routes to hybrid — proven by a graph-only doc (reachable only via the named
// person, sharing no query terms) surfacing, which the FTS-only baseline cannot do.
// This also exercises hybridSearchTrace's vecOK/useVec production vector arm, the
// deep-trace re-query, and the semantic MMR rerank.
func TestRt_DefaultSearchSemanticRoutesToHybrid(t *testing.T) {
	srv := fakeOllama(t, []float64{1, 0, 0, 0})
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Sanity: the chosen embedder really is semantic (drives defaultSearch's branch).
	emb, embErr := chooseEmbedderFor(cfg)
	if embErr != nil || !embedderIsSemantic(emb) {
		t.Fatalf("fakeOllama should yield a semantic embedder (err=%v)", embErr)
	}

	// Neil's memory shares NO words with the query below; reachable only via the graph arm.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/plan", Scope: "global", Type: "email", Title: "Q3 logistics",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "Booking the venue and catering for the offsite.",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	run(t, "write", "--scope", "global", "--type", "note", "--title", "Decoy A", "--text", "alpha decoy one")
	run(t, "write", "--scope", "global", "--type", "note", "--title", "Decoy B", "--text", "beta decoy two")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	got, err := defaultSearch(ctx, cfg, "what did Neil Patel decide", "", 10)
	if err != nil {
		t.Fatalf("defaultSearch semantic: %v", err)
	}
	if !ids(got)["gmail_thread/plan"] {
		t.Fatalf("semantic defaultSearch must reach the graph-only doc; got %v", idList(got))
	}

	// hybridSearchTrace under the semantic embedder must run the vector arm.
	_, tr, err := hybridSearchTrace(ctx, cfg, "what did Neil Patel decide", "", 5, 0)
	if err != nil {
		t.Fatalf("hybridSearchTrace semantic: %v", err)
	}
	if len(tr.Vec) == 0 {
		t.Fatal("semantic embedder must populate the vector arm (tr.Vec)")
	}

	// Deep trace (tracePool > pool) re-queries the vector + graph arms at the deeper pool.
	_, trDeep, err := hybridSearchTrace(ctx, cfg, "what did Neil Patel decide", "", 5, 60)
	if err != nil {
		t.Fatalf("hybridSearchTrace deep: %v", err)
	}
	if trDeep.PreTruncPool != 60 {
		t.Fatalf("deep trace PreTruncPool = %d, want 60", trDeep.PreTruncPool)
	}
	if len(trDeep.Vec) == 0 {
		t.Fatal("deep trace must re-query the vector arm")
	}

	// Semantic MMR (Config.MMR=true, real useVec) reranks without error and still
	// returns the graph-only admission (a pure permutation never drops it).
	mmrCfg := cfg
	mmrCfg.MMR = true
	mmrGot, err := hybridSearch(ctx, mmrCfg, "what did Neil Patel decide", "", 10)
	if err != nil {
		t.Fatalf("semantic MMR hybridSearch: %v", err)
	}
	if !ids(mmrGot)["gmail_thread/plan"] {
		t.Fatalf("semantic MMR must preserve the graph-only admission; got %v", idList(mmrGot))
	}
}

// TestRt_HybridFusedScoreTieBreak forces two docs to EQUAL fused RRF score and pins
// the deterministic id-ascending tie-break in hybridSearchTrace's fused sort. Both
// docs are Neil's graph evidence AND (under the constant-vector fake embedder) sit
// in the vector arm; their ranks are swapped between the two equal-weight arms
// (vector orders by id asc, graph by created_at desc), so their fused scores are
// identical and only the id tie-break decides the order.
func TestRt_HybridFusedScoreTieBreak(t *testing.T) {
	srv := fakeOllama(t, []float64{1, 0, 0, 0})
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Two Neil emails, ids a<b, b NEWER. Text shares no query term (so neither is an
	// FTS hit); both are Neil's evidence (graph arm) and both carry a stored vector.
	for _, m := range []struct{ id, created string }{
		{"gmail_thread/a", "2026-05-01T00:00:00Z"},
		{"gmail_thread/b", "2026-05-02T00:00:00Z"},
	} {
		if err := writeMemory(cfg, Memory{
			ID: m.id, Scope: "global", Type: "email", Title: "Q3 logistics",
			CreatedAt: m.created, Text: "Booking the venue and catering for the offsite.",
			Meta: map[string]any{
				"from":  []string{"neil@example.com"},
				"to":    []string{"adit@x.com"},
				"names": map[string]string{"neil@example.com": "Neil Patel"},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	got, err := hybridSearch(ctx, cfg, "what did Neil Patel decide", "", 10)
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	// vec arm: [a,b] (id asc); graph arm: [b,a] (created_at desc) ⇒ equal fused score
	// ⇒ id-ascending tie-break ⇒ a before b.
	if want := []string{"gmail_thread/a", "gmail_thread/b"}; !reflect.DeepEqual(idList(got), want) {
		t.Fatalf("tie-break order = %v, want %v (equal fused score ⇒ id asc)", idList(got), want)
	}
}
