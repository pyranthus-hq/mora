package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// filters_contract_test.go — issue #241: optional trusted-source ("source") and
// time-window ("since_hours") filters on search_memory and context_memory. This
// file is the FROZEN CONTRACT, mirroring confidence_contract_test.go's style:
// every test drives the real MCP dispatch (mcpResult/callMCPTool) and asserts on
// the marshaled JSON. Neither arg exists yet, so every filtering assertion below
// fails RED at the assertion level today (the file compiles clean against the
// current codebase — no new Go types are referenced).
//
// FROZEN INTERFACE (integrator decision, pinned here):
//
//   - Optional params on BOTH search_memory and context_memory:
//     "source"      (string)  — connector family ("gmail") or family:instance
//     ("gmail:work"), matched via the SAME digestSourceMatches semantics the
//     digest --source filter already freezes (digest_source_filter_test.go):
//     a bare family selects every instance of that family; a family:instance
//     value selects only that instance.
//     "since_hours" (integer) — a positive look-back window; a memory whose
//     CreatedAt parses (RFC3339) to before now-since_hours is excluded. The
//     governing timestamp is Memory.CreatedAt — the SAME field digest/brief's
//     since-days filter and the confidence envelope's freshest_source_at both
//     already treat as the recency instant (filters.go's memoryMatchesPreviewFilters,
//     confidence.go's confidenceFreshest) — never a lexical string compare
//     (repository invariant): the instant is time.Parse'd before comparison.
//
//   - PRE-RANK, not post-hoc: both filters are applied inside each retrieval
//     arm BEFORE that arm's ranked list is truncated to its pool/limit, so a
//     filtered-out memory can never crowd a matching memory out of the pool.
//     TestFiltersSearchMemoryPreRankSourceProof /
//     TestFiltersSearchMemoryPreRankSinceHoursProof / TestFiltersHybridFTSArmPreRankProof
//     below are the adversarial fixtures that only pass under pre-rank semantics
//     (a naive "rank first, filter the returned page after" implementation
//     returns emptier/wrong results on these fixtures).
//
//   - Response receipt: the response gains a top-level "filters" object
//     ONLY when at least one filter param was supplied, echoing exactly the
//     supplied filter(s) — e.g. {"source":"imessage","since_hours":24}. Omitted
//     params ⇒ no "filters" key ⇒ byte-identical to pre-#241 output.
//
//   - source="imessage" can never return a gmail memory, regardless of query
//     or limit (TestFiltersSearchMemoryImessageCannotReturnGmail /
//     TestFiltersContextMemoryImessageCannotReturnGmail).
//
//   - Confidence interaction (search_memory's opt-in `confidence` envelope,
//     #238, frozen/untouched): a source EXCLUDED by an active source filter
//     must not appear in confidence.missing_sources and must not affect
//     health_impact — exclusion-by-filter is a caller choice, not a coverage
//     gap. missing_sources continues to list only non-fresh enabled sources
//     NOT excluded by the filter.
//
//   - Fail closed: an unrecognized source value, or a since_hours that is not
//     a positive integer, is an explicit tool error (isError:true) — never a
//     silent no-filter/no-match.
//
// CLOCK NOTE: since_hours is evaluated against the server's real time.Now() at
// call time (there is no injectable clock through the MCP surface — mirroring
// every other real-time MCP arg, e.g. sourceHealthAll's freshness checks). The
// boundary test below therefore uses a several-second margin around the cutoff
// rather than a literal single tick, so the pinned behavior (which side of the
// cutoff is included, which timestamp field governs, RFC3339 instant parsing
// rather than lexical comparison) is exercised deterministically without
// flaking on scheduling jitter between fixture-write time and dispatch time.

// filtersStructured drives tool `name` over the real MCP dispatch and returns
// the decoded structuredContent map, failing (RED today for any filter-shaped
// assertion) if the call errored or returned a non-object payload.
func filtersStructured(t *testing.T, name, args string) map[string]any {
	t.Helper()
	res := mcpResult(t, budgetCall(name, args))
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("%s(%s): unexpected tool error: %v", name, args, res)
	}
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("%s result missing object structuredContent: %v", name, res)
	}
	return sc
}

