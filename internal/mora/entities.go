package mora

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Entity is a thing the vault refers to repeatedly — a scope (project/namespace),
// a tag, a [[wikilink]], or a "- [Category]" line. It's the read-only,
// deterministic first cut of the entity graph (I1): no NLP, no schema change, just
// the structure already present in the Markdown.
type Entity struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // scope | tag | link | category
	Count     int      `json:"count"`
	MemoryIDs []string `json:"memory_ids,omitempty"`
	// Salience is the frozen person-ranking sort key (salience_micros, int64) written
	// by buildGraph into the entities table (Phase 14). It is carried for the graph
	// read/ranking path (14-04 populates it from the column and ranks person-kind on
	// it); the existing read leaves it 0, so structural entities and the current
	// surfaces are unaffected. omitempty keeps it out of JSON for non-person entities.
	Salience int64 `json:"salience,omitempty"`
}

var (
	wikilinkRe = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)
	categoryRe = regexp.MustCompile(`(?m)^\s*-\s*\[([^\[\]]+)\]`)
)

// extractEntities aggregates entities across the given memories. Counts are
// distinct-memory counts (a [[link]] used twice in one memory counts once).
// Sorted by Count desc, then Kind, then Name — stable and demo-friendly.
func extractEntities(mems []Memory) []Entity {
	type key struct{ kind, name string }
	seen := map[key]map[string]bool{} // (kind,name) -> set of memory IDs

	add := func(kind, name, id string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		k := key{kind, name}
		if seen[k] == nil {
			seen[k] = map[string]bool{}
		}
		seen[k][id] = true
	}

	for _, m := range mems {
		add("scope", m.Scope, m.ID)
		for _, t := range m.Tags {
			add("tag", t, m.ID)
		}
		hay := m.Title + "\n" + m.Text
		for _, mm := range wikilinkRe.FindAllStringSubmatch(hay, -1) {
			add("link", mm[1], m.ID)
		}
		for _, loc := range categoryRe.FindAllStringSubmatchIndex(m.Text, -1) {
			name := m.Text[loc[2]:loc[3]]
			if isCheckboxMarker(name) {
				continue
			}
			// "- [Title](url)" is a Markdown link, not a category: skip when the
			// closing ] is immediately followed by (.
			if loc[1] < len(m.Text) && m.Text[loc[1]] == '(' {
				continue
			}
			add("category", name, m.ID)
		}
	}

	out := make([]Entity, 0, len(seen))
	for k, ids := range seen {
		idList := make([]string, 0, len(ids))
		for id := range ids {
			idList = append(idList, id)
		}
		sort.Strings(idList)
		out = append(out, Entity{Name: k.name, Kind: k.kind, Count: len(idList), MemoryIDs: idList})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// isCheckboxMarker reports whether a "- [x]" bracket body is a task checkbox
// (empty, space, or x/X) rather than a real category name.
func isCheckboxMarker(s string) bool {
	switch strings.TrimSpace(s) {
	case "", "x", "X":
		return true
	}
	return false
}

// cmdEntities implements `mora entities [name] [--json]`: a read-only view of the
// vault's entity graph. With a name, it filters to memories referencing that
// entity (matched by name across any kind).
func cmdEntities(ctx context.Context, args []string, stdout io.Writer) error {
	// The optional entity name is positional and may sit before OR after --json
	// (Go's flag package stops at the first positional), so split flags out by hand.
	jsonOut := false
	var positional []string
	for _, a := range args {
		switch a {
		case "--json", "-json", "--json=true":
			jsonOut = true
		default:
			positional = append(positional, a)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	entities, err := graphListEntities(ctx, cfg)
	if err != nil {
		return err
	}

	if len(positional) > 0 {
		name := strings.Join(positional, " ")
		return entityDetailGraph(ctx, cfg, stdout, entities, name, jsonOut)
	}

	if jsonOut {
		return emit(stdout, entities, true)
	}
	if len(entities) == 0 {
		fmt.Fprintln(stdout, "No entities found yet. Ingest some memories first (mora ingest run --all).")
		return nil
	}
	printEntities(stdout, entities)
	return nil
}

func printEntities(w io.Writer, entities []Entity) {
	order := []string{"person", "scope", "link", "category", "tag"}
	labels := map[string]string{"person": "People", "scope": "Scopes", "link": "Links", "category": "Categories", "tag": "Tags"}
	for _, kind := range order {
		var group []Entity
		for _, e := range entities {
			if e.Kind == kind {
				group = append(group, e)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", labels[kind])
		for _, e := range group {
			fmt.Fprintf(w, "  %-32s %d\n", e.Name, e.Count)
		}
	}
	fmt.Fprintln(w, "\nTip: `mora entities \"<name>\"` shows what's known about one of them.")
}

// entityDetailGraph prints/returns the memories referencing the named entity,
// graph-backed (loads them from the index table, not a file rescan). `entities`
// is the already-sorted graphListEntities output, so the first case-insensitive
// name match is the most-mentioned one.
func entityDetailGraph(ctx context.Context, cfg Config, w io.Writer, entities []Entity, name string, jsonOut bool) error {
	var match *Entity
	for i := range entities {
		if strings.EqualFold(entities[i].Name, name) {
			match = &entities[i]
			break
		}
	}
	if match == nil {
		if jsonOut {
			return emit(w, map[string]any{"name": name, "found": false, "memories": []Memory{}}, true)
		}
		fmt.Fprintf(w, "No entity named %q.\n", name)
		return nil
	}
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	refs, err := loadMemoriesByID(ctx, db, match.MemoryIDs)
	if err != nil {
		return err
	}
	if jsonOut {
		return emit(w, map[string]any{"name": match.Name, "kind": match.Kind, "count": match.Count, "found": true, "memories": refs}, true)
	}
	fmt.Fprintf(w, "%s (%s) — %d memories\n", match.Name, match.Kind, match.Count)
	for _, m := range refs {
		fmt.Fprintf(w, "  [%s] %s\n", m.ID, m.Title)
	}
	return nil
}

// MCP get_entity caps: keep the agent-facing detail view well under the token
// ceiling. The most-recent N referencing memories (bodies snippeted) plus bounded
// incoming edges/neighbors; true totals stay in "count"/"degree".
const (
	mcpEntityMemoriesCap  = 20
	mcpEntityEdgesCap     = 25
	mcpEntityNeighborsCap = 15
)

// entitiesForMCP returns the entity list for the list_entities MCP tool,
// graph-backed (materialized entities/edges tables), token-bounded for the agent
// surface: the per-entity memory_ids arrays are dropped (they dominate the payload —
// thousands of ids on a busy scope, ~450 KB on a real vault — and an agent that needs
// them calls get_entity), rows are ranked by salience so the most-relevant lead, an
// optional kind filters the set, and limit caps the count (the full 700+ entity list
// would otherwise edge over the token ceiling). limit<=0 means unlimited (CLI/tests).
// The CLI keeps full fidelity by calling graphListEntities directly.
func entitiesForMCP(ctx context.Context, cfg Config, kind string, limit int) ([]Entity, error) {
	all, err := graphListEntities(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ents := make([]Entity, 0, len(all))
	for _, e := range all {
		if kind != "" && !strings.EqualFold(e.Kind, kind) {
			continue
		}
		e.MemoryIDs = nil
		ents = append(ents, e)
	}
	sort.SliceStable(ents, func(i, j int) bool {
		if ents[i].Salience != ents[j].Salience {
			return ents[i].Salience > ents[j].Salience
		}
		if ents[i].Count != ents[j].Count {
			return ents[i].Count > ents[j].Count
		}
		return ents[i].Name < ents[j].Name
	})
	if limit > 0 && len(ents) > limit {
		ents = ents[:limit]
	}
	return ents, nil
}

// entityMemoriesForMCP returns the memories referencing a named entity plus graph
// provenance (incoming edges, aliases, degree) for the get_entity MCP tool. Token-
// bounded for the agent surface: bodies are snippeted and capped to the most-recent
// mcpEntityMemoriesCap (a high-degree person otherwise dumps hundreds of KB / ~227k
// tokens of full bodies). The true total stays in "count"; the CLI keeps full bodies.
func entityMemoriesForMCP(ctx context.Context, cfg Config, name string) (map[string]any, error) {
	res, err := graphGetEntity(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	if mems, ok := res["memories"].([]Memory); ok && len(mems) > 0 {
		mems = append([]Memory(nil), mems...) // copy before the in-place sort — never reorder the caller's slice
		sort.SliceStable(mems, func(i, j int) bool { return mems[i].CreatedAt > mems[j].CreatedAt })
		if len(mems) > mcpEntityMemoriesCap {
			mems = mems[:mcpEntityMemoriesCap]
			res["memories_truncated"] = true
		}
		res["memories"] = snippetMemories(mems)
	}
	// A high-degree person otherwise dumps every incoming edge + co-occurring
	// neighbor; cap both (true totals stay in "degree"/"count").
	if edges, ok := res["edges"].([]map[string]any); ok && len(edges) > mcpEntityEdgesCap {
		res["edges"] = edges[:mcpEntityEdgesCap]
		res["edges_truncated"] = true
	}
	if nbrs, ok := res["neighbors"].([]map[string]any); ok && len(nbrs) > mcpEntityNeighborsCap {
		res["neighbors"] = nbrs[:mcpEntityNeighborsCap]
		res["neighbors_truncated"] = true
	}
	return res, nil
}
