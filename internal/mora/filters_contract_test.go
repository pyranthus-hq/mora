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
//   - Health/confidence both honor the exclusion: the ALWAYS-PRESENT "health"
//     rollup (compactHealthOf) is scoped the SAME way as the opt-in confidence
//     envelope — a source excluded by an active source filter must not drag
//     the top-level health.state/health.sources banner into degraded/
//     unhealthy either. Both are the SAME underlying signal
//     (sourceHealthAll/worstSource) projected two ways; excluding a source
//     from one without the other would be an inconsistent half-fix.
//
//   - source value space: the family:instance grammar is the SAME as
//     digestSourceMatches — a KNOWN family with an instance suffix that
//     matches no actual memory (e.g. "gmail:doesnotexist") is well-formed and
//     simply yields zero matches (not a tool error): digestSourceMatches has
//     no notion of "ambiguous" or "nonexistent" instances, only exact-key /
//     family-prefix / no-match, and there is no enumerable universe of valid
//     account labels to validate an instance suffix against (Account is a
//     free-form per-token label, not a catalog value). Only the FAMILY
//     component is validated against the connector catalog and fails closed.
//
// CLOCK NOTE: since_hours is evaluated against briefClock() — the SAME
// package-level, test-injectable clock var digest/brief already use for their
// own since-days/since-hours cutoffs (mora.go's `var briefClock = time.Now`,
// consumed by buildDigest/filteredBriefDigest in mcp.go) — never a bare
// time.Now() call inside the filter machinery. pinFiltersClock below freezes
// it exactly like brief_cmd_test.go's pinBriefClock, so the boundary test pins
// a LITERAL instant with no jitter margin, not an approximation.

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

