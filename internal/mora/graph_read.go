package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/graphstore"
	"os"
	"sort"
	"strings"
	"time"
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

// publicEntityKind preserves source-native phone identities internally as people
// (their participation and salience are real graph evidence) while preventing an
// unnamed dial string from masquerading as a person's display name. A trusted
// address-book display keeps the ordinary person kind.
func publicEntityKind(id, storedKind, display string) string {
	kind := listKind(id, storedKind)
	if kind != "person" || !strings.HasPrefix(id, "person:") {
		return kind
	}
	identity := strings.TrimPrefix(id, "person:")
	if isPhoneNumber(identity) && (strings.TrimSpace(display) == "" || strings.TrimSpace(display) == identity) {
		return "artifact"
	}
	return kind
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
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
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
	return graphstore.LiveEvidenceByEntity(ctx, db)
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
	evidence, err := graphstore.LiveEvidenceByEntity(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := graphstore.ListEntityRows(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []Entity
	for _, row := range rows {
		ids := evidence[row.ID]
		if len(ids) == 0 {
			continue
		}
		out = append(out, Entity{Name: row.DisplayName, Kind: publicEntityKind(row.ID, row.Kind, row.DisplayName), Count: len(ids), MemoryIDs: ids, Salience: row.SalienceMicros})
	}
	sortEntitiesLegacy(out)
	return out, nil
}

// loadMemoriesByID reads memories from the index table by id (graph-backed, no
// file rescan), newest first. It is the graph read chokepoint reached by
// get_entity, list_entities AND the meeting brief, so a memory with a pending
// delete op is suppressed here (B4) — miss this and meeting_prep keeps quoting a
// deleted memory back at the user, the exact harm the pending ledger prevents.
func loadMemoriesByID(ctx context.Context, cfg Config, db *sql.DB, ids []string) ([]Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := graphstore.LoadMemoriesByID(ctx, db, ids, graphstore.LoadOptions{Hydrate: parseMemory})
	if err != nil {
		return nil, err
	}
	out = suppressPendingDeletes(cfg, out)
	return currentMemories(cfg, out, time.Now())
}

// coOccurringPeople returns the OTHER person entity ids that share at least one
// memory (thread/event) with the given person, via a query-time self-join over the
// hub-rooted participation edges — co-occurrence is never materialized, so an
// N-participant memory costs O(N) edge rows, not O(N²). Tombstoned edges are
// excluded (live reads only). Sorted, deduped.
func coOccurringPeople(ctx context.Context, db *sql.DB, personID string) ([]string, error) {
	return graphstore.CoOccurringPeople(ctx, db, personID)
}

// aliasMatches reports whether name equals any alias (case-insensitive) in the
// JSON aliases array, enabling precise email/handle lookups in get_entity.
func aliasMatches(aliasesJSON, name string) bool { return graphstore.AliasMatches(aliasesJSON, name) }

// graphGetEntity returns the memories referencing a named entity plus graph
// provenance (incoming edges, aliases, degree, spec kind) from the materialized
// tables. Legacy keys {name,kind,count,found,memories} are preserved exactly.
func graphGetEntity(ctx context.Context, cfg Config, name string) (map[string]any, error) {
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	match, err := graphstore.FindEntity(ctx, db, name)
	if err != nil {
		return nil, err
	}
	if match == nil {
		return map[string]any{"name": name, "found": false, "memories": []Memory{}}, nil
	}
	incoming, evidence, err := graphstore.IncomingEdges(ctx, db, match.ID)
	if err != nil {
		return nil, err
	}
	if len(evidence) == 0 {
		return map[string]any{"name": name, "found": false, "memories": []Memory{}}, nil
	}
	edges := make([]map[string]any, 0, len(incoming))
	for _, e := range incoming {
		edges = append(edges, map[string]any{"neighbor": e.Neighbor, "rel": e.Rel, "direction": "in", "evidence_id": e.EvidenceID, "observed_at": e.ObservedAt})
	}
	mems, err := loadMemoriesByID(ctx, cfg, db, evidence)
	if err != nil {
		return nil, err
	}
	var aliases []string
	if match.AliasesJSON != "" {
		_ = json.Unmarshal([]byte(match.AliasesJSON), &aliases)
	}
	if aliases == nil {
		aliases = []string{}
	}
	neighbors := []map[string]any{}
	if strings.HasPrefix(match.ID, "person:") {
		ids, err := graphstore.CoOccurringPeople(ctx, db, match.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			var display, kind string
			if err := db.QueryRowContext(ctx, `SELECT display_name, kind FROM entities WHERE id = ?`, id).Scan(&display, &kind); err != nil {
				display = strings.TrimPrefix(id, "person:")
				kind = "person"
			}
			neighbors = append(neighbors, map[string]any{"id": id, "display_name": display, "type": publicEntityKind(id, kind, display)})
		}
	}
	return map[string]any{"name": match.DisplayName, "kind": publicEntityKind(match.ID, match.Kind, match.DisplayName), "count": len(evidence), "found": true, "memories": mems, "graph_kind": match.Kind, "display_name": match.DisplayName, "aliases": aliases, "salience": match.SalienceMicros, "degree": len(edges), "edges": edges, "neighbors": neighbors}, nil
}
