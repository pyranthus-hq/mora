package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// confidence_contract_test.go — issue #238: a compact "confidence" envelope on
// search_memory and think, derived ONLY from data already computed at ranking
// time (Memory.Score, Memory.CreatedAt, sourceHealth/compactHealth) — no new
// scoring system. This file is the FROZEN CONTRACT: every test below drives the
// real MCP dispatch (mcpResult/callMCPTool, exactly like the rest of this
// package's MCP tests) and asserts on the marshaled JSON the handler returns.
// The "confidence" key does not exist yet, so every assertion below fails RED
// at the assertion level today; the file compiles clean and needs no new Go
// types (T0 style, matching digest_mcp_test.go/mora_mcp_budget_test.go).
//
// GATING (frozen, mirrors the digest/brief `envelope` boolean precedent locked
// by TestMCPDigestKnobIsAlive / TestMCPGateDigestEnvelopeOffByteIdentical in
// mora_mcp_budget_test.go): both tools accept an opt-in per-call boolean
// argument named `confidence` (default false, same shape as digest/brief's
// `envelope` mcpParam). OFF (arg omitted, or explicitly `"confidence":false`)
// MUST leave the response byte-identical to today's shape — no `confidence`
// key anywhere in the envelope. ON adds exactly one top-level sibling key,
// `confidence`, beside the tool's existing payload (`results`/`freshness`/
// `health` for search_memory; `think`/`health` for think). This is a per-call
// arg, not a config.toml-persisted knob — the wording in the issue ("gated by
// a config knob") is satisfied by literally mirroring the digest/brief
// `envelope` mechanism, which is itself a per-call knob, not a durable
// setting; see the report to the integrator for this reading of the frozen
// decision.
//
// FROZEN SHAPE (JSON), target <300 bytes serialized:
//
//	"confidence": {
//	  "strength":           "strong" | "moderate" | "weak",
//	  "max_score":          float64,  // best Memory.Score already computed at ranking time, over the RETURNED set
//	  "mean_score":         float64,  // arithmetic mean Memory.Score over the RETURNED set
//	  "freshest_source_at": string,   // RFC3339 max Memory.CreatedAt over the RETURNED set; "" when the set is empty
//	  "missing_sources":    []string, // sorted, [] (never null) — EVERY enabled connector
//	                                  //   instance that reads non-fresh (stale/failed/never)
//	                                  //   via sourceHealthAll AT CALL TIME. Whether that source
//	                                  //   contributed a memory to the RETURNED set is IRRELEVANT:
//	                                  //   a fully-dark source (failed/never, contributed nothing)
//	                                  //   is exactly the "incomplete coverage" case this field
//	                                  //   exists to surface, and it must be listed even though it
//	                                  //   contributed zero rows.
//	  "health_impact":      "none" | "stale" | "failed" | "never", // worst state among missing_sources; "none" when empty
//	}
//
// health_impact severity mirrors this repo's EXISTING worst-first precedence
// for unhealthy states (health.go's healthStateRank / worstSource doc comment:
// "failed (an active error) outranks never (no data point at all) outranks
// stale (data exists, just aging)"): failed > never > stale. This contract
// reuses that precedent rather than freezing a new one.
//
// Field choice ("the minimal honest set", per the issue): max_score/mean_score
// are the raw already-ranked numbers every row already carries, just rolled up
// once instead of asking the caller to rescan the result array. freshest_source_at
// answers "how current is this evidence" without a second pass. missing_sources/
// health_impact are a CALL-SCOPED projection of the existing sourceHealth signal
// this packet's predecessor (compactHealth) already carries on every response —
// "which of my enabled sources are unhealthy right now" (regardless of whether
// this particular query happened to retrieve anything from them) is the
// "incomplete coverage" question the issue asks for: a source that never
// contributed because it is dark is the case that matters most, and a
// contribution-gated definition can never surface it. AMENDED from an earlier
// draft that gated missing_sources on "contributed a memory to the returned
// set" — that reading made a fully-dark source (failed/never, contributed
// nothing) permanently invisible, which is exactly backwards for a field
// whose job is to flag incomplete coverage.
//
// STRENGTH BUCKETING — two mechanisms, one per tool, because the two tools'
// Score fields are on fundamentally different ALREADY-EXISTING scales (see
// mora-retrieval-and-ranking / hybrid.go); this contract does not invent a
// third scale, it buckets each tool's own already-computed number:
//
//   search_memory routes through defaultSearch, which is FTS-only
//   (searchMemories, search.go) under the static-hash embedder — the ONLY path
//   exercised in this repo's CI (no Ollama, no network egress). Memory.Score
//   there is raw SQLite bm25(): more negative == a better match, unbounded
//   magnitude (scales with per-query IDF / corpus size — see hook.go's
//   hookRecallDefaultThreshold for the existing "lower is better" precedent).
//   Bucketed on max_score:
//     max_score <= confidenceSearchStrongMax   (-4.0)              -> "strong"
//     confidenceSearchStrongMax < max_score
//       <= confidenceSearchModerateMax (-1.5)                      -> "moderate"
//     max_score > confidenceSearchModerateMax (incl. no results,
//       max_score == 0)                                            -> "weak"
//   These two constants are CALIBRATED against seedConfidenceFixture's own
//   100-doc background corpus (which establishes realistic bm25/IDF stats —
//   a 1-2 doc corpus produces a degenerate ~1e-6 scale that does not
//   generalize) with wide margins: measured -6.75 / -3.13 / -1.19 against a
//   -4.0 / -1.5 split. They are NOT claimed as a universal production
//   calibration — see risk (1) below.
//
//   think ALWAYS routes through hybridSearchTrace (buildThink), which fuses
//   arms by Reciprocal Rank Fusion (rrfWeighted) — a RANK-based score, not a
//   match-quality magnitude. Under a single active arm (this repo's test/CI
//   reality with no Ollama), RRF's fused score for the top hit is nearly
//   CONSTANT regardless of how good the underlying match is (rank 0 always
//   scores ~fts_weight/(k+1)); three isolated single-hit queries of clearly
//   different match quality measured 0.136/0.125/0.115 — a rank-position
//   artifact of near-simultaneous inserts, not a quality signal. Bucketing
//   think's strength off raw Score magnitude the way search_memory does would
//   therefore be noise. Instead this contract buckets think's strength off
//   ThinkGaps — a deterministic signal ALREADY computed by computeGaps at
//   buildThink time (still "no new scoring system": it re-labels existing gap
//   output, it does not compute anything new):
//     len(Evidence) == 0                    -> "weak"     (CoverageHoles fires)
//     len(Evidence) > 0 AND !Gaps.empty()    -> "moderate" (Stale/ThinCoverage/
//                                                            CoverageHoles/
//                                                            RetrievalCaveats)
//     len(Evidence) > 0 AND  Gaps.empty()    -> "strong"
//
// OPEN QUESTIONS / RISKS FOR THE INTEGRATOR (repeated in the final report):
//  1. search_memory's absolute bm25 thresholds are fixture-calibrated, not
//     corpus-invariant; a real vault's IDF stats could shift where "strong"
//     actually lands. A percentile/relative-rank scheme may be more robust
//     long-term than a fixed magnitude split — flagged, not resolved here.
//  2. RESOLVED by the missing_sources amendment above: because missing_sources
//     is now a CALL-SCOPED read of sourceHealthAll (every enabled instance's
//     health state), not a per-evidence-row projection, neither tool needs to
//     walk its returned rows' Source field to compute it. This also means the
//     original concern here — that ThinkEvidence (think.go) carries no Source
//     field, forcing a read from the underlying []Memory buildThink already
//     holds — no longer applies for think either: both tools can derive
//     missing_sources identically, straight from sourceHealthAll(cfg, now),
//     with no ThinkEvidence schema change and no per-row Source plumbing.
//  3. The two tools use structurally different strength derivations (score
//     magnitude vs. gap presence). That asymmetry is a deliberate, documented
//     choice given each tool's own scale — not an oversight — but the
//     integrator may want a single shared derivation; this contract does not
//     mandate one.

