package mora

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
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
	printHealthBannerLine(stdout, cfg, time.Now())
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
		printHealthBannerLine(w, cfg, time.Now())
		fmt.Fprintf(w, "No entity named %q.\n", name)
		return nil
	}
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	refs, err := loadMemoriesByID(ctx, cfg, db, match.MemoryIDs)
	if err != nil {
		return err
	}
	if jsonOut {
		return emit(w, map[string]any{"name": match.Name, "kind": match.Kind, "count": match.Count, "found": true, "memories": refs}, true)
	}
	printHealthBannerLine(w, cfg, time.Now())
	fmt.Fprintf(w, "%s (%s) — %d memories\n", match.Name, match.Kind, match.Count)
	for _, m := range refs {
		fmt.Fprintf(w, "  [%s] %s\n", m.ID, m.Title)
	}
	return nil
}

// MCP get_entity caps (fallback when no max_tokens): keep the agent-facing detail
// view well under the token ceiling. True totals stay in "count"/"degree".
const (
	mcpEntityMemoriesCap  = 20
	mcpEntityEdgesCap     = 25
	mcpEntityNeighborsCap = 15
)

// neighborTypeStub is the placeholder neighbor kind until Track A (#70) lands
// real person/org/repo/service/artifact typing. T0 pins dossier shape, not values.
const neighborTypeStub = "person"

// EntityEvidence is one cited memory line in a get_entity dossier — every
// surfaced fact carries {id, channel, date} so agents never read uncited bodies.
type EntityEvidence struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
	Snippet   string `json:"snippet"`
}

// EntityNeighbor is a typed 1-hop co-occurrence neighbor in the dossier.
type EntityNeighbor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

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

