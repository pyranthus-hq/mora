package mora

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Finding (Codex P1): a pre-S1 index.db has memories+fts but no entities/edges
// tables. ensureIndexDB only rebuilds when the file is MISSING, so an upgraded
// user's first graph read errors "no such table: entities". It must self-heal.
func TestGraphReadSelfHealsPreS1Index(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{ID: "m1", Scope: "personal", Title: "T", Text: "[[Neil]]", CreatedAt: "2026-05-30T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-S1 index.db: drop the graph tables, leaving only memories+fts.
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE entities`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE edges`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ents, err := entitiesForMCP(ctx, cfg, "", 0)
	if err != nil {
		t.Fatalf("graph read on a pre-S1 index.db must self-heal, not error: %v", err)
	}
	found := false
	for _, e := range ents {
		if e.Kind == "link" && e.Name == "Neil" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected link Neil after self-heal rebuild, got %+v", ents)
	}
}

// Finding (Codex P1 / wf): hub ids use SafeFilename, which maps "/", ":", " " all
// to "_" -> distinct StableIDs collapse to one hub node + edge src.
func TestBuildGraphHubIDsAreInjective(t *testing.T) {
	mems := []Memory{
		{ID: "x/y", Scope: "personal", Title: "A", Text: "[[Shared]]", CreatedAt: "2026-05-01T00:00:00Z"},
		{ID: "x_y", Scope: "personal", Title: "B", Text: "[[Shared]]", CreatedAt: "2026-05-02T00:00:00Z"},
	}
	ents, edges, _ := buildGraph(mems)
	hubs := map[string]bool{}
	for _, e := range ents {
		if strings.HasPrefix(e.ID, "memory:") {
			hubs[e.ID] = true
		}
	}
	if len(hubs) != 2 {
		t.Fatalf("distinct memories x/y and x_y collapsed to hub ids %v (want 2)", hubs)
	}
	srcs := map[string]bool{}
	for _, ed := range edges {
		if ed.Dst == "link:Shared" {
			srcs[ed.Src] = true
		}
	}
	if len(srcs) != 2 {
		t.Fatalf("edge srcs collapsed to %v (want 2 distinct hubs)", srcs)
	}
}

// Finding (Codex P1 / wf): stored entity stats count tombstoned evidence while
// every live read filters invalidated_at IS NULL -> stored row disagrees.
func TestGraphStatsExcludeTombstones(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, Memory{ID: "live1", Scope: "personal", Tags: []string{"shared"}, Title: "L", Text: "x", CreatedAt: "2026-05-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, Memory{ID: "dead1", Scope: "personal", Tags: []string{"shared"}, Title: "D", Text: "y", CreatedAt: "2026-04-01T00:00:00Z", DeletedAt: "2026-04-05T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ents := readEntities(t, cfg)
	if e := ents["tag:shared"]; e.mentionCount != 1 {
		t.Fatalf("stored mention_count for tag:shared = %d, want 1 (tombstone excluded, matching live reads)", e.mentionCount)
	}
}

// Finding (Codex P1 / wf): get_entity resolves from the stored entities row, so a
// tombstone-only entity returns found:true,count:0,memories:[] while list drops it.
func TestGetEntityTombstoneOnlyIsNotFound(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, Memory{ID: "dead", Scope: "personal", Tags: []string{"ghost"}, Title: "G", Text: "x", CreatedAt: "2026-04-01T00:00:00Z", DeletedAt: "2026-04-05T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	res, err := entityMemoriesForMCP(ctx, cfg, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if res["found"] != false {
		t.Fatalf("a tombstone-only entity must be found:false, got %+v", res)
	}
}

// Finding (Codex P1 / wf): graph-backed get_entity dropped Memory fields (provider,
// provider_id, last_synced, ...) that the old listMemories path returned.
func TestGetEntityPreservesMemoryFields(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, Memory{ID: "g1", Scope: "personal", Type: "email", Title: "T", Text: "[[Neil]]",
		CreatedAt: "2026-05-01T00:00:00Z", Provider: "gmail", ProviderID: "gmail_thread/abc", LastSynced: "2026-05-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	res, err := entityMemoriesForMCP(ctx, cfg, "Neil")
	if err != nil {
		t.Fatal(err)
	}
	mems, ok := res["memories"].([]Memory)
	if !ok || len(mems) != 1 {
		t.Fatalf("memories = %v", res["memories"])
	}
	m := mems[0]
	if m.Provider != "gmail" || m.ProviderID != "gmail_thread/abc" || m.LastSynced != "2026-05-02T00:00:00Z" {
		t.Fatalf("get_entity dropped Memory fields: provider=%q provider_id=%q last_synced=%q", m.Provider, m.ProviderID, m.LastSynced)
	}
}
