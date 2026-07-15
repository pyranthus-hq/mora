package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type edgeVals struct {
	validFrom     sql.NullString
	observedAt    sql.NullString
	invalidatedAt sql.NullString
	validToNull   bool
}

// readEdges reads the materialized edges keyed by "src|rel|dst|evidence_id".
func readEdges(t *testing.T, cfg Config) map[string]edgeVals {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT src, rel, dst, evidence_id, valid_from, valid_to, observed_at, invalidated_at FROM edges`)
	if err != nil {
		return map[string]edgeVals{}
	}
	defer rows.Close()
	out := map[string]edgeVals{}
	for rows.Next() {
		var src, rel, dst, ev string
		var vf, vt, obs, inv sql.NullString
		if err := rows.Scan(&src, &rel, &dst, &ev, &vf, &vt, &obs, &inv); err != nil {
			t.Fatal(err)
		}
		out[src+"|"+rel+"|"+dst+"|"+ev] = edgeVals{validFrom: vf, observedAt: obs, invalidatedAt: inv, validToNull: !vt.Valid}
	}
	return out
}

type entRow struct {
	kind         string
	display      string
	mentionCount int
}

// readEntities reads the materialized entities table. A missing table yields an
// empty map (so assertions fail as a clean RED rather than erroring).
func readEntities(t *testing.T, cfg Config) map[string]entRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, kind, display_name, mention_count FROM entities`)
	if err != nil {
		return map[string]entRow{}
	}
	defer rows.Close()
	out := map[string]entRow{}
	for rows.Next() {
		var id string
		var e entRow
		if err := rows.Scan(&id, &e.kind, &e.display, &e.mentionCount); err != nil {
			t.Fatal(err)
		}
		out[id] = e
	}
	return out
}

func TestRebuildMaterializesStructuralEntities(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(Memory{ID: "m1", Scope: "project:demo", Title: "Kickoff with [[Neil]]", Tags: []string{"pilot"},
		Text: "Talked to [[Neil]].\n- [Decision] adopt MCP", CreatedAt: "2026-05-30T10:00:00Z"})
	mustWrite(Memory{ID: "m2", Scope: "project:demo", Title: "Follow-up", Tags: []string{"pilot", "urgent"},
		Text: "[[Neil]] confirmed.", CreatedAt: "2026-05-31T10:00:00Z"})

	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	ents := readEntities(t, cfg)
	check := func(id, wantKind, wantDisplay string, wantCount int) {
		t.Helper()
		e, ok := ents[id]
		if !ok {
			t.Fatalf("entity %q missing from entities table (have %d entities)", id, len(ents))
		}
		if e.kind != wantKind || e.display != wantDisplay || e.mentionCount != wantCount {
			t.Fatalf("entity %q = %+v, want kind=%s display=%q count=%d", id, e, wantKind, wantDisplay, wantCount)
		}
	}
	// A "project:" scope maps to spec kind "project"; display preserves the raw name.
	check("scope:project:demo", "project", "project:demo", 2)
	// Tags, [[wikilinks]], and - [Category] lines map to spec kind "topic".
	// Counts are distinct-memory (Neil appears in m1 title+body+m2 -> 2, not 3).
	check("tag:pilot", "topic", "pilot", 2)
	check("tag:urgent", "topic", "urgent", 1)
	check("link:Neil", "topic", "Neil", 2)
	check("category:Decision", "topic", "Decision", 1)
}

func TestRebuildMaterializesStructuralEdges(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(Memory{ID: "m1", Scope: "project:demo", Title: "Kickoff with [[Neil]]", Tags: []string{"pilot"},
		Text: "Talked to [[Neil]].\n- [Decision] adopt MCP", CreatedAt: "2026-05-30T10:00:00Z"})
	mustWrite(Memory{ID: "m2", Scope: "project:demo", Title: "Follow-up", Tags: []string{"urgent"},
		Text: "[[Neil]] confirmed.", CreatedAt: "2026-05-31T10:00:00Z"})
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	edges := readEdges(t, cfg)
	has := func(key string) {
		t.Helper()
		if _, ok := edges[key]; !ok {
			t.Fatalf("missing edge %q (have %d edges)", key, len(edges))
		}
	}
	// [[wikilinks]] are MENTIONS; the title+body double-reference of Neil in m1
	// dedupes to one edge (PK src,rel,dst,evidence_id).
	has("memory:m1|MENTIONS|link:Neil|m1")
	has("memory:m2|MENTIONS|link:Neil|m2")
	// scope, tag, category are ABOUT, anchored on the per-memory hub.
	has("memory:m1|ABOUT|scope:project:demo|m1")
	has("memory:m1|ABOUT|category:Decision|m1")
	has("memory:m2|ABOUT|tag:urgent|m2")
}

