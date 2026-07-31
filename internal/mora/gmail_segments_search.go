package mora

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

// Issue #243 — segment-grain retrieval. This file owns the search-side half
// of the evidence-segment feature: an ADDITIONAL candidate arm over
// gmail_segments_fts, mapped to PARENT memory ids before fusion/slot
// accounting (frozen interface #3), and the compact "evidence" receipt
// attached to a parent's search_memory row per DQ5 (§2).

// GmailSegmentEvidence is the compact receipt search_memory attaches to a
// Gmail parent row that has at least one query-matching segment (DQ5, §2) —
// exactly {evidence_ref, sender, at, snippet}, the parent's STRONGEST
// matching segment (best score, then lowest evidence_ref lexicographically).
type GmailSegmentEvidence struct {
	EvidenceRef string `json:"evidence_ref"`
	Sender      string `json:"sender"`
	At          string `json:"at"`
	Snippet     string `json:"snippet"`
}

// Fusion tuning for the segment arm. k matches defaultFusion.k (hybrid.go) —
// both a plain-FTS-only fusion (this file) and the hybrid fusion (hybrid.go)
// use the SAME damping so a query's ranking behavior does not depend on
// which arm combination happened to be active. The parent-grain arm keeps a
// heavier weight (the exact-match anchor, mirroring defaultFusion.fts); the
// segment arm's weight is what lets a short, sharply-matching segment
// promote a heavily-diluted parent past decoys that only agree at parent
// grain (the buried-message acceptance criterion).
const (
	gmailSegmentFusionK      = 10
	gmailSegmentParentWeight = 1.5
	gmailSegmentArmWeight    = 1.0
)

