package mora

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// vecPool is a tiny helper: ordered ids + their fused scores + their vectors, the
// three inputs mmrRerank takes. Vectors are hand-built UNIT vectors so cosine (a raw
// dot product of L2-normalized vectors) yields the exact similarities the assertions
// rely on.
func relMap(pairs ...any) map[string]float64 {
	m := map[string]float64{}
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i].(string)] = pairs[i+1].(float64)
	}
	return m
}

// TestMMRRerankNoOpAtLambdaOne: λ=1 zeroes the diversity term, so greedy MMR reduces
// to pure relevance order — which IS the incoming fused order. Output == input.
func TestMMRRerankNoOpAtLambdaOne(t *testing.T) {
	ids := []string{"a", "b", "c"}
	rel := relMap("a", 1.0, "b", 0.6, "c", 0.2)
	vec := map[string][]float32{"a": {1, 0}, "b": {1, 0}, "c": {0, 1}} // dups present, but λ=1 ignores them
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 1.0})
	if !reflect.DeepEqual(got, ids) {
		t.Fatalf("λ=1 must equal fused order, got %v want %v", got, ids)
	}
}

// TestMMRRerankSingleAndEmpty: trivial pools and a pool with fewer than two
// vector-backed candidates are clean no-ops (nothing to diversify against).
func TestMMRRerankSingleAndEmpty(t *testing.T) {
	if got := mmrRerank(nil, nil, nil, mmrParams{lambda: 0.7}); len(got) != 0 {
		t.Fatalf("empty ⇒ empty, got %v", got)
	}
	if got := mmrRerank([]string{"x"}, relMap("x", 0.5), map[string][]float32{"x": {1, 0}}, mmrParams{lambda: 0.7}); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("single ⇒ unchanged, got %v", got)
	}
	// Two candidates but only one has a vector ⇒ <2 in the diversify pool ⇒ unchanged.
	ids := []string{"a", "b"}
	got := mmrRerank(ids, relMap("a", 1.0, "b", 0.5), map[string][]float32{"a": {1, 0}}, mmrParams{lambda: 0.7})
	if !reflect.DeepEqual(got, ids) {
		t.Fatalf("<2 vectors ⇒ unchanged, got %v", got)
	}
}

// TestMMRRerankAllEqualRelevance: an all-equal pool has span≤ε; relNorm falls back to
// 1.0 for all (no divide-by-zero, no NaN, no panic), the fused-rank-0 doc seeds, and
// the result is a valid permutation.
func TestMMRRerankAllEqualRelevance(t *testing.T) {
	ids := []string{"a", "b", "c"}
	rel := relMap("a", 0.5, "b", 0.5, "c", 0.5)
	vec := map[string][]float32{"a": {1, 0}, "b": {0, 1}, "c": {0, 1}}
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.7})
	if got[0] != "a" {
		t.Fatalf("all-equal relevance ⇒ seed is fused-rank-0 (a), got %v", got)
	}
	assertPermutation(t, ids, got)
}

// TestClampPosNegativeCosine locks the signed-cosine bug fix. Unit: clampPos floors
// negatives at 0. Integration: an anti-similar low-relevance doc (cos=-1 to the seed)
// must NOT be promoted above a more-relevant orthogonal doc — without the clamp the
// negative cosine would flip into a +0.5 novelty BONUS and wrongly win.
func TestClampPosNegativeCosine(t *testing.T) {
	for _, c := range []struct {
		in, want float64
	}{{-1, 0}, {-1e-9, 0}, {0, 0}, {0.5, 0.5}, {1, 1}} {
		if got := clampPos(c.in); got != c.want {
			t.Fatalf("clampPos(%v)=%v want %v", c.in, got, c.want)
		}
	}
	// A=seed (rel 1.0), Z=orthogonal & more relevant (rel 0.8), Y=anti-similar & less
	// relevant (rel 0.3). Correct order after A: Z then Y. Raw (unclamped) cosine would
	// give Y a +0.5 bonus and pick Y before Z.
	ids := []string{"A", "Z", "Y"}
	rel := relMap("A", 1.0, "Z", 0.8, "Y", 0.3)
	vec := map[string][]float32{"A": {1, 0}, "Z": {0, 1}, "Y": {-1, 0}}
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.5})
	if rankOf("Z", got) > rankOf("Y", got) {
		t.Fatalf("clamp failed: anti-similar Y promoted above more-relevant Z, got %v", got)
	}
}

