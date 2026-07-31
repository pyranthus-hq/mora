package mora

import "time"

// confidence.go — issue #238: a compact, opt-in "confidence" envelope for
// search_memory and think, gated by a per-call boolean arg (mirroring the
// digest/brief `envelope` mcpParam precedent). See confidence_contract_test.go's
// header doc comment for the FROZEN spec this file implements. Every field
// here is derived ONLY from data already computed at ranking time
// (Memory.Score/CreatedAt, ThinkEvidence.Score/CreatedAt/Gaps, sourceHealthAll)
// — this is not a new scoring system, just a rollup of existing signals.

// confidenceSearchStrongBound / confidenceSearchModerateBound are the frozen
// search_memory bm25 bucket boundaries (contract's "STRENGTH BUCKETING"
// section). search_memory's Score is raw SQLite bm25(): more negative is a
// better match, unbounded magnitude (search.go). Named distinctly from the
// contract test's own confidenceSearchStrongMax/confidenceSearchModerateMax
// (test-local constants that assert against these same numbers) to avoid a
// duplicate declaration in this package.
const (
	confidenceSearchStrongBound   = -4.0
	confidenceSearchModerateBound = -1.5
)

// confidenceEnvelope is the FROZEN "confidence" object shape (contract's
// "FROZEN SHAPE" section). MissingSources is always a non-nil (possibly
// empty) slice so it marshals to `[]`, never `null`.
type confidenceEnvelope struct {
	Strength         string   `json:"strength"`
	MaxScore         float64  `json:"max_score"`
	MeanScore        float64  `json:"mean_score"`
	FreshestSourceAt string   `json:"freshest_source_at"`
	MissingSources   []string `json:"missing_sources"`
	HealthImpact     string   `json:"health_impact"`
}

// searchConfidence builds the confidence envelope for search_memory. mems is
// the post-budget slice actually being returned to the caller — the
// contract's "RETURNED set" that max_score/mean_score/freshest_source_at are
// scoped over.
func searchConfidence(mems []Memory, cfg Config, now time.Time) confidenceEnvelope {
	scores := make([]float64, len(mems))
	dates := make([]string, len(mems))
	for i, m := range mems {
		scores[i] = m.Score
		dates[i] = m.CreatedAt
	}
	best, mean := confidenceScoreStats(scores)
	missing, impact := confidenceSourceGaps(cfg, now)
	return confidenceEnvelope{
		Strength:         confidenceSearchStrength(best),
		MaxScore:         best,
		MeanScore:        mean,
		FreshestSourceAt: confidenceFreshest(dates),
		MissingSources:   missing,
		HealthImpact:     impact,
	}
}

// thinkConfidence builds the confidence envelope for think, over res.Evidence
// (already limit-bounded by buildThink — think has no separate byte-budget
// truncation pass the way search_memory's budgetSearchResults does).
func thinkConfidence(res ThinkResult, cfg Config, now time.Time) confidenceEnvelope {
	scores := make([]float64, len(res.Evidence))
	dates := make([]string, len(res.Evidence))
	for i, e := range res.Evidence {
		scores[i] = e.Score
		dates[i] = e.CreatedAt
	}
	best, mean := confidenceScoreStats(scores)
	missing, impact := confidenceSourceGaps(cfg, now)
	return confidenceEnvelope{
		Strength:         confidenceThinkStrength(res),
		MaxScore:         best,
		MeanScore:        mean,
		FreshestSourceAt: confidenceFreshest(dates),
		MissingSources:   missing,
		HealthImpact:     impact,
	}
}

// confidenceSearchStrength buckets search_memory's raw bm25 max_score per the
// frozen thresholds above; more negative is a better match, so "strong" is
// the LOW (most negative) end.
func confidenceSearchStrength(maxScore float64) string {
	switch {
	case maxScore <= confidenceSearchStrongBound:
		return "strong"
	case maxScore <= confidenceSearchModerateBound:
		return "moderate"
	default:
		return "weak"
	}
}

// confidenceThinkStrength buckets think's strength off ThinkGaps — a
// deterministic signal already computed by computeGaps at buildThink time
// (contract's "STRENGTH BUCKETING" section) — rather than off raw Score,
// which is a near-constant RRF rank artifact under this repo's
// single-active-arm CI reality (no Ollama).
func confidenceThinkStrength(res ThinkResult) string {
	switch {
	case len(res.Evidence) == 0:
		return "weak"
	case !res.Gaps.empty():
		return "moderate"
	default:
		return "strong"
	}
}

// confidenceScoreStats returns (max, mean) over scores — the literal numeric
// maximum (not a "best match" direction judgment: search_memory's bm25 and
// think's RRF-fused score are on different already-computed scales, and
// max_score/mean_score are a plain rollup of whichever numbers the returned
// set already carries; strength bucketing, not this rollup, is where each
// tool's own scale is interpreted — see confidenceSearchStrength/
// confidenceThinkStrength). An empty set reports 0/0, the contract's
// zero-result/zero-evidence case.
func confidenceScoreStats(scores []float64) (max, mean float64) {
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

// confidenceFreshest returns the MAX CreatedAt (RFC3339) across dates, as the
// original string of the winning entry — never a reformatted timestamp — or
// "" for an empty set. Unparseable entries are skipped rather than crashing;
// ties keep the first winning entry encountered.
func confidenceFreshest(dates []string) string {
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

// confidenceSourceGaps is the CALL-SCOPED projection of sourceHealthAll: every
// enabled connector instance that reads non-fresh right now (already sorted by
// Key — sourceHealthAll's own contract), plus the worst state among them
// (health.go's healthStateRank precedence via worstSource) — regardless of
// whether any of them contributed a memory to this call's returned set (see
// the contract's "AMENDED" missing_sources reasoning: a fully-dark source
// that contributed nothing is exactly the incomplete-coverage case this field
// exists to surface).
func confidenceSourceGaps(cfg Config, now time.Time) ([]string, string) {
	all := sourceHealthAll(cfg, now)
	missing := make([]string, 0, len(all))
	for _, h := range all {
		if h.State != healthFresh {
			missing = append(missing, h.Key)
		}
	}
	impact := "none"
	if worst := worstSource(all); worst != nil {
		impact = worst.State
	}
	return missing, impact
}
