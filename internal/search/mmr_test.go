package search

import (
	"context"
	"database/sql"
	"reflect"

	embedpkg "github.com/pyranthus-hq/mora/internal/embed"
	_ "modernc.org/sqlite"
	"testing"
)

func relMap(pairs ...any) map[string]float64 {
	m := map[string]float64{}
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i].(string)] = pairs[i+1].(float64)
	}
	return m
}

func rankOf(id string, ranked []string) int {
	for i, v := range ranked {
		if v == id {
			return i
		}
	}
	return -1
}
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

func TestMMRRerankNoOpAtLambdaOne(t *testing.T) {
	ids := []string{"a", "b", "c"}
	rel := relMap("a", 1.0, "b", 0.6, "c", 0.2)
	vec := map[string][]float32{"a": {1, 0}, "b": {1, 0}, "c": {0, 1}} // dups present, but λ=1 ignores them
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 1.0})
	if !reflect.DeepEqual(got, ids) {
		t.Fatalf("λ=1 must equal fused order, got %v want %v", got, ids)
	}
}

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

func TestMMRRerankDiversifies(t *testing.T) {
	ids := []string{"A", "dup", "B"}
	rel := relMap("A", 1.0, "dup", 0.7, "B", 0.4)
	vec := map[string][]float32{"A": {1, 0}, "dup": {1, 0}, "B": {0, 1}} // dup is a perfect copy of A
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.5})
	if rankOf("B", got) > rankOf("dup", got) {
		t.Fatalf("MMR should select novel B before redundant dup, got %v", got)
	}
}

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

func TestMMRRerankTieBreakFusedRank(t *testing.T) {
	ids := []string{"A", "t1", "t2"}
	rel := relMap("A", 1.0, "t1", 0.5, "t2", 0.5)
	vec := map[string][]float32{"A": {1, 0}, "t1": {0, 1}, "t2": {0, 1}} // t1,t2 identical ⇒ tie
	got := mmrRerank(ids, rel, vec, mmrParams{lambda: 0.7})
	if !reflect.DeepEqual(got, []string{"A", "t1", "t2"}) {
		t.Fatalf("tie must resolve to fused order, got %v", got)
	}
}

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

func TestMMRExportedSurfaceAndVectorLoad(t *testing.T) {
	ids := []string{"a", "b"}
	rel := map[string]float64{"a": 2, "b": 1}
	vecs := map[string][]float32{"a": {1, 0}, "b": {0, 1}}
	if got := MMRRerank(ids, rel, vecs, 1); !reflect.DeepEqual(got, ids) {
		t.Fatalf("rerank=%v", got)
	}
	if !MMRActive(false, true) || MMRActive(false, false) || ClampPositive(-1) != 0 {
		t.Fatal("exported policy surface changed")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE mem_vectors(memory_id TEXT, model TEXT, vec BLOB)`); err != nil {
		t.Fatal(err)
	}
	want := []float32{0.5, -0.25}
	if _, err = db.Exec(`INSERT INTO mem_vectors VALUES(?,?,?)`, "a", "model", embedpkg.EncodeVec(want)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadVectorsByID(context.Background(), db, "model", []string{"a", "missing"})
	if err != nil || !reflect.DeepEqual(got, map[string][]float32{"a": want}) {
		t.Fatalf("load=(%v,%v)", got, err)
	}
	empty, err := LoadVectorsByID(context.Background(), db, "model", nil)
	if err != nil || empty != nil {
		t.Fatalf("empty=(%v,%v)", empty, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVectorsByID(context.Background(), db, "model", []string{"a"}); err == nil {
		t.Fatal("closed db query succeeded")
	}
}