// confidenceSearchStrongMax / confidenceSearchModerateMax are the FROZEN
// search_memory bucket boundaries documented above. Test-local (not production
// code): they exist purely so the boundary tests below assert the actual
// numeric split the implementation must honor, rather than a vague "some
// threshold exists".
const (
	confidenceSearchStrongMax   = -4.0
	confidenceSearchModerateMax = -1.5
)

// seedConfidenceFixture builds one deterministic synthetic vault covering every
// scenario this packet's tests need:
//
//   - a 100-doc background corpus (Source "notes", no health tracking) so FTS
//     bm25 IDF stats are realistic instead of degenerate (a 1-2 doc corpus
//     produces bm25 magnitudes ~1e-6, which does not generalize).
//   - two enabled connector instances: gmail (FRESH, synced 1h ago) and
//     imessage (STALE, synced 96h ago — imessage's health threshold is 48h,
//     health.go sourceHealthLocalThreshold). Because missing_sources is
//     call-scoped over sourceHealthAll (see the FROZEN SHAPE doc comment
//     above), imessage being fixture-wide stale means EVERY confidence:true
//     test below reports missing_sources=[imessage]/health_impact=stale,
//     regardless of which source actually matched that test's query.
//   - four target memories, each matched by its OWN unique rare query term so
//     every scenario below is a clean, isolated single-hit query:
//   - "quorlath"  -> strong-evidence  (gmail,    fresh,  recent -> strong)
//   - "vantrex"   -> moderate-evidence (gmail,   fresh,  recent -> moderate);
//     vantrex-old shares the term with an OLDER CreatedAt so
//     the freshest_source_at test proves MAX(CreatedAt), not
//     "whichever row search happens to return first" (search
//     orders by score, not date).
//   - "brindolex" -> weak-evidence    (imessage, STALE, old    -> weak)
//   - "wexoform"  -> healthy-baseline (gmail,    fresh,  recent -> strong)
//
// All CreatedAt/sync timestamps are fixed strings or now-relative deltas (no
// bare time.Now() comparisons against a moving wall clock beyond what
// seedSyncStatus already uses elsewhere in this package), so scores/dates are
// stable across runs. rebuildIndex runs once at the end (bulk-seed, single
// rebuild — mirrors seedBudgetFixture/digestSeedUnindexed's pattern).
func seedConfidenceFixture(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	for i := 0; i < 100; i++ {
		m := Memory{
			ID:        fmt.Sprintf("bg%03d", i),
			Scope:     "global",
			Type:      "insight",
			Title:     fmt.Sprintf("Background %d", i),
			CreatedAt: "2026-06-01T00:00:00Z",
			Source:    "notes",
			Text:      "background filler content about unrelated topics number " + fmt.Sprint(i),
		}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed background %d: %v", i, err)
		}
	}

	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-1*time.Hour))     // fresh
	seedSyncStatus(t, cfg, "imessage", time.Now().Add(-96*time.Hour)) // stale (>48h threshold)

	fixtures := []Memory{
		{
			ID: "strong-evidence", Scope: "global", Type: "email", Title: "Strong",
			CreatedAt: "2026-07-30T00:00:00Z", Source: "gmail",
			Text: "quorlath quorlath quorlath rare term about the rollout",
		},
		{
			ID: "moderate-evidence", Scope: "global", Type: "email", Title: "Moderate",
			CreatedAt: "2026-07-15T00:00:00Z", Source: "gmail",
			Text: "some context around the topic and then vantrex appears once here with more surrounding words padding it out nicely for realism",
		},
		{
			// Older row sharing the "vantrex" term — proves freshest_source_at
			// takes the MAX(CreatedAt) across the whole returned set, not just
			// the top-ranked (by score) row.
			ID: "vantrex-old", Scope: "global", Type: "email", Title: "Vantrex older",
			CreatedAt: "2026-05-01T00:00:00Z", Source: "gmail",
			Text: "an older note that also happens to mention vantrex once in passing",
		},
		{
			ID: "weak-evidence", Scope: "global", Type: "imessage", Title: "Weak",
			CreatedAt: "2026-06-10T00:00:00Z", Source: "imessage",
			Text: "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt in culpa qui officia deserunt mollit anim id est laborum brindolex sed ut perspiciatis unde omnis iste natus error sit voluptatem accusantium doloremque laudantium totam rem aperiam eaque ipsa quae ab illo inventore veritatis et quasi architecto",
		},
		{
			ID: "healthy-baseline", Scope: "global", Type: "email", Title: "Baseline",
			CreatedAt: "2026-07-29T00:00:00Z", Source: "gmail",
			Text: "wexoform wexoform wexoform another rare term for the baseline scenario",
		},
	}
	for _, m := range fixtures {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	return cfg
}

