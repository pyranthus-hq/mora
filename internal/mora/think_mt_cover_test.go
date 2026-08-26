package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// think_mt_cover_test.go covers think.go's error/branch remainder: the search and
// open-loops error returns in buildThink, computeGaps' index error and its
// vector-arm association-only caveat, entityExists' DB error/alias paths, and the
// snippet-term / match-window edge cases.

// TestMt_BuildThinkSearchError: a broken index fails the retrieval up front.
func TestMt_BuildThinkSearchError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	run(t, "write", "--scope", "global", "--type", "note", "--title", "t", "--text", "hello world")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mtBreakIndex(t, cfg)
	if _, err := buildThink(ctx, cfg, "hello", "", 5, time.Now()); err == nil {
		t.Fatal("buildThink over a broken index should surface the retrieval error")
	}
}

// TestMt_BuildThinkOpenLoopsError: retrieval and gap analysis succeed, but a
// live-tasks.md that is a DIRECTORY makes the open-loops join fail — buildThink
// surfaces that error.
func TestMt_BuildThinkOpenLoopsError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	run(t, "write", "--scope", "global", "--type", "note", "--title", "plan", "--text", "the launch plan")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(cfg.VaultDir, "live-tasks.md")
	_ = os.Remove(live)
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatalf("mkdir live-tasks.md-as-dir: %v", err)
	}
	if _, err := buildThink(ctx, cfg, "the launch plan", "", 5, time.Now()); err == nil {
		t.Fatal("buildThink should surface the open-loops listTasks error")
	}
}

// TestMt_ComputeGapsIndexError: computeGaps surfaces the index error from its own
// ensureIndexDB (after the pure staleness pass).
func TestMt_ComputeGapsIndexError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	run(t, "write", "--scope", "global", "--type", "note", "--title", "t", "--text", "hello world")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mtBreakIndex(t, cfg)
	now := time.Now()
	mems := []Memory{{ID: "m1", CreatedAt: now.Format(time.RFC3339)}}
	if _, err := computeGaps(ctx, cfg, "anything", mems, retrievalTrace{}, now); err == nil {
		t.Fatal("computeGaps over a broken index should error")
	}
}

// TestMt_ComputeGapsVectorArmAssociationCaveat: when the graph arm fired and EVERY
// returned memory is present ONLY in the vector arm's association set (never a
// direct FTS/vector hit on the returned id), the association-only caveat fires —
// exercising the vector-arm fold in the B3 analysis.
func TestMt_ComputeGapsVectorArmAssociationCaveat(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	run(t, "write", "--scope", "global", "--type", "note", "--title", "t", "--text", "hello world")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// m1 is returned & graph-associated, but the direct FTS/vector sets contain only
	// OTHER ids (the vector arm carries an unrelated id) → association-only.
	mems := []Memory{{ID: "m1", CreatedAt: now.Format(time.RFC3339)}}
	tr := retrievalTrace{Vec: []string{"other-vec-id"}, Graph: []string{"m1"}}
	g, err := computeGaps(ctx, cfg, "some topic", mems, tr, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.RetrievalCaveats) != 1 || !strings.Contains(g.RetrievalCaveats[0], "people-graph association") {
		t.Fatalf("expected one association-only caveat, got %v", g.RetrievalCaveats)
	}
}

// TestMt_EntityExistsClosedDB: a closed DB fails both the display-name lookup and
// the alias fallback, so entityExists reports false.
func TestMt_EntityExistsClosedDB(t *testing.T) {
	if entityExists(testCtx(t), mtClosedDB(t), "Nobody Here") {
		t.Fatal("entityExists over a closed db should be false")
	}
}

// TestMt_EntityExistsAliasMatch: an entity whose display_name differs from the name
// but whose aliases contain it is found via the alias fallback.
func TestMt_EntityExistsAliasMatch(t *testing.T) {
	db := mtScratchDB(t)
	if _, err := db.Exec(`CREATE TABLE entities (id TEXT, display_name TEXT, aliases TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO entities VALUES ('person:x@a.com','Xavier Onassis','["the alias"]')`); err != nil {
		t.Fatal(err)
	}
	if !entityExists(testCtx(t), db, "The Alias") {
		t.Fatal("entityExists should match via a case-insensitive alias")
	}
	if entityExists(testCtx(t), db, "Totally Absent") {
		t.Fatal("entityExists should NOT match an absent name")
	}
}

// TestMt_EntityExistsAliasScanError: a NULL aliases value fails the Scan inside the
// alias-fallback loop, and entityExists reports false rather than crashing.
func TestMt_EntityExistsAliasScanError(t *testing.T) {
	db := mtScratchDB(t)
	if _, err := db.Exec(`CREATE TABLE entities (id TEXT, display_name TEXT, aliases TEXT)`); err != nil {
		t.Fatal(err)
	}
	// display_name won't match the query (forces the alias fallback); aliases is NULL
	// so the Scan into a string fails.
	if _, err := db.Exec(`INSERT INTO entities (id, display_name, aliases) VALUES ('person:x','Xavier', NULL)`); err != nil {
		t.Fatal(err)
	}
	if entityExists(testCtx(t), db, "Nomatch Name") {
		t.Fatal("entityExists should be false when the alias Scan fails")
	}
}

// TestMt_SnippetTermsShortTokenAndCap: single-rune tokens are dropped and the term
// list is bounded at snippetTermCap.
func TestMt_SnippetTermsShortTokenAndCap(t *testing.T) {
	// "z" survives the stopword filter (not a function word) but is a single rune → dropped.
	got := snippetTerms("z hello world")
	joined := mtRunesJoin(got)
	if strings.Contains(joined, "|z|") {
		t.Fatalf("single-rune token 'z' should be dropped: %q", joined)
	}
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "world") {
		t.Fatalf("discriminative terms missing: %q", joined)
	}

	// More distinct terms than the cap → bounded at snippetTermCap.
	words := []string{"alphaa", "betaa", "gammaa", "deltaa", "epsilo", "zetaa", "etaaa",
		"thetaa", "iotaaa", "kappaa", "lambda", "muuuuu", "nuuuuu", "xiiiii"}
	if n := len(snippetTerms(strings.Join(words, " "))); n != snippetTermCap {
		t.Fatalf("snippetTerms cap = %d, want %d", n, snippetTermCap)
	}
}

// TestMt_EarliestQueryMatchSkipsWordPrefix: a term that only occurs as the PREFIX
// of a longer word is skipped; a later standalone occurrence is returned.
func TestMt_EarliestQueryMatchSkipsWordPrefix(t *testing.T) {
	// "dan" is a prefix of "danger" (mid-word — skipped), then stands alone at index 7.
	r := []rune("danger dan")
	if got := earliestQueryMatch(r, "dan"); got != 7 {
		t.Fatalf("earliestQueryMatch = %d, want 7 (skips the 'danger' prefix, finds standalone 'dan')", got)
	}
}

// mtRunesJoin renders a [][]rune term list as a pipe-delimited string for
// substring assertions.
func mtRunesJoin(terms [][]rune) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		parts = append(parts, string(t))
	}
	return "|" + strings.Join(parts, "|") + "|"
}