// entityDossierForMCP returns a budget-bounded, fully-cited entity dossier for
// the get_entity MCP tool: merged identities (aliases), typed neighbors, top-N
// cited evidence titles, and salience. Raw memory bodies are never shipped —
// expand via read_memory using each evidence id.
func entityDossierForMCP(ctx context.Context, cfg Config, name string, maxTokens int) (map[string]any, error) {
	tokenBudget, _ := resolveContextBudgetTokens(cfg, maxTokens)
	budgetChars := mcpEntityBudgetChars(cfg, maxTokens)
	raw, err := graphGetEntity(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	return buildEntityDossierPayload(raw, name, tokenBudget, budgetChars), nil
}

// entityMemoriesForMCP is the test/compat entry point — delegates to the dossier
// builder with the default token budget.
func entityMemoriesForMCP(ctx context.Context, cfg Config, name string) (map[string]any, error) {
	return entityDossierForMCP(ctx, cfg, name, 0)
}

func evidenceSource(m Memory) string {
	if m.Source != "" {
		return m.Source
	}
	if m.Provider != "" {
		return m.Provider
	}
	return m.Type
}

func memoryToEvidence(m Memory, center string) EntityEvidence {
	snipped := snippetMemories([]Memory{m}, center)
	snippet := ""
	if len(snipped) > 0 {
		snippet = snipped[0].Text
	}
	return EntityEvidence{
		ID:        m.ID,
		Title:     m.Title,
		Source:    evidenceSource(m),
		CreatedAt: m.CreatedAt,
		Snippet:   snippet,
	}
}

func buildEntityDossierPayload(raw map[string]any, queryName string, tokenBudget, budgetChars int) map[string]any {
	out := map[string]any{
		"budget_unit": budgetUnitTokens,
		"budget":      tokenBudget,
	}
	if raw["found"] != true {
		out["name"] = queryName
		out["found"] = false
		out["used"] = 0
		return out
	}
	out["name"] = raw["name"]
	out["display_name"] = raw["display_name"]
	out["kind"] = raw["kind"]
	out["found"] = true
	out["count"] = raw["count"]
	if gk, ok := raw["graph_kind"]; ok {
		out["graph_kind"] = gk
	}
	if sal, ok := raw["salience"].(int64); ok && sal > 0 {
		out["salience"] = sal
	}
	if deg, ok := raw["degree"]; ok {
		out["degree"] = deg
	}
	aliases, _ := raw["aliases"].([]string)
	if aliases == nil {
		aliases = []string{}
	}
	out["aliases"] = aliases

	// Cited evidence from memories, newest first (already sorted in graphGetEntity).
	var evidence []EntityEvidence
	if mems, ok := raw["memories"].([]Memory); ok {
		for _, m := range mems {
			evidence = append(evidence, memoryToEvidence(m, queryName))
		}
	}
	evidence, evidenceTrunc := budgetEntityEvidence(evidence, budgetChars/2)
	out["evidence"] = evidence
	if evidenceTrunc {
		out["evidence_truncated"] = true
	}

	// Typed neighbors — kind stubbed until Track A.
	neighbors := []EntityNeighbor{}
	if nbrs, ok := raw["neighbors"].([]map[string]any); ok {
		for _, n := range nbrs {
			neighbors = append(neighbors, EntityNeighbor{
				ID:          fmt.Sprint(n["id"]),
				DisplayName: fmt.Sprint(n["display_name"]),
				Type:        neighborTypeStub,
			})
		}
	}
	if len(neighbors) > mcpEntityNeighborsCap {
		neighbors = neighbors[:mcpEntityNeighborsCap]
		out["neighbors_truncated"] = true
	}
	out["neighbors"] = neighbors

	// Incoming edges (provenance), capped.
	edges := []map[string]any{}
	if rawEdges, ok := raw["edges"].([]map[string]any); ok {
		edges = append(edges, rawEdges...)
	}
	if len(edges) > mcpEntityEdgesCap {
		edges = edges[:mcpEntityEdgesCap]
		out["edges_truncated"] = true
	}
	out["edges"] = edges

	usedBytes := jsonLen(out)
	out["used"] = estimateTokensUsed(usedBytes)
	return out
}

// budgetEntityEvidence greedily keeps cited evidence rows under budgetChars. The
// bound is HARD: no row may exceed the remaining budget. If the first row alone
// is too large, its fields are truncated to fit (#69).
func budgetEntityEvidence(items []EntityEvidence, budgetChars int) ([]EntityEvidence, bool) {
	if budgetChars <= 2 || len(items) == 0 {
		return nil, len(items) > 0
	}
	const jsonSep = 2
	kept := make([]EntityEvidence, 0, len(items))
	used := 2 // JSON array `[` + `]` overhead (conservative)
	truncated := false
	for _, e := range items {
		e := e
		cost := jsonLen(e) + jsonSep
		if used+cost > budgetChars {
			if len(kept) > 0 {
				truncated = true
				break
			}
			fitted, ok := fitEntityEvidence(e, budgetChars-used-jsonSep)
			if !ok {
				truncated = true
				break
			}
			e = fitted
			cost = jsonLen(e) + jsonSep
			truncated = true
		}
		if used+cost > budgetChars {
			truncated = true
			break
		}
		kept = append(kept, e)
		used += cost
	}
	if len(kept) < len(items) {
		truncated = true
	}
	return kept, truncated
}

// fitEntityEvidence shrinks fields until the JSON encoding fits maxBytes.
func fitEntityEvidence(e EntityEvidence, maxBytes int) (EntityEvidence, bool) {
	if maxBytes <= 0 {
		return EntityEvidence{}, false
	}
	out := e
	for jsonLen(out) > maxBytes && len(out.Snippet) > 0 {
		out.Snippet = truncateRunes(out.Snippet, len(out.Snippet)-1)
	}
	for jsonLen(out) > maxBytes && len(out.Title) > 0 {
		out.Title = truncateRunes(out.Title, len(out.Title)-1)
	}
	for jsonLen(out) > maxBytes && len(out.CreatedAt) > 0 {
		out.CreatedAt = truncateRunes(out.CreatedAt, len(out.CreatedAt)-1)
	}
	for jsonLen(out) > maxBytes && len(out.Source) > 0 {
		out.Source = truncateRunes(out.Source, len(out.Source)-1)
	}
	for jsonLen(out) > maxBytes && len(out.ID) > 0 {
		out.ID = truncateRunes(out.ID, len(out.ID)-1)
	}
	return out, jsonLen(out) <= maxBytes
}

// contextItemJSON is the compact items[] shape for `mora context --json`: a
// RECEIPT for one memory the assembly packed — id, title, created_at, no body
// text. The `context` blob is the content lane; items[] is the provenance
// lane, so a consumer can audit or re-fetch exactly what was chosen (#200).
type contextItemJSON struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	// Corroborating mirrors a cluster head's Memory.Corroborating (issue #237,
	// round-2/round-3 P1 scoping fix): a nested array on the head's own
	// receipt, the same compact four-key ref shape as search_memory/think —
	// not a second candidate shape. Empty/absent for non-head receipts.
	Corroborating []CorroboratingRef `json:"corroborating,omitempty"`
}

// contextReceipts returns one receipt per packed memory, in pack order,
// dropping from the tail if the whole set cannot fit inside budgetChars. The
// caller reserves the receipts' JSON cost BEFORE building the blob — the old
// leftover-budget order guaranteed an empty items[] on any vault big enough
// to fill the blob, which was every real vault (#200; #69 owns the hard
// bound this preserves).
func contextReceipts(items []Memory, budgetChars int) []contextItemJSON {
	kept := []contextItemJSON{}
	used := 2 // enclosing "[]"
	const jsonSep = 2
	for _, m := range items {
		row := contextItemJSON{ID: m.ID, Title: m.Title, CreatedAt: m.CreatedAt, Corroborating: m.Corroborating}
		cost := jsonLen(row) + jsonSep
		if used+cost > budgetChars {
			break
		}
		kept = append(kept, row)
		used += cost
	}
	return kept
}
