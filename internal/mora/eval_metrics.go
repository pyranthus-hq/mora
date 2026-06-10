package mora

// Pure-Go, in-process IR metrics for the T2 retrieval-recall eval (see
// docs/design/2026-06-05-t2-recall-eval-design.md). stdlib only (math via none,
// sort, database/sql for the index probe) so it runs in every `go test`,
// byte-identical across darwin/arm64 + linux/amd64, zero deps/network/Python.
//
// Formulas ported from trec_eval (recip_rank, recall) — never imported (CGO).
// nDCG is deliberately omitted from the MVP: at |Rel|≈1 with binary labels it
// degenerates to a noisier MRR. The graded `rel` column is parsed but unused so
// nDCG can be backfilled if Mora ever has genuinely multi-relevant queries.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

// relevant returns the set of doc_ids with rel>0 for a query.
func relevant(rel map[string]int) map[string]bool {
	r := make(map[string]bool, len(rel))
	for id, g := range rel {
		if g > 0 {
			r[id] = true
		}
	}
	return r
}

// recallAtK = |relevant ∩ top-k| / |relevant|. Caller MUST exclude
// NONE/negative-control rows before aggregating (they have no relevant set).
func recallAtK(ranked []string, rel map[string]int, k int) float64 {
	want := relevant(rel)
	if len(want) == 0 {
		return 0 // never average this in; NONE rows are a separate abstention metric
	}
	hit := 0
	for i, id := range ranked {
		if i >= k {
			break
		}
		if want[id] {
			hit++
		}
	}
	return float64(hit) / float64(len(want))
}

// hitAtK = 1 if any relevant doc is in top-k, else 0 (= Recall@k when |Rel|=1).
func hitAtK(ranked []string, rel map[string]int, k int) float64 {
	want := relevant(rel)
	for i, id := range ranked {
		if i >= k {
			break
		}
		if want[id] {
			return 1
		}
	}
	return 0
}

// reciprocalRank = 1/(rank of first relevant), 0 if none (trec_eval recip_rank).
func reciprocalRank(ranked []string, rel map[string]int) float64 {
	want := relevant(rel)
	for i, id := range ranked {
		if want[id] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// rankOf returns the 0-based rank of id in ranked, or -1 if absent.
func rankOf(id string, ranked []string) int {
	for i, x := range ranked {
		if x == id {
			return i
		}
	}
	return -1
}

// meanBy averages a per-query metric over qids in a fixed (sorted) order so the
// aggregate is deterministic regardless of map iteration order. It sorts a COPY
// so it never mutates the caller's slice (a pure-looking averaging helper must
// have no side effects).
func meanBy(qids []string, f func(qid string) float64) float64 {
	if len(qids) == 0 {
		return 0
	}
	ids := append([]string(nil), qids...)
	sort.Strings(ids)
	var sum float64
	for _, q := range ids {
		sum += f(q)
	}
	return sum / float64(len(ids))
}

// existsInMemoriesTable reports whether a memory row exists for id — the
// COVERAGE probe in the §6 attribution switch. (false, nil) means the labeled id
// was never ingested (fix = connector), NOT that retrieval failed. A genuine
// DB/schema/connection error is returned as a non-nil error and MUST NOT be
// swallowed into false — otherwise an infrastructure fault silently misclassifies
// every gold doc as COVERAGE, misrouting the whole diagnosis to the connector
// (a named top risk). The caller fails loud on a real error.
func existsInMemoriesTable(ctx context.Context, db *sql.DB, id string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, id).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}