// filtersToolError drives tool `name` and returns (errorText, isError) without
// failing the test — for the fail-closed error-case pins.
func filtersToolError(t *testing.T, name, args string) (string, bool) {
	t.Helper()
	return mcpToolText(t, budgetCall(name, args))
}

// writeFilterMemory seeds one memory with an explicit Provider/Account so the
// real production keying seam (sourceInstanceKey, connectors.go) — not the
// per-item Source/ProviderID label — is what the source filter matches against,
// exactly like the digest source-filter tests (digest_source_filter_test.go)
// already establish for the digest surface.
func writeFilterMemory(t *testing.T, cfg Config, id, provider, account, createdAt, text string) {
	t.Helper()
	m := Memory{
		ID: id, Scope: "global", Type: "insight", Title: id,
		Provider: provider, Account: account, CreatedAt: createdAt, Text: text,
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// filterResultIDs extracts every "id" from a search_memory/context_memory-style
// []any "results"/"memories" array.
func filterResultIDs(t *testing.T, arr []any) []string {
	t.Helper()
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		row, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("result row = %T, want object: %v", v, v)
		}
		id, _ := row["id"].(string)
		out = append(out, id)
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// --- search_memory (FTS-only arm, search.go's searchMemories) --------------

// TestFiltersSearchMemoryPreRankSourceProof pins pre-rank source filtering: 3
// gmail memories heavily repeat "zorblatt" (strong bm25 rank), 1 imessage
// memory mentions it once (weak rank). With limit=1, a "rank first, filter the
// returned page after" implementation fetches ONLY the top-ranked gmail row
// (SQL ORDER BY ... LIMIT 1) and then discards it as non-matching — producing
// EMPTY results. Correct pre-rank semantics keep scanning past the SQL LIMIT
// boundary until a source-matching row is found, returning the imessage memory.
func TestFiltersSearchMemoryPreRankSourceProof(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	writeFilterMemory(t, cfg, "gmail-1", "gmail", "", "2026-07-01T00:00:00Z", "zorblatt zorblatt zorblatt one")
	writeFilterMemory(t, cfg, "gmail-2", "gmail", "", "2026-07-02T00:00:00Z", "zorblatt zorblatt zorblatt two")
	writeFilterMemory(t, cfg, "gmail-3", "gmail", "", "2026-07-03T00:00:00Z", "zorblatt zorblatt zorblatt three")
	writeFilterMemory(t, cfg, "imsg-1", "imessage", "", "2026-07-04T00:00:00Z", "just a passing zorblatt mention")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"zorblatt","source":"imessage","limit":1}`)
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if !containsID(ids, "imsg-1") {
		t.Fatalf("pre-rank source filter: want imsg-1 in results (imessage match beyond the gmail-dominated rank-1 page), got %v", ids)
	}
	for _, id := range ids {
		if id != "imsg-1" {
			t.Fatalf("source=imessage must never return a non-imessage row; got %v", ids)
		}
	}
}

// TestFiltersSearchMemoryPreRankSinceHoursProof is PreRankSourceProof's
// since_hours analog: 3 old, heavily-repeated "throlbex" memories rank far
// above 1 recent, weakly-matching one. A naive "rank first, filter the
// returned page" implementation truncates to the old rows at limit=1 and
// returns empty after filtering; pre-rank semantics find the in-window row.
func TestFiltersSearchMemoryPreRankSinceHoursProof(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	old := time.Now().Add(-90 * 24 * time.Hour) // far outside any sane since_hours window below
	writeFilterMemory(t, cfg, "old-1", "gmail", "", old.Format(time.RFC3339), "throlbex throlbex throlbex one")
	writeFilterMemory(t, cfg, "old-2", "gmail", "", old.Add(time.Hour).Format(time.RFC3339), "throlbex throlbex throlbex two")
	writeFilterMemory(t, cfg, "old-3", "gmail", "", old.Add(2*time.Hour).Format(time.RFC3339), "throlbex throlbex throlbex three")
	recent := time.Now().Add(-1 * time.Hour)
	writeFilterMemory(t, cfg, "recent-1", "gmail", "", recent.Format(time.RFC3339), "a lone throlbex mention")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"throlbex","since_hours":24,"limit":1}`)
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if !containsID(ids, "recent-1") {
		t.Fatalf("pre-rank since_hours filter: want recent-1 (in-window, beyond the old-dominated rank-1 page), got %v", ids)
	}
	for _, id := range ids {
		if id == "old-1" || id == "old-2" || id == "old-3" {
			t.Fatalf("since_hours=24 must exclude 90-day-old rows; got %v", ids)
		}
	}
}

