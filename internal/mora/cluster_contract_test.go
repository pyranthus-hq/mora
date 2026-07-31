package mora

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// Issue #237 — corroborating-record clustering. Contract only; NO implementation
// lives in this file or anywhere else in this commit. Every "contract" test below
// is expected to be RED today (it asserts a "corroborating" key/shape that does
// not exist in the codebase yet); every "pin" test is expected to be GREEN today
// (it captures CURRENT behavior as a regression guard for the future
// implementation). Each test's doc comment says which it is.
//
// ---------------------------------------------------------------------------
// FROZEN INTERFACE (the contract a future implementation must satisfy)
// ---------------------------------------------------------------------------
//
// A search_memory result entry MAY gain a "corroborating" array. When present,
// that entry is a CLUSTER HEAD and "corroborating" holds compact refs to the
// other records the vault believes describe the SAME real-world event:
//
//	{"id": "...", "title": "...", "source": "...", "created_at": "..."}
//
// exactly those four keys (id/title/source/created_at — reusing Memory's own
// JSON field names), nothing more. A member's "id" is the member's Memory.ID
// verbatim (stable citation — an agent can read_memory it unchanged).
//
// A cluster consumes exactly ONE result slot (the head); the slots its members
// would otherwise have occupied are freed for other, genuinely distinct
// memories. Nothing is hidden: every member is still reachable, either as the
// head or inside "corroborating".
//
// MINIMAL ANCHOR RULES (deliberately small — two rules, OR'd; clusters are the
// connected components of the "linked" relation below, so a hub record, e.g. a
// calendar event, can transitively corroborate two leaf records that share no
// direct anchor with each other):
//
//	Rule 1 — PROVIDER ANCHOR EQUALITY: two memories link when both carry a
//	non-empty ProviderID and (Provider, ProviderID) is identical (e.g. the same
//	Gmail thread or calendar event ingested twice, once per account). Cheap:
//	one field-equality check, no parsing.
//
//	Rule 2 — PERSON-ENTITY + TIME-WINDOW OVERLAP: two memories link when
//	  (a) their participant-identity sets intersect, where a memory's identity
//	      set is the lowercased, trimmed union of Meta["from"], Meta["to"],
//	      Meta["cc"], Meta["attendees"], Meta["organizer"], and the "handle" of
//	      each Meta["participants"] pair (exactly the fields personRefs already
//	      reads in graph.go — no new extraction); AND
//	  (b) their real-world instants — Meta["occurred_at"] if present, else
//	      CreatedAt (the same fallback itemOccurredAt already uses in urgent.go)
//	      — are within clusterContractWindow (24h) of each other.
//	Both cheap: existing Meta fields, no new connector work, no embedding calls.
//
// DETERMINISM (the tie-break this contract pins):
//
//	HEAD = the cluster member ranked best by the search that produced the
//	candidate pool (i.e. the member that would have appeared earliest in
//	TODAY's unclustered ranked list) — ties broken by the id-ascending
//	tie-break searchMemories/ftsSearchIDs/hybridSearchTrace already apply, so
//	clustering introduces no new tie-break machinery.
//
//	MEMBER ORDER inside "corroborating" mirrors that same relative rank order
//	(head excluded).
//
// A query that touches no cluster at all must produce output BYTE-IDENTICAL to
// today's (no "corroborating" key anywhere in the envelope) — clustering must
// be a pure no-op off its own trigger condition.
//
// SCOPING NOTE: the "corroborating" JSON shape is an MCP search_memory
// presentation concern. The hybrid-path coverage below (TestClusterContract
// HybridSeamPreTruncate) therefore checks the same "one slot per cluster"
// property at the raw []Memory / id level directly against hybridSearch /
// hybridSearchTrace (the post-fusion/pre-truncate seam), rather than re-running
// the full MCP JSON-shape assertions a second time — per the DAG task's
// explicit allowance that "a unit-level seam test at the post-fusion/
// pre-truncate stage is acceptable" for the hybrid path.
//
// clusterContractWindow is this contract's frozen time-window constant. It is a
// TEST-ONLY constant (no production code exists yet to import it from); a real
// implementation should expose the equivalent value from production code, and
// this constant's value (24h) is what these tests fixture against.
const clusterContractWindowHours = 24

