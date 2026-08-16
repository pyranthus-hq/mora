package confidence

import (
	"github.com/pyranthus-hq/mora/internal/health"
	"github.com/pyranthus-hq/mora/internal/search"
	"reflect"
	"testing"
)

func TestScoreStats(t *testing.T) {
	cases := []struct {
		in        []float64
		max, mean float64
	}{{nil, 0, 0}, {[]float64{-4, -2, -6}, -2, -4}, {[]float64{3}, 3, 3}}
	for _, tc := range cases {
		max, mean := ScoreStats(tc.in)
		if max != tc.max || mean != tc.mean {
			t.Fatalf("ScoreStats(%v)=(%v,%v)", tc.in, max, mean)
		}
	}
}
func TestFreshest(t *testing.T) {
	dates := []string{"bad", "2026-01-03T00:00:00+02:00", "2026-01-02T23:00:00Z", "2026-01-02T23:00:00+00:00"}
	if got := Freshest(dates); got != dates[2] {
		t.Fatalf("got %q", got)
	}
	if Freshest(nil) != "" {
		t.Fatal("empty freshest")
	}
}
func TestStrengthPolicies(t *testing.T) {
	for _, tc := range []struct {
		score float64
		want  string
	}{{-4, "strong"}, {-1.5, "moderate"}, {-1.49, "weak"}} {
		if got := SearchStrength(tc.score); got != tc.want {
			t.Fatalf("score %v: %s", tc.score, got)
		}
	}
	if GapStrength(false, false) != "weak" || GapStrength(true, true) != "moderate" || GapStrength(true, false) != "strong" {
		t.Fatal("gap strength")
	}
	strong := search.LexicalCoverage{FullRows: 2, FullSources: 2}
	if DirectStrength(true, false, strong) != "strong" || DirectStrength(true, false, search.LexicalCoverage{FullRows: 2, FullSources: 1}) != "moderate" || DirectStrength(true, true, strong) != "moderate" || DirectStrength(false, false, strong) != "weak" {
		t.Fatal("direct strength")
	}
}
func TestSourceGaps(t *testing.T) {
	missing, impact := SourceGaps(nil)
	if missing == nil || len(missing) != 0 || impact != "none" {
		t.Fatalf("empty=%v %q", missing, impact)
	}
	all := []health.Source{{Key: "z", State: health.Stale}, {Key: "a", State: health.Fresh}, {Key: "n", State: health.Never}, {Key: "f", State: health.Failed}}
	missing, impact = SourceGaps(all)
	if !reflect.DeepEqual(missing, []string{"z", "n", "f"}) || impact != health.Failed {
		t.Fatalf("got %v %q", missing, impact)
	}
}