// gmailSegmentQueryArm runs query against gmail_segments_fts and returns, for
// every DISTINCT parent memory with at least one matching segment: the
// parent ids in STRONGEST-first order (best bm25 score, then lowest
// evidence_ref lexicographically — the same tie-break DQ5 pins for evidence
// attachment) and a map of that parent's WINNING segment's evidence receipt.
// A parent's rank-arm membership and its evidence receipt share exactly one
// query pass, so they can never disagree about which segment "won".
//
// Errors are returned to the caller, which treats the segment arm as
// best-effort BY DESIGN — a deliberate policy, asymmetric with the FATAL
// fts/vec/graph arms (a real error from ftsSearchIDs/vectorSearchIDs/
// graphExpandIDs aborts the whole search): both searchMemories (search.go)
// and hybridSearchTrace (hybrid.go) swallow a non-nil error from this
// function and simply skip the arm — no segment promotion, no evidence —
// degrading to plain parent-grain retrieval rather than failing
// search_memory outright. This is intentional: gmail_segments_fts is
// guaranteed present on any index this binary's schema version accepts
// (openIndexRO's schema gate forces a rebuild first), so an error here is
// exceptional rather than routine, and the enhancement it powers is judged
// not worth failing a whole search over. No logging infrastructure exists
// for this arm — unlike the fatal arms, whose errors already propagate as a
// Go error the caller returns and can log — so today a swallowed error here
// is silent beyond the missing evidence/promotion it causes; that gap is a
// known, accepted tradeoff of the best-effort policy, not an oversight to
// fix by adding a new logging path.
func gmailSegmentQueryArm(ctx context.Context, db *sql.DB, query, scope string, filters ...searchFilters) ([]string, map[string]GmailSegmentEvidence, error) {
	f := oneFilter(filters)
	match := ftsQuery(query)
	if match == "" {
		return nil, nil, nil
	}
	q := `SELECT gs.evidence_ref, gs.memory_id, gs.sender, gs.at, gs.text
	      FROM gmail_segments_fts
	      JOIN gmail_segments gs ON gs.evidence_ref = gmail_segments_fts.evidence_ref
	      JOIN memories m ON m.id = gs.memory_id
	      WHERE gmail_segments_fts MATCH ?`
	args := []any{match}
	if scope != "" {
		q += ` AND m.scope = ?`
		args = append(args, scope)
	}
	// I5: D's trusted source/time boundary applies inside the segment arm's
	// joined-memory query, before BM25 ordering. An excluded segment must never
	// earn a rank, consume a candidate slot, or be hydrated back into results.
	if predicate, predicateArgs := f.sqlPredicate(); predicate != "" {
		q += predicate
		args = append(args, predicateArgs...)
	}
	q += ` ORDER BY bm25(gmail_segments_fts), gs.evidence_ref`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []string
	evidence := map[string]GmailSegmentEvidence{}
	for rows.Next() {
		var evRef, memID, sender, at, text string
		if err := rows.Scan(&evRef, &memID, &sender, &at, &text); err != nil {
			return nil, nil, err
		}
		if _, seen := evidence[memID]; seen {
			continue // first occurrence per parent (ORDER BY above) is already the strongest
		}
		evidence[memID] = GmailSegmentEvidence{
			EvidenceRef: evRef,
			Sender:      sender,
			At:          at,
			Snippet:     matchSnippet(text, query, searchSnippetLen),
		}
		ids = append(ids, memID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return ids, evidence, nil
}

// admitGmailSegmentCandidates hydrates the top pool parents named by the
// segment arm that parent-grain FTS did not admit to its own pool. The two
// arms remain semantically distinct: this function only expands the candidate
// set; fuseGmailSegmentArm receives the original parentIDs separately, so a
// segment-only candidate earns no fabricated parent-arm RRF contribution.
//
// Rows are loaded from the same memories projection searchMemories already
// queried, then hydrated from their vault paths in the same way. Visibility
// filtering remains downstream in searchMemories, once, over the combined raw
// candidate ranking.
func admitGmailSegmentCandidates(ctx context.Context, db *sql.DB, candidates []Memory, segIDs []string, pool int) ([]Memory, error) {
	if pool <= 0 || len(segIDs) == 0 {
		return candidates, nil
	}
	if len(segIDs) > pool {
		segIDs = segIDs[:pool]
	}
	existing := make(map[string]bool, len(candidates))
	for _, m := range candidates {
		existing[m.ID] = true
	}
	missing := make([]string, 0, len(segIDs))
	for _, id := range segIDs {
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
	rows, err := db.QueryContext(ctx,
		`SELECT id, scope, type, title, tags, source, created_at, path, text
		 FROM memories WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return candidates, err
	}
	defer rows.Close()
	loaded := make(map[string]Memory, len(missing))
	for rows.Next() {
		var m Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text); err != nil {
			return candidates, err
		}
		m.Tags = splitCSV(tags)
		if full, ferr := parseMemory(m.Path); ferr == nil {
			m = full
		}
		loaded[m.ID] = m
	}
	if err := rows.Err(); err != nil {
		return candidates, err
	}
	out := append([]Memory(nil), candidates...)
	for _, id := range missing {
		if m, ok := loaded[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// fuseGmailSegmentArm re-ranks candidates by RRF-fusing their existing
// parent-grain order with the segment-grain arm (frozen interface #3: an
// ADDITIONAL candidate source... before fusion/slot accounting). candidates
// is the union hydrated by admitGmailSegmentCandidates; parentIDs is strictly
// the original parent-grain arm. Keeping those inputs separate prevents a
// segment-only candidate from receiving an invented parent-arm rank.
func fuseGmailSegmentArm(candidates []Memory, parentIDs, segIDs []string) []Memory {
	if len(candidates) == 0 {
		return candidates // nothing to re-rank; preserve nil-vs-empty exactly
	}
	byID := make(map[string]Memory, len(candidates))
	for _, m := range candidates {
		byID[m.ID] = m
	}
	fused := rrfWeighted([][]string{parentIDs, segIDs}, []float64{gmailSegmentParentWeight, gmailSegmentArmWeight}, gmailSegmentFusionK)
	ids := make([]string, 0, len(fused))
	for id := range fused {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if fused[ids[i]] != fused[ids[j]] {
			return fused[ids[i]] > fused[ids[j]]
		}
		return ids[i] < ids[j]
	})
	out := make([]Memory, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			continue // outside both bounded candidate pools
		}
		m.Score = fused[id]
		out = append(out, m)
	}
	return out
}

// attachGmailSegmentEvidence stamps each result row's Evidence field from the
// per-query evidence map built alongside the segment arm — DQ5's rule
// (§2): attachment is a pure function of "does this returned parent have a
// query-matching segment", independent of which arm actually ranked it in.
// A row with no entry is left untouched (Evidence stays nil ⇒ omitted from
// JSON — the byte-identity guarantee for non-participating memories).
func attachGmailSegmentEvidence(rows []Memory, evidence map[string]GmailSegmentEvidence) {
	if len(evidence) == 0 {
		return
	}
	for i := range rows {
		if ev, ok := evidence[rows[i].ID]; ok {
			ev := ev
			rows[i].Evidence = &ev
		}
	}
}
