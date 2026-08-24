package mora

import (
	"context"
	confidencepkg "github.com/pyranthus-hq/mora/internal/confidence"
	"sort"
	"time"
)

const (
	confidenceSearchStrongBound   = confidencepkg.SearchStrongBound
	confidenceSearchModerateBound = confidencepkg.SearchModerateBound
	confidenceScaleBM25           = confidencepkg.ScaleBM25
	confidenceScaleRRFFused       = confidencepkg.ScaleRRFFused
)

type confidenceEnvelope = confidencepkg.Envelope

func searchConfidence(ctx context.Context, cfg Config, mems []Memory, scoreFused bool, fullMems, localMems []Memory, trace retrievalTrace, query string, now time.Time) confidenceEnvelope {
	return searchConfidenceFor(ctx, cfg, mems, scoreFused, fullMems, localMems, trace, query, now, "", true)
}

func searchConfidenceFor(ctx context.Context, cfg Config, mems []Memory, scoreFused bool, fullMems, localMems []Memory, trace retrievalTrace, query string, now time.Time, requestedSource string, currentState bool) confidenceEnvelope {
	scores := make([]float64, len(mems))
	dates := make([]string, len(mems))
	for i, m := range mems {
		scores[i] = m.Score
		dates[i] = m.CreatedAt
	}
	best, mean := confidencepkg.ScoreStats(scores)
	missing, impact := confidenceSourceGapsFor(cfg, now, requestedSource, currentState)
	coverageMems := returnedMemoryRows(mems, fullMems)
	gapMems := returnedMemoryRows(mems, localMems)

	// Path-aware (#238/I5): key off the SAME score-domain decision that actually
	// scored this call's results (threaded in as scoreFused), never a
	// second, independently-timed probe. defaultSearchForMCP already applied
	// HEALTH-12's "degrade visibly, don't hard-fail" precedent (hybrid.go)
	// when it made this decision; searchConfidence just reads it.
	strength := confidencepkg.SearchStrength(best)
	scale := confidenceScaleBM25
	if scoreFused {
		scale = confidenceScaleRRFFused
		strength = confidenceFusedSearchStrength(ctx, cfg, query, gapMems, coverageMems, trace, now)
	} else if strength == "strong" && memoryLexicalCoverage(query, coverageMems).FullRows == 0 {
		// BM25 can strongly rank one shared word from a larger question. Without
		// one row covering the whole lexical relation, strong is not justified.
		strength = "moderate"
	}

	if currentState && len(missing) > 0 {
		strength = lowerConfidenceStrength(strength)
	}
	return confidenceEnvelope{
		Strength:         strength,
		Scale:            scale,
		MaxScore:         best,
		MeanScore:        mean,
		FreshestSourceAt: confidencepkg.Freshest(dates),
		MissingSources:   missing,
		HealthImpact:     impact,
	}
}
func thinkConfidence(res ThinkResult, cfg Config, now time.Time) confidenceEnvelope {
	scores := make([]float64, len(res.Evidence))
	dates := make([]string, len(res.Evidence))
	for i, e := range res.Evidence {
		scores[i] = e.Score
		dates[i] = e.CreatedAt
	}
	best, mean := confidencepkg.ScoreStats(scores)
	missing, impact := confidenceSourceGaps(cfg, now)
	return confidenceEnvelope{
		Strength:         confidenceThinkStrength(res),
		Scale:            confidenceScaleRRFFused,
		MaxScore:         best,
		MeanScore:        mean,
		FreshestSourceAt: confidencepkg.Freshest(dates),
		MissingSources:   missing,
		HealthImpact:     impact,
	}
}
func confidenceThinkStrength(res ThinkResult) string {
	return confidencepkg.DirectStrength(len(res.Evidence) > 0, !res.Gaps.Empty(), thinkLexicalCoverage(res))
}
func confidenceFusedSearchStrength(ctx context.Context, cfg Config, query string, gapMems, coverageMems []Memory, tr retrievalTrace, now time.Time) string {
	if len(gapMems) == 0 && len(coverageMems) == 0 {
		return "weak"
	}
	gaps, err := computeGaps(ctx, cfg, query, gapMems, tr, now)
	if err != nil {
		return "weak"
	}
	coverage := memoryLexicalCoverage(query, coverageMems)
	return confidencepkg.DirectStrength(true, !gaps.Empty(), coverage)
}
func confidenceSourceGaps(cfg Config, now time.Time) ([]string, string) {
	return confidencepkg.SourceGaps(sourceHealthAll(cfg, now))
}

func confidenceSourceGapsFor(cfg Config, now time.Time, requestedSource string, currentState bool) ([]string, string) {
	if !currentState {
		return []string{}, "none"
	}
	all := sourceHealthAll(cfg, now)
	kept := make([]sourceHealth, 0, len(all))
	for _, health := range all {
		if requestedSource != "" && !digestSourceMatches(health.Key, requestedSource) {
			continue
		}
		kept = append(kept, health)
	}
	missing, impact := confidencepkg.SourceGaps(kept)
	sort.Strings(missing)
	return missing, impact
}

func lowerConfidenceStrength(strength string) string {
	switch strength {
	case "strong":
		return "moderate"
	case "moderate":
		return "weak"
	default:
		return strength
	}
}
