// Package confidence owns deterministic retrieval-confidence rollups and policy.
package confidence

import (
	"github.com/pyranthus-hq/mora/internal/health"
	"github.com/pyranthus-hq/mora/internal/search"
	"time"
)

const (
	SearchStrongBound   = -4.0
	SearchModerateBound = -1.5
	ScaleBM25           = "bm25"
	ScaleRRFFused       = "rrf_fused"
)

type Envelope struct {
	Strength         string   `json:"strength"`
	Scale            string   `json:"scale"`
	MaxScore         float64  `json:"max_score"`
	MeanScore        float64  `json:"mean_score"`
	FreshestSourceAt string   `json:"freshest_source_at"`
	MissingSources   []string `json:"missing_sources"`
	HealthImpact     string   `json:"health_impact"`
}

func ScoreStats(scores []float64) (max, mean float64) {
	if len(scores) == 0 {
		return 0, 0
	}
	max = scores[0]
	sum := 0.0
	for _, s := range scores {
		sum += s
		if s > max {
			max = s
		}
	}
	return max, sum / float64(len(scores))
}
func Freshest(dates []string) string {
	var best time.Time
	var bestStr string
	for _, d := range dates {
		t, err := time.Parse(time.RFC3339, d)
		if err != nil {
			continue
		}
		if bestStr == "" || t.After(best) {
			best = t
			bestStr = d
		}
	}
	return bestStr
}
func SearchStrength(maxScore float64) string {
	switch {
	case maxScore <= SearchStrongBound:
		return "strong"
	case maxScore <= SearchModerateBound:
		return "moderate"
	default:
		return "weak"
	}
}
func GapStrength(hasResults, hasGaps bool) string {
	switch {
	case !hasResults:
		return "weak"
	case hasGaps:
		return "moderate"
	default:
		return "strong"
	}
}
func DirectStrength(hasResults, hasGaps bool, coverage search.LexicalCoverage) string {
	strength := GapStrength(hasResults, hasGaps)
	if strength != "strong" {
		return strength
	}
	if coverage.FullRows >= 2 && coverage.FullSources >= 2 {
		return "strong"
	}
	return "moderate"
}
func SourceGaps(all []health.Source) ([]string, string) {
	missing := make([]string, 0, len(all))
	for _, h := range all {
		if h.State != health.Fresh {
			missing = append(missing, h.Key)
		}
	}
	impact := "none"
	if worst := health.Worst(all); worst != nil {
		impact = worst.State
	}
	return missing, impact
}
