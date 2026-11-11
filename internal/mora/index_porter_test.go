package mora

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

func porterDDL(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, table).Scan(&ddl); err != nil {
		t.Fatalf("read %s ddl: %v", table, err)
	}
	return ddl
}

// A fresh rebuild creates both FTS tables with the porter tokenizer, and a
// singular query finds a plural body (and the reverse).
func TestIndexFTSUsesPorterStemming(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_porter", []Memory{
		coreBIdxmem(newID(), "global", "insight", "Team lunches", "We discussed the decisions about passports and clusters."),
	})
	db, err := openIndexRO(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"memories_fts", "gmail_segments_fts"} {
		if ddl := porterDDL(t, db, table); !strings.Contains(strings.ToLower(ddl), "porter") {
			t.Fatalf("%s is not created with the porter tokenizer: %s", table, ddl)
		}
	}
	for _, q := range []string{"decision", "passport", "cluster", "lunch", "Lunches"} {
		got, err := searchMemories(context.Background(), cfg, q, "", 5)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) != 1 {
			t.Fatalf("query %q found %d memories, want 1 (stemming)", q, len(got))
		}
	}
}

// An index at the CURRENT schema version whose FTS tables predate porter must be
// reported stale, rebuilt by `index rebuild --if-needed`, then be stemmed, and a
// second run must be a no-op (no rebuild loop). user_version is never touched.
func TestRebuildReplacesUnstemmedFTSTables(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_porter_old", []Memory{
		coreBIdxmem(newID(), "global", "insight", "Team notes", "The tentacles were discussed."),
	})
	old, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE memories_fts`,
		`CREATE VIRTUAL TABLE memories_fts USING fts5(id, scope, title, tags, source, text)`,
		`INSERT INTO memories_fts SELECT id, scope, title, tags, source, text FROM memories`,
		`DROP TABLE gmail_segments_fts`,
		`CREATE VIRTUAL TABLE gmail_segments_fts USING fts5(evidence_ref UNINDEXED, text)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var uv int
	if err := old.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil || uv != indexSchemaVersion {
		t.Fatalf("fixture must stay at the current version: uv=%d err=%v", uv, err)
	}
	_ = old.Close()

	if got, _ := searchMemories(context.Background(), cfg, "tentacle", "", 5); len(got) != 0 {
		t.Fatalf("fixture should be unstemmed, got %d hits", len(got))
	}
	// (a) the match check reports stale.
	current, err := indexSchemaMatches(context.Background(), cfg)
	if err != nil || current {
		t.Fatalf("indexSchemaMatches = %v, %v; want false (unstemmed FTS)", current, err)
	}
	// (b) --if-needed rebuilds.
	var out bytes.Buffer
	if err := cmdIndex(testCtx(t), []string{"rebuild", "--if-needed"}, &out, testStderr, bytes.NewBuffer(nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "indexed ") || strings.Contains(out.String(), "not needed") {
		t.Fatalf("--if-needed did not rebuild: %q", out.String())
	}
	// (c) stemmed search works.
	got, err := searchMemories(context.Background(), cfg, "tentacle", "", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("tentacle -> tentacles after rebuild: %v %v", got, err)
	}
	// (d) a second run is a no-op.
	out.Reset()
	if err := cmdIndex(testCtx(t), []string{"rebuild", "--if-needed"}, &out, testStderr, bytes.NewBuffer(nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rebuild not needed") {
		t.Fatalf("second --if-needed rebuilt again: %q", out.String())
	}
}

func TestEntityWordPrefixMatch(t *testing.T) {
	es := []Entity{
		{Name: "Saahith Kalakuntla", Kind: "person", Count: 9},
		{Name: "Saahith Other", Kind: "person", Count: 2},
		{Name: "Saahithya Rao", Kind: "person", Count: 50},
		{Name: "Priya", Kind: "person", Count: 1},
	}
	if got, others := entityWordPrefixMatch(es, "saahith"); got == nil || got.Name != "Saahith Kalakuntla" || len(others) != 1 || others[0] != "Saahith Other" {
		t.Fatalf("first-name query = %+v %v, want Saahith Kalakuntla + [Saahith Other]", got, others)
	}
	for _, q := range []string{"", "  ", "...", "Saah", "Kalakuntla"} {
		if got, _ := entityWordPrefixMatch(es, q); got != nil {
			t.Fatalf("query %q must not match, got %+v", q, got)
		}
	}
}

func TestEntityDetailPrefixMatchIsDisclosed(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_prefix", []Memory{
		coreBIdxmem(newID(), "global", "insight", "Sam Rivera", "x"),
	})
	es := []Entity{
		{Name: "Sam Rivera", Kind: "person", Count: 5},
		{Name: "Sam Okafor", Kind: "person", Count: 2},
		{Name: "Priya Shah", Kind: "person", Count: 1},
	}
	var out bytes.Buffer
	if err := entityDetailGraph(context.Background(), cfg, &out, es, "sam", false); err != nil {
		t.Fatal(err)
	}
	txt := out.String()
	if !strings.Contains(txt, "Sam Rivera") || !strings.Contains(txt, "also matched: Sam Okafor") {
		t.Fatalf("text output must name the match and the other candidate:\n%s", txt)
	}
	out.Reset()
	if err := entityDetailGraph(context.Background(), cfg, &out, es, "sam", true); err != nil {
		t.Fatal(err)
	}
	var js map[string]any
	if err := json.Unmarshal(out.Bytes(), &js); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	cands, _ := js["candidates"].([]any)
	if js["matched_by"] != "prefix" || len(cands) != 1 || cands[0] != "Sam Okafor" {
		t.Fatalf("json = %v", js)
	}
	// Exact match: unchanged, no prefix markers.
	out.Reset()
	if err := entityDetailGraph(context.Background(), cfg, &out, es, "Sam Okafor", true); err != nil {
		t.Fatal(err)
	}
	js = nil
	_ = json.Unmarshal(out.Bytes(), &js)
	if _, ok := js["matched_by"]; ok {
		t.Fatalf("exact match must not carry matched_by: %v", js)
	}
}
