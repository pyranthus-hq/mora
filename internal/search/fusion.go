package search

import "sort"

const StandardRRFK = 60.0

// FuseScores computes weighted reciprocal-rank-fusion scores without mutating inputs.
func FuseScores(lists [][]string, weights []float64, k float64) map[string]float64 {
	scores := map[string]float64{}
	for li, list := range lists {
		weight := 1.0
		if li < len(weights) {
			weight = weights[li]
		}
		for rank, id := range list {
			scores[id] += weight / (k + float64(rank+1))
		}
	}
	return scores
}

// FuseRanked returns the deterministic fused order (score descending, ID ascending) and scores.
func FuseRanked(lists [][]string, weights []float64, k float64) ([]string, map[string]float64) {
	scores := FuseScores(lists, weights, k)
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids, scores
}