// filtersFixedNow is a fixed UTC instant this file pins briefClock to for
// deterministic since_hours boundary tests (mirrors brief_cmd_test.go's
// briefFixedNow/pinBriefClock pattern exactly, kept file-local so this
// contract does not depend on another test file's private fixture).
var filtersFixedNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// pinFiltersClock freezes briefClock — the SAME clock var digest/brief's
// since-days/since-hours cutoffs already use (mora.go) — to filtersFixedNow
// for the duration of the test, restoring it on cleanup. #241's since_hours
// MUST read this same var (not a bare time.Now()) so this pin actually
// controls the cutoff the filter computes.
func pinFiltersClock(t *testing.T) {
	t.Helper()
	old := briefClock
	briefClock = func() time.Time { return filtersFixedNow }
	t.Cleanup(func() { briefClock = old })
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

// TestFiltersSearchMemorySinceHoursBoundary pins the EXACT deterministic
// boundary against a briefClock-pinned instant (no jitter margin needed): the
// governing instant is Memory.CreatedAt parsed as RFC3339 and compared against
// briefClock()-since_hours, never a lexical string compare. A memory dated
// EXACTLY at the cutoff is INSIDE the window (inclusive); one dated a single
// second before it is outside.
func TestFiltersSearchMemorySinceHoursBoundary(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pinFiltersClock(t)

	const sinceHours = 1
	cutoff := filtersFixedNow.Add(-sinceHours * time.Hour)
	writeFilterMemory(t, cfg, "at-cutoff", "gmail", "", cutoff.Format(time.RFC3339), "boundarypin content at cutoff")
	writeFilterMemory(t, cfg, "one-sec-outside", "gmail", "", cutoff.Add(-time.Second).Format(time.RFC3339), "boundarypin content outside")
	writeFilterMemory(t, cfg, "one-sec-inside", "gmail", "", cutoff.Add(time.Second).Format(time.RFC3339), "boundarypin content inside")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", fmt.Sprintf(`{"query":"boundarypin","since_hours":%d,"limit":10}`, sinceHours))
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if !containsID(ids, "at-cutoff") {
		t.Fatalf("a memory dated EXACTLY at the cutoff must be included (inclusive boundary), got %v", ids)
	}
	if !containsID(ids, "one-sec-inside") {
		t.Fatalf("a memory 1s inside the window must be included, got %v", ids)
	}
	if containsID(ids, "one-sec-outside") {
		t.Fatalf("a memory 1s outside the window must be excluded, got %v", ids)
	}
}

// TestFiltersContextMemorySinceHoursBoundary is the boundary pin's
// context_memory analog, over the hybridSearch query path.
func TestFiltersContextMemorySinceHoursBoundary(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pinFiltersClock(t)

	const sinceHours = 2
	cutoff := filtersFixedNow.Add(-sinceHours * time.Hour)
	writeFilterMemory(t, cfg, "ctx-at-cutoff", "gmail", "", cutoff.Format(time.RFC3339), "ctxboundarypin content at cutoff")
	writeFilterMemory(t, cfg, "ctx-one-sec-outside", "gmail", "", cutoff.Add(-time.Second).Format(time.RFC3339), "ctxboundarypin content outside")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", fmt.Sprintf(`{"query":"ctxboundarypin","since_hours":%d}`, sinceHours))
	ctxText, _ := sc["context"].(string)
	if !containsSub(ctxText, "at cutoff") {
		t.Fatalf("a memory dated EXACTLY at the cutoff must be included (inclusive boundary); context: %s", ctxText)
	}
	if containsSub(ctxText, "outside") {
		t.Fatalf("a memory 1s outside the window must be excluded; context: %s", ctxText)
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

// TestFiltersVectorArmPreRankProof isolates hybrid.go's vectorSearchIDs
// SPECIFICALLY, under REAL semantic routing (fakeOllama, mirroring
// TestRt_DefaultSearchSemanticRoutesToHybrid's env seam exactly — Ollama
// opted in + a reachable fake daemon, sanity-checked via
// chooseEmbedderFor/embedderIsSemantic before asserting).
//
// fakeOllama returns the SAME fixed vector for every embedding call regardless
// of content, so every memory ties at cosine similarity 1.0 — the arm's ONLY
// discriminator at that point is the id-ascending tie-break. 51 gmail decoys
// are named to sort BEFORE the imessage target ("aaa-decoy-*" < "zzz-target"),
// and the query term appears in NO memory's text and matches NO person (so the
// FTS and graph arms both contribute ZERO candidates) — isolating success to
// the vector arm alone. A naive "compute cosine for every row, sort, cut to
// pool=50, filter after" implementation fills all 50 slots with gmail decoys
// (51 > 50) and never even reaches the target; pre-rank semantics skip the
// cosine computation for a filtered-out row entirely, so only the target ever
// occupies a pool slot.
func TestFiltersVectorArmPreRankProof(t *testing.T) {
	srv := fakeOllama(t, []float64{1, 0, 0, 0})
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// Sanity: the chosen embedder really is semantic (drives the hybrid route).
	if emb, embErr := chooseEmbedderFor(cfg); embErr != nil || !embedderIsSemantic(emb) {
		t.Fatalf("fakeOllama should yield a semantic embedder (err=%v)", embErr)
	}

	for i := 0; i < 51; i++ {
		writeFilterMemory(t, cfg, fmt.Sprintf("aaa-decoy-%02d", i), "gmail", "",
			fmt.Sprintf("2026-06-%02dT00:00:00Z", (i%28)+1),
			"unrelated vector-arm filler content")
	}
	writeFilterMemory(t, cfg, "zzz-target", "imessage", "", "2026-07-05T00:00:00Z", "the vector arm target body")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"qzxvecarmprobe","source":"imessage","limit":10}`)
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if !containsID(ids, "zzz-target") {
		t.Fatalf("vector-arm pre-rank filter: want zzz-target surfaced past 51 id-earlier gmail decoys tied at cosine=1.0; got %v", ids)
	}
	for _, id := range ids {
		if id != "zzz-target" {
			t.Fatalf("source=imessage must never return a gmail decoy via the vector arm; got %v", ids)
		}
	}
}

// TestFiltersGraphArmPreRankProof isolates hybrid.go's graphExpandIDs
// SPECIFICALLY, with NO Ollama configured (the default static embedder still
// populates mem_vectors, so vecOK is true and the graph arm runs — but useVec
// is false, so the vector arm contributes nothing). The query is a person's
// display name, resolved via the entity gazetteer (built from Meta at index
// time) — never appearing literally in any memory's Text — so the FTS arm
// also contributes zero candidates. 51 gmail decoys and 1 imessage target all
// share an edge to the SAME person, but the decoys are dated NEWER than the
// target: a naive "ORDER BY created_at DESC LIMIT pool, filter after"
// per-person query fills pool=50 with the 51 newer decoys and never reaches
// the older target; pre-rank semantics drop the per-person SQL LIMIT and
// Go-filter (by source) before the cut, so the target is found regardless of
// how many newer non-matching rows exist for that person.
func TestFiltersGraphArmPreRankProof(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	person := map[string]any{
		"from":  []string{"zqvethran@example.com"},
		"to":    []string{"adit@x.com"},
		"names": map[string]string{"zqvethran@example.com": "Zqvethran Bolgo"},
	}
	for i := 0; i < 51; i++ {
		m := Memory{
			ID: fmt.Sprintf("graph-decoy-%02d", i), Scope: "global", Type: "email",
			Title: fmt.Sprintf("graph-decoy-%02d", i), Provider: "gmail",
			CreatedAt: fmt.Sprintf("2026-07-%02dT00:00:00Z", (i%27)+1),
			Text:      "unrelated graph-arm filler content, no name here",
			Meta:      person,
		}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed graph-decoy-%02d: %v", i, err)
		}
	}
	target := Memory{
		ID: "graph-target", Scope: "global", Type: "imessage", Title: "graph-target",
		Provider: "imessage", CreatedAt: "2026-05-01T00:00:00Z",
		Text: "the graph arm target body, also no name here", Meta: person,
	}
	if err := writeMemory(cfg, target); err != nil {
		t.Fatalf("seed graph-target: %v", err)
	}
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"query":"Zqvethran Bolgo","source":"imessage"}`)
	ctxText, _ := sc["context"].(string)
	if !containsSub(ctxText, "graph arm target body") {
		t.Fatalf("graph-arm pre-rank filter: want the imessage target surfaced past 51 newer gmail decoys sharing the same person edge; context: %s", ctxText)
	}
	if containsSub(ctxText, "graph-arm filler content") {
		t.Fatalf("source=imessage must never surface a gmail decoy via the graph arm; context: %s", ctxText)
	}
}

// TestFiltersContextMemoryFiltersReceiptShape mirrors the search_memory
// receipt-shape pin for context_memory.
func TestFiltersContextMemoryFiltersReceiptShape(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-recv-1", "imessage", "", time.Now().Format(time.RFC3339), "ctxreceiptcheck content")
	mustRebuild(t, cfg)

	both := filtersStructured(t, "context_memory", `{"query":"ctxreceiptcheck","source":"imessage","since_hours":24}`)
	f, ok := both["filters"].(map[string]any)
	if !ok || len(f) != 2 || f["source"] != "imessage" || fmt.Sprint(f["since_hours"]) != "24" {
		t.Fatalf("filters receipt = %v, want exactly {source:imessage, since_hours:24}", f)
	}
}

// TestFiltersContextMemoryNonPositiveSinceHoursErrors mirrors the
// search_memory fail-closed since_hours pin for context_memory.
func TestFiltersContextMemoryNonPositiveSinceHoursErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-any-2", "gmail", "", "2026-07-01T00:00:00Z", "ctxfeatherlox content")
	mustRebuild(t, cfg)

	for _, bad := range []string{`"since_hours":0`, `"since_hours":-5`, `"since_hours":1.5`} {
		args := fmt.Sprintf(`{"query":"ctxfeatherlox",%s}`, bad)
		text, isErr := filtersToolError(t, "context_memory", args)
		if !isErr {
			t.Fatalf("%s: non-positive/fractional since_hours must be a tool error; got isError=false, text=%s", args, text)
		}
	}
}

