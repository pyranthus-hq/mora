package embed

import (
	"math"
	"reflect"
	"testing"
)

// mustEmbed calls Embed and fails the test on error — the static embedder never
// errors, so this keeps the deterministic-embedding assertions readable after the
// D1 interface change to Embed(text) ([]float32, error).
func mustEmbed(t *testing.T, e Embedder, text string) []float32 {
	t.Helper()
	v, err := e.Embed(text)
	if err != nil {
		t.Fatalf("Embed(%q): unexpected error %v", text, err)
	}
	return v
}

func TestStaticEmbedderDeterministic(t *testing.T) {
	e := defaultEmbedder()
	a := mustEmbed(t, e, "quarterly planning with the team")
	b := mustEmbed(t, e, "quarterly planning with the team")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("embedding is not deterministic for identical text")
	}
	if len(a) != e.Dim() {
		t.Fatalf("dim = %d, want %d", len(a), e.Dim())
	}
}

func TestStaticEmbedderUnitLength(t *testing.T) {
	v := mustEmbed(t, defaultEmbedder(), "hello world")
	var ss float64
	for _, x := range v {
		ss += float64(x) * float64(x)
	}
	if math.Abs(ss-1) > 1e-4 {
		t.Fatalf("embedding not unit length: |v|^2 = %v", ss)
	}
	// Empty text -> all-zero vector (defined, not NaN).
	z := mustEmbed(t, defaultEmbedder(), "")
	for _, x := range z {
		if x != 0 {
			t.Fatal("empty text should embed to the zero vector")
		}
	}
}

func TestStaticEmbedderSemanticOrdering(t *testing.T) {
	e := defaultEmbedder()
	q := mustEmbed(t, e, "project planning meeting")
	near := mustEmbed(t, e, "planning the project meetings")
	far := mustEmbed(t, e, "zebra giraffe ocean current")
	if cosine(q, near) <= cosine(q, far) {
		t.Fatalf("related text should be closer: near=%.3f far=%.3f", cosine(q, near), cosine(q, far))
	}
}

func TestStaticEmbedderSubwordSignal(t *testing.T) {
	e := defaultEmbedder()
	// Distinct tokens but shared subword (launch/launching) -> positive similarity,
	// which is what lets the vector arm recall where exact FTS would miss.
	if cosine(mustEmbed(t, e, "launch"), mustEmbed(t, e, "launching the rocket")) <= 0 {
		t.Fatal("expected positive subword similarity for launch/launching")
	}
}

func TestEncodeDecodeVecRoundTrip(t *testing.T) {
	v := mustEmbed(t, defaultEmbedder(), "round trip me")
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