// TestMMRRerankDiversifies: with a redundant duplicate and a novel-but-less-relevant
// doc, MMR selects the novel doc before the duplicate (the core diversity behavior).
func TestMMRRerankDiversifies(t *testing.T) {
	ids := []string{"A", "dup", "B"}
	rel := relMap("A", 1.0, "dup", 0.7, "B", 0.4)
	vec := map[string][]float32{"A": {1, 0}, "dup": {1, 0}, "B": {0, 1}} // dup is a perfect copy of A
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.5})
	if rankOf("B", got) > rankOf("dup", got) {
		t.Fatalf("MMR should select novel B before redundant dup, got %v", got)
	}
}

// TestMMRRerankMissingVectorPinned: a candidate with no stored vector keeps its exact
// fused index while the vector-backed docs around it reorder.
func TestMMRRerankMissingVectorPinned(t *testing.T) {
	ids := []string{"a", "gOnly", "b", "c"}
	rel := relMap("a", 1.0, "gOnly", 0.5, "b", 0.8, "c", 0.6)
	vec := map[string][]float32{"a": {1, 0, 0}, "b": {1, 0, 0}, "c": {0, 1, 0}} // gOnly absent
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.5})
	if got[1] != "gOnly" {
		t.Fatalf("missing-vector doc must stay pinned at index 1, got %v", got)
	}
	if rankOf("c", got) > rankOf("b", got) {
		t.Fatalf("vector-backed c (novel) should be promoted over redundant b, got %v", got)
	}
	assertPermutation(t, ids, got)
}

// TestMMRRerankMixedVecAndPinnedTopK: a vector-backed doc promoted from a low fused
// rank can cross OVER a pinned (no-vector) doc's position; the pinned doc keeps its
// absolute index and the output is a valid permutation.
func TestMMRRerankMixedVecAndPinnedTopK(t *testing.T) {
	ids := []string{"a", "b", "gPinned", "c"}
	rel := relMap("a", 1.0, "b", 0.9, "gPinned", 0.7, "c", 0.5)
	vec := map[string][]float32{"a": {1, 0, 0}, "b": {1, 0, 0}, "c": {0, 1, 0}} // gPinned absent
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.5})
	if got[2] != "gPinned" {
		t.Fatalf("pinned doc must keep absolute index 2, got %v", got)
	}
	if rankOf("c", got) > rankOf("b", got) {
		t.Fatalf("novel c should cross redundant b, got %v", got)
	}
	assertPermutation(t, ids, got)
}

// TestMMRRerankPinnedDocDoesNotRescalePool locks the fix for the relNorm-domain bug:
// a pinned (no-vector) doc with an EXTREME fused score must not change the relative MMR
// order of the vector-backed pool. relNorm is min-maxed over the pool only, so adding a
// pinned outlier (low or high) cannot stretch the span and silently rescale effective λ.
func TestMMRRerankPinnedDocDoesNotRescalePool(t *testing.T) {
	rel := relMap("A", 1.0, "dup", 0.92, "B", 0.80)
	vec := map[string][]float32{"A": {1, 0}, "dup": {1, 0}, "B": {0, 1}} // dup is a perfect copy of A
	base := mmrRerank([]string{"A", "dup", "B"}, rel, vec, mmrParams{lambda: 0.7})

	// A pinned doc at the LOW extreme (fused 0.0) appended to the tail.
	relLo := relMap("A", 1.0, "dup", 0.92, "B", 0.80, "P", 0.0)
	gotLo := mmrRerank([]string{"A", "dup", "B", "P"}, relLo, vec, mmrParams{lambda: 0.7})
	if gotLo[3] != "P" {
		t.Fatalf("pinned doc must stay at its fused index 3, got %v", gotLo)
	}
	if !reflect.DeepEqual(gotLo[:3], base) {
		t.Fatalf("low-extreme pinned doc rescaled the pool order: %v (pool %v) vs base %v", gotLo, gotLo[:3], base)
	}

	// A pinned doc at the HIGH extreme (fused 2.0) prepended to the head.
	relHi := relMap("A", 1.0, "dup", 0.92, "B", 0.80, "P", 2.0)
	gotHi := mmrRerank([]string{"P", "A", "dup", "B"}, relHi, vec, mmrParams{lambda: 0.7})
	if gotHi[0] != "P" {
		t.Fatalf("pinned doc must stay at its fused index 0, got %v", gotHi)
	}
	if !reflect.DeepEqual(gotHi[1:], base) {
		t.Fatalf("high-extreme pinned doc rescaled the pool order: %v (pool %v) vs base %v", gotHi, gotHi[1:], base)
	}
}

