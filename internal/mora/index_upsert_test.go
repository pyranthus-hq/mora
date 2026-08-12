package mora

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// idxUpsertMemCount reads the memories table row count from the on-disk index.
func idxUpsertMemCount(t *testing.T, cfg Config) int {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	return n
}

// idxUpsertMetaCount reads index_meta.memory_count (the bookkeeping value the full
// rebuild also maintains; TestCoreB_IdxRebuildBuildsCorpusGraphVectors pins it).
func idxUpsertMetaCount(t *testing.T, cfg Config) string {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM index_meta WHERE key='memory_count'`).Scan(&v); err != nil {
		t.Fatalf("read index_meta memory_count: %v", err)
	}
	return v
}

// idxUpsertStampVaultID force-stamps index_meta.vault_id, simulating an index that
// was built from a DIFFERENT vault than the one the marker on disk identifies.
func idxUpsertStampVaultID(t *testing.T, cfg Config, id string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO index_meta(key,value) VALUES('vault_id',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, id); err != nil {
		t.Fatalf("stamp vault_id: %v", err)
	}
}

// TestIndexUpsertAddsAndReplacesSingleRow pins the core contract: upserting a new
// authored memory adds exactly its one row to memories + FTS and keeps
// index_meta.memory_count consistent, and re-upserting the SAME id replaces the row
// in place (no duplicate) with the new body searchable and the old body gone.
func TestIndexUpsertAddsAndReplacesSingleRow(t *testing.T) {
	seed := []Memory{
		coreBIdxmem("mem_seed_a", "global", "insight", "Seed A", "alpha body one"),
		coreBIdxmem("mem_seed_b", "global", "insight", "Seed B", "beta body two"),
	}
	cfg := coreBIdxpopulatedVault(t, "v_upsert_single", seed)
	ctx := context.Background()

	// Add a brand-new memory via the write path + incremental upsert.
	m := coreBIdxmem("mem_new_c", "global", "insight", "New C", "gammaneedle body three")
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := indexUpsert(ctx, cfg, m); err != nil {
		t.Fatalf("indexUpsert add: %v", err)
	}
	if got := idxUpsertMemCount(t, cfg); got != 3 {
		t.Fatalf("memories rows after add = %d, want 3", got)
	}
	if got := idxUpsertMetaCount(t, cfg); got != "3" {
		t.Fatalf("index_meta memory_count after add = %q, want 3", got)
	}
	res, err := searchMemories(ctx, cfg, "gammaneedle", "", 8)
	if err != nil {
		t.Fatalf("search after add: %v", err)
	}
	if len(res) != 1 || res[0].ID != "mem_new_c" {
		t.Fatalf("search gammaneedle = %+v, want exactly mem_new_c", res)
	}
	// The stored path column must match what a full rebuild would store (memoryPath),
	// so an incrementally-added row is indistinguishable from a rebuilt one.
	if got, want := res[0].Path, memoryPath(cfg, m); got != want {
		t.Fatalf("stored path = %q, want %q (must match rebuild's memoryPath)", got, want)
	}

	// Re-upsert the SAME id with changed body: replace-in-place, no duplicate row.
	m.Text = "deltaneedle body four"
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := indexUpsert(ctx, cfg, m); err != nil {
		t.Fatalf("indexUpsert replace: %v", err)
	}
	if got := idxUpsertMemCount(t, cfg); got != 3 {
		t.Fatalf("memories rows after replace = %d, want 3 (no dup)", got)
	}
	if got := idxUpsertMetaCount(t, cfg); got != "3" {
		t.Fatalf("index_meta memory_count after replace = %q, want 3", got)
	}
	// New body is searchable; the stale body is gone from the FTS row.
	if res, err := searchMemories(ctx, cfg, "deltaneedle", "", 8); err != nil || len(res) != 1 {
		t.Fatalf("search deltaneedle = %+v err=%v, want exactly one hit", res, err)
	}
	if res, err := searchMemories(ctx, cfg, "gammaneedle", "", 8); err != nil || len(res) != 0 {
		t.Fatalf("search stale gammaneedle = %+v err=%v, want zero hits", res, err)
	}
}

// TestIndexUpsertConcurrentWriters is the campaign's core concurrency guarantee:
// ~20 goroutines each writeMemory + indexUpsert a distinct memory in parallel. After
// the fan-in the index row count must equal the vault file count, every memory must be
// searchable, no writer may surface a raw "database is locked", and index_meta stays
// consistent. Run with -race (CI does) to shake out data races on the shared handle path.
func TestIndexUpsertConcurrentWriters(t *testing.T) {
	// Seed one memory + a rebuilt, identity-bound index so every concurrent call
	// takes the incremental fast path (an empty/unbound index would delegate to a
	// full rebuild — the very serialization this feature removes).
	cfg := coreBIdxpopulatedVault(t, "v_upsert_conc",
		[]Memory{coreBIdxmem("mem_seed", "global", "insight", "Seed", "seed body")})
	ctx := context.Background()

	const workers = 20
	var wg sync.WaitGroup
	errs := make([]error, workers)
	tokens := make([]string, workers)
	for i := 0; i < workers; i++ {
		i := i
		token := fmt.Sprintf("concneedle%02d", i)
		tokens[i] = token
		m := coreBIdxmem(fmt.Sprintf("mem_conc%02d", i), "global", "insight",
			fmt.Sprintf("Concurrent %02d", i), token+" parallel write body")
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeMemory(cfg, m); err != nil {
				errs[i] = fmt.Errorf("writeMemory: %w", err)
				return
			}
			errs[i] = indexUpsert(ctx, cfg, m)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d error: %v", i, err)
		}
		if err != nil && strings.Contains(err.Error(), "database is locked") {
			t.Errorf("worker %d surfaced a raw lock error: %v", i, err)
		}
	}
	if t.Failed() {
		t.Fatalf("concurrent writers surfaced errors")
	}

	// Index row count must equal the number of vault memory files (seed + workers).
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantRows := len(files)
	if wantRows != workers+1 {
		t.Fatalf("vault file count = %d, want %d (seed + %d workers)", wantRows, workers+1, workers)
	}
	if got := idxUpsertMemCount(t, cfg); got != wantRows {
		t.Fatalf("index memories rows = %d, want %d (== vault file count)", got, wantRows)
	}
	if got := idxUpsertMetaCount(t, cfg); got != fmt.Sprintf("%d", wantRows) {
		t.Fatalf("index_meta memory_count = %q, want %d", got, wantRows)
	}

	// Every concurrently-written memory must be individually searchable.
	for i, token := range tokens {
		res, err := searchMemories(ctx, cfg, token, "", 8)
		if err != nil {
			t.Fatalf("search %q: %v", token, err)
		}
		if len(res) != 1 || res[0].ID != fmt.Sprintf("mem_conc%02d", i) {
			t.Fatalf("search %q = %+v, want exactly mem_conc%02d", token, res, i)
		}
	}
}

// TestIndexUpsertBlockedVaultDegrades pins the vault-identity guard on the upsert
// path: when the on-disk marker names a different vault than the index was built
// from, indexUpsert must NOT touch the index and must return errRebuildBlocked (the
// same sentinel callers already degrade on), leaving today's write-path semantics
// intact (vault write durable, index untouched, warning surfaced).
func TestIndexUpsertBlockedVaultDegrades(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_marker_alpha",
		[]Memory{coreBIdxmem("mem_x", "global", "insight", "X", "body x")})
	ctx := context.Background()
	// Marker on disk = v_marker_alpha; force the index to claim a different origin.
	idxUpsertStampVaultID(t, cfg, "v_index_beta")

	before := idxUpsertMemCount(t, cfg)
	m := coreBIdxmem("mem_blocked", "global", "insight", "Blocked", "should not be indexed")
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	err := indexUpsert(ctx, cfg, m)
	if !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("indexUpsert on identity mismatch = %v, want errRebuildBlocked", err)
	}
	if after := idxUpsertMemCount(t, cfg); after != before {
		t.Fatalf("index row count changed on a blocked upsert: %d -> %d (index must be untouched)", before, after)
	}
	// A block record is written so `mora doctor` can surface it (mirrors rebuild).
	if _, present, _ := readBlockRecord(cfg); !present {
		t.Fatalf("expected a last-rebuild-block.json advisory after a blocked upsert")
	}
}

// TestIndexUpsertColdStartDelegatesToRebuild pins that when there is no usable index
// yet (fresh install: the very first authored write), indexUpsert delegates to the
// full rebuild — producing a COMPLETE schema (graph + vectors, not a partial index)
// and a bound vault identity — rather than writing a half-built index.
func TestIndexUpsertColdStartDelegatesToRebuild(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "") // deterministic static embedder
	cfg := sandboxCfg(t)
	ctx := context.Background()

	m := coreBIdxmem("mem_first", "global", "insight", "First", "coldneedle first ever write")
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := indexUpsert(ctx, cfg, m); err != nil {
		t.Fatalf("cold-start indexUpsert: %v", err)
	}

	// The memory is indexed and searchable.
	if got := idxUpsertMemCount(t, cfg); got != 1 {
		t.Fatalf("memories rows = %d, want 1", got)
	}
	if res, err := searchMemories(ctx, cfg, "coldneedle", "", 8); err != nil || len(res) != 1 {
		t.Fatalf("search coldneedle = %+v err=%v, want one hit", res, err)
	}
	// Delegation built the FULL schema: the vector table exists and is populated.
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var vecs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mem_vectors`).Scan(&vecs); err != nil {
		t.Fatalf("mem_vectors missing after cold-start (partial schema?): %v", err)
	}
	if vecs != 1 {
		t.Fatalf("mem_vectors rows = %d, want 1 (full rebuild should populate vectors)", vecs)
	}
	// Identity is bound: a marker exists and the index recorded its id.
	if _, present, err := readVaultMarker(cfg); err != nil || !present {
		t.Fatalf("vault marker present=%v err=%v after cold-start", present, err)
	}
}