// TestFiltersSearchMemoryFamilyVsInstance pins digestSourceMatches' family vs
// exact-instance semantics on the search surface: "gmail" selects every gmail
// account instance; "gmail:work" selects only that one.
func TestFiltersSearchMemoryFamilyVsInstance(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	writeFilterMemory(t, cfg, "gmail-default", "gmail", "", "2026-07-01T00:00:00Z", "quixolar family default account")
	writeFilterMemory(t, cfg, "gmail-work", "gmail", "work", "2026-07-02T00:00:00Z", "quixolar family work account")
	writeFilterMemory(t, cfg, "imsg-decoy", "imessage", "", "2026-07-03T00:00:00Z", "quixolar decoy imessage")
	mustRebuild(t, cfg)

	family := filtersStructured(t, "search_memory", `{"query":"quixolar","source":"gmail","limit":10}`)
	famResults, _ := family["results"].([]any)
	famIDs := filterResultIDs(t, famResults)
	if !containsID(famIDs, "gmail-default") || !containsID(famIDs, "gmail-work") {
		t.Fatalf("source=gmail must select every gmail account instance, got %v", famIDs)
	}
	if containsID(famIDs, "imsg-decoy") {
		t.Fatalf("source=gmail must not select imessage, got %v", famIDs)
	}

	instance := filtersStructured(t, "search_memory", `{"query":"quixolar","source":"gmail:work","limit":10}`)
	instResults, _ := instance["results"].([]any)
	instIDs := filterResultIDs(t, instResults)
	if len(instIDs) != 1 || instIDs[0] != "gmail-work" {
		t.Fatalf("source=gmail:work must select ONLY the work instance, got %v", instIDs)
	}
}

// TestFiltersSearchMemoryImessageCannotReturnGmail is the frozen adversarial
// pin: no query/limit combination lets source=imessage leak a gmail memory.
func TestFiltersSearchMemoryImessageCannotReturnGmail(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	writeFilterMemory(t, cfg, "gmail-x", "gmail", "", "2026-07-01T00:00:00Z", "vintorak shared term")
	writeFilterMemory(t, cfg, "imsg-x", "imessage", "", "2026-07-02T00:00:00Z", "vintorak shared term")
	mustRebuild(t, cfg)

	for _, limit := range []int{1, 2, 10, 50} {
		sc := filtersStructured(t, "search_memory", fmt.Sprintf(`{"query":"vintorak","source":"imessage","limit":%d}`, limit))
		results, _ := sc["results"].([]any)
		ids := filterResultIDs(t, results)
		if containsID(ids, "gmail-x") {
			t.Fatalf("limit=%d: source=imessage returned gmail-x: %v", limit, ids)
		}
	}
}

// TestFiltersSearchMemoryByteIdenticalWhenOmitted pins the backward-compat
// guarantee: omitting source/since_hours must produce a response with no
// top-level "filters" key, and the SAME payload whether or not the (now-
// registered) params are simply absent from the call.
func TestFiltersSearchMemoryByteIdenticalWhenOmitted(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "plain-1", "gmail", "", "2026-07-01T00:00:00Z", "wobsprevil plain content")
	mustRebuild(t, cfg)

	a := filtersStructured(t, "search_memory", `{"query":"wobsprevil"}`)
	if _, ok := a["filters"]; ok {
		t.Fatalf("omitted source/since_hours must not produce a top-level filters key: %v", payloadKeys(a))
	}
	b := filtersStructured(t, "search_memory", `{"query":"wobsprevil","limit":8}`)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("byte-identity broken between equivalent omitted-filter calls:\na=%s\nb=%s", ab, bb)
	}
}

