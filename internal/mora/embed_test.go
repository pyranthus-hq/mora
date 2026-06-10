package mora

import (
	"math"
	"reflect"
	"testing"
)

func TestStaticEmbedderDeterministic(t *testing.T) {
	e := defaultEmbedder()
	a := e.Embed("quarterly planning with the team")
	b := e.Embed("quarterly planning with the team")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("embedding is not deterministic for identical text")
	}
	if len(a) != e.Dim() {
		t.Fatalf("dim = %d, want %d", len(a), e.Dim())
	}
}

func TestStaticEmbedderUnitLength(t *testing.T) {
	v := defaultEmbedder().Embed("hello world")
	var ss float64
	for _, x := range v {
		ss += float64(x) * float64(x)
	}
	if math.Abs(ss-1) > 1e-4 {
		t.Fatalf("embedding not unit length: |v|^2 = %v", ss)
	}
	// Empty text -> all-zero vector (defined, not NaN).
	z := defaultEmbedder().Embed("")
	for _, x := range z {
		if x != 0 {
			t.Fatal("empty text should embed to the zero vector")
		}
	}
}

func TestStaticEmbedderSemanticOrdering(t *testing.T) {
	e := defaultEmbedder()
	q := e.Embed("project planning meeting")
	near := e.Embed("planning the project meetings")
	far := e.Embed("zebra giraffe ocean current")
	if cosine(q, near) <= cosine(q, far) {
		t.Fatalf("related text should be closer: near=%.3f far=%.3f", cosine(q, near), cosine(q, far))
	}
}

func TestStaticEmbedderSubwordSignal(t *testing.T) {
	e := defaultEmbedder()
	// Distinct tokens but shared subword (launch/launching) -> positive similarity,
	// which is what lets the vector arm recall where exact FTS would miss.
	if cosine(e.Embed("launch"), e.Embed("launching the rocket")) <= 0 {
		t.Fatal("expected positive subword similarity for launch/launching")
	}
}

func TestEncodeDecodeVecRoundTrip(t *testing.T) {
	v := defaultEmbedder().Embed("round trip me")
	got := decodeVec(encodeVec(v))
	if !reflect.DeepEqual(v, got) {
		t.Fatal("encodeVec/decodeVec did not round-trip")
	}
}

func TestCosineMismatchedDimsIsZero(t *testing.T) {
	if cosine([]float32{1, 0}, []float32{1, 0, 0}) != 0 {
		t.Fatal("mismatched dims must score 0, never panic")
	}
}

func TestRRFFusion(t *testing.T) {
	// id "b" is rank-1 in list A and rank-2 in list B; id "a" is rank-2 in A only.
	score := rrf([][]string{{"x", "b", "a"}, {"y", "b"}}, rrfK)
	if score["b"] <= score["a"] {
		t.Fatalf("b (in both lists) should outscore a: b=%.4f a=%.4f", score["b"], score["a"])
	}
	// Rank-1 of a single list beats rank-3 of a single list.
	if score["x"] <= score["a"] {
		t.Fatalf("rank-1 x should beat rank-3 a: x=%.4f a=%.4f", score["x"], score["a"])
	}
}