// ---------------------------------------------------------------------------
// Fixture: one real-world event (a Q3 planning review) corroborated by a
// calendar event + a Gmail thread + an iMessage conversation, plus five
// distractor memories that share lexical terms with the query ("Q3",
// "Planning") but belong to different people and/or fall outside the time
// window, so they must NOT cluster with the review. All identities are
// synthetic (@example.com, generic first names) and all data is
// programmatically seeded — no testdata files, no real people.
//
// The three corroborating records and their anchors:
//
//	gcal_primary/evt-q3review   (event)  attendees=[alice,bob]      2026-05-10T15:00Z
//	gmail_thread/th-q3review    (email)  from=alice to=bob          2026-05-09T18:00Z
//	imessage/chat-q3review      (message) participant=bob            2026-05-10T16:30Z
//
// No single pair shares BOTH endpoints, but all three fall inside one 24h
// window and pairwise share at least one of {alice, bob} — Rule 2 links
// event<->email (alice) and event<->imessage (bob), and the connected
// component (via the event as hub) includes all three.
//
// Measured (2026-07-31, this repo, static embedder, FTS-only path) rank order
// for the query "Q3 Planning Review", verified by direct execution against
// searchMemories/hybridSearch on this exact fixture:
//
//	#0 gmail_thread/th-q3review   (cluster)
//	#1 gcal_primary/evt-q3review  (cluster)
//	#2 imessage/chat-q3review     (cluster)
//	#3 note/q3-retro              (distractor)
//	#4 imessage/chat-q3launch     (distractor)
//	#5 gcal_primary/evt-q3budget  (distractor)
//	#6 gmail_thread/th-q3sales    (distractor)
//	#7 note/offsite-planning      (distractor)
//
// so with limit=5 TODAY's unclustered top-5 is 3 cluster hits + 2 distractors
// (note/q3-retro, imessage/chat-q3launch) — the bug. Once clustered, the head
// (gmail_thread/th-q3review, best-ranked) should consume 1 slot, freeing 2 more
// for the next-best distractors (gcal_primary/evt-q3budget, gmail_thread/th-
// q3sales), for 5 total distinct results.
func seedClusterFixture(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	seed := func(id, typ, title, source, providerID, createdAt string, meta map[string]any, text string) {
		t.Helper()
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "personal", Type: typ, Title: title, Source: source,
			ProviderID: providerID, CreatedAt: createdAt, Meta: meta, Text: text,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// The three corroborating records (one real-world event).
	seed("gcal_primary/evt-q3review", "event", "Q3 Planning Review", "calendar", "primary/evt-q3review",
		"2026-05-08T12:00:00Z",
		map[string]any{
			"occurred_at": "2026-05-10T15:00:00Z",
			"attendees":   []string{"alice@example.com", "bob@example.com"},
			"organizer":   "alice@example.com",
		},
		"Quarterly planning review to align on Q3 roadmap priorities and budget allocation across teams.")

	seed("gmail_thread/th-q3review", "email", "Re: Q3 Planning Review - prep notes", "gmail", "thread/th-q3review",
		"2026-05-09T18:05:00Z",
		map[string]any{
			"occurred_at": "2026-05-09T18:00:00Z",
			"from":        []string{"alice@example.com"},
			"to":          []string{"bob@example.com"},
		},
		"Sending the Q3 planning review prep notes and budget allocation numbers ahead of the review.")

	seed("imessage/chat-q3review", "message", "Q3 Planning Review follow-up", "imessage", "imessage/chat-q3review",
		"2026-05-10T16:35:00Z",
		map[string]any{
			"occurred_at":  "2026-05-10T16:30:00Z",
			"participants": []map[string]string{{"handle": "bob@example.com", "name": "Bob Rivera"}},
		},
		"Great Q3 planning review today, sending over the budget allocation follow up numbers we discussed.")

	// Distractors: share lexical terms but different people / different real
	// events / outside the window — must remain distinct after clustering.
	seed("gmail_thread/th-q3sales", "email", "Q3 Sales Planning Sync", "gmail", "thread/th-q3sales",
		"2026-04-01T00:00:00Z",
		map[string]any{"occurred_at": "2026-04-01T00:00:00Z", "from": []string{"carol@example.com"}, "to": []string{"dave@example.com"}},
		"Quick sync on Q3 sales planning targets and territory splits.")

	seed("note/offsite-planning", "note", "Team Offsite Planning Notes", "filesystem", "",
		"2026-04-05T00:00:00Z", nil,
		"Planning logistics for the fall offsite: venue, catering, and travel.")

	seed("gcal_primary/evt-q3budget", "event", "Q3 Budget Planning Kickoff", "calendar", "primary/evt-q3budget",
		"2026-04-10T00:00:00Z",
		map[string]any{"occurred_at": "2026-04-10T09:00:00Z", "attendees": []string{"eve@example.com", "frank@example.com"}, "organizer": "eve@example.com"},
		"Kickoff for Q3 budget planning cycle across finance and ops.")

	seed("imessage/chat-q3launch", "message", "Q3 Launch Planning Chat", "imessage", "imessage/chat-q3launch",
		"2026-04-15T00:00:00Z",
		map[string]any{"occurred_at": "2026-04-15T00:00:00Z", "participants": []map[string]string{{"handle": "grace@example.com", "name": "Grace Lin"}}},
		"Planning drinks after the Q3 launch wraps up.")

	seed("note/q3-retro", "note", "Q3 Planning Retro Notes", "filesystem", "",
		"2026-04-20T00:00:00Z", nil,
		"Retro notes from Q3 planning process improvements and team feedback.")

	ctx := context.Background()
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	return cfg
}

const clusterQuery = "Q3 Planning Review"

// clusterHeadID / clusterMemberIDs / freedDistractorIDs are the fixture's
// measured constants (see the doc comment above seedClusterFixture).
const clusterHeadID = "gmail_thread/th-q3review"

var clusterMemberIDsInOrder = []string{"gcal_primary/evt-q3review", "imessage/chat-q3review"}
var clusterAllIDs = []string{clusterHeadID, "gcal_primary/evt-q3review", "imessage/chat-q3review"}
var freedDistractorIDs = []string{"note/q3-retro", "imessage/chat-q3launch", "gcal_primary/evt-q3budget", "gmail_thread/th-q3sales"}

// structuredPayload unwraps a tools/call CallToolResult envelope (as returned
// by mcpResult: {content, isError, structuredContent}) down to the tool's own
// object-shaped payload — the mirror toCallToolResult ships for object-shaped
// results (mora-mcp-contract §1 rule 4). search_memory's payload lives there.
func structuredPayload(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent object in CallToolResult: %#v", res)
	}
	return sc
}

