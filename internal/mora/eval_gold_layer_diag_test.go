package mora

// THROWAWAY DIAGNOSTIC (2026-09-20). Not a regression test.
//
// The window diagnostic established that two golds fuse at rank #2 and still
// never reach the user. Fusion rank is not the failure, so the record is being
// removed somewhere between fusion and display. There are exactly two places
// that can do that: the visibility filtering inside loadMemoriesByID, and the
// Issue #237 corroborating-record clustering in clusterAndTruncate. This probe
// asks which one, per gold, by inspecting both layers directly.

import (
	"os"
	"testing"
)

func TestEvalGoldLayerDiagnostic(t *testing.T) {
	if os.Getenv("MORA_EVAL_LIVE") == "" {
		t.Skip("set MORA_EVAL_LIVE=1 to inspect the real vault read-only")
	}
	queries := loadQueries(t, "live_queries.tsv")
	rel, _, qids := loadQrels(t, "live_qrels.tsv")
	cfg := liveCfgOrSkip(t)
	ctx := testCtx(t)

	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		t.Fatalf("ensureIndexDB: %v", err)
	}
	defer db.Close()

	rankOf := func(ids []string, target string) int {
		for i, id := range ids {
			if id == target {
				return i + 1
			}
		}
		return 0
	}

	for _, qid := range qids {
		q, ok := queries[qid]
		if !ok {
			continue
		}
		golds, isNeg := goldIDs(rel[qid])
		if isNeg || len(golds) == 0 {
			continue
		}
		rerankOv = nil
		result, tr := mustHybridTrace(t, ctx, cfg, q, kHybrid, 400)

		// Layer 2: what survives visibility filtering, given the FULL fused order.
		survivors, err := loadMemoriesByID(ctx, cfg, db, tr.Fused)
		if err != nil {
			t.Fatalf("loadMemoriesByID: %v", err)
		}
		alive := map[string]bool{}
		for _, m := range survivors {
			alive[m.ID] = true
		}

		// Layer 3: what the clustering folded into somebody else's row.
		foldedInto := map[string]string{}
		for _, m := range result {
			for _, c := range m.Corroborating {
				foldedInto[c.ID] = m.ID
			}
		}
		top := idList(result)

		t.Logf("\n%s  %q  (fused=%d, visible-after-filter=%d, rows=%d)",
			qid, q, len(tr.Fused), len(survivors), len(result))
		for _, g := range golds {
			fr := rankOf(tr.Fused, g)
			switch {
			case rankOf(top, g) > 0:
				t.Logf("   %-52s fused#%-3d  DISPLAYED at row %d", trimID(g), fr, rankOf(top, g))
			case fr == 0:
				t.Logf("   %-52s ABSENT FROM FUSION (retrieval, not display)", trimID(g))
			case !alive[g]:
				t.Logf("   %-52s fused#%-3d  LOST TO VISIBILITY FILTER (pending delete or superseded)", trimID(g), fr)
			case foldedInto[g] != "":
				t.Logf("   %-52s fused#%-3d  FOLDED INTO %s as corroborating", trimID(g), fr, trimID(foldedInto[g]))
			default:
				t.Logf("   %-52s fused#%-3d  SURVIVED BOTH LAYERS YET NOT SHOWN (truncate/backfill)", trimID(g), fr)
			}
		}
	}
}
