package mora

// THROWAWAY DIAGNOSTIC (2026-09-18 spike). Not a regression test.
//
// Answers one question: when a query's gold does not reach the user, is it inside
// the rerank window, deeper in the pool, or missing from every retrieval arm?
// Those three answers point at three different layers, and only the first is a
// ranking problem a reranker could fix.
//
// MORA_EVAL_OLLAMA=1 runs it against an isolated copy of the vault re-indexed
// with the Ollama embedder, so the window question can be asked of a semantic
// index rather than the static-hash one in production.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvalWindowDiagnostic(t *testing.T) {
	if os.Getenv("MORA_EVAL_LIVE") == "" {
		t.Skip("set MORA_EVAL_LIVE=1 to score the real vault read-only")
	}
	queries := loadQueries(t, "live_queries.tsv")
	rel, _, qids := loadQrels(t, "live_qrels.tsv")

	cfg := liveCfgOrSkip(t)
	label := "static-hash (live index, read-only)"

	if os.Getenv("MORA_EVAL_OLLAMA") != "" {
		t.Setenv("MORA_EMBEDDER", "ollama")
		model := chooseEmbedderModelID(t)
		if !strings.HasPrefix(model, "ollama:") {
			t.Skipf("Ollama daemon unreachable (embedder resolved to %q)", model)
		}
		realCfg, err := loadConfigFor(testCtx(t))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		srcVault := realCfg.VaultDir
		if v := os.Getenv("MORA_EVAL_LIVE"); v != "1" && v != "true" && dirHasAny(v, "memories", "sources") {
			srcVault = v
		}
		withTempHome(t)
		run(t, "init")
		tmp := mustConfig(t)
		for _, sub := range []string{"memories", "sources"} {
			if s := filepath.Join(srcVault, sub); dirHasAny(srcVault, sub) {
				copyTree(t, s, filepath.Join(tmp.VaultDir, sub))
			}
		}
		t.Setenv("MORA_EMBEDDER", "ollama")
		if _, err := rebuildIndex(testCtx(t), tmp); err != nil {
			t.Fatalf("rebuildIndex (ollama): %v", err)
		}
		cfg = tmp
		label = model + " (isolated copy)"
	}

	ctx := testCtx(t)
	const window = 30 // the rerank stage's window

	rankOf := func(ids []string, target string) int {
		for i, id := range ids {
			if id == target {
				return i + 1
			}
		}
		return 0
	}

	tally := map[string]int{}
	t.Logf("=== window diagnostic: %s ===", label)

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
		mems, tr := mustHybridTrace(t, ctx, cfg, q, kHybrid, 400)
		top := idList(mems)

		t.Logf("\n%s  %q  (fused=%d)", qid, q, len(tr.Fused))
		for _, g := range golds {
			fr := rankOf(tr.Fused, g)
			var verdict, bucket string
			switch {
			case rankOf(top, g) > 0:
				bucket, verdict = "VISIBLE", "VISIBLE (in top-10)"
			case fr > 0 && fr <= window:
				bucket, verdict = "RERANKABLE", fmt.Sprintf("IN WINDOW at fused #%d - RERANK CAN FIX", fr)
			case fr > 0:
				bucket, verdict = "BEYOND_WINDOW", fmt.Sprintf("BEYOND WINDOW at fused #%d - needs deeper window", fr)
			default:
				bucket, verdict = "ABSENT", "ABSENT FROM FUSION - retrieval problem, rerank cannot help"
			}
			tally[bucket]++
			t.Logf("   gold %-46s %s", trimID(g), verdict)
			t.Logf("        arms: fts#%d vec#%d graph#%d seg#%d",
				rankOf(tr.FTS, g), rankOf(tr.Vec, g), rankOf(tr.Graph, g), rankOf(tr.Segment, g))
		}
	}
	t.Logf("\n=== TALLY (%s) ===", label)
	for _, b := range []string{"VISIBLE", "RERANKABLE", "BEYOND_WINDOW", "ABSENT"} {
		t.Logf("  %-14s %d", b, tally[b])
	}
}

func trimID(s string) string {
	if len(s) > 46 {
		return s[:43] + "..."
	}
	return s
}