func TestGraphBiTemporalStamps(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	// observed_at = last_synced when present.
	mustWrite(Memory{ID: "synced", Scope: "personal", Title: "S", Text: "see [[Foo]]",
		CreatedAt: "2026-05-01T00:00:00Z", LastSynced: "2026-05-02T00:00:00Z"})
	// tombstone -> invalidated_at = deleted_at.
	mustWrite(Memory{ID: "tomb", Scope: "personal", Title: "T", Text: "see [[Bar]]",
		CreatedAt: "2026-04-01T00:00:00Z", DeletedAt: "2026-04-05T00:00:00Z"})
	// no last_synced -> observed_at falls back to created_at; invalidated NULL.
	mustWrite(Memory{ID: "plain", Scope: "personal", Title: "P", Tags: []string{"gamma"}, Text: "x",
		CreatedAt: "2026-03-01T00:00:00Z"})
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	edges := readEdges(t, cfg)
	get := func(key string) edgeVals {
		t.Helper()
		e, ok := edges[key]
		if !ok {
			t.Fatalf("missing edge %q (have %d)", key, len(edges))
		}
		return e
	}
	wantSet := func(ns sql.NullString, want, ctx string) {
		t.Helper()
		if !ns.Valid || ns.String != want {
			t.Fatalf("%s: got %+v, want %q", ctx, ns, want)
		}
	}
	wantNull := func(ns sql.NullString, ctx string) {
		t.Helper()
		if ns.Valid {
			t.Fatalf("%s: got %q, want NULL", ctx, ns.String)
		}
	}

	s := get("memory:synced|MENTIONS|link:Foo|synced")
	wantSet(s.validFrom, "2026-05-01T00:00:00Z", "synced.valid_from")
	wantSet(s.observedAt, "2026-05-02T00:00:00Z", "synced.observed_at(last_synced)")
	wantNull(s.invalidatedAt, "synced.invalidated_at")
	if !s.validToNull {
		t.Fatal("valid_to must be NULL in I1")
	}

	tb := get("memory:tomb|MENTIONS|link:Bar|tomb")
	wantSet(tb.invalidatedAt, "2026-04-05T00:00:00Z", "tomb.invalidated_at(deleted_at)")

	p := get("memory:plain|ABOUT|tag:gamma|plain")
	wantSet(p.observedAt, "2026-03-01T00:00:00Z", "plain.observed_at(falls back to created_at)")
	wantNull(p.invalidatedAt, "plain.invalidated_at")
}

func TestBuildGraphDeterministic(t *testing.T) {
	mems := []Memory{
		{ID: "a", Scope: "project:x", Tags: []string{"t2", "t1"}, Title: "[[Zeta]]", Text: "[[Alpha]] and [[Zeta]]\n- [Cat]", CreatedAt: "2026-01-02T00:00:00Z"},
		{ID: "b", Scope: "personal", Tags: []string{"t1"}, Text: "[[Alpha]]", CreatedAt: "2026-01-01T00:00:00Z", LastSynced: "2026-01-03T00:00:00Z"},
		{ID: "c", Scope: "project:x", Text: "no entities", CreatedAt: "2026-01-04T00:00:00Z"},
	}
	e1, g1, _ := buildGraph(mems)
	e2, g2, _ := buildGraph(mems)
	if !reflect.DeepEqual(e1, e2) {
		t.Fatalf("entities nondeterministic:\n%+v\n%+v", e1, e2)
	}
	if !reflect.DeepEqual(g1, g2) {
		t.Fatalf("edges nondeterministic:\n%+v\n%+v", g1, g2)
	}
}

func TestListEntitiesIsGraphBacked(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(Memory{ID: "m1", Scope: "project:demo", Title: "Kickoff with [[Neil]]", Tags: []string{"pilot"},
		Text: "[[Neil]]\n- [Decision] adopt", CreatedAt: "2026-05-30T10:00:00Z"})
	mustWrite(Memory{ID: "m2", Scope: "project:demo", Title: "F", Tags: []string{"urgent"},
		Text: "[[Neil]] ok", CreatedAt: "2026-05-31T10:00:00Z"})
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Remove every vault file: a file-scan implementation would now find nothing;
	// a graph-backed read still answers from the materialized entities/edges tables.
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}

	ents, err := entitiesForMCP(ctx, cfg, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	find := func(kind, name string) *Entity {
		for i := range ents {
			if ents[i].Kind == kind && ents[i].Name == name {
				return &ents[i]
			}
		}
		return nil
	}
	if e := find("link", "Neil"); e == nil || e.Count != 2 {
		t.Fatalf("link Neil should survive file removal from the table: %+v", e)
	}
	if e := find("scope", "project:demo"); e == nil || e.Count != 2 {
		t.Fatalf("scope project:demo: %+v", e)
	}
	if e := find("tag", "urgent"); e == nil || e.Count != 1 {
		t.Fatalf("tag urgent: %+v", e)
	}
	// Per-memory hub nodes / spec kinds must NOT leak into the legacy list.
	for _, e := range ents {
		if e.Kind == "thread" || e.Kind == "event" || strings.HasPrefix(e.Name, "memory:") {
			t.Fatalf("hub entity leaked into list_entities: %+v", e)
		}
	}
}