// TestMMRRerankTieBreakFusedRank: candidates with identical MMR scores resolve to the
// earlier fused rank (deterministic, no map-order leak).
func TestMMRRerankTieBreakFusedRank(t *testing.T) {
	ids := []string{"A", "t1", "t2"}
	rel := relMap("A", 1.0, "t1", 0.5, "t2", 0.5)
	vec := map[string][]float32{"A": {1, 0}, "t1": {0, 1}, "t2": {0, 1}} // t1,t2 identical ⇒ tie
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.7})
	if !reflect.DeepEqual(got, []string{"A", "t1", "t2"}) {
		t.Fatalf("tie must resolve to fused order, got %v", got)
	}
}

// TestMMRRerankDeterminismCrossRun: same input, 50 runs, byte-identical output.
func TestMMRRerankDeterminismCrossRun(t *testing.T) {
	ids := []string{"a", "dup", "b", "c", "d"}
	rel := relMap("a", 1.0, "dup", 0.9, "b", 0.7, "c", 0.5, "d", 0.3)
	vec := map[string][]float32{"a": {1, 0, 0}, "dup": {1, 0, 0}, "b": {0, 1, 0}, "c": {0, 0, 1}, "d": {0, 1, 0}}
	first := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.6})
	for i := 0; i < 50; i++ {
		if got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.6}); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %v vs %v", i, got, first)
		}
	}
}

// TestMMRRerankDoesNotMutateInput: the input ids slice and rel map are untouched.
func TestMMRRerankDoesNotMutateInput(t *testing.T) {
	ids := []string{"a", "dup", "b"}
	idsCopy := append([]string(nil), ids...)
	rel := relMap("a", 1.0, "dup", 0.7, "b", 0.4)
	vec := map[string][]float32{"a": {1, 0}, "dup": {1, 0}, "b": {0, 1}}
	_ = mmrRerank(ids, rel, vec, mmrParams{lambda: 0.5})
	if !reflect.DeepEqual(ids, idsCopy) {
		t.Fatalf("input ids mutated: %v want %v", ids, idsCopy)
	}
	if rel["a"] != 1.0 || rel["dup"] != 0.7 || rel["b"] != 0.4 {
		t.Fatalf("input rel mutated: %v", rel)
	}
}

// TestConfigMMRGating locks mmr() precedence: off by default; the MMR bool yields
// default params with force=false; the mmrOv seam always wins and is the ONLY source
// of force.
func TestConfigMMRGating(t *testing.T) {
	if p := (Config{}).mmr(); p != nil {
		t.Fatalf("default Config ⇒ MMR off (nil), got %+v", p)
	}
	p := (Config{MMR: true}).mmr()
	if p == nil || p.lambda != defaultLambda || p.force {
		t.Fatalf("MMR:true ⇒ {λ=%v, force=false}, got %+v", defaultLambda, p)
	}
	ov := &mmrParams{lambda: 0.3, force: true}
	if got := (Config{mmrOv: ov}).mmr(); got != ov {
		t.Fatalf("mmrOv must win, got %+v", got)
	}
	if got := (Config{MMR: true, mmrOv: ov}).mmr(); got != ov {
		t.Fatalf("mmrOv must win over MMR bool, got %+v", got)
	}
}