// TestFiltersSourceKnownFamilyUnknownInstanceIsWellFormedZeroMatch pins the
// "SOURCE VALUE SPACE" edge: a KNOWN family with an instance suffix matching
// no actual account ("gmail:doesnotexist") is well-formed (isError:false) and
// simply matches nothing — it must NOT fall back to matching the bare/default
// "gmail" instance (mirroring TestDigestSourceFilter's own pinned asymmetry:
// digestSourceMatches("gmail", "gmail:work") is false) and must NOT be
// rejected as if it were an unrecognized family.
func TestFiltersSourceKnownFamilyUnknownInstanceIsWellFormedZeroMatch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "instance-default", "gmail", "", "2026-07-01T00:00:00Z", "ghostaccount content")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"ghostaccount","source":"gmail:doesnotexist"}`)
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if len(ids) != 0 {
		t.Fatalf("source=gmail:doesnotexist must match zero memories (must not fall back to the bare gmail instance), got %v", ids)
	}
}

// TestFiltersSearchMemoryHealthExcludesFilteredSource pins the SAME
// excluded-by-filter carve-out on the ALWAYS-PRESENT top-level "health" rollup
// (compactHealthOf) that TestFiltersSearchMemoryConfidenceExcludesFilteredSource
// pins for the opt-in confidence envelope — both project the SAME
// sourceHealthAll/worstSource signal, so excluding a source from one without
// the other would be an inconsistent half-fix. gmail is enabled+stale (would
// normally degrade health.state/health.sources); imessage is enabled+fresh.
// Filtering to source=imessage must report a HEALTHY banner.
func TestFiltersSearchMemoryHealthExcludesFilteredSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-30*time.Hour))
	seedSyncStatus(t, cfg, "imessage", time.Now().Add(-1*time.Hour))
	writeFilterMemory(t, cfg, "health-1", "imessage", "", time.Now().Format(time.RFC3339), "healthgap content")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"healthgap","source":"imessage"}`)
	health, ok := sc["health"].(map[string]any)
	if !ok {
		t.Fatalf("search_memory must always carry a health object: %v", payloadKeys(sc))
	}
	if health["state"] != "healthy" {
		t.Fatalf("health.state = %v, want healthy (gmail's staleness must not leak through an active source filter)", health["state"])
	}
	if health["sources"] != "fresh" {
		t.Fatalf("health.sources = %v, want fresh (only imessage, itself fresh, is in scope under source=imessage)", health["sources"])
	}
}

