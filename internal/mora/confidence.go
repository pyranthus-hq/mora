package mora

import (
	"context"
	"time"
)

// confidence.go — issue #238: a compact, opt-in "confidence" envelope for
// search_memory and think, gated by a per-call boolean arg (mirroring the
// digest/brief `envelope` mcpParam precedent). See confidence_contract_test.go's
// header doc comment for the FROZEN spec this file implements. Every field
// here is derived ONLY from data already computed at ranking time
// (Memory.Score/CreatedAt, ThinkEvidence.Score/CreatedAt/Gaps, sourceHealthAll)
// — this is not a new scoring system, just a rollup of existing signals.
//
// #238 fix: search_memory's Score is only a raw bm25 magnitude on the
// FTS-only path. When a semantic embedder is active, defaultSearch routes to
// hybridSearch (hybrid.go), which overwrites Score with the RRF-fused value
// — positive, near-constant across match quality (empirically ~0.01-0.15;
// see confidence_contract_test.go's "#238 AMENDMENT" doc comment for the
// measured repro). Bucketing that value against the bm25 bounds always read
// "weak", regardless of how strong or gap-free the match was. The fix keys
// search_memory's strength derivation on the SAME routing decision
// defaultSearchForMCP already made for this call (hybrid.go): bm25 bucketing
// stays put on the FTS-only path; the semantic/hybrid path shares think's
// existing gap-based rule (confidenceGapStrength) instead, computed from the
// SAME retrieval trace that call already produced. The envelope also gains a
// "scale" field ("bm25" | "rrf_fused") so callers can tell which number space
// max_score/mean_score are actually in.
//
// #238 P1/P2 AMENDMENT: the FIRST cut of this fix independently re-evaluated
// chooseEmbedderFor/embedderIsSemantic here (a second, separately-timed HTTP
// probe) and, on the semantic path, re-ran the ENTIRE hybridSearchTrace
// pipeline (a second embedding round-trip + retrieval + fusion) just to
// recover the trace mcpSearchMemory's own defaultSearch call already computed
// and discarded. Two probes of live Ollama reachability that can disagree
// with each other AND with the routing decision that actually scored the
// results is a real defect (not just waste): if reachability flips between
// the two, the envelope can label an FTS-scored result "rrf_fused" or vice
// versa. The fix threads the ACTUAL routing decision (SemanticPath) and the
// ACTUAL retrieval trace (Local/Trace) from mcpSearchMemory's own
// defaultSearchForMCP call into searchConfidence — zero additional embedder
// probes, zero additional retrieval passes, anywhere in the confidence path.

// confidenceSearchStrongBound / confidenceSearchModerateBound are the frozen
// search_memory bm25 bucket boundaries (contract's "STRENGTH BUCKETING"
// section) — FTS-only path only, post-#238. search_memory's Score there is
// raw SQLite bm25(): more negative is a better match, unbounded magnitude
// (search.go). Named distinctly from the contract test's own
// confidenceSearchStrongMax/confidenceSearchModerateMax (test-local constants
// that assert against these same numbers) to avoid a duplicate declaration in
// this package.
const (
	confidenceSearchStrongBound   = -4.0
	confidenceSearchModerateBound = -1.5
)

// confidenceScaleBM25 / confidenceScaleRRFFused are the frozen "scale" values
// (contract's "FROZEN SHAPE" section, #238 amendment): they tell the caller
// which already-computed number space max_score/mean_score live in, so a raw
// bm25 negative and an RRF-fused positive are never misread as the same kind
// of number.
const (
	confidenceScaleBM25     = "bm25"
	confidenceScaleRRFFused = "rrf_fused"
)

