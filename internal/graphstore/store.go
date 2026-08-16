package graphstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/memory"
	"sort"
	"strings"
)

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func Write(ctx context.Context, tx *sql.Tx, r graph.Result) error {
	es, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO entities (id, kind, display_name, aliases, mention_count, first_seen, last_seen, salience_micros) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer es.Close()
	for _, e := range r.Entities {
		a := e.Aliases
		if a == nil {
			a = []string{}
		}
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if _, err = es.ExecContext(ctx, e.ID, e.Kind, e.DisplayName, string(b), e.MentionCount, nullStr(e.FirstSeen), nullStr(e.LastSeen), e.Salience); err != nil {
			return err
		}
	}
	eds, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO edges (src, rel, dst, evidence_id, valid_from, valid_to, observed_at, invalidated_at) VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`)
	if err != nil {
		return err
	}
	defer eds.Close()
	for _, e := range r.Edges {
		if _, err = eds.ExecContext(ctx, e.Src, e.Rel, e.Dst, e.EvidenceID, nullStr(e.ValidFrom), nullStr(e.ObservedAt), nullStr(e.InvalidatedAt)); err != nil {
			return err
		}
	}
	ms, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO person_merges (member_a, member_b, signal, detail) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ms.Close()
	for _, m := range r.Merges {
		if _, err = ms.ExecContext(ctx, m.A, m.B, m.Signal, m.Detail); err != nil {
			return err
		}
	}
	return nil
}

type EntityRow struct {
	ID, Kind, DisplayName, AliasesJSON string
	MentionCount                       int
	SalienceMicros                     int64
}
type IncomingEdge struct{ Neighbor, Rel, EvidenceID, ObservedAt string }

func LiveEvidenceByEntity(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT dst, evidence_id FROM edges WHERE invalidated_at IS NULL ORDER BY dst, evidence_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
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
			out[dst] = append(out[dst], ev)
		}
	}
	return out, rows.Err()
}
func ListEntityRows(ctx context.Context, db *sql.DB) ([]EntityRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, kind, display_name, salience_micros FROM entities WHERE id NOT LIKE 'memory:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntityRow
	for rows.Next() {
		var r EntityRow
		var sal sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Kind, &r.DisplayName, &sal); err != nil {
			return nil, err
		}
		r.SalienceMicros = sal.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}
func AliasMatches(raw, name string) bool {
	if raw == "" {
		return false
	}
	var a []string
	if json.Unmarshal([]byte(raw), &a) != nil {
		return false
	}
	for _, v := range a {
		if strings.EqualFold(v, name) {
			return true
		}
	}
	return false
}
func FindEntity(ctx context.Context, db *sql.DB, name string) (*EntityRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, kind, display_name, aliases, mention_count, salience_micros FROM entities WHERE id NOT LIKE 'memory:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var match *EntityRow
	for rows.Next() {
		var r EntityRow
		var sal sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Kind, &r.DisplayName, &r.AliasesJSON, &r.MentionCount, &sal); err != nil {
			return nil, err
		}
		r.SalienceMicros = sal.Int64
		if !strings.EqualFold(r.DisplayName, name) && !AliasMatches(r.AliasesJSON, name) {
			continue
		}
		if match == nil || r.MentionCount > match.MentionCount || (r.MentionCount == match.MentionCount && r.ID < match.ID) {
			c := r
			match = &c
		}
	}
	return match, rows.Err()
}
func IncomingEdges(ctx context.Context, db *sql.DB, id string) ([]IncomingEdge, []string, error) {
	rows, err := db.QueryContext(ctx, `SELECT src, rel, evidence_id, observed_at FROM edges WHERE dst = ? AND invalidated_at IS NULL ORDER BY src, evidence_id`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var edges []IncomingEdge
	var evidence []string
	seen := map[string]bool{}
	for rows.Next() {
		var e IncomingEdge
		var obs sql.NullString
		if err := rows.Scan(&e.Neighbor, &e.Rel, &e.EvidenceID, &obs); err != nil {
			return nil, nil, err
		}
		e.ObservedAt = obs.String
		edges = append(edges, e)
		if !seen[e.EvidenceID] {
			seen[e.EvidenceID] = true
			evidence = append(evidence, e.EvidenceID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Strings(evidence)
	return edges, evidence, nil
}
func CoOccurringPeople(ctx context.Context, db *sql.DB, id string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT e2.dst FROM edges e1 JOIN edges e2 ON e1.src = e2.src LEFT JOIN entities ent ON e2.dst = ent.id WHERE e1.dst = ? AND e1.rel IN ('PARTICIPATED_IN','ATTENDED') AND e2.rel IN ('PARTICIPATED_IN','ATTENDED') AND e2.dst <> e1.dst AND (ent.kind IS NULL OR ent.kind = 'person') AND e1.invalidated_at IS NULL AND e2.invalidated_at IS NULL ORDER BY e2.dst`, id)
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

type LoadOptions struct {
	Hydrate func(string) (memory.Memory, error)
}

func LoadMemoriesByID(ctx context.Context, db *sql.DB, ids []string, o LoadOptions) ([]memory.Memory, error) {
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
	var out []memory.Memory
	for rows.Next() {
		var m memory.Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text); err != nil {
			return nil, err
		}
		m.Tags = genericutil.SplitCSV(tags)
		if o.Hydrate != nil {
			if full, e := o.Hydrate(m.Path); e == nil {
				full.Score = m.Score
				m = full
			}
		}
		out = append(out, m)
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