// TestMMRActivePredicate: MMR runs under a semantic embedder (useVec) or the forced
// eval seam, never under static-hash from a production (force=false) config.
func TestMMRActivePredicate(t *testing.T) {
	if mmrActive(false, &mmrParams{force: false}) {
		t.Fatal("static-hash + production params ⇒ MMR must NOT run")
	}
	if !mmrActive(false, &mmrParams{force: true}) {
		t.Fatal("forced seam ⇒ MMR runs even under static-hash")
	}
	if !mmrActive(true, &mmrParams{force: false}) {
		t.Fatal("semantic embedder ⇒ MMR runs")
	}
}

// TestLoadVectorsByID: present ids return decoded vectors under the active model;
// absent ids and a wrong model id are simply missing from the map (caller pins them).
func TestLoadVectorsByID(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	db := openRO(t, cfg)
	defer db.Close()
	model := defaultEmbedder().ModelID()

	got, err := loadVectorsByID(ctx, db, model, []string{"synth/migration-1", "synth/oauth-exact", "synth/does-not-exist"})
	if err != nil {
		t.Fatalf("loadVectorsByID: %v", err)
	}
	if len(got["synth/migration-1"]) == 0 || len(got["synth/oauth-exact"]) == 0 {
		t.Fatalf("present ids must return non-empty vectors, got %v", got)
	}
	if _, ok := got["synth/does-not-exist"]; ok {
		t.Fatal("absent id must be missing from the map")
	}
	wrong, err := loadVectorsByID(ctx, db, "ollama:nonexistent-model", []string{"synth/migration-1"})
	if err != nil {
		t.Fatalf("loadVectorsByID (wrong model): %v", err)
	}
	if len(wrong) != 0 {
		t.Fatalf("wrong model ⇒ empty map, got %v", wrong)
	}
}

// forcedMMR is a Config with MMR forced ON under the static-hash floor (the eval
// seam) at the given λ — the only way to exercise the rerank deterministically in
// CGO=0 CI (no Ollama).
func forcedMMR(base Config, lambda float64) Config {
	c := base
	c.mmrOv = &mmrParams{lambda: lambda, force: true}
	return c
}

// TestMMRDoesNotMutateTraceFused locks that MMR never touches tr.Fused: the §6 eval
// attribution must keep seeing the PURE fusion ranking, not the reranked order. Forced
// MMR vs off must yield byte-identical tr.Fused.
func TestMMRDoesNotMutateTraceFused(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	q := "Postgres database migration to the new cluster"
	_, trOff, err := hybridSearchTrace(ctx, cfg, q, "", kHybrid, tracePoolDepth)
	if err != nil {
		t.Fatal(err)
	}
	_, trOn, err := hybridSearchTrace(ctx, forcedMMR(cfg, defaultLambda), q, "", kHybrid, tracePoolDepth)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(trOff.Fused, trOn.Fused) {
		t.Fatalf("MMR altered tr.Fused (§6 attribution corrupted):\noff=%v\non =%v", trOff.Fused, trOn.Fused)
	}
}

// TestMMRProductionOffByteIdentical locks the hard requirement: with the default
// Config (MMR off), the hybrid path emits EXACTLY the fused order (truncated) for
// every golden query — i.e. the production default path is byte-identical to the
// pre-W2 behavior, MMR contributing nothing until opted in.
func TestMMRProductionOffByteIdentical(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	queries := loadQueries(t, filepath.Join("testdata", "eval", "golden_queries.tsv"))
	for qid, q := range queries {
		mems, tr, err := hybridSearchTrace(ctx, cfg, q, "", kHybrid, 0)
		if err != nil {
			t.Fatalf("%s: %v", qid, err)
		}
		want := tr.Fused
		if len(want) > kHybrid {
			want = want[:kHybrid]
		}
		got := idList(mems)
		if len(got) == 0 && len(want) == 0 {
			continue // negative controls (q4/q9) retrieve nothing — nil vs empty is not a reorder
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: MMR-off must equal fused order\n got=%v\nwant=%v", qid, got, want)
		}
	}
}