// searchStructured drives search_memory over the real MCP dispatch and returns
// the decoded structuredContent map (mirrors digestMCPStructured's pattern for
// digest).
func searchStructured(t *testing.T, args string) map[string]any {
	t.Helper()
	res := mcpResult(t, budgetCall("search_memory", args))
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("search_memory result missing object structuredContent: %v", res)
	}
	return sc
}

// thinkStructured is searchStructured's think analog.
func thinkStructured(t *testing.T, args string) map[string]any {
	t.Helper()
	res := mcpResult(t, budgetCall("think", args))
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("think result missing object structuredContent: %v", res)
	}
	return sc
}

// mustConfidence extracts the top-level "confidence" object, failing (RED,
// today, always) if it is absent or not an object.
func mustConfidence(t *testing.T, sc map[string]any) map[string]any {
	t.Helper()
	raw, ok := sc["confidence"]
	if !ok {
		t.Fatalf("response is missing the top-level %q key (confidence:true); got keys: %v", "confidence", payloadKeys(sc))
	}
	conf, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("confidence = %T, want a JSON object; got: %v", raw, raw)
	}
	return conf
}

// confidenceWantFields is the FROZEN exact field set (§ "FROZEN SHAPE" above).
// Extra or missing keys both fail — the block must stay this compact.
var confidenceWantFields = []string{
	"strength", "max_score", "mean_score", "freshest_source_at", "missing_sources", "health_impact",
}