func TestGetEntityReturnsGraphProvenance(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(Memory{ID: "m1", Scope: "project:demo", Title: "Kickoff with [[Neil]]", Tags: []string{"pilot"},
		Text: "[[Neil]]", CreatedAt: "2026-05-30T10:00:00Z"})
	mustWrite(Memory{ID: "m2", Scope: "project:demo", Title: "F", Text: "[[Neil]] ok", CreatedAt: "2026-05-31T10:00:00Z"})
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	res, err := entityMemoriesForMCP(ctx, cfg, "neil") // case-insensitive, like the old path
	if err != nil {
		t.Fatal(err)
	}
	// Legacy fields preserved exactly.
	if res["found"] != true {
		t.Fatalf("found = %v, want true", res["found"])
	}
	if res["kind"] != "link" {
		t.Fatalf("kind = %v, want link (legacy)", res["kind"])
	}
	if res["count"] != 2 {
		t.Fatalf("count = %v, want 2", res["count"])
	}
	evidence, ok := res["evidence"].([]EntityEvidence)
	if !ok {
		if rows, ok2 := res["evidence"].([]any); !ok2 || len(rows) != 2 {
			t.Fatalf("evidence = %v, want 2 cited rows", res["evidence"])
		}
	} else if len(evidence) != 2 {
		t.Fatalf("evidence = %v, want 2 cited rows", evidence)
	}
	if res["budget_unit"] != budgetUnitTokens {
		t.Fatalf("budget_unit = %v", res["budget_unit"])
	}
	// Graph provenance extras (the new value-add).
	if res["graph_kind"] != "topic" {
		t.Fatalf("graph_kind = %v, want topic", res["graph_kind"])
	}
	edges, ok := res["edges"].([]map[string]any)
	if !ok || len(edges) < 2 {
		t.Fatalf("edges extra missing/short: %v", res["edges"])
	}
	for _, e := range edges {
		if e["rel"] != "MENTIONS" {
			t.Fatalf("edge rel = %v, want MENTIONS", e["rel"])
		}
		if s, _ := e["evidence_id"].(string); s == "" {
			t.Fatalf("edge missing evidence_id: %v", e)
		}
	}
}

// TestMCPEntitiesRoundTrip exercises the agent-facing list_entities + get_entity
// MCP tools end-to-end (JSON-RPC over the stdio server), now graph-backed.
func TestMCPEntitiesRoundTrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, Memory{ID: "m1", Scope: "project:demo", Title: "Kickoff with [[Neil]]", Tags: []string{"pilot"}, Text: "[[Neil]] sync", CreatedAt: "2026-05-30T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, Memory{ID: "m2", Scope: "project:demo", Title: "F", Text: "[[Neil]] ok", CreatedAt: "2026-05-31T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	listText, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_entities","arguments":{}}}`)
	if isErr {
		t.Fatalf("list_entities unexpectedly isError; text=%s", listText)
	}
	var listRes struct {
		Entities []Entity      `json:"entities"`
		Health   compactHealth `json:"health"`
	}
	if err := json.Unmarshal([]byte(listText), &listRes); err != nil {
		t.Fatalf("list_entities text decode: %v\n%s", err, listText)
	}
	ents := listRes.Entities
	foundNeil := false
	for _, e := range ents {
		if e.Kind == "link" && e.Name == "Neil" && e.Count == 2 {
			foundNeil = true
		}
		if strings.HasPrefix(e.Name, "memory:") || e.Kind == "thread" || e.Kind == "event" {
			t.Fatalf("hub node leaked through MCP list_entities: %+v", e)
		}
	}
	if !foundNeil {
		t.Fatalf("MCP list_entities missing link Neil count 2: %+v", ents)
	}

	getText, isErr2 := mcpToolText(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_entity","arguments":{"name":"Neil"}}}`)
	if isErr2 {
		t.Fatalf("get_entity unexpectedly isError; text=%s", getText)
	}
	var getWrapped struct {
		Entity map[string]any `json:"entity"`
		Health compactHealth  `json:"health"`
	}
	if err := json.Unmarshal([]byte(getText), &getWrapped); err != nil {
		t.Fatalf("get_entity text decode: %v\n%s", err, getText)
	}
	getRes := getWrapped.Entity
	if getRes["found"] != true {
		t.Fatalf("get_entity found: %+v", getRes)
	}
	if getRes["graph_kind"] != "topic" {
		t.Fatalf("get_entity graph_kind: %+v", getRes["graph_kind"])
	}
	evidence, ok := getRes["evidence"].([]any)
	if !ok || len(evidence) < 2 {
		t.Fatalf("get_entity cited evidence: %+v", getRes["evidence"])
	}
	if getRes["budget_unit"] != budgetUnitTokens {
		t.Fatalf("get_entity budget_unit: %+v", getRes["budget_unit"])
	}
}
