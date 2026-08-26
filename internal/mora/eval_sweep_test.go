package mora

// Fusion-weight tuning harness (the grid behind defaultFusion).
//
// TestEvalWeightSweep indexes an ISOLATED COPY of the live vault ONCE under Ollama,
// then re-scores the live golden set in-process for each candidate fusionParams
// (arm weights + RRF damping k). It exists because the live recall eval found a
// FUSION-dominant profile — gold docs retrieved by an arm but ranked past the cutoff
// — so the lever is fusion shape, not the embedder. Run it to pick the params that
// maximize HIT / minimize FUSION, then bake the winner into defaultFusion. The vec
// arm is only meaningful here because the index is built under a semantic embedder
// (chooseEmbedder() == ollama); under static-hash hybridSearch drops the vec arm.
//
// Report-only; never gates. Skips unless MORA_EVAL_LIVE=1, the live qrels exist, and
// a local Ollama daemon answers.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvalWeightSweep(t *testing.T) {
	if os.Getenv("MORA_EVAL_LIVE") == "" {
		t.Skip("set MORA_EVAL_LIVE=1 (+ live qrels + a running Ollama daemon) to sweep fusion weights")
	}
	qPath, rPath := "live_queries.tsv", "live_qrels.tsv"
	if _, err := os.Stat(rPath); err != nil {
		t.Skipf("hand-label %s (+ %s) first — the sweep needs gold labels", rPath, qPath)
	}
	// Build the index under Ollama so the vec arm carries real semantic signal.
	t.Setenv("MORA_EMBEDDER", "ollama")
	model := chooseEmbedderModelID(t)
	if !strings.HasPrefix(model, "ollama:") {
		t.Skipf("Ollama daemon unreachable (embedder=%q) — sweep needs it; skipping", model)
	}

	realCfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	srcVault := realCfg.VaultDir
	if v := os.Getenv("MORA_EVAL_LIVE"); v != "1" && v != "true" && dirHasAny(v, "memories", "sources") {
		srcVault = v
	}
	if !dirHasAny(srcVault, "memories", "sources") {
		t.Skipf("no live vault markdown at %s to copy", srcVault)
	}

	ctx := testCtx(t)
	queries := loadQueries(t, qPath)
	rel, meta, qids := loadQrels(t, rPath)

	// Isolated copy — index ONCE under Ollama; the sweep only varies fusion params.
	withTempHome(t)
	run(t, "init")
	tmpCfg := mustConfig(t)
	for _, sub := range []string{"memories", "sources"} {
		s := filepath.Join(srcVault, sub)
		if _, err := os.Stat(s); err == nil {
			copyTree(t, s, filepath.Join(tmpCfg.VaultDir, sub))
		}
	}
	if _, err := rebuildIndex(ctx, tmpCfg); err != nil {
		t.Fatalf("rebuildIndex (ollama): %v", err)
	}

	// Baseline reference: FTS-only + hybrid-at-defaultFusion, both surfaces.
	db := openRO(t, tmpCfg)
	t.Logf("=== baseline (defaultFusion=%+v) ===", defaultFusion)
	reportEval(t, ctx, tmpCfg, db, queries, rel, meta, qids)
	db.Close()

	type cand struct{ fts, vec, graph, k float64 }
	var grid []cand
	for _, k := range []float64{5, 10, 20, 60} {
		for _, fts := range []float64{1, 1.5, 2, 3} {
			for _, vec := range []float64{0.5, 1} {
				grid = append(grid, cand{fts: fts, vec: vec, graph: 1, k: k})
			}
		}
	}

	t.Logf("=== fusion sweep (Ollama, isolated copy of %s; %d gold queries; k_hyb=%d cutoff) ===", srcVault, len(qids), kHybrid)
	type result struct {
		c             cand
		hit, fus, ret int
	}
	best := result{hit: -1}
	for _, c := range grid {
		fp := fusionParams(c)
		cfg := tmpCfg
		cfg.SetFusionOverride(&fp)
		hist, _ := bucketHistogram(t, ctx, cfg, queries, rel, qids)
		r := result{c: c, hit: hist[bHIT], fus: hist[bFUSION], ret: hist[bRETRIEVAL]}
		t.Logf("k=%-4g fts=%-4g vec=%-4g graph=%g  ->  HIT=%2d FUSION=%2d RETRIEVAL=%d",
			c.k, c.fts, c.vec, c.graph, r.hit, r.fus, r.ret)
		if r.hit > best.hit || (r.hit == best.hit && r.fus < best.fus) {
			best = r
		}
	}
	t.Logf("BEST: k=%g fts=%g vec=%g graph=%g  ->  HIT=%d FUSION=%d RETRIEVAL=%d  (bake into defaultFusion if it beats baseline hybrid HIT)",
		best.c.k, best.c.fts, best.c.vec, best.c.graph, best.hit, best.fus, best.ret)
}