// TestFiltersContextMemoryHealthExcludesFilteredSource is the health-banner
// pin's context_memory analog.
func TestFiltersContextMemoryHealthExcludesFilteredSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-30*time.Hour))
	seedSyncStatus(t, cfg, "imessage", time.Now().Add(-1*time.Hour))
	writeFilterMemory(t, cfg, "ctx-health-1", "imessage", "", time.Now().Format(time.RFC3339), "ctxhealthgap content")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"query":"ctxhealthgap","source":"imessage"}`)
	health, ok := sc["health"].(map[string]any)
	if !ok {
		t.Fatalf("context_memory must always carry a health object: %v", payloadKeys(sc))
	}
	if health["state"] != "healthy" || health["sources"] != "fresh" {
		t.Fatalf("health = %v, want state=healthy/sources=fresh (gmail's staleness excluded by the active source filter)", health)
	}
}

// --- composite source grammar (malformed, not just unknown) ----------------

// TestFiltersSourceEmptyInstanceErrors pins fail-closed behavior on
// "gmail:" — a KNOWN family with an EMPTY instance after the colon. This is
// structurally malformed (not a legitimate zero-match instance selector like
// "gmail:doesnotexist" — see TestFiltersSourceKnownFamilyUnknownInstanceIsWellFormedZeroMatch)
// and must never be silently accepted or silently degraded to a family-only
// match.
func TestFiltersSourceEmptyInstanceErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "grammar-1", "gmail", "", "2026-07-01T00:00:00Z", "grammarcheck content")
	mustRebuild(t, cfg)

	text, isErr := filtersToolError(t, "search_memory", `{"query":"grammarcheck","source":"gmail:"}`)
	if !isErr {
		t.Fatalf(`source="gmail:" (empty instance) must be a tool error (fail closed); got isError=false, text=%s`, text)
	}
}

// TestFiltersSourceMultiColonErrors pins fail-closed behavior on
// "gmail:work:extra" — the family:instance grammar is a SINGLE colon; a
// multi-colon composite has no defined meaning and must never be silently
// accepted (e.g. by ignoring everything after the second colon).
func TestFiltersSourceMultiColonErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "grammar-2", "gmail", "work", "2026-07-01T00:00:00Z", "grammarcheck2 content")
	mustRebuild(t, cfg)

	text, isErr := filtersToolError(t, "search_memory", `{"query":"grammarcheck2","source":"gmail:work:extra"}`)
	if !isErr {
		t.Fatalf(`source="gmail:work:extra" (multi-colon) must be a tool error (fail closed); got isError=false, text=%s`, text)
	}
}

// TestFiltersContextMemorySourceEmptyInstanceErrors /
// MultiColonErrors mirror the two composite-grammar pins for context_memory.
func TestFiltersContextMemorySourceEmptyInstanceErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-grammar-1", "gmail", "", "2026-07-01T00:00:00Z", "ctxgrammarcheck content")
	mustRebuild(t, cfg)

	text, isErr := filtersToolError(t, "context_memory", `{"query":"ctxgrammarcheck","source":"gmail:"}`)
	if !isErr {
		t.Fatalf(`source="gmail:" (empty instance) must be a tool error (fail closed); got isError=false, text=%s`, text)
	}
}

func TestFiltersContextMemorySourceMultiColonErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-grammar-2", "gmail", "work", "2026-07-01T00:00:00Z", "ctxgrammarcheck2 content")
	mustRebuild(t, cfg)

	text, isErr := filtersToolError(t, "context_memory", `{"query":"ctxgrammarcheck2","source":"gmail:work:extra"}`)
	if !isErr {
		t.Fatalf(`source="gmail:work:extra" (multi-colon) must be a tool error (fail closed); got isError=false, text=%s`, text)
	}
}

// --- subscribed shared corpus (share.go's union with local results) -------
//
// defaultSearchForMCP fuses LOCAL results with every subscribed share
// corpus's own results (unionSharedResults -> searchSharedCorpora ->
// searchShareIndex) BEFORE returning from search_memory/context_memory. The
// frozen "source=imessage can never return gmail" / pre-rank pins apply to
// this FINAL fused set, not just the local half — a subscribed corpus is
// part of what the caller actually receives. These tests build a REAL
// share subscription via the SAME fixture helpers share_gen_test.go/
// share_test.go already establish (writeTestIdentity/registerSub/publishGen/
// buildShareRepoFixture), matching this subsystem's own test convention of
// calling searchSharedCorpora directly rather than only through MCP
// dispatch — searchSharedCorpora itself is not an MCP tool.

// TestFiltersSharedCorpusExcludesFilteredSource pins that a subscribed
// corpus's own gmail memory is excluded by an active source=imessage filter,
// exactly like a local one would be. The query term ("shforlanx") appears in
// NO local memory (an empty vault, only the subscription is seeded), so the
// local arm contributes nothing — any result MUST have come from the shared
// arm, isolating its filter behavior. A POSITIVE CONTROL runs first
// (unfiltered): without it, the exclusion assertion below would pass
// trivially even if the shared arm returned nothing at all — the control
// proves the shared arm genuinely surfaces this memory absent a filter,
// so its absence WITH the filter is meaningful.
func TestFiltersSharedCorpusExcludesFilteredSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "sharefiltertest")

	gmailShared := Memory{
		ID: "share-gmail-1", Scope: "project:acme", Type: "email", Title: "share-gmail-1",
		Provider: "gmail", CreatedAt: "2026-07-01T00:00:00Z", Text: "shforlanx shared gmail body",
	}
	publishGen(t, cfg, "sharefiltertest", id, []Memory{gmailShared})

	// Positive control: unfiltered, the shared arm must actually surface this
	// memory (proves the fixture and the shared-corpus plumbing genuinely
	// work, so the exclusion below is not a vacuous "found nothing either
	// way" pass).
	unfiltered := filtersStructured(t, "search_memory", `{"query":"shforlanx"}`)
	unfilteredResults, _ := unfiltered["results"].([]any)
	unfilteredIDs := filterResultIDs(t, unfilteredResults)
	if !containsID(unfilteredIDs, "share-gmail-1") {
		t.Fatalf("positive control: unfiltered search_memory must surface the subscribed corpus's gmail memory, got %v", unfilteredIDs)
	}

	sc := filtersStructured(t, "search_memory", `{"query":"shforlanx","source":"imessage"}`)
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if containsID(ids, "share-gmail-1") {
		t.Fatalf("source=imessage must exclude a subscribed corpus's gmail memory, got %v", ids)
	}
}

// TestFiltersSharedCorpusPreRankCrowdingProof is the shared-arm analog of
// TestFiltersHybridFTSArmPreRankProof: 51 gmail decoys in a SUBSCRIBED
// corpus (nothing locally matches this query at all, so the local arm
// contributes zero candidates — the ONLY way the target can surface is via
// the shared arm's own pre-rank filtering) rank far above 1 weak imessage
// match, also in the shared corpus. With limit=1, a naive "rank first inside
// the shared index's own LIMIT, filter the returned page after"
// implementation returns nothing once filtered; pre-rank semantics (the SQL
// WHERE predicate inside searchShareIndex, share.go) still surface it.
func TestFiltersSharedCorpusPreRankCrowdingProof(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "sharecrowdtest")

	var mems []Memory
	for i := 0; i < 51; i++ {
		mems = append(mems, Memory{
			ID: fmt.Sprintf("share-decoy-%02d", i), Scope: "project:acme", Type: "email",
			Title: fmt.Sprintf("share-decoy-%02d", i), Provider: "gmail",
			CreatedAt: fmt.Sprintf("2026-06-%02dT00:00:00Z", (i%28)+1),
			Text:      "shquorlex shquorlex shquorlex decoy filler",
		})
	}
	mems = append(mems, Memory{
		ID: "share-target", Scope: "project:acme", Type: "imessage", Title: "share-target",
		Provider: "imessage", CreatedAt: "2026-07-05T00:00:00Z", Text: "a lone shquorlex mention",
	})
	publishGen(t, cfg, "sharecrowdtest", id, mems)

	sc := filtersStructured(t, "search_memory", `{"query":"shquorlex","source":"imessage","limit":1}`)
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if !containsID(ids, "share-target") {
		t.Fatalf("shared-arm pre-rank filter: want share-target surfaced past 51 gmail decoys crowding the shared arm's limit=1; got %v", ids)
	}
}

// --- since_hours overflow (fail closed, never silently disabled/inverted,
//     and NEVER an invented product ceiling) -------------------------------
//
// since_hours ultimately feeds time.Duration(hours) * time.Hour — a
// pathological caller-supplied value large enough to overflow int64
// nanoseconds would wrap to an unpredictable (possibly negative) duration,
// silently disabling or inverting the filter (the worst failure mode: a
// caller who asked to narrow the result set instead gets an UNFILTERED or
// backwards-filtered one with no error). The chosen behavior is FAIL CLOSED
// at the TRUE arithmetic boundary — floor(math.MaxInt64 / int64(time.Hour))
// ≈ 2,562,047 hours (~292 years), search_filters.go's maxSinceHours — never
// a smaller invented "product policy" ceiling: issue #241 specifies
// since_hours as any positive integer, so a large-but-representable value
// (e.g. 100000 hours, ~11.4 years) MUST stay valid and behave correctly.

// TestFiltersSearchMemorySinceHoursOverflowErrors pins fail-closed behavior
// on an out-of-range since_hours large enough to overflow the
// hours*time.Hour nanosecond conversion — including a value exactly ONE
// past the true derived boundary (maxSinceHours+1), not just grossly
// oversized inputs.
func TestFiltersSearchMemorySinceHoursOverflowErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "overflow-1", "gmail", "", "2026-07-01T00:00:00Z", "overflowcheck content")
	mustRebuild(t, cfg)

	for _, bad := range []string{`1e15`, `9223372036854775807`, `2562048`} {
		args := fmt.Sprintf(`{"query":"overflowcheck","since_hours":%s}`, bad)
		text, isErr := filtersToolError(t, "search_memory", args)
		if !isErr {
			t.Fatalf("since_hours=%s (beyond the true int64-nanosecond boundary) must be a tool error (fail closed); got isError=false, text=%s", bad, text)
		}
	}
}

// TestFiltersSearchMemorySinceHoursLargeButSafeIsAccepted is the boundary's
// other half — the NO-INVENTED-CEILING pin: 100000 hours (~11.4 years) is
// mathematically safe (nowhere near the ~2.56M-hour true boundary) and MUST
// be accepted and filter correctly, proving the fail-closed check above is
// derived from the actual arithmetic, not an arbitrary smaller product
// ceiling.
func TestFiltersSearchMemorySinceHoursLargeButSafeIsAccepted(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	old := time.Now().Add(-200000 * time.Hour) // outside a 100000h window
	recent := time.Now().Add(-1 * time.Hour)   // inside it
	writeFilterMemory(t, cfg, "largesafe-old", "gmail", "", old.Format(time.RFC3339), "largesafecheck content old")
	writeFilterMemory(t, cfg, "largesafe-recent", "gmail", "", recent.Format(time.RFC3339), "largesafecheck content recent")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"largesafecheck","since_hours":100000}`)
	results, _ := sc["results"].([]any)
	ids := filterResultIDs(t, results)
	if !containsID(ids, "largesafe-recent") {
		t.Fatalf("since_hours=100000 (safe, large) must accept the request and include the in-window memory, got %v", ids)
	}
	if containsID(ids, "largesafe-old") {
		t.Fatalf("since_hours=100000 must still exclude the out-of-window memory (200000h old), got %v", ids)
	}
}