// confidenceEnvelope is the FROZEN "confidence" object shape (contract's
// "FROZEN SHAPE" section). MissingSources is always a non-nil (possibly
// empty) slice so it marshals to `[]`, never `null`.
type confidenceEnvelope struct {
	Strength         string   `json:"strength"`
	Scale            string   `json:"scale"`
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
//
// semanticPath/localMems/trace are THREADED, not recomputed: they are the
// SAME routing decision and retrieval trace mcpSearchMemory's own
// defaultSearchForMCP call already produced for this request (hybrid.go).
// This is the #238 P1/P2 fix — searchConfidence never independently probes
// chooseEmbedderFor/embedderIsSemantic and never re-runs hybridSearchTrace;
// it only reads what retrieval already decided and already computed.
// localMems/trace are the PRE-share-union local results (zero value on the
// FTS-only path), mirroring buildThink's documented LOCAL-only gap scope
// (think.go) — query is passed through only as a plain string for
// computeGaps' own gap analysis, not for any additional retrieval.
func searchConfidence(ctx context.Context, cfg Config, mems []Memory, semanticPath bool, localMems []Memory, trace retrievalTrace, query string, now time.Time) confidenceEnvelope {
	scores := make([]float64, len(mems))
	dates := make([]string, len(mems))
	for i, m := range mems {
		scores[i] = m.Score
		dates[i] = m.CreatedAt
	}
	best, mean := confidenceScoreStats(scores)
	missing, impact := confidenceSourceGaps(cfg, now)

	// Path-aware (#238): key off the SAME routing decision that actually
	// scored this call's results (threaded in as semanticPath), never a
	// second, independently-timed probe. defaultSearchForMCP already applied
	// HEALTH-12's "degrade visibly, don't hard-fail" precedent (hybrid.go)
	// when it made this decision; searchConfidence just reads it.
	strength := confidenceSearchStrength(best)
	scale := confidenceScaleBM25
	if semanticPath {
		scale = confidenceScaleRRFFused
		strength = confidenceSemanticSearchStrength(ctx, cfg, query, localMems, trace, now)
	}

	return confidenceEnvelope{
		Strength:         strength,
		Scale:            scale,
		MaxScore:         best,
		MeanScore:        mean,
		FreshestSourceAt: confidenceFreshest(dates),
		MissingSources:   missing,
		HealthImpact:     impact,
	}
}

// thinkConfidence builds the confidence envelope for think, over res.Evidence
// (already limit-bounded by buildThink — think has no separate byte-budget
// truncation pass the way search_memory's budgetSearchResults does). think's
// scale is ALWAYS rrf_fused: buildThink routes through hybridSearchTrace
// regardless of embedder (mora-retrieval-and-ranking's documented bypass), so
// think's Score is never a raw bm25 magnitude.
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
		Scale:            confidenceScaleRRFFused,
		MaxScore:         best,
		MeanScore:        mean,
		FreshestSourceAt: confidenceFreshest(dates),
		MissingSources:   missing,
		HealthImpact:     impact,
	}
}

// confidenceSearchStrength buckets search_memory's raw bm25 max_score per the
// frozen thresholds above (FTS-only path only, post-#238); more negative is a
// better match, so "strong" is the LOW (most negative) end.
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

// confidenceGapStrength is the shared gap-based strength rule (contract's
// "STRENGTH BUCKETING" section): no results -> weak; results present and the
// gap analysis is non-empty -> moderate; results present and gap-free ->
// strong. Used wherever Score is an RRF-fused rank artifact rather than a
// bucketable match-quality magnitude: think (always) and, since #238,
// search_memory's semantic/hybrid path.
func confidenceGapStrength(hasResults bool, gaps ThinkGaps) string {
	switch {
	case !hasResults:
		return "weak"
	case !gaps.empty():
		return "moderate"
	default:
		return "strong"
	}
}

// confidenceThinkStrength buckets think's strength off ThinkGaps — a
// deterministic signal already computed by computeGaps at buildThink time
// (contract's "STRENGTH BUCKETING" section) — rather than off raw Score,
// which is a near-constant RRF rank artifact regardless of embedder (think
// always routes through hybridSearchTrace).
func confidenceThinkStrength(res ThinkResult) string {
	return confidenceGapStrength(len(res.Evidence) > 0, res.Gaps)
}

// confidenceSemanticSearchStrength buckets search_memory's strength on the
// semantic/hybrid path using the SAME gap-based rule think uses
// (confidenceGapStrength), because under RRF fusion Memory.Score there is a
// near-constant rank artifact (~0.01-0.15 regardless of match quality — see
// confidence_contract_test.go's "#238 AMENDMENT" doc comment), not a
// match-quality magnitude the bm25 thresholds can bucket.
//
// mems/tr are THREADED from mcpSearchMemory's own defaultSearchForMCP call
// (#238 P1/P2 fix) — this function no longer re-runs hybridSearchTrace to
// recover them; it reuses computeGaps exactly as think does, over the SAME
// (mems, trace) pair retrieval already produced. mems is deliberately the
// PRE-share-union local set, mirroring think's choice to compute gaps over
// LOCAL results only (buildThink doc comment, think.go): a shared-corpus
// contribution is never compared against the personal index's own retrieval
// trace or entity graph.
//
// A computeGaps error still fails closed to "weak" — this is an opt-in,
// best-effort signal, and it must never turn a working search_memory call
// into a hard failure or overclaim confidence when the gap analysis can't be
// run. There is no longer a recompute to fail; this guard covers computeGaps
// itself (e.g. an index read error).
func confidenceSemanticSearchStrength(ctx context.Context, cfg Config, query string, mems []Memory, tr retrievalTrace, now time.Time) string {
	if len(mems) == 0 {
		return "weak"
	}
	gaps, err := computeGaps(ctx, cfg, query, mems, tr, now)
	if err != nil {
		return "weak"
	}
	return confidenceGapStrength(true, gaps)
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