// TestFiltersSearchMemoryUnknownSourceErrors pins fail-closed behavior on an
// unrecognized source value — never a silent no-filter/empty-match.
func TestFiltersSearchMemoryUnknownSourceErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "any-1", "gmail", "", "2026-07-01T00:00:00Z", "nonsensehook content")
	mustRebuild(t, cfg)

	text, isErr := filtersToolError(t, "search_memory", `{"query":"nonsensehook","source":"not_a_real_connector"}`)
	if !isErr {
		t.Fatalf("unknown source must be a tool error (fail closed); got isError=false, text=%s", text)
	}
}

// TestFiltersSearchMemoryNonPositiveSinceHoursErrors pins fail-closed behavior
// on a since_hours value that is not a positive integer: zero, negative, and
// fractional all must error rather than silently pass through as no-filter.
func TestFiltersSearchMemoryNonPositiveSinceHoursErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "any-2", "gmail", "", "2026-07-01T00:00:00Z", "featherlox content")
	mustRebuild(t, cfg)

	for _, bad := range []string{`"since_hours":0`, `"since_hours":-5`, `"since_hours":1.5`} {
		args := fmt.Sprintf(`{"query":"featherlox",%s}`, bad)
		text, isErr := filtersToolError(t, "search_memory", args)
		if !isErr {
			t.Fatalf("%s: non-positive/fractional since_hours must be a tool error; got isError=false, text=%s", args, text)
		}
	}
}

// TestFiltersSearchMemoryFiltersReceiptShape pins the exact "filters" receipt
// object: present only when supplied, echoing exactly the supplied filter(s).
func TestFiltersSearchMemoryFiltersReceiptShape(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "recv-1", "imessage", "", time.Now().Format(time.RFC3339), "receiptcheck content")
	mustRebuild(t, cfg)

	both := filtersStructured(t, "search_memory", `{"query":"receiptcheck","source":"imessage","since_hours":24}`)
	f, ok := both["filters"].(map[string]any)
	if !ok {
		t.Fatalf("both filters supplied: want top-level filters object, got %v", payloadKeys(both))
	}
	if len(f) != 2 || f["source"] != "imessage" || fmt.Sprint(f["since_hours"]) != "24" {
		t.Fatalf("filters receipt = %v, want exactly {source:imessage, since_hours:24}", f)
	}

	sourceOnly := filtersStructured(t, "search_memory", `{"query":"receiptcheck","source":"imessage"}`)
	fs, ok := sourceOnly["filters"].(map[string]any)
	if !ok || len(fs) != 1 || fs["source"] != "imessage" {
		t.Fatalf("source-only filters receipt = %v, want exactly {source:imessage}", fs)
	}

	hoursOnly := filtersStructured(t, "search_memory", `{"query":"receiptcheck","since_hours":24}`)
	fh, ok := hoursOnly["filters"].(map[string]any)
	if !ok || len(fh) != 1 || fmt.Sprint(fh["since_hours"]) != "24" {
		t.Fatalf("since_hours-only filters receipt = %v, want exactly {since_hours:24}", fh)
	}
}