// TestFiltersContextMemorySinceHoursOverflowErrors mirrors the overflow pin
// for context_memory.
func TestFiltersContextMemorySinceHoursOverflowErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeFilterMemory(t, cfg, "ctx-overflow-1", "gmail", "", "2026-07-01T00:00:00Z", "ctxoverflowcheck content")
	mustRebuild(t, cfg)

	text, isErr := filtersToolError(t, "context_memory", `{"query":"ctxoverflowcheck","since_hours":1e15}`)
	if !isErr {
		t.Fatalf("since_hours=1e15 (overflow range) must be a tool error (fail closed); got isError=false, text=%s", text)
	}
}

// --- excluded_by_filter explicit marker (issue #241 acceptance: "Health/
// confidence output distinguishes excluded_by_filter from
// unavailable/unhealthy sources") -------------------------------------------
//
// Omission from missing_sources/health alone leaves it ambiguous whether an
// absent source is healthy or was excluded by the caller's own filter — the
// issue asks the output to DISTINGUISH the two, not just hide one. A new
// top-level "excluded_by_filter" array (sibling to "filters"/"confidence"/
// "health", never nested inside confidence's frozen shape) lists every
// enabled connector instance an active source filter excluded, explicit and
// machine-readable — present only when the source filter is active AND
// excludes at least one enabled source.