// TestMMRPreservesGraphOnlyAdmission is the W1 killer-feature guarantee: docs admitted
// ONLY via the people-graph arm (q2 "Neil Patel"→synth/venue, q7 "Maya Chen"→
// synth/newhire) must STILL be in the hybrid top-k after a forced MMR rerank — a pure
// permutation that seeds on the top relevance doc never drops a graph-only admission.
func TestMMRPreservesGraphOnlyAdmission(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ q, gold string }{
		{"what did Neil Patel organize", "synth/venue"},
		{"what did Maya Chen send about onboarding", "synth/newhire"},
	} {
		mems, err := hybridSearch(ctx, forcedMMR(cfg, defaultLambda), tc.q, "", kHybrid)
		if err != nil {
			t.Fatalf("%q: %v", tc.q, err)
		}
		if rankOf(tc.gold, idList(mems)) < 0 {
			t.Fatalf("MMR dropped graph-only admission %s for %q: got %v", tc.gold, tc.q, idList(mems))
		}
	}
}

// TestMMRForcedNoVectorsNoOp locks the pre-I2 safety: an index with memories but NO
// stored vectors (mem_vectors empty ⇒ vecOK=false) must, even under the forcing eval
// seam, skip MMR and return the fused order — never deref the nil Embedder that is
// only set inside the vecOK block. (Found by codex review of the W2 diff.)
func TestMMRForcedNoVectorsNoOp(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-I2 index: memories indexed, but the vector table emptied.
	rw, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.ExecContext(ctx, `DELETE FROM mem_vectors`); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	q := "PKCE authorization code flow" // q1: a real FTS hit ⇒ the pool is non-empty
	off, err := hybridSearch(ctx, cfg, q, "", kHybrid)
	if err != nil {
		t.Fatal(err)
	}
	on, err := hybridSearch(ctx, forcedMMR(cfg, defaultLambda), q, "", kHybrid)
	if err != nil {
		t.Fatalf("forced MMR with no vectors must not error/panic: %v", err)
	}
	if !reflect.DeepEqual(idList(off), idList(on)) {
		t.Fatalf("forced MMR with no vectors must no-op (fused order):\noff=%v\non =%v", idList(off), idList(on))
	}
}

// TestEvalMMRPoolPrecondition locks the crowding the regression gate depends on: q5's
// fused pool must reach >= kFTS+1 (so an MMR demotion can cross the cutoff) AND the
// MMR-off baseline must keep BOTH near-dup golds in the top-kFTS (so a drop is a real
// regression, not pre-existing noise). If a future fixture edit erodes the q5
// distractor pool, this fails first — before the gate can silently go blind.
func TestEvalMMRPoolPrecondition(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	q := "Postgres database migration to the new cluster"
	_, tr := mustHybridTrace(t, ctx, cfg, q, kHybrid, tracePoolDepth)
	if len(tr.Fused) < kFTS+1 {
		t.Fatalf("q5 fused pool=%d, want >=%d — distractors no longer crowd kFTS", len(tr.Fused), kFTS+1)
	}
	mems, err := hybridSearch(ctx, cfg, q, "", kHybrid)
	if err != nil {
		t.Fatal(err)
	}
	q5gold := map[string]int{"synth/migration-1": 1, "synth/migration-2": 1}
	if r := recallAtK(idList(mems), q5gold, kFTS); r != 1.0 {
		t.Fatalf("MMR-off Recall@%d baseline=%.2f, want 1.0 — both golds must be in top-%d pre-rerank: %v", kFTS, r, kFTS, idList(mems))
	}
}

