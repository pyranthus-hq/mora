package mora

// Cross-encoder reranking of the fused candidate window. Default-OFF and
// byte-identical to the fused order unless a reranker is wired in, mirroring
// the MMR stage that runs just above it.
//
// Three properties make this safe to put in the retrieval path:
//
//  1. PURE PERMUTATION. The stage reorders the top rerankWindow ids and nothing
//     else. It never adds a candidate, never drops one, never edits one, and
//     never touches the tail below the window. A wrong judgment therefore costs
//     a few slots of ordering, never a disappeared result.
//  2. FAIL-OPEN. Any error -- transport, timeout, malformed body, a score for an
//     id we did not ask about -- returns the input order unchanged. Search must
//     not fail because a third party is down, so a reranker outage is
//     indistinguishable from the reranker being off.
//  3. BOUNDED INPUT. Only the head window is scored, each document truncated to
//     rerankSnippet bytes, so the request size is a function of the constants
//     below rather than of vault size or document length.

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strings"
)

const (
	// rerankWindow is how many fused candidates get re-scored. It is deliberately
	// larger than a typical limit (so the stage can promote a doc the truncate
	// would have cut) and small enough that the whole window fits one request
	// with room to spare.
	rerankWindow = 30

	// rerankSnippet caps each document's contribution. Retrieval decisions key
	// off the opening of a memory far more than its tail, and an unbounded body
	// would let one long thread crowd out every other candidate.
	rerankSnippet = 800
)

// rerankDoc is the minimal view of a candidate the reranker needs: enough to
// judge relevance, and nothing that could carry state back into the index.
type rerankDoc struct {
	ID    string
	Title string
	Text  string
}

// reranker scores query/document pairs. A score is a relevance signal only --
// higher sorts earlier -- with no required scale, so implementations are free
// to return probabilities, logits, or ordinals.
type reranker interface {
	Name() string
	Score(ctx context.Context, query string, docs []rerankDoc) (map[string]float64, error)
}

// rerankOv is the test/eval seam and the ONLY way a reranker reaches the search
// path in a test, mirroring mmrOv. Production resolves through configRerank.
var rerankOv reranker

// configRerank returns the active reranker, or nil when reranking is off.
// The eval/test override wins; else MORA_RERANK selects one by name; else nil.
// Single source of truth, mirroring configMMR.
func configRerank(c Config) reranker {
	if rerankOv != nil {
		return rerankOv
	}
	switch strings.TrimSpace(os.Getenv("MORA_RERANK")) {
	case "jev":
		r, err := newJevReranker()
		if err != nil {
			// A misconfigured reranker is off, not fatal: the deterministic
			// fused order is always a correct answer.
			return nil
		}
		return r
	default:
		return nil
	}
}

// applyRerank permutes the head of ids by the reranker's scores and returns the
// new order. It returns ids unchanged on every failure path.
func applyRerank(ctx context.Context, db *sql.DB, r reranker, query string, ids []string) []string {
	if r == nil || len(ids) < 2 {
		return ids
	}
	window := len(ids)
	if window > rerankWindow {
		window = rerankWindow
	}
	head := ids[:window]

	docs, err := loadRerankDocs(ctx, db, head)
	if err != nil || len(docs) == 0 {
		return ids
	}
	scores, err := r.Score(ctx, query, docs)
	if err != nil || len(scores) == 0 {
		return ids
	}

	// Original position is the tie-break AND the fallback for any id the
	// reranker declined to score, so an incomplete answer degrades to the fused
	// order for the unscored ids instead of dumping them at one end.
	pos := make(map[string]int, len(head))
	for i, id := range head {
		pos[id] = i
	}
	reordered := append([]string(nil), head...)
	sort.SliceStable(reordered, func(a, b int) bool {
		ida, idb := reordered[a], reordered[b]
		sa, oka := scores[ida]
		sb, okb := scores[idb]
		switch {
		case oka && okb && sa != sb:
			return sa > sb
		case oka != okb:
			return oka // a scored candidate outranks an unscored one
		default:
			return pos[ida] < pos[idb]
		}
	})
	if !isPermutation(head, reordered) {
		return ids
	}
	out := make([]string, 0, len(ids))
	out = append(out, reordered...)
	out = append(out, ids[window:]...)
	return out
}

// isPermutation reports whether b contains exactly the elements of a. It is the
// guard that turns "the reranker returned something strange" into a no-op
// rather than a silently altered candidate set.
func isPermutation(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, id := range a {
		seen[id]++
	}
	for _, id := range b {
		seen[id]--
		if seen[id] < 0 {
			return false
		}
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// loadRerankDocs reads title/text for the window, preserving the caller's order
// and skipping ids the index cannot resolve.
func loadRerankDocs(ctx context.Context, db *sql.DB, ids []string) ([]rerankDoc, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := `SELECT id, COALESCE(title,''), COALESCE(text,'') FROM memories WHERE id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := make(map[string]rerankDoc, len(ids))
	for rows.Next() {
		var d rerankDoc
		if err := rows.Scan(&d.ID, &d.Title, &d.Text); err != nil {
			return nil, err
		}
		d.Text = companionText(d.Text, rerankSnippet)
		byID[d.ID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]rerankDoc, 0, len(ids))
	for _, id := range ids {
		if d, ok := byID[id]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}