// TestFiltersSearchMemoryExcludedByFilterMarker pins the explicit marker on
// search_memory: gmail (enabled) is excluded by source=imessage and must be
// named in "excluded_by_filter", not just silently absent from
// confidence.missing_sources.
func TestFiltersSearchMemoryExcludedByFilterMarker(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", time.Now().Add(-1*time.Hour))
	writeFilterMemory(t, cfg, "excl-1", "imessage", "", time.Now().Format(time.RFC3339), "excludedmarker content")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"excludedmarker","source":"imessage"}`)
	excl, ok := sc["excluded_by_filter"].([]any)
	if !ok {
		t.Fatalf("search_memory with an active source filter that excludes an enabled source must carry excluded_by_filter: %v", payloadKeys(sc))
	}
	if len(excl) != 1 || excl[0] != "gmail" {
		t.Fatalf("excluded_by_filter = %v, want exactly [\"gmail\"]", excl)
	}
}

// TestFiltersSearchMemoryExcludedByFilterAbsentWhenNoExclusion pins the
// omission half: with only imessage enabled, source=imessage excludes
// nothing, so the key must not appear at all (never an empty array — the
// frozen "no key when nothing to report" pattern this codebase uses
// throughout, e.g. results_truncated/shares_unhealthy).
func TestFiltersSearchMemoryExcludedByFilterAbsentWhenNoExclusion(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "imessage")
	seedSyncStatus(t, cfg, "imessage", time.Now().Add(-1*time.Hour))
	writeFilterMemory(t, cfg, "excl-2", "imessage", "", time.Now().Format(time.RFC3339), "noexclusion content")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "search_memory", `{"query":"noexclusion","source":"imessage"}`)
	if _, ok := sc["excluded_by_filter"]; ok {
		t.Fatalf("excluded_by_filter must be absent when the filter excludes no enabled source: %v", payloadKeys(sc))
	}
}

// TestFiltersContextMemoryExcludedByFilterMarker mirrors the explicit-marker
// pin for context_memory.
func TestFiltersContextMemoryExcludedByFilterMarker(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", time.Now().Add(-1*time.Hour))
	writeFilterMemory(t, cfg, "ctx-excl-1", "imessage", "", time.Now().Format(time.RFC3339), "ctxexcludedmarker content")
	mustRebuild(t, cfg)

	sc := filtersStructured(t, "context_memory", `{"query":"ctxexcludedmarker","source":"imessage"}`)
	excl, ok := sc["excluded_by_filter"].([]any)
	if !ok || len(excl) != 1 || excl[0] != "gmail" {
		t.Fatalf("excluded_by_filter = %v (ok=%v), want exactly [\"gmail\"]", excl, ok)
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