// TestEvalMMRNoRegression is the W2 SHIP GATE. Under the static-hash floor (forced via
// the mmrOv seam, the only CGO=0-CI-deterministic way to exercise MMR), it asserts MMR
// regresses NO golden query's Recall@{kFTS,kHybrid} or MRR across a λ band that BRACKETS
// the shipped 0.7. The band matters: at λ=0.7 MMR happens to be a no-op on this small
// fixture, so testing only 0.7 would be vacuous (comparing equal lists). The lower band
// values DO reorder q5's tail (a redundant distractor is demoted while both golds stay
// in-k), so the no-regression assertions actually exercise the relevance/diversity
// tradeoff — a bug that demoted a still-relevant doc at an active λ would FIRE. A
// non-vacuity guard fails loud if MMR turns out to be inert at every swept λ, and a λ=0
// (pure-diversity) control proves the fixture can register a demotion across k at all.
func TestEvalMMRNoRegression(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("testdata", "eval")
	queries := loadQueries(t, filepath.Join(dir, "golden_queries.tsv"))
	rel, _, qids := loadQrels(t, filepath.Join(dir, "golden_qrels.tsv"))

	idsFor := func(c Config, qid string) []string {
		mems, err := hybridSearch(ctx, c, queries[qid], "", kHybrid)
		if err != nil {
			t.Fatalf("hybridSearch(%s): %v", qid, err)
		}
		return idList(mems)
	}
	scored := func(qid string) bool {
		_, isNeg := goldIDs(rel[qid])
		return !isNeg // skip NONE negative-control rows (q4/q9)
	}

	off := cfg
	offOrder := map[string][]string{}
	for _, qid := range qids {
		offOrder[qid] = idsFor(off, qid)
	}

	// No-regression across a band bracketing the shipped λ=0.7 (defaultLambda included).
	band := []float64{0.4, 0.5, 0.6, defaultLambda}
	reordered := false
	for _, lambda := range band {
		on := forcedMMR(cfg, lambda)
		for _, qid := range qids {
			if !scored(qid) {
				continue
			}
			onIDs := idsFor(on, qid)
			if !reflect.DeepEqual(onIDs, offOrder[qid]) {
				reordered = true
			}
			for _, k := range []int{kFTS, kHybrid} {
				if rOn, rOff := recallAtK(onIDs, rel[qid], k), recallAtK(offOrder[qid], rel[qid], k); rOn < rOff-1e-9 {
					t.Errorf("MMR REGRESSED Recall@%d for %s at λ=%.2f: %.4f -> %.4f", k, qid, lambda, rOff, rOn)
				}
			}
			if mOn, mOff := reciprocalRank(onIDs, rel[qid]), reciprocalRank(offOrder[qid], rel[qid]); mOn < mOff-1e-9 {
				t.Errorf("MMR REGRESSED MRR for %s at λ=%.2f: %.4f -> %.4f", qid, lambda, mOff, mOn)
			}
		}
	}

	// Non-vacuity: the band must actually exercise the reranker on at least one query,
	// else the no-regression assertions above compared equal lists and prove nothing.
	if !reordered {
		t.Fatal("GATE VACUOUS: MMR reordered no scored query across the whole λ band — the no-regression checks are comparing identical lists; tune the q5 distractor pool")
	}

	// Self-proving sensitivity: pure diversity (λ=0) MUST drop q5 Recall@kFTS. If it does
	// not, the q5 distractor pool no longer crowds the cutoff and the gate is blind to a
	// demotion crossing k — fail loud (modeled on TestEvalAB's vecHits==0 fatal).
	bad := forcedMMR(cfg, 0.0)
	rBase := recallAtK(offOrder["q5"], rel["q5"], kFTS)
	rBad := recallAtK(idsFor(bad, "q5"), rel["q5"], kFTS)
	if !(rBad < rBase-1e-9) {
		t.Fatalf("GATE BLIND: λ=0 did not drop q5 Recall@%d (%.4f -> %.4f) — the q5 distractor pool no longer crosses k; fix the fixture", kFTS, rBase, rBad)
	}
}