// TestFiltersSearchMemoryConfidenceExcludesFilteredSource pins the frozen
// interaction with #238's confidence envelope (confidence.go, untouched): a
// source EXCLUDED by an active source filter must not appear in
// confidence.missing_sources and must not affect health_impact. gmail is
// enabled+stale (would normally appear in missing_sources); imessage is
// enabled+fresh. Filtering to source=imessage must report missing_sources=[]
// and health_impact="none" — gmail's staleness is invisible once the caller
// has chosen to exclude it, not a coverage gap.
func TestFiltersSearchMemoryConfidenceExcludesFilteredSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-30*time.Hour))   // stale (>24h google threshold)
	seedSyncStatus(t, cfg, "imessage", time.Now().Add(-1*time.Hour)) // fresh
	writeFilterMemory(t, cfg, "conf-1", "imessage", "", time.Now().Format(time.RFC3339), "confidencegap content")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"confidencegap","source":"imessage","confidence":true}`)
	conf, ok := sc["confidence"].(map[string]any)
	if !ok {
		t.Fatalf("confidence:true must produce a confidence object: %v", payloadKeys(sc))
	}
	missing, _ := conf["missing_sources"].([]any)
	if len(missing) != 0 {
		t.Fatalf("source=imessage must exclude gmail from missing_sources (caller's own filter choice, not a gap); got %v", missing)
	}
	if conf["health_impact"] != "none" {
		t.Fatalf("health_impact = %v, want none (gmail's staleness must not leak through a filter the caller applied)", conf["health_impact"])
	}
}

// TestFiltersSearchMemorySinceHoursBoundary pins the deterministic boundary:
// the governing instant is Memory.CreatedAt parsed as RFC3339 and compared
// against now-since_hours, never a lexical string compare. A margin of a few
// seconds around the cutoff absorbs scheduling jitter between fixture-write
// time and dispatch time without weakening what is actually pinned (which side
// of the cutoff is included).
func TestFiltersSearchMemorySinceHoursBoundary(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	const sinceHours = 1
	cutoff := time.Now().Add(-sinceHours * time.Hour)
	writeFilterMemory(t, cfg, "just-inside", "gmail", "", cutoff.Add(5*time.Second).Format(time.RFC3339), "boundarypin content inside")
	writeFilterMemory(t, cfg, "just-outside", "gmail", "", cutoff.Add(-5*time.Second).Format(time.RFC3339), "boundarypin content outside")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", fmt.Sprintf(`{"query":"boundarypin","since_hours":%d,"limit":10}`, sinceHours))
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if !containsID(ids, "just-inside") {
		t.Fatalf("a memory just inside the since_hours window must be included, got %v", ids)
	}
	if containsID(ids, "just-outside") {
		t.Fatalf("a memory just outside the since_hours window must be excluded, got %v", ids)
	}
}

// --- context_memory (hybrid path: FTS + vector + graph arms, hybrid.go) ----

// TestFiltersContextMemoryAppliesSourceAndSinceHours is a functional pin (no
// pool crowding) that both filters actually narrow context_memory's assembled
// context, on the query path (hybridSearch).
func TestFiltersContextMemoryAppliesSourceAndSinceHours(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-gmail", "gmail", "", "2026-07-01T00:00:00Z", "plimsauge context gmail body")
	writeFilterMemory(t, cfg, "ctx-imsg", "imessage", "", time.Now().Format(time.RFC3339), "plimsauge context imessage body")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"query":"plimsauge","source":"imessage"}`)
	ctxText, _ := sc["context"].(string)
	if !containsSub(ctxText, "imessage body") {
		t.Fatalf("context_memory source=imessage must include the imessage body; got: %s", ctxText)
	}
	if containsSub(ctxText, "gmail body") {
		t.Fatalf("context_memory source=imessage must exclude the gmail body; got: %s", ctxText)
	}
}

