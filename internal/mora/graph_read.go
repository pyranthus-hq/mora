package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// legacyKindFromID recovers the legacy public kind (scope|tag|link|category) from
// a prefixed canonical entity id, e.g. "scope:project:demo" -> "scope". This keeps
// list_entities/get_entity backward-compatible while entities.kind stores the spec
// kind (project|topic|...).
func legacyKindFromID(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[:i]
	}
	return id
}

// listKind picks the public kind for the legacy entity list. Person ids carry a
// canonical "person:" prefix but their stored kind distinguishes real people from
// automated senders (A1: "person" vs "service"), so we surface the stored kind for
// them. Structural ids (scope/tag/link/category) keep deriving their legacy kind
// from the id prefix — their stored kind is the spec kind (project/topic/...).
func listKind(id, storedKind string) string {
	if strings.HasPrefix(id, "person:") {
		return storedKind
	}
	return legacyKindFromID(id)
}

// sortEntitiesLegacy orders entities the way extractEntities did: count desc, then
// kind, then name — stable and demo-friendly.
func sortEntitiesLegacy(es []Entity) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Count != es[j].Count {
			return es[i].Count > es[j].Count
		}
		if es[i].Kind != es[j].Kind {
			return es[i].Kind < es[j].Kind
		}
		if es[i].Name != es[j].Name {
			return es[i].Name < es[j].Name
		}
		// Final tie-break on the evidence id list so two distinct entities sharing a
		// display name (e.g. two people named "Alex") sort in a byte-stable order
		// (codex S4: same count/kind/name was otherwise non-deterministic).
		return strings.Join(es[i].MemoryIDs, ",") < strings.Join(es[j].MemoryIDs, ",")
	})
}

// ensureIndexDB opens the index read-only, building it first if the graph isn't
// present yet. It rebuilds both when the db is missing AND when the db predates
// S1 (legacy memories+fts but no entities/edges) — otherwise an upgraded user's
// first graph read would fail with "no such table: entities". A version-stale
// index self-heals or errors per openIndexRO's indexAutoHeal policy. Caller
// must Close.
func ensureIndexDB(ctx context.Context, cfg Config) (*sql.DB, error) {
	if !graphReady(cfg) {
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			return nil, err
		}
	}
	return openIndexRO(ctx, cfg)
}

