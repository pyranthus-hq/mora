package segments

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
	searchpkg "github.com/pyranthus-hq/mora/internal/search"
	"strings"
)

const (
	DefaultParentPool  = 50
	EvidenceIDChunkLen = 200
	FusionK            = 10
	ParentWeight       = 1.5
	ArmWeight          = 1.0
)

func Query(ctx context.Context, db *sql.DB, query, scope string, pool int, filter searchpkg.Filter, snippetLen int) ([]string, map[string]memory.GmailSegmentEvidence, error) {
	return WinnerQuery(ctx, db, query, scope, pool, nil, filter, snippetLen)
}
func WinnerQuery(ctx context.Context, db *sql.DB, query, scope string, pool int, parentIDs []string, filter searchpkg.Filter, snippetLen int) ([]string, map[string]memory.GmailSegmentEvidence, error) {
	if pool <= 0 {
		return nil, nil, nil
	}
	match := searchpkg.FTSQuery(query)
	if match == "" {
		return nil, nil, nil
	}
	q := `WITH ranked_segments AS (SELECT gs.evidence_ref, gs.memory_id, gs.sender, gs.at, gs.block_refs, gs.text, gmail_segments_fts.rank AS segment_score FROM gmail_segments_fts JOIN gmail_segments gs ON gs.evidence_ref = gmail_segments_fts.evidence_ref JOIN memories m ON m.id = gs.memory_id WHERE gmail_segments_fts MATCH ?`
	args := []any{match}
	if scope != "" {
		q += ` AND m.scope = ?`
		args = append(args, scope)
	}
	if predicate, predicateArgs := filter.SQLPredicate(); predicate != "" {
		q += predicate
		args = append(args, predicateArgs...)
	}
	if len(parentIDs) > 0 {
		ph := make([]string, len(parentIDs))
		for i, id := range parentIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		q += ` AND gs.memory_id IN (` + strings.Join(ph, ",") + `)`
	}
	q += `), parent_winners AS (SELECT evidence_ref, memory_id, sender, at, block_refs, text, segment_score, ROW_NUMBER() OVER (PARTITION BY memory_id ORDER BY segment_score, evidence_ref) AS parent_rank FROM ranked_segments) SELECT evidence_ref, memory_id, sender, at, block_refs, text FROM parent_winners WHERE parent_rank = 1 ORDER BY segment_score, evidence_ref, memory_id LIMIT ?`
	args = append(args, pool)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []string
	evidence := map[string]memory.GmailSegmentEvidence{}
	for rows.Next() {
		var ref, id, sender, at, refsJSON, text string
		if err := rows.Scan(&ref, &id, &sender, &at, &refsJSON, &text); err != nil {
			return nil, nil, err
		}
		var refs []string
		_ = json.Unmarshal([]byte(refsJSON), &refs)
		evidence[id] = memory.GmailSegmentEvidence{EvidenceRef: ref, Sender: sender, At: at, Direction: Direction(refs), Snippet: searchpkg.MatchSnippet(text, query, snippetLen)}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return ids, evidence, nil
}
func isSegmentMemory(m memory.Memory) bool {
	return isGmail(m) || m.Provider == "imessage" || m.Type == "imessage"
}
func CompleteEvidence(ctx context.Context, db *sql.DB, query, scope string, rows []memory.Memory, evidence map[string]memory.GmailSegmentEvidence, filter searchpkg.Filter, snippetLen int) map[string]memory.GmailSegmentEvidence {
	if len(rows) == 0 {
		return evidence
	}
	missing := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, m := range rows {
		if !isSegmentMemory(m) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if _, ok := evidence[m.ID]; !ok {
			missing = append(missing, m.ID)
		}
	}
	if len(missing) == 0 {
		return evidence
	}
	if evidence == nil {
		evidence = map[string]memory.GmailSegmentEvidence{}
	}
	for start := 0; start < len(missing); start += EvidenceIDChunkLen {
		end := start + EvidenceIDChunkLen
		if end > len(missing) {
			end = len(missing)
		}
		chunk := missing[start:end]
		_, found, err := WinnerQuery(ctx, db, query, scope, len(chunk), chunk, filter, snippetLen)
		if err != nil {
			return evidence
		}
		for id, receipt := range found {
			evidence[id] = receipt
		}
	}
	return evidence
}

type HydrateFunc func(path string) (memory.Memory, error)

func AdmitCandidates(ctx context.Context, db *sql.DB, candidates []memory.Memory, segmentIDs []string, pool int, hydrate HydrateFunc) ([]memory.Memory, error) {
	if pool <= 0 || len(segmentIDs) == 0 {
		return candidates, nil
	}
	if len(segmentIDs) > pool {
		segmentIDs = segmentIDs[:pool]
	}
	existing := map[string]bool{}
	for _, m := range candidates {
		existing[m.ID] = true
	}
	var missing []string
	for _, id := range segmentIDs {
		if !existing[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return candidates, nil
	}
	ph := make([]string, len(missing))
	args := make([]any, len(missing))
	for i, id := range missing {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT id, scope, type, title, tags, source, created_at, path, text FROM memories WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return candidates, err
	}
	defer rows.Close()
	loaded := map[string]memory.Memory{}
	for rows.Next() {
		var m memory.Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text); err != nil {
			return candidates, err
		}
		m.Tags = genericutil.SplitCSV(tags)
		if hydrate != nil {
			if full, ferr := hydrate(m.Path); ferr == nil {
				m = full
			}
		}
		loaded[m.ID] = m
	}
	if err := rows.Err(); err != nil {
		return candidates, err
	}
	out := append([]memory.Memory(nil), candidates...)
	for _, id := range missing {
		if m, ok := loaded[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}
func FuseCandidates(candidates []memory.Memory, parentIDs, segmentIDs []string) []memory.Memory {
	if len(candidates) == 0 {
		return candidates
	}
	byID := map[string]memory.Memory{}
	for _, m := range candidates {
		byID[m.ID] = m
	}
	ids, scores := searchpkg.FuseRanked([][]string{parentIDs, segmentIDs}, []float64{ParentWeight, ArmWeight}, FusionK)
	out := make([]memory.Memory, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			continue
		}
		m.Score = scores[id]
		out = append(out, m)
	}
	return out
}
func AttachEvidence(rows []memory.Memory, evidence map[string]memory.GmailSegmentEvidence) {
	if len(evidence) == 0 {
		return
	}
	for i := range rows {
		if ev, ok := evidence[rows[i].ID]; ok {
			copy := ev
			rows[i].Evidence = &copy
		}
	}
}