func assertConfidenceShape(t *testing.T, conf map[string]any) {
	t.Helper()
	for _, f := range confidenceWantFields {
		if _, ok := conf[f]; !ok {
			t.Fatalf("confidence object missing field %q; got keys: %v", f, payloadKeys(conf))
		}
	}
	if len(conf) != len(confidenceWantFields) {
		t.Fatalf("confidence object has %d fields, want exactly %d (%v); got keys: %v",
			len(conf), len(confidenceWantFields), confidenceWantFields, payloadKeys(conf))
	}
	if _, ok := conf["strength"].(string); !ok {
		t.Fatalf("confidence.strength = %T, want string", conf["strength"])
	}
	switch conf["strength"] {
	case "strong", "moderate", "weak":
	default:
		t.Fatalf("confidence.strength = %v, want one of strong|moderate|weak", conf["strength"])
	}
	if _, ok := conf["max_score"].(float64); !ok {
		t.Fatalf("confidence.max_score = %T, want number", conf["max_score"])
	}
	if _, ok := conf["mean_score"].(float64); !ok {
		t.Fatalf("confidence.mean_score = %T, want number", conf["mean_score"])
	}
	if _, ok := conf["freshest_source_at"].(string); !ok {
		t.Fatalf("confidence.freshest_source_at = %T, want string", conf["freshest_source_at"])
	}
	missing, ok := conf["missing_sources"].([]any)
	if !ok {
		t.Fatalf("confidence.missing_sources = %T, want array", conf["missing_sources"])
	}
	prev := ""
	for i, v := range missing {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("confidence.missing_sources[%d] = %T, want string", i, v)
		}
		if i > 0 && s < prev {
			t.Fatalf("confidence.missing_sources is not sorted: %v", missing)
		}
		prev = s
	}
	hi, ok := conf["health_impact"].(string)
	if !ok {
		t.Fatalf("confidence.health_impact = %T, want string", conf["health_impact"])
	}
	switch hi {
	case "none", "stale", "failed", "never":
	default:
		t.Fatalf("confidence.health_impact = %q, want one of none|stale|failed|never", hi)
	}
}