// resultRows extracts the "results" array of a search_memory MCP envelope as
// plain map[string]any rows — deliberately untyped (no new Go types), per the
// DAG task's architecture note that contract tests assert on marshaled JSON.
func resultRows(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	payload := structuredPayload(t, res)
	raw, ok := payload["results"].([]any)
	if !ok {
		t.Fatalf("results is not an array (or missing): %#v", payload["results"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("result row is not an object: %#v", r)
		}
		out = append(out, m)
	}
	return out
}

func rowID(t *testing.T, row map[string]any) string {
	t.Helper()
	id, ok := row["id"].(string)
	if !ok {
		t.Fatalf("row has no string id: %#v", row)
	}
	return id
}

// envelopeContainsKey reports whether key appears ANYWHERE in the raw JSON text
// of the search_memory envelope (used for the byte-identity / nothing-new pin).
func envelopeContainsKey(t *testing.T, res map[string]any, key string) bool {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return strings.Contains(string(b), `"`+key+`"`)
}

// ---------------------------------------------------------------------------
//  1. BUG REPRODUCTION — currently PASSES. Proves that today search_memory
//     returns all three corroborating records of the Q3 review as independent
//     hits, consuming 3 of the top 5 slots that should hold 5 DISTINCT
//     real-world things.
//
// ---------------------------------------------------------------------------
func TestClusterBugRepro_CorroboratingRecordsOccupyMultipleSlots(t *testing.T) {
	seedClusterFixture(t)

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+clusterQuery+`","limit":5}`))
	rows := resultRows(t, res)
	if len(rows) != 5 {
		t.Fatalf("expected 5 raw results at limit=5, got %d: %#v", len(rows), rows)
	}

	seen := map[string]bool{}
	for _, row := range rows {
		seen[rowID(t, row)] = true
	}
	var clusterHitsInTop5 int
	for _, id := range clusterAllIDs {
		if seen[id] {
			clusterHitsInTop5++
		}
	}
	// THE BUG: all three corroborating records of the SAME real-world event
	// independently occupy top-5 slots today.
	if clusterHitsInTop5 != 3 {
		t.Fatalf("bug reproduction failed to reproduce: expected all 3 corroborating records "+
			"(%v) to occupy independent top-5 slots today, but only %d did — got rows %v. "+
			"If this test now fails, EITHER the fixture ranking drifted (re-measure and update "+
			"the constants above) OR clustering has already landed (this test should then be "+
			"deleted, not fixed).", clusterAllIDs, clusterHitsInTop5, rows)
	}
	// And critically: they appear as bare independent rows, no "corroborating" key.
	if envelopeContainsKey(t, res, "corroborating") {
		t.Fatalf("unexpected corroborating key present today — bug repro assumption invalidated")
	}
}

