package mora

import (
	"testing"
)

// TestTombstoneExcludedFromSearch is the P1 regression guard for the
// tombstone-leak: connector-produced tombstones (a memory file carrying a
// deleted_at, e.g. a cancelled calendar event) must NOT surface in retrieval.
// digest/graph/salience already skip DeletedAt != ""; searchMemories (and thus
// the read-only recall hook, which calls it) did not. The fix excludes
// tombstones from the searchable `memories`/`memories_fts`/`mem_vectors` tables
// at index-build, which is the single chokepoint every search arm JOINs through.
func TestTombstoneExcludedFromSearch(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "") // deterministic static-hash; keep it hermetic
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	// Two memories sharing a distinctive term; one is tombstoned.
	live := Memory{ID: "synth/live", Scope: "global", Type: "note", Title: "Quarterly zephyrquux plan",
		CreatedAt: "2026-01-01T00:00:00Z", Source: "obsidian", Text: "The zephyrquux rollout is on track."}
	dead := Memory{ID: "synth/dead", Scope: "global", Type: "note", Title: "Old zephyrquux draft",
		CreatedAt: "2026-01-02T00:00:00Z", Source: "obsidian", Text: "The zephyrquux rollout was cancelled.",
		DeletedAt: "2026-01-03T00:00:00Z"}
	for _, m := range []Memory{live, dead} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}

	// 1. FTS search (the recall-hook path) must return the live memory and never the tombstone.
	got, err := searchMemories(ctx, cfg, "zephyrquux", "", 10)
	if err != nil {
		t.Fatalf("searchMemories: %v", err)
	}
	var sawLive bool
	for _, m := range got {
		if m.ID == "synth/dead" {
			t.Errorf("tombstone synth/dead leaked into searchMemories results: %+v", got)
		}
		if m.ID == "synth/live" {
			sawLive = true
		}
	}
	if !sawLive {
		t.Errorf("live memory synth/live missing from search results: %+v", got)
	}

	// 2. Chokepoint: the tombstone must not be in the `memories` table at all
	//    (so the FTS / vector / graph JOINs all drop it), while the live one is.
	db := openRO(t, cfg)
	defer db.Close()
	if ok, err := existsInMemoriesTable(ctx, db, "synth/dead"); err != nil || ok {
		t.Errorf("existsInMemoriesTable(synth/dead) = (%v, %v), want (false, nil) — tombstone must not be indexed", ok, err)
	}
	if ok, err := existsInMemoriesTable(ctx, db, "synth/live"); err != nil || !ok {
		t.Errorf("existsInMemoriesTable(synth/live) = (%v, %v), want (true, nil)", ok, err)
	}

	// 3. The tombstone must also be absent from the FTS index and the vector table
	//    (defense in depth: not relying solely on the JOIN to memories).
	var ftsN, vecN int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memories_fts WHERE id = ?`, "synth/dead").Scan(&ftsN); err != nil {
		t.Fatalf("count memories_fts: %v", err)
	}
	if ftsN != 0 {
		t.Errorf("tombstone synth/dead present in memories_fts (%d rows), want 0", ftsN)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mem_vectors WHERE memory_id = ?`, "synth/dead").Scan(&vecN); err != nil {
		t.Fatalf("count mem_vectors: %v", err)
	}
	if vecN != 0 {
		t.Errorf("tombstone synth/dead present in mem_vectors (%d rows), want 0", vecN)
	}
}

// TestTombstoneExcludedFromListMemories closes the same leak on the raw browse /
// session-start surfaces: listMemories backs `mora list`, list_memory (MCP), and
// the no-query context_memory fallback (session-start briefing). A connector
// tombstone (e.g. a cancelled calendar event) must not surface there as a "recent
// memory". findMemory (explicit by-id read, used by delete/read mutation paths)
// is intentionally NOT filtered — fetching a known id is not a context surface.
func TestTombstoneExcludedFromListMemories(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	live := Memory{ID: "live-note", Scope: "global", Type: "note", Title: "Live note",
		CreatedAt: "2026-01-01T00:00:00Z", Source: "obsidian", Text: "still here"}
	dead := Memory{ID: "dead-note", Scope: "global", Type: "note", Title: "Dead note",
		CreatedAt: "2026-01-02T00:00:00Z", Source: "obsidian", Text: "cancelled", DeletedAt: "2026-01-03T00:00:00Z"}
	for _, m := range []Memory{live, dead} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}

	got, err := listMemories(cfg, "", 100)
	if err != nil {
		t.Fatalf("listMemories: %v", err)
	}
	var sawLive bool
	for _, m := range got {
		if m.ID == "dead-note" {
			t.Errorf("tombstone dead-note leaked into listMemories: %+v", got)
		}
		if m.ID == "live-note" {
			sawLive = true
		}
	}
	if !sawLive {
		t.Errorf("live memory live-note missing from listMemories: %+v", got)
	}

	// findMemory is a deliberate exception: an explicit by-id read still resolves a
	// tombstone (delete/read paths depend on it), and the resolved record carries
	// its deleted_at so the caller can see it is a tombstone.
	m, err := findMemory(cfg, "dead-note")
	if err != nil {
		t.Fatalf("findMemory(dead-note) should still resolve by id: %v", err)
	}
	if m.DeletedAt == "" {
		t.Errorf("findMemory(dead-note) lost the deleted_at marker: %+v", m)
	}
}