func confidenceMissingSources(conf map[string]any) []string {
	raw, _ := conf["missing_sources"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func strSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- search_memory ----------------------------------------------------------

// TestConfidenceSearchMemoryStrongEvidenceHealthySource pins the top-level
// contract for search_memory's healthy/strong path: a dominant, recent, fresh-
// source match. This is the "healthy baseline" scenario from the deliverable
// list, using the isolated "wexoform" query so no other fixture memory
// contaminates the result set.
func TestConfidenceSearchMemoryStrongEvidenceHealthySource(t *testing.T) {
	seedConfidenceFixture(t)
	sc := searchStructured(t, `{"query":"wexoform","confidence":true}`)
	conf := mustConfidence(t, sc)
	assertConfidenceShape(t, conf)

	if conf["strength"] != "strong" {
		t.Fatalf("strength = %v, want strong (healthy-baseline: fresh gmail source, dominant match); max_score=%v", conf["strength"], conf["max_score"])
	}
	maxScore := conf["max_score"].(float64)
	if maxScore > confidenceSearchStrongMax {
		t.Fatalf("max_score = %v, want <= %v (the frozen strong boundary) for the healthy-baseline fixture", maxScore, confidenceSearchStrongMax)
	}
	if conf["freshest_source_at"] != "2026-07-29T00:00:00Z" {
		t.Fatalf("freshest_source_at = %v, want the healthy-baseline memory's CreatedAt", conf["freshest_source_at"])
	}
	// AMENDED: missing_sources is call-scoped over ALL enabled instances, not
	// just this query's contributors. This fixture always has imessage enabled
	// and stale (seedConfidenceFixture), so even a query whose only match is a
	// healthy gmail row still surfaces imessage's incomplete coverage.
	if got := confidenceMissingSources(conf); !strSlicesEqual(got, []string{"imessage"}) {
		t.Fatalf("missing_sources = %v, want [imessage] (call-scoped: imessage is enabled+stale fixture-wide, regardless of whether this query's match came from gmail)", got)
	}
	if conf["health_impact"] != "stale" {
		t.Fatalf("health_impact = %v, want stale (imessage's sourceHealth state, call-scoped)", conf["health_impact"])
	}
}

// TestConfidenceSearchMemoryWeakEvidenceUnhealthySource covers the deliverable's
// "weak evidence (low scores)" AND "incomplete coverage (a relevant source
// stale/failed via sourceHealth)" scenarios in one fixture: the ONLY memory
// matching "brindolex" is a diluted single mention in a long document, sourced
// from imessage, which this fixture seeded as STALE (last synced 96h ago,
// past the 48h local-connector threshold).
func TestConfidenceSearchMemoryWeakEvidenceUnhealthySource(t *testing.T) {
	seedConfidenceFixture(t)
	sc := searchStructured(t, `{"query":"brindolex","confidence":true}`)
	conf := mustConfidence(t, sc)
	assertConfidenceShape(t, conf)

	if conf["strength"] != "weak" {
		t.Fatalf("strength = %v, want weak (diluted single mention in a long doc); max_score=%v", conf["strength"], conf["max_score"])
	}
	maxScore := conf["max_score"].(float64)
	if maxScore <= confidenceSearchModerateMax {
		t.Fatalf("max_score = %v, want > %v (the frozen moderate/weak boundary)", maxScore, confidenceSearchModerateMax)
	}
	if conf["freshest_source_at"] != "2026-06-10T00:00:00Z" {
		t.Fatalf("freshest_source_at = %v, want the weak-evidence memory's CreatedAt", conf["freshest_source_at"])
	}
	// Value unchanged from the pre-amendment contract (imessage was always
	// this query's only contributor AND is fixture-wide stale), but the
	// reasoning is now call-scoped, not contribution-scoped: imessage would
	// appear here even if this query had matched nothing at all.
	if got := confidenceMissingSources(conf); !strSlicesEqual(got, []string{"imessage"}) {
		t.Fatalf("missing_sources = %v, want [imessage] (imessage is enabled+stale fixture-wide, call-scoped)", got)
	}
	if conf["health_impact"] != "stale" {
		t.Fatalf("health_impact = %v, want stale (imessage's sourceHealth state)", conf["health_impact"])
	}
}

// TestConfidenceSearchMemoryModerateEvidenceFreshestIsMax pins the moderate
// bucket AND proves freshest_source_at is MAX(CreatedAt) over the whole
// returned set, not just the top-ranked (by score) row: "vantrex" matches both
// moderate-evidence (2026-07-15, better bm25 rank) and vantrex-old
// (2026-05-01, worse rank) — the older row must not win freshest_source_at
// even though search orders by score, not date.
func TestConfidenceSearchMemoryModerateEvidenceFreshestIsMax(t *testing.T) {
	seedConfidenceFixture(t)
	sc := searchStructured(t, `{"query":"vantrex","confidence":true}`)
	conf := mustConfidence(t, sc)
	assertConfidenceShape(t, conf)

	if conf["strength"] != "moderate" {
		t.Fatalf("strength = %v, want moderate; max_score=%v", conf["strength"], conf["max_score"])
	}
	maxScore := conf["max_score"].(float64)
	if maxScore <= confidenceSearchStrongMax || maxScore > confidenceSearchModerateMax {
		t.Fatalf("max_score = %v, want strictly inside the moderate band (%v, %v]", maxScore, confidenceSearchStrongMax, confidenceSearchModerateMax)
	}
	if conf["freshest_source_at"] != "2026-07-15T00:00:00Z" {
		t.Fatalf("freshest_source_at = %v, want 2026-07-15T00:00:00Z (MAX CreatedAt across both vantrex rows, not the older vantrex-old row)", conf["freshest_source_at"])
	}
	// AMENDED: both vantrex rows are from the fresh gmail source, but
	// missing_sources is call-scoped over ALL enabled instances (see the
	// FROZEN SHAPE doc comment), so the fixture-wide-stale imessage instance
	// still surfaces here even though it contributed nothing to this query.
	if got := confidenceMissingSources(conf); !strSlicesEqual(got, []string{"imessage"}) {
		t.Fatalf("missing_sources = %v, want [imessage] (call-scoped: imessage is enabled+stale fixture-wide, regardless of the vantrex rows' own gmail source)", got)
	}
	if conf["health_impact"] != "stale" {
		t.Fatalf("health_impact = %v, want stale (imessage's sourceHealth state, call-scoped)", conf["health_impact"])
	}
}

// TestConfidenceSearchMemoryNoResults is the empty-result edge case: an
// unmatched query must still carry a well-formed confidence block rather than
// omitting it or erroring. AMENDED: missing_sources/health_impact are
// call-scoped over sourceHealthAll, not derived from the (empty) returned
// set, so a zero-result query still reports the fixture's stale imessage
// instance — that is exactly the "incomplete coverage" story this field
// exists to tell, not a signal that gets silenced just because nothing
// matched.
func TestConfidenceSearchMemoryNoResults(t *testing.T) {
	seedConfidenceFixture(t)
	sc := searchStructured(t, `{"query":"nonexistentzzzzterm","confidence":true}`)
	conf := mustConfidence(t, sc)
	assertConfidenceShape(t, conf)

	if conf["strength"] != "weak" {
		t.Fatalf("strength = %v, want weak for zero results", conf["strength"])
	}
	if conf["max_score"] != float64(0) || conf["mean_score"] != float64(0) {
		t.Fatalf("max_score/mean_score = %v/%v, want 0/0 for zero results", conf["max_score"], conf["mean_score"])
	}
	if conf["freshest_source_at"] != "" {
		t.Fatalf("freshest_source_at = %v, want \"\" for zero results", conf["freshest_source_at"])
	}
	if got := confidenceMissingSources(conf); !strSlicesEqual(got, []string{"imessage"}) {
		t.Fatalf("missing_sources = %v, want [imessage] for zero results (call-scoped over sourceHealthAll: imessage is enabled+stale fixture-wide even though nothing was retrieved)", got)
	}
	if conf["health_impact"] != "stale" {
		t.Fatalf("health_impact = %v, want stale for zero results (imessage's sourceHealth state, call-scoped)", conf["health_impact"])
	}
}

// TestConfidenceSearchMemoryKnobOffByteIdentical pins the frozen gating
// contract (mirrors TestMCPGateDigestEnvelopeOffByteIdentical): omitting
// `confidence` and passing `"confidence":false` must produce byte-identical
// structuredContent, and NEITHER may carry a `confidence` key. This is the
// backward-compat guarantee the "knob OFF" half of the frozen decision
// requires.
func TestConfidenceSearchMemoryKnobOffByteIdentical(t *testing.T) {
	seedConfidenceFixture(t)

	def := searchStructured(t, `{"query":"quorlath"}`)
	off := searchStructured(t, `{"query":"quorlath","confidence":false}`)

	if _, has := def["confidence"]; has {
		t.Fatalf("search_memory {query} (no confidence arg) must NOT carry a confidence key; keys=%v", payloadKeys(def))
	}
	if _, has := off["confidence"]; has {
		t.Fatalf("search_memory confidence:false must NOT carry a confidence key; keys=%v", payloadKeys(off))
	}

	defB, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal default payload: %v", err)
	}
	offB, err := json.Marshal(off)
	if err != nil {
		t.Fatalf("marshal confidence:false payload: %v", err)
	}
	if string(defB) != string(offB) {
		t.Fatalf("search_memory {query} and {query,confidence:false} must be byte-identical\n def: %s\n off: %s", defB, offB)
	}
}

// TestConfidenceSearchMemoryUnderT0Ceiling is the T0 guard: the confidence-on
// envelope for search_memory must stay under the EXISTING 8000-token ceiling
// (mora_mcp_budget_test.go budgetCases' search_default row) — this packet must
// not need, and must not receive, a raised ceiling.
func TestConfidenceSearchMemoryUnderT0Ceiling(t *testing.T) {
	seedConfidenceFixture(t)
	b := measureEnvelope(t, budgetCall("search_memory", `{"query":"quorlath","confidence":true}`))
	tok := (b + charsPerToken - 1) / charsPerToken
	if tok > 8000 {
		t.Fatalf("search_memory confidence-on envelope = %d tok (%d B) > the existing 8000 ceiling; the confidence block must be trimmed, the ceiling must NOT be raised", tok, b)
	}
	t.Logf("search_memory confidence-on: %d tok (%d B), ceiling 8000", tok, b)
}

// --- think -------------------------------------------------------------------

// TestConfidenceThinkStrongEvidenceNoGaps pins think's healthy/strong path: a
// recent, well-covered match with an empty ThinkGaps (Gaps.empty()==true).
func TestConfidenceThinkStrongEvidenceNoGaps(t *testing.T) {
	seedConfidenceFixture(t)
	sc := thinkStructured(t, `{"query":"quorlath","confidence":true}`)
	conf := mustConfidence(t, sc)
	assertConfidenceShape(t, conf)

	if conf["strength"] != "strong" {
		t.Fatalf("strength = %v, want strong (non-empty evidence, no ThinkGaps)", conf["strength"])
	}
	if conf["freshest_source_at"] != "2026-07-30T00:00:00Z" {
		t.Fatalf("freshest_source_at = %v, want the strong-evidence memory's CreatedAt", conf["freshest_source_at"])
	}
	// AMENDED: call-scoped over sourceHealthAll, not this query's evidence
	// sources — imessage is enabled+stale fixture-wide even though this
	// query's only match came from the fresh gmail source.
	if got := confidenceMissingSources(conf); !strSlicesEqual(got, []string{"imessage"}) {
		t.Fatalf("missing_sources = %v, want [imessage] (call-scoped: imessage is enabled+stale fixture-wide)", got)
	}
	if conf["health_impact"] != "stale" {
		t.Fatalf("health_impact = %v, want stale (imessage's sourceHealth state, call-scoped)", conf["health_impact"])
	}
}

// TestConfidenceThinkModerateEvidenceStaleGap covers think's "incomplete
// coverage" scenario via the ALREADY-COMPUTED ThinkGaps.Stale signal: the
// only memory matching "brindolex" is from 2026-06-10 — more than
// thinkStaleDays (30) before this test's real run time (2026-07-31 or later),
// so computeGaps already flags it Stale. Its source (imessage) is also the
// packet's stale connector, so this fixture doubles as the "relevant source
// stale via sourceHealth" scenario for think.
func TestConfidenceThinkModerateEvidenceStaleGap(t *testing.T) {
	seedConfidenceFixture(t)
	sc := thinkStructured(t, `{"query":"brindolex","confidence":true}`)
	conf := mustConfidence(t, sc)
	assertConfidenceShape(t, conf)

	if conf["strength"] != "moderate" {
		t.Fatalf("strength = %v, want moderate (non-empty evidence, but ThinkGaps.Stale fires)", conf["strength"])
	}
	if conf["freshest_source_at"] != "2026-06-10T00:00:00Z" {
		t.Fatalf("freshest_source_at = %v, want the weak-evidence memory's CreatedAt", conf["freshest_source_at"])
	}
	// Value unchanged from the pre-amendment contract (imessage was already
	// this query's only evidence source AND is fixture-wide stale), but the
	// reasoning is now call-scoped, not contribution-scoped.
	if got := confidenceMissingSources(conf); !strSlicesEqual(got, []string{"imessage"}) {
		t.Fatalf("missing_sources = %v, want [imessage] (call-scoped: imessage is enabled+stale fixture-wide)", got)
	}
	if conf["health_impact"] != "stale" {
		t.Fatalf("health_impact = %v, want stale", conf["health_impact"])
	}
}

// TestConfidenceThinkWeakEvidenceNoMatch covers think's zero-evidence path:
// computeGaps already emits CoverageHoles ("No memory matched this query.")
// for an unmatched query, which this contract re-labels as strength=weak.
// AMENDED: missing_sources/health_impact are call-scoped over sourceHealthAll,
// not derived from the (empty) evidence set, so a zero-evidence query still
// reports the fixture's stale imessage instance.
func TestConfidenceThinkWeakEvidenceNoMatch(t *testing.T) {
	seedConfidenceFixture(t)
	sc := thinkStructured(t, `{"query":"nonexistentzzzzterm","confidence":true}`)
	conf := mustConfidence(t, sc)
	assertConfidenceShape(t, conf)

	if conf["strength"] != "weak" {
		t.Fatalf("strength = %v, want weak for zero evidence", conf["strength"])
	}
	if conf["max_score"] != float64(0) || conf["mean_score"] != float64(0) {
		t.Fatalf("max_score/mean_score = %v/%v, want 0/0 for zero evidence", conf["max_score"], conf["mean_score"])
	}
	if conf["freshest_source_at"] != "" {
		t.Fatalf("freshest_source_at = %v, want \"\" for zero evidence", conf["freshest_source_at"])
	}
	if got := confidenceMissingSources(conf); !strSlicesEqual(got, []string{"imessage"}) {
		t.Fatalf("missing_sources = %v, want [imessage] for zero evidence (call-scoped over sourceHealthAll: imessage is enabled+stale fixture-wide even though nothing was retrieved)", got)
	}
	if conf["health_impact"] != "stale" {
		t.Fatalf("health_impact = %v, want stale for zero evidence (imessage's sourceHealth state, call-scoped)", conf["health_impact"])
	}
}

// TestConfidenceThinkKnobOffByteIdentical is search_memory's byte-identity test,
// mirrored for think.
func TestConfidenceThinkKnobOffByteIdentical(t *testing.T) {
	seedConfidenceFixture(t)

	def := thinkStructured(t, `{"query":"quorlath"}`)
	off := thinkStructured(t, `{"query":"quorlath","confidence":false}`)

	if _, has := def["confidence"]; has {
		t.Fatalf("think {query} (no confidence arg) must NOT carry a confidence key; keys=%v", payloadKeys(def))
	}
	if _, has := off["confidence"]; has {
		t.Fatalf("think confidence:false must NOT carry a confidence key; keys=%v", payloadKeys(off))
	}

	defB, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal default payload: %v", err)
	}
	offB, err := json.Marshal(off)
	if err != nil {
		t.Fatalf("marshal confidence:false payload: %v", err)
	}
	if string(defB) != string(offB) {
		t.Fatalf("think {query} and {query,confidence:false} must be byte-identical\n def: %s\n off: %s", defB, offB)
	}
}

// TestConfidenceThinkUnderT0Ceiling is the T0 guard for think: the
// confidence-on envelope must stay under the EXISTING 6000-token ceiling
// (mora_mcp_budget_test.go budgetCases' think row).
func TestConfidenceThinkUnderT0Ceiling(t *testing.T) {
	seedConfidenceFixture(t)
	b := measureEnvelope(t, budgetCall("think", `{"query":"quorlath","confidence":true}`))
	tok := (b + charsPerToken - 1) / charsPerToken
	if tok > 6000 {
		t.Fatalf("think confidence-on envelope = %d tok (%d B) > the existing 6000 ceiling; the confidence block must be trimmed, the ceiling must NOT be raised", tok, b)
	}
	t.Logf("think confidence-on: %d tok (%d B), ceiling 6000", tok, b)
}