// ---------------------------------------------------------------------------
//  2. CONTRACT — one cluster, one slot, on the FTS-only path (search_memory /
//     MCP end-to-end, default static embedder => defaultSearch routes through
//     searchMemories, NOT hybridSearch). RED today: today's output has no
//     "corroborating" key and 3 independent cluster rows instead of 1 head.
//
// ---------------------------------------------------------------------------
func TestClusterContractFTSOnly_OneClusterOneSlot(t *testing.T) {
	seedClusterFixture(t)

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+clusterQuery+`","limit":5}`))
	rows := resultRows(t, res)

	if len(rows) != 5 {
		t.Fatalf("expected 5 distinct results at limit=5 (1 cluster head + 4 freed distractors), got %d: %v",
			len(rows), rows)
	}

	// Exactly one top-level row is a member of the Q3-review cluster (the head);
	// the other two members must NOT appear as bare top-level rows.
	var headRow map[string]any
	clusterRowsSeen := 0
	for _, row := range rows {
		id := rowID(t, row)
		for _, cid := range clusterAllIDs {
			if id == cid {
				clusterRowsSeen++
				headRow = row
			}
		}
	}
	if clusterRowsSeen != 1 {
		t.Fatalf("expected exactly 1 of the 3 corroborating records as a top-level result "+
			"(the cluster head), got %d — rows: %v", clusterRowsSeen, rows)
	}
	if headRow == nil {
		t.Fatal("no cluster head row found")
	}
	if id := rowID(t, headRow); id != clusterHeadID {
		t.Fatalf("cluster head = %s, want %s (best-ranked member, per the frozen tie-break)", id, clusterHeadID)
	}

	// The 4 freed slots must be filled by the next-best DISTINCT distractors —
	// not by re-showing cluster members, and not left empty.
	var otherIDs []string
	for _, row := range rows {
		id := rowID(t, row)
		if id != clusterHeadID {
			otherIDs = append(otherIDs, id)
		}
	}
	sort.Strings(otherIDs)
	wantOthers := append([]string(nil), freedDistractorIDs...)
	sort.Strings(wantOthers)
	if len(otherIDs) != len(wantOthers) {
		t.Fatalf("expected %d freed distractor slots, got %d: %v", len(wantOthers), len(otherIDs), otherIDs)
	}
	for i := range otherIDs {
		if otherIDs[i] != wantOthers[i] {
			t.Fatalf("freed distractor slots = %v, want %v", otherIDs, wantOthers)
		}
	}

	// The head's corroborating array: exact keys, verbatim ids, frozen order.
	corr, ok := headRow["corroborating"].([]any)
	if !ok {
		t.Fatalf("cluster head %s has no corroborating array: %#v", clusterHeadID, headRow)
	}
	if len(corr) != len(clusterMemberIDsInOrder) {
		t.Fatalf("corroborating has %d members, want %d (%v)", len(corr), len(clusterMemberIDsInOrder), clusterMemberIDsInOrder)
	}
	for i, raw := range corr {
		ref, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("corroborating[%d] is not an object: %#v", i, raw)
		}
		var keys []string
		for k := range ref {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		wantKeys := []string{"created_at", "id", "source", "title"}
		if len(keys) != len(wantKeys) {
			t.Fatalf("corroborating[%d] keys = %v, want exactly %v", i, keys, wantKeys)
		}
		for j := range keys {
			if keys[j] != wantKeys[j] {
				t.Fatalf("corroborating[%d] keys = %v, want exactly %v", i, keys, wantKeys)
			}
		}
		gotID, _ := ref["id"].(string)
		if gotID != clusterMemberIDsInOrder[i] {
			t.Fatalf("corroborating[%d].id = %q, want %q (verbatim, frozen rank order) — got array %v",
				i, gotID, clusterMemberIDsInOrder[i], corr)
		}
	}

	// No result row other than the head may carry a corroborating key at all.
	for _, row := range rows {
		if rowID(t, row) == clusterHeadID {
			continue
		}
		if _, present := row["corroborating"]; present {
			t.Fatalf("non-head row %s unexpectedly carries a corroborating key", rowID(t, row))
		}
	}
}

