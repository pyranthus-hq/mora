package mora

// The Jev rerank A/B: the same live vault, the same hand-labeled queries, scored
// once with the fused order and once with the reranked order.
//
// Double opt-in on purpose. MORA_EVAL_LIVE alone already runs several evals; it
// must not start shipping vault text to a third party as a side effect of
// running one of them. MORA_EVAL_JEV is the second, explicit switch.
//
// Read-only: reranking writes nothing, so unlike the Ollama A/B (which re-embeds
// and therefore works on an isolated copy) this scores the real index in place.

import (
	"os"
	"sort"
	"testing"
)

const jevInputCostPerMillionUSD = 0.042

func TestEvalJevRerankAB(t *testing.T) {
	if os.Getenv("MORA_EVAL_LIVE") == "" {
		t.Skip("set MORA_EVAL_LIVE=1 to score the real vault read-only")
	}
	if os.Getenv("MORA_EVAL_JEV") == "" {
		t.Skip("set MORA_EVAL_JEV=1 to send the candidate window to TypeSafe's API (vault text leaves the machine)")
	}
	qPath, rPath := "live_queries.tsv", "live_qrels.tsv"
	if _, err := os.Stat(rPath); err != nil {
		t.Skipf("hand-label %s (+ %s) first — the A/B needs gold labels", rPath, qPath)
	}
	j, err := newJevReranker()
	if err != nil {
		t.Skipf("no TypeSafe credentials (%v) — set TYPESAFE_API_KEY or ~/.typesafe/api-key.env", err)
	}

	cfg := liveCfgOrSkip(t)
	ctx := testCtx(t)
	queries := loadQueries(t, qPath)
	rel, _, qids := loadQrels(t, rPath)

	type row struct {
		qid                    string
		q                      string
		baseR10, baseMRR       float64
		rerankedR10, rerankMRR float64
	}
	var rows []row
	var nScored int
	var sumBaseR10, sumBaseMRR, sumRerR10, sumRerMRR float64

	for _, qid := range qids {
		q, ok := queries[qid]
		if !ok {
			t.Logf("WARN %s: no query text — skipped", qid)
			continue
		}
		golds, isNeg := goldIDs(rel[qid])
		if isNeg || len(golds) == 0 {
			// Negative controls have no relevant set; reranking a window that
			// should stay empty is not a recall question.
			continue
		}

		rerankOv = nil
		base, _ := mustHybridTrace(t, ctx, cfg, q, kHybrid, kHybrid*5)
		baseIDs := idList(base)

		rerankOv = j
		reranked, _ := mustHybridTrace(t, ctx, cfg, q, kHybrid, kHybrid*5)
		rerankedIDs := idList(reranked)
		rerankOv = nil

		r := row{
			qid: qid, q: q,
			baseR10:     recallAtK(baseIDs, rel[qid], kHybrid),
			baseMRR:     reciprocalRank(baseIDs, rel[qid]),
			rerankedR10: recallAtK(rerankedIDs, rel[qid], kHybrid),
			rerankMRR:   reciprocalRank(rerankedIDs, rel[qid]),
		}
		rows = append(rows, r)
		nScored++
		sumBaseR10 += r.baseR10
		sumBaseMRR += r.baseMRR
		sumRerR10 += r.rerankedR10
		sumRerMRR += r.rerankMRR
	}
	t.Cleanup(func() { rerankOv = nil })

	if nScored == 0 {
		t.Skip("no scoreable queries in the live qrels")
	}

	sort.Slice(rows, func(a, b int) bool {
		da := rows[a].rerankMRR - rows[a].baseMRR
		db := rows[b].rerankMRR - rows[b].baseMRR
		if da != db {
			return da < db // regressions first: they are what decides this
		}
		return rows[a].qid < rows[b].qid
	})

	t.Logf("=== Jev rerank A/B (%s, window=%d, k=%d, n=%d labeled queries) ===",
		j.Name(), rerankWindow, kHybrid, nScored)
	var improved, regressed, unchanged int
	for _, r := range rows {
		dMRR := r.rerankMRR - r.baseMRR
		switch {
		case dMRR > 0:
			improved++
		case dMRR < 0:
			regressed++
		default:
			unchanged++
		}
		t.Logf("%-6s %-46q R@%d %.3f->%.3f  MRR %.3f->%.3f  (dMRR %+.3f)",
			r.qid, truncQ(r.q, 44), kHybrid, r.baseR10, r.rerankedR10, r.baseMRR, r.rerankMRR, dMRR)
	}

	n := float64(nScored)
	t.Logf("--- aggregate over %d queries ---", nScored)
	t.Logf("Recall@%d  fused %.4f  reranked %.4f  (delta %+.4f)",
		kHybrid, sumBaseR10/n, sumRerR10/n, (sumRerR10-sumBaseR10)/n)
	t.Logf("MRR       fused %.4f  reranked %.4f  (delta %+.4f)",
		sumBaseMRR/n, sumRerMRR/n, (sumRerMRR-sumBaseMRR)/n)
	t.Logf("queries improved %d / regressed %d / unchanged %d", improved, regressed, unchanged)

	u := j.Usage()
	cost := float64(u.InputTokens) / 1e6 * jevInputCostPerMillionUSD
	t.Logf("measured spend: %d calls, %d input tokens, $%.6f (at $%.3f/M input)",
		u.Calls, u.InputTokens, cost, jevInputCostPerMillionUSD)
	if u.Calls > 0 {
		t.Logf("per-query: %d input tokens, $%.8f", u.InputTokens/u.Calls, cost/float64(u.Calls))
	}

	// Report-only, like the other A/B arms. A 13-query gold set is a direction,
	// not a verdict, and a gate here would let a small sample freeze the design.
	if sumRerMRR < sumBaseMRR {
		t.Logf("NOTE: reranking LOWERED mean MRR on this set — do not ship it on this evidence")
	}
}

func truncQ(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