// TestFiltersContextMemoryNoQueryPathAppliesFilters pins filtering on
// context_memory's no-query "recency briefing" fallback (listMemories,
// memfile.go), not just the hybridSearch query path.
func TestFiltersContextMemoryNoQueryPathAppliesFilters(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "brief-gmail", "gmail", "", time.Now().Format(time.RFC3339), "recencybrief gmail body only here")
	writeFilterMemory(t, cfg, "brief-imsg", "imessage", "", time.Now().Add(-time.Minute).Format(time.RFC3339), "recencybrief imessage body only here")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"source":"imessage"}`)
	ctxText, _ := sc["context"].(string)
	if !containsSub(ctxText, "imessage body only here") {
		t.Fatalf("no-query context_memory source=imessage must include the imessage note; got: %s", ctxText)
	}
	if containsSub(ctxText, "gmail body only here") {
		t.Fatalf("no-query context_memory source=imessage must exclude the gmail note; got: %s", ctxText)
	}
}

// TestFiltersContextMemoryImessageCannotReturnGmail repeats the adversarial
// pin on context_memory's hybrid path.
func TestFiltersContextMemoryImessageCannotReturnGmail(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "hy-gmail", "gmail", "", "2026-07-01T00:00:00Z", "sundrivex shared hybrid term")
	writeFilterMemory(t, cfg, "hy-imsg", "imessage", "", "2026-07-02T00:00:00Z", "sundrivex shared hybrid term")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"query":"sundrivex","source":"imessage"}`)
	ctxText, _ := sc["context"].(string)
	if containsSub(ctxText, "hy-gmail") {
		t.Fatalf("context_memory source=imessage must never surface a gmail memory id; got: %s", ctxText)
	}
}

// TestFiltersContextMemoryByteIdenticalWhenOmitted mirrors the search_memory
// byte-identity pin for context_memory.
func TestFiltersContextMemoryByteIdenticalWhenOmitted(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-plain", "gmail", "", "2026-07-01T00:00:00Z", "hobknuckle plain context")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"query":"hobknuckle"}`)
	if _, ok := sc["filters"]; ok {
		t.Fatalf("omitted source/since_hours must not produce a top-level filters key: %v", payloadKeys(sc))
	}
}

// TestFiltersContextMemoryUnknownSourceErrors mirrors the search_memory
// fail-closed pin for context_memory.
func TestFiltersContextMemoryUnknownSourceErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-any", "gmail", "", "2026-07-01T00:00:00Z", "gobfrenzy content")
	mustRebuild(t, cfg)

	text, isErr := filtersToolError(t, "context_memory", `{"query":"gobfrenzy","source":"not_a_real_connector"}`)
	if !isErr {
		t.Fatalf("unknown source must be a tool error (fail closed); got isError=false, text=%s", text)
	}
}

// TestFiltersHybridFTSArmPreRankProof is the hybrid-path analog of
// TestFiltersSearchMemoryPreRankSourceProof, targeting hybrid.go's ftsSearchIDs
// specifically: hybridSearch's arm pool floors at 50 (limit*5, min 50), so 51
// gmail decoys ranked above 1 weak imessage match are enough to crowd a naive
// "ORDER BY bm25 LIMIT pool, filter after" implementation into returning
// nothing once filtered — while pre-rank semantics still surface the imessage
// row.
func TestFiltersHybridFTSArmPreRankProof(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	for i := 0; i < 51; i++ {
		writeFilterMemory(t, cfg, fmt.Sprintf("decoy-%02d", i), "gmail", "",
			fmt.Sprintf("2026-06-%02dT00:00:00Z", (i%28)+1),
			"quorvenal quorvenal quorvenal decoy filler")
	}
	writeFilterMemory(t, cfg, "hy-weak-imsg", "imessage", "", "2026-07-05T00:00:00Z", "a lone quorvenal mention")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"query":"quorvenal","source":"imessage"}`)
	ctxText, _ := sc["context"].(string)
	if !containsSub(ctxText, "lone quorvenal mention") {
		t.Fatalf("hybrid FTS-arm pre-rank filter: want the imessage row surfaced past 51 gmail decoys crowding the pool=50 floor; context: %s", ctxText)
	}
}

// mustRebuild is a t.Helper wrapper over rebuildIndex, matching this file's
// (cfg-only, no context plumbing) test style.
func mustRebuild(t *testing.T, cfg Config) {
	t.Helper()
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
}

// containsSub is a tiny strings.Contains alias kept for call-site readability
// (the assertions above read as "context must containSub the body text").
func containsSub(s, sub string) bool {
	return strings.Contains(s, sub)
}