// TestEvalMMRAB is the REPORT-ONLY semantic benefit measurement: it re-indexes the
// synthetic fixture under a real Ollama embedder and prints MMR-off vs on Recall@k +
// MRR across a small λ sweep. It NEVER gates (the static-hash gate proves the algorithm
// + safety floor; this measures whether MMR helps under true semantics — the evidence
// required before Config.MMR is ever flipped on by default). Opt in with
// MORA_EVAL_LIVE=1 + a running Ollama daemon; skipped in CGO=0 CI.
func TestEvalMMRAB(t *testing.T) {
	if os.Getenv("MORA_EVAL_LIVE") == "" {
		t.Skip("set MORA_EVAL_LIVE=1 (+ a running Ollama daemon) to measure MMR semantic benefit")
	}
	t.Setenv("MORA_EMBEDDER", "ollama")
	model := chooseEmbedder().ModelID()
	if !strings.HasPrefix(model, "ollama:") {
		t.Skipf("Ollama unreachable (embedder=%q) — the AB needs real vectors; skipping (never gates)", model)
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex (ollama): %v", err)
	}
	db := openRO(t, cfg)
	var nVec int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM mem_vectors WHERE model = ?`, model).Scan(&nVec)
	db.Close()
	if nVec == 0 {
		t.Skipf("0 vectors stored for %q — embedding degraded to static; skipping", model)
	}

	dir := filepath.Join("testdata", "eval")
	queries := loadQueries(t, filepath.Join(dir, "golden_queries.tsv"))
	rel, _, qids := loadQrels(t, filepath.Join(dir, "golden_qrels.tsv"))
	scored := func() []string {
		var out []string
		for _, qid := range qids {
			if len(relevant(rel[qid])) > 0 {
				out = append(out, qid)
			}
		}
		return out
	}()
	rdb := openRO(t, cfg)
	defer rdb.Close()
	// intraListRedundancy = mean pairwise clampPos(cosine) among a result list — the
	// quantity MMR actually minimizes. Recall@k/MRR are relevance metrics blind to
	// diversity, so on a set where the gold docs stay in-k they can't show MMR's effect;
	// this can. Lower ⇒ less repetitive top-k.
	intraListRedundancy := func(ids []string) float64 {
		vecs, err := loadVectorsByID(ctx, rdb, model, ids)
		if err != nil {
			t.Fatalf("loadVectorsByID: %v", err)
		}
		var sum float64
		var n int
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				vi, oi := vecs[ids[i]]
				vj, oj := vecs[ids[j]]
				if oi && oj {
					sum += clampPos(cosine(vi, vj))
					n++
				}
			}
		}
		if n == 0 {
			return 0
		}
		return sum / float64(n)
	}
	order := func(c Config, qid string) []string {
		mems, err := hybridSearch(ctx, c, queries[qid], "", kHybrid)
		if err != nil {
			t.Fatalf("hybridSearch(%s): %v", qid, err)
		}
		return idList(mems)
	}
	measure := func(c Config) (recall, mrr, redundancy, reorderFrac float64) {
		var reordered int
		recall = meanBy(scored, func(qid string) float64 { return recallAtK(order(c, qid), rel[qid], kHybrid) })
		mrr = meanBy(scored, func(qid string) float64 { return reciprocalRank(order(c, qid), rel[qid]) })
		redundancy = meanBy(scored, func(qid string) float64 { return intraListRedundancy(order(c, qid)) })
		for _, qid := range scored {
			if !reflect.DeepEqual(order(c, qid), order(cfg, qid)) {
				reordered++
			}
		}
		return recall, mrr, redundancy, float64(reordered) / float64(len(scored))
	}
	rOff, mOff, dOff, _ := measure(cfg)
	t.Logf("=== MMR A/B under %s (synthetic golden set, %d vectors, %d scored queries) ===", model, nVec, len(scored))
	t.Logf("MMR off            Recall@%d=%.4f MRR=%.4f intra-list-redundancy=%.4f", kHybrid, rOff, mOff, dOff)
	for _, lambda := range []float64{0.5, 0.7, 0.9} {
		c := cfg
		c.mmrOv = &mmrParams{lambda: lambda} // force=false: real useVec under Ollama
		r, m, d, rf := measure(c)
		t.Logf("MMR on  (λ=%.1f)    Recall@%d=%.4f MRR=%.4f intra-list-redundancy=%.4f (reordered %.0f%% of queries vs off)",
			lambda, kHybrid, r, m, d, rf*100)
	}
}

// assertPermutation fails unless got is a permutation of want (same multiset of ids).
func assertPermutation(t *testing.T, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("length changed: got %v want a permutation of %v", got, want)
	}
	cw, cg := map[string]int{}, map[string]int{}
	for _, x := range want {
		cw[x]++
	}
	for _, x := range got {
		cg[x]++
	}
	if !reflect.DeepEqual(cw, cg) {
		t.Fatalf("not a permutation: got %v want a permutation of %v", got, want)
	}
}