// ---------------------------------------------------------------------------
//  3. CONTRACT — deterministic: same query run twice yields byte-identical
//     envelopes (head selection + member order are stable, not just "correct
//     once"). This assertion is written against the SAME query/limit as test 2
//     and will start failing the moment clustering exists non-deterministically;
//     today it degenerates to "today's unclustered output is deterministic",
//     which already holds (see TestHybridDeterministic) — included here so the
//     determinism requirement is pinned in the SAME place as the shape contract.
//
// ---------------------------------------------------------------------------
func TestClusterContractDeterministic(t *testing.T) {
	seedClusterFixture(t)
	line := budgetCall("search_memory", `{"query":"`+clusterQuery+`","limit":5}`)

	a := mcpResult(t, line)
	b := mcpResult(t, line)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("nondeterministic clustered envelope:\nA=%s\nB=%s", ab, bb)
	}
}

// ---------------------------------------------------------------------------
//  4. CONTRACT — Rule 1 (provider anchor equality) fires independently of
//     Rule 2. Two memories share an identical (Provider, ProviderID) anchor but
//     have DISJOINT participants and occurred_at more than clusterContract
//     WindowHours apart — Rule 2 alone would not link them. RED today.
//
// ---------------------------------------------------------------------------
func TestClusterContractProviderAnchorEquality(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	seed := func(id, providerID, createdAt string, attendees []string, occurredAt string) {
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "personal", Type: "event", Title: "Zephyrion Standing Sync",
			Source: "calendar", ProviderID: providerID, CreatedAt: createdAt,
			Meta: map[string]any{"occurred_at": occurredAt, "attendees": attendees, "organizer": attendees[0]},
			Text: "Zephyrion standing sync notes and action items.",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// Same anchor (duplicate ingestion via two accounts), disjoint people, >24h apart.
	seed("gcal_a/evt-zephyrion", "primary/evt-zephyrion", "2026-06-01T00:00:00Z",
		[]string{"alice@example.com", "bob@example.com"}, "2026-06-01T09:00:00Z")
	seed("gcal_b/evt-zephyrion-dup", "primary/evt-zephyrion", "2026-06-05T00:00:00Z",
		[]string{"carol@example.com", "dave@example.com"}, "2026-06-05T09:00:00Z")

	ctx := context.Background()
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	res := mcpResult(t, budgetCall("search_memory", `{"query":"Zephyrion","limit":5}`))
	rows := resultRows(t, res)
	if len(rows) != 1 {
		t.Fatalf("Rule 1 (provider anchor equality) contract: expected 1 clustered result "+
			"(same ProviderID => same anchor, independent of participants/time-window), got %d: %v",
			len(rows), rows)
	}
	corr, ok := rows[0]["corroborating"].([]any)
	if !ok || len(corr) != 1 {
		t.Fatalf("Rule 1 head missing corroborating member: %#v", rows[0])
	}
	ref, _ := corr[0].(map[string]any)
	if got, _ := ref["id"].(string); got != "gcal_b/evt-zephyrion-dup" && got != "gcal_a/evt-zephyrion" {
		t.Fatalf("Rule 1 corroborating id not verbatim: %#v", ref)
	}
}

// ---------------------------------------------------------------------------
//  5. PIN — a query matching no cluster at all must remain byte-identical to
//     today: no "corroborating" key appears ANYWHERE in the envelope. Passes
//     today (no such key exists yet); guards the "clustering is a no-op off its
//     trigger condition, nothing hidden" requirement for whoever implements it.
//
// ---------------------------------------------------------------------------
func TestClusterContractNoClusterByteIdentical(t *testing.T) {
	seedClusterFixture(t)

	// "offsite" only matches note/offsite-planning — a singleton, no anchor
	// shared with anything else in the fixture.
	res := mcpResult(t, budgetCall("search_memory", `{"query":"offsite logistics","limit":5}`))
	rows := resultRows(t, res)
	if len(rows) != 1 || rowID(t, rows[0]) != "note/offsite-planning" {
		t.Fatalf("expected exactly the singleton offsite note, got %v", rows)
	}
	if envelopeContainsKey(t, res, "corroborating") {
		t.Fatal("a query touching no cluster must never surface a corroborating key")
	}

	// Byte-identical across repeated calls too (no incidental nondeterminism).
	line := budgetCall("search_memory", `{"query":"offsite logistics","limit":5}`)
	a := mcpResult(t, line)
	b := mcpResult(t, line)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("nondeterministic no-cluster envelope:\nA=%s\nB=%s", ab, bb)
	}
}