// graphReady reports whether the index exists and already carries the S1 graph
// tables. A pre-S1 index.db has memories+fts but no entities/edges.
func graphReady(cfg Config) bool {
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('entities','edges')`).Scan(&n); err != nil {
		return false
	}
	return n == 2
}

// liveEvidenceByEntity returns the distinct, sorted evidence memory ids per entity
// from the non-invalidated edges (live reads filter invalidated_at IS NULL).
func liveEvidenceByEntity(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT dst, evidence_id FROM edges WHERE invalidated_at IS NULL ORDER BY dst, evidence_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDst := map[string][]string{}
	seen := map[string]map[string]bool{}
	for rows.Next() {
		var dst, ev string
		if err := rows.Scan(&dst, &ev); err != nil {
			return nil, err
		}
		if seen[dst] == nil {
			seen[dst] = map[string]bool{}
		}
		if !seen[dst][ev] {
			seen[dst][ev] = true
			byDst[dst] = append(byDst[dst], ev)
		}
	}
	return byDst, rows.Err()
}

// graphListEntities returns the legacy entity list ({name,kind,count,memory_ids})
// from the materialized graph tables, excluding the per-memory hub nodes. This is
// the graph-backed replacement for extractEntities(listMemories(...)).
func graphListEntities(ctx context.Context, cfg Config) ([]Entity, error) {
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	evidence, err := liveEvidenceByEntity(ctx, db)
	if err != nil {
		return nil, err
	}
	// salience_micros is the frozen person-ranking sort key (Phase 14-03). Read it into
	// Entity.Salience so the People overview can rank by salience (14-04). NULL/absent
	// (structural rows, or a pre-14-03 DB whose ALTER hasn't run) scans to 0 via
	// sql.NullInt64 — purely additive, the existing fields/gate are unchanged.
	rows, err := db.QueryContext(ctx, `SELECT id, kind, display_name, salience_micros FROM entities WHERE id NOT LIKE 'memory:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entity
	for rows.Next() {
		var id, storedKind, display string
		var sal sql.NullInt64
		if err := rows.Scan(&id, &storedKind, &display, &sal); err != nil {
			return nil, err
		}
		ids := evidence[id]
		if len(ids) == 0 {
			continue // no live evidence -> not a live entity
		}
		out = append(out, Entity{Name: display, Kind: listKind(id, storedKind), Count: len(ids), MemoryIDs: ids, Salience: sal.Int64})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortEntitiesLegacy(out)
	return out, nil
}

// loadMemoriesByID reads memories from the index table by id (graph-backed, no
// file rescan), newest first.
func loadMemoriesByID(ctx context.Context, db *sql.DB, ids []string) ([]Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT id, scope, type, title, tags, source, created_at, path, text FROM memories WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text); err != nil {
			return nil, err
		}
		m.Tags = splitCSV(tags)
		// The index table is a lossy projection (no provider/last_synced/etc.).
		// Hydrate full fidelity from the source file so get_entity returns the same
		// Memory shape the old listMemories path did; fall back to the row if the
		// file is unreadable.
		if full, ferr := parseMemory(m.Path); ferr == nil {
			full.Score = m.Score
			out = append(out, full)
		} else {
			out = append(out, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// coOccurringPeople returns the OTHER person entity ids that share at least one
// memory (thread/event) with the given person, via a query-time self-join over the
// hub-rooted participation edges — co-occurrence is never materialized, so an
// N-participant memory costs O(N) edge rows, not O(N²). Tombstoned edges are
// excluded (live reads only). Sorted, deduped.
func coOccurringPeople(ctx context.Context, db *sql.DB, personID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT e2.dst
		FROM edges e1
		JOIN edges e2 ON e1.src = e2.src
		WHERE e1.dst = ?
		  AND e1.rel IN ('PARTICIPATED_IN','ATTENDED')
		  AND e2.rel IN ('PARTICIPATED_IN','ATTENDED')
		  AND e2.dst <> e1.dst
		  AND e2.dst LIKE 'person:%'
		  AND e1.invalidated_at IS NULL
		  AND e2.invalidated_at IS NULL
		ORDER BY e2.dst`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var dst string
		if err := rows.Scan(&dst); err != nil {
			return nil, err
		}
		out = append(out, dst)
	}
	return out, rows.Err()
}

// aliasMatches reports whether name equals any alias (case-insensitive) in the
// JSON aliases array, enabling precise email/handle lookups in get_entity.
func aliasMatches(aliasesJSON, name string) bool {
	if aliasesJSON == "" {
		return false
	}
	var aliases []string
	if json.Unmarshal([]byte(aliasesJSON), &aliases) != nil {
		return false
	}
	for _, a := range aliases {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

// graphGetEntity returns the memories referencing a named entity plus graph
// provenance (incoming edges, aliases, degree, spec kind) from the materialized
// tables. Legacy keys {name,kind,count,found,memories} are preserved exactly.
func graphGetEntity(ctx context.Context, cfg Config, name string) (map[string]any, error) {
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Resolve the entity by case-insensitive display name (excluding hub nodes),
	// preferring the most-mentioned on ties — deterministic, matches the old
	// "first by count desc" behavior.
	rows, err := db.QueryContext(ctx, `SELECT id, kind, display_name, aliases, mention_count FROM entities WHERE id NOT LIKE 'memory:%'`)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id, specKind, display, aliases string
		mention                        int
	}
	var match *cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.specKind, &c.display, &c.aliases, &c.mention); err != nil {
			rows.Close()
			return nil, err
		}
		// Match the display name OR any exact alias (email/handle/name variant), so a
		// precise lookup like get_entity("neil@example.com") disambiguates two people
		// who share a display name (codex S4). Name lookups still pick the
		// most-mentioned match deterministically.
		if !strings.EqualFold(c.display, name) && !aliasMatches(c.aliases, name) {
			continue
		}
		if match == nil || c.mention > match.mention || (c.mention == match.mention && c.id < match.id) {
			cc := c
			match = &cc
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if match == nil {
		return map[string]any{"name": name, "found": false, "memories": []Memory{}}, nil
	}

	edgeRows, err := db.QueryContext(ctx, `SELECT src, rel, evidence_id, observed_at FROM edges WHERE dst = ? AND invalidated_at IS NULL ORDER BY src, evidence_id`, match.id)
	if err != nil {
		return nil, err
	}
	var edges []map[string]any
	var evidence []string
	seen := map[string]bool{}
	for edgeRows.Next() {
		var src, rel, ev string
		var obs sql.NullString
		if err := edgeRows.Scan(&src, &rel, &ev, &obs); err != nil {
			edgeRows.Close()
			return nil, err
		}
		edges = append(edges, map[string]any{"neighbor": src, "rel": rel, "direction": "in", "evidence_id": ev, "observed_at": obs.String})
		if !seen[ev] {
			seen[ev] = true
			evidence = append(evidence, ev)
		}
	}
	edgeRows.Close()
	if err := edgeRows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(evidence)

	// Live-evidence gate: an entity whose every edge is invalidated (e.g. all its
	// evidence memories are tombstoned) is not a live entity. graphListEntities
	// already drops it; mirror that here so get_entity and list_entities agree
	// instead of returning {found:true, count:0, memories:[]}.
	if len(evidence) == 0 {
		return map[string]any{"name": name, "found": false, "memories": []Memory{}}, nil
	}

	mems, err := loadMemoriesByID(ctx, db, evidence)
	if err != nil {
		return nil, err
	}
	var aliases []string
	if match.aliases != "" {
		_ = json.Unmarshal([]byte(match.aliases), &aliases)
	}
	if aliases == nil {
		aliases = []string{}
	}

	// 1-hop neighbors: for a person, the people they share a thread/event with
	// (query-time co-occurrence self-join), resolved to display names. This is the
	// seam I2 hybrid retrieval expands into the candidate pool.
	neighbors := []map[string]any{}
	if strings.HasPrefix(match.id, "person:") {
		coIDs, err := coOccurringPeople(ctx, db, match.id)
		if err != nil {
			return nil, err
		}
		for _, cid := range coIDs {
			var disp string
			if err := db.QueryRowContext(ctx, `SELECT display_name FROM entities WHERE id = ?`, cid).Scan(&disp); err != nil {
				disp = strings.TrimPrefix(cid, "person:")
			}
			neighbors = append(neighbors, map[string]any{"id": cid, "display_name": disp})
		}
	}

	return map[string]any{
		"name": match.display,
		// Surface the stored kind for person ids (person|service) so get_entity agrees
		// with list_entities; structural ids keep their legacy prefix-derived kind.
		"kind":         listKind(match.id, match.specKind),
		"count":        len(evidence),
		"found":        true,
		"memories":     mems,
		"graph_kind":   match.specKind,
		"display_name": match.display,
		"aliases":      aliases,
		"degree":       len(edges),
		"edges":        edges,
		"neighbors":    neighbors,
	}, nil
}
