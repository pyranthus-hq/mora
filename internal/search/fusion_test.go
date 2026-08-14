package search

import (
	"math"
	"reflect"
	"testing"
)

func TestRRFFusion(t *testing.T) {
	// id "b" is rank-1 in list A and rank-2 in list B; id "a" is rank-2 in A only.
	score := FuseScores([][]string{{"x", "b", "a"}, {"y", "b"}}, nil, StandardRRFK)
	if score["b"] <= score["a"] {
		t.Fatalf("b (in both lists) should outscore a: b=%.4f a=%.4f", score["b"], score["a"])
	}
	// Rank-1 of a single list beats rank-3 of a single list.
	if score["x"] <= score["a"] {
		t.Fatalf("rank-1 x should beat rank-3 a: x=%.4f a=%.4f", score["x"], score["a"])
	}
}

func TestFuseScoresWeightsAndMissingDefaults(t *testing.T) {
	lists := [][]string{{"a", "b"}, {"b", "c"}, {"c"}}
	got := FuseScores(lists, []float64{2}, 10)
	want := map[string]float64{"a": 2.0 / 11, "b": 2.0/12 + 1.0/11, "c": 1.0/12 + 1.0/11}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
func TestFuseRankedDeterministicTiesAndEmpty(t *testing.T) {
	ids, scores := FuseRanked([][]string{{"z"}, {"a"}}, nil, 10)
	if !reflect.DeepEqual(ids, []string{"a", "z"}) || len(scores) != 2 {
		t.Fatalf("ids=%v scores=%v", ids, scores)
	}
	ids, scores = FuseRanked(nil, nil, 10)
	if len(ids) != 0 || len(scores) != 0 {
		t.Fatalf("empty=(%v,%v)", ids, scores)
	}
}
func TestFuseDoesNotMutateInputs(t *testing.T) {
	lists := [][]string{{"a", "b"}}
	weights := []float64{2}
	_, _ = FuseRanked(lists, weights, 10)
	if !reflect.DeepEqual(lists, [][]string{{"a", "b"}}) || !reflect.DeepEqual(weights, []float64{2}) {
		t.Fatalf("mutated lists=%v weights=%v", lists, weights)
	}
}
func TestStandardRRFK(t *testing.T) {
	if math.Abs(StandardRRFK-60) > 0 {
		t.Fatalf("k=%v", StandardRRFK)
	}
}