// ---------------------------------------------------------------------------
//  6. PIN — the clustered fixture's search_memory envelope stays comfortably
//     under the T0 gate's 8000-token search_memory ceiling
//     (mora_mcp_budget_test.go, budgetCases()). Passes today (a small 8-memory
//     fixture is nowhere near the ceiling even pre-clustering); guards against
//     a future implementation that inflates the envelope past budget by, e.g.,
//     embedding full corroborating bodies instead of compact refs.
//
// ---------------------------------------------------------------------------
func TestClusterContractBudgetUnderCeiling(t *testing.T) {
	seedClusterFixture(t)
	line := budgetCall("search_memory", `{"query":"`+clusterQuery+`","limit":5}`)
	bytes := measureEnvelope(t, line)
	tok := bytes / charsPerToken
	const searchMemoryCeiling = 8000 // mora_mcp_budget_test.go budgetCases(), tool:"search_memory"
	if tok > searchMemoryCeiling {
		t.Fatalf("clustered search_memory envelope = %d tokens (%d bytes), exceeds the T0 ceiling %d",
			tok, bytes, searchMemoryCeiling)
	}
}

// ---------------------------------------------------------------------------
//  7. CONTRACT (hybrid path, unit-level seam) — the SAME one-slot-per-cluster
//     property must hold when defaultSearch would route through hybridSearch
//     (a semantic embedder). We can't stand up Ollama in a hermetic unit test,
//     so — per this task's explicit allowance — we test the seam directly:
//     hybridSearch/hybridSearchTrace exercise the real FTS+graph fusion machinery
//     (hybrid.go rrfWeighted, truncate) even under the static embedder (only the
//     vector arm is skipped; see embedderIsSemantic in hybrid.go), which is
//     exactly the post-fusion/pre-truncate stage a clustering hook must sit
//     inside. This checks distinctness/slot-consumption at the raw []Memory/id
//     level (no "corroborating" JSON shape here — that shape is an MCP
//     search_memory presentation concern, already pinned end-to-end in test 2).
//     RED today: hybridSearch(limit=5) returns all 3 cluster members
//     independently, same as the FTS-only path.
//
// ---------------------------------------------------------------------------
func TestClusterContractHybridSeamPreTruncate(t *testing.T) {
	cfg := seedClusterFixture(t)
	ctx := context.Background()

	// Precondition: pre-truncate, the fused ranking already contains all 3
	// cluster members (proving the raw material a clustering hook would need
	// is present before the hybrid.go:264-266 truncate-to-limit runs).
	_, tr, err := hybridSearchTrace(ctx, cfg, clusterQuery, "", 5, 60)
	if err != nil {
		t.Fatalf("hybridSearchTrace: %v", err)
	}
	fusedSet := map[string]bool{}
	for _, id := range tr.Fused {
		fusedSet[id] = true
	}
	for _, id := range clusterAllIDs {
		if !fusedSet[id] {
			t.Fatalf("pre-truncate fused ranking missing cluster member %s: %v", id, tr.Fused)
		}
	}

	// Contract: post-truncate (limit=5), only ONE of the 3 members should
	// survive as an independent hit; the freed slots go to distinct memories.
	got, err := hybridSearch(ctx, cfg, clusterQuery, "", 5)
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 distinct results at limit=5, got %d: %v", len(got), idList(got))
	}
	clusterHits := 0
	for _, m := range got {
		for _, cid := range clusterAllIDs {
			if m.ID == cid {
				clusterHits++
			}
		}
	}
	if clusterHits != 1 {
		t.Fatalf("hybrid path: expected exactly 1 independent hit from the 3-member cluster "+
			"(the rest corroborating, not occupying their own slots), got %d — rows: %v",
			clusterHits, idList(got))
	}
}