// TestAuthoredWriteReconcilesAllProjections proves #316's stronger boundary:
// write_memory is instantly FTS-searchable and its full derived projections are
// reconciled before it reports healthy.
func TestAuthoredWriteReconcilesAllProjections(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	res, err := callMCPTool(ctx, "write_memory", map[string]any{
		"title": "Reconcile all arms", "text": "reconcileallneedle Alice owns the launch decision", "scope": "project:launch", "type": "decision",
	})
	if err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	id, warning, err := parseWriteResult(res)
	if err != nil || id == "" || warning != "" {
		t.Fatalf("write result id=%q warning=%q err=%v", id, warning, err)
	}
	got, err := searchMemories(ctx, cfg, "reconcileallneedle", "", 8)
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("immediate FTS = %+v err=%v, want %s", got, err, id)
	}
	if ops, err := listPendingOps(cfg); err != nil || len(ops) != 0 {
		t.Fatalf("pending after completed reconciliation = %+v err=%v", ops, err)
	}
	if h := indexHealthOf(cfg, time.Now()); h.State != idxFresh {
		t.Fatalf("index health = %+v, want fresh", h)
	}
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var vecs, entities int
	if err := db.QueryRow("SELECT COUNT(*) FROM mem_vectors WHERE memory_id=?", id).Scan(&vecs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&entities); err != nil {
		t.Fatal(err)
	}
	if vecs != 1 {
		t.Fatalf("vectors for %s = %d, want 1", id, vecs)
	}
	if entities == 0 {
		t.Fatal("graph has no entities after reconciliation")
	}
}
