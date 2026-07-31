package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// Issue #237 — corroborating-record clustering. Contract only; NO implementation
// lives in this file or anywhere else in this commit. Every "contract" test below
// is expected to be RED today (it asserts a "corroborating" key/shape that does
// not exist in the codebase yet); every "pin" test is expected to be GREEN today
// (it captures CURRENT behavior as a regression guard for the future
// implementation). Each test's doc comment says which it is.
//
// AMENDMENT (integrator decision, #237): this contract narrows Rule 2
// (person-entity + time-window) below. The narrowing removes the CreatedAt
// fallback, tightens the window to a strict inequality, replaces "connected
// components" with a star topology anchored on the cluster head, and adds a
// refusal cap for over-wide candidate clusters. TestClusterBugRepro_
// CorroboratingRecordsOccupyMultipleSlots — the pre-fix repro pin — is
// retired: its own comment said it must be deleted once clustering lands, and
// keeping it would contradict the all-green acceptance requirement for a
// contract test file with no matching implementation in this commit.
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
// MINIMAL ANCHOR RULES (deliberately small — two rules, OR'd):
//
//	Rule 1 — PROVIDER ANCHOR EQUALITY: two memories link when both carry a
//	non-empty ProviderID and (Provider, ProviderID) is identical (e.g. the same
//	Gmail thread or calendar event ingested twice, once per account). Cheap:
//	one field-equality check, no parsing. Rule 1 anchor-equality groups MAY
//	still union transitively (same anchor is genuinely the same event, so A=B
//	and B=C correctly implies A=C).
//
//	Rule 2 — PERSON-ENTITY + TIME-WINDOW OVERLAP (NARROWED): two memories link
//	when
//	  (a) BOTH carry an explicit, non-empty Meta["occurred_at"] — a
//	      real-world-instant anchor. There is NO CreatedAt fallback, ever: a
//	      record missing meta.occurred_at can never link via Rule 2, full
//	      stop. CreatedAt is ingest/write time, not event time — treating it as
//	      an event anchor is a false signal. (Not hypothetical: the old
//	      fallback collapsed a 200-thread same-sender fixture, all sharing one
//	      CreatedAt, into a single giant cluster — see
//	      TestClusterContractNoOccurredAtNoFallback and
//	      TestClusterContractAntiHubRefusalCap below.); AND
//	  (b) their participant-identity sets intersect, where a memory's identity
//	      set is the lowercased, trimmed union of Meta["from"], Meta["to"],
//	      Meta["cc"], Meta["attendees"], Meta["organizer"], and the "handle" of
//	      each Meta["participants"] pair (exactly the fields personRefs already
//	      reads in graph.go — no new extraction); AND
//	  (c) their occurred_at instants are within a STRICT clusterContractWindow
//	      (24h): |Δ occurred_at| < 24h. Exactly 24h apart does NOT cluster —
//	      the boundary is exclusive (see TestClusterContractWindowBoundary).
//	All still cheap: existing Meta fields, no new connector work, no embedding
//	calls.
//
// STAR TOPOLOGY (no transitive closure for Rule 2): a candidate cluster forms
// around a HEAD (see DETERMINISM below). A non-head record joins the head's
// cluster only if it satisfies Rule 1 OR Rule 2 *pairwise with the head
// itself* — not with any other non-head member. Two members that qualify
// with each other but not with the head do NOT both end up in the same
// cluster on that basis alone (see TestClusterContractNoTransitiveChain).
// Rule 1 is the one exception noted above: anchor-equality groups may still
// union transitively, because identical (Provider, ProviderID) is definitional
// identity, not a fuzzy heuristic.
//
// REFUSAL CAP: if a greedy candidate cluster would exceed 5 total members
// (head + corroborating), the ENTIRE candidate is refused — every one of its
// records stays an independent hit; none of them cluster with each other at
// all. This is precision-first: a hub pattern (e.g. one sender fanning out to
// many distinct recipients, all sharing an instant) is a sign the anchor is
// too weak to safely collapse, not evidence of one real-world event (see
// TestClusterContractAntiHubRefusalCap).
//
// GREEDY DETERMINISTIC FORMATION: iterate the ranked result pool in rank order
// (score desc, id asc per the existing tie-break). The strongest unclustered
// result seeds a new cluster as its head; scan the remaining unclustered
// results in the same rank order and attach each one that qualifies against
// THAT head (Rule 1 or Rule 2); if the resulting candidate (head + attached)
// would exceed 5 members, refuse the whole candidate (all revert to
// independent) instead of attaching; otherwise commit it and continue
// iterating past the now-clustered records.
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
// this constant's value (24h, strict/exclusive) is what these tests fixture
// against.
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
// All three fall inside one strict 24h window and, since a memory's identity
// set is the union of its from/to/attendees/organizer/participant fields (not
// scored per-field), each of the two non-head records shares at least one of
// {alice, bob} directly WITH THE HEAD (gmail_thread/th-q3review, whose
// identity set is {alice, bob} — both the sender and the recipient) — so this
// fixture clusters under the star topology (pairwise-with-head) with no
// transitive closure required.
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
//  1. CONTRACT — one cluster, one slot, on the FTS-only path (search_memory /
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
//  2. CONTRACT — deterministic: same query run twice yields byte-identical
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
//  3. CONTRACT — Rule 1 (provider anchor equality) fires independently of
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
//  4. PIN — a query matching no cluster at all must remain byte-identical to
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
//  5. PIN — the clustered fixture's search_memory envelope stays comfortably
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
//  6. CONTRACT (hybrid path, unit-level seam) — the SAME one-slot-per-cluster
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

// ---------------------------------------------------------------------------
//
//  7. CONTRACT — star topology has NO transitive closure for Rule 2. Three
//     memories form a chain: A<->B qualify pairwise (shared person "xavier"),
//     B<->C qualify pairwise (shared person "yolanda"), but A<->C share NO
//     person at all. All three carry occurred_at within the same hour (well
//     inside the 24h window), so identity-set overlap — not time — is the
//     only discriminator. Title/Text/CreatedAt are byte-identical across all
//     three so FTS ties their score, falling back to the id-ascending
//     tie-break searchMemories/hybridSearchTrace already apply (see
//     retrieval_rt_cover_test.go's own "equal fused score => id asc" pin) —
//     no new tie-break machinery; "chain/a-alpha" sorts first, so it is the
//     measured, deterministic HEAD.
//
//     Contract: A qualifies with B pairwise (share xavier, in-window), so B
//     must join A's cluster (RED today — no implementation exists). A does
//     NOT qualify with C directly (no shared identity), so C must NOT join
//     A's cluster even though C qualifies with B — a strictly transitive
//     (connected-components) implementation would wrongly pull C in too; this
//     pins the star-topology narrowing against that regression.
//
// ---------------------------------------------------------------------------
func TestClusterContractNoTransitiveChain(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	const title = "Chain Topology Regression Fixture"
	const text = "Chain topology regression fixture body text for deterministic tie-break testing."
	const createdAt = "2026-06-15T09:00:00Z"

	seed := func(id string, from, to []string, occurredAt string) {
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "personal", Type: "email", Title: title, Source: "gmail",
			CreatedAt: createdAt,
			Meta:      map[string]any{"occurred_at": occurredAt, "from": from, "to": to},
			Text:      text,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// A <-> B share "xavier"; B <-> C share "yolanda"; A <-> C share nothing.
	seed("chain/a-alpha", []string{"wendy@example.com"}, []string{"xavier@example.com"}, "2026-06-15T10:00:00Z")
	seed("chain/b-bridge", []string{"xavier@example.com"}, []string{"yolanda@example.com"}, "2026-06-15T11:00:00Z")
	seed("chain/c-gamma", []string{"yolanda@example.com"}, []string{"zack@example.com"}, "2026-06-15T12:00:00Z")

	ctx := context.Background()
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+title+`","limit":3}`))
	rows := resultRows(t, res)

	// Measured: byte-identical title/text/createdAt across all 3 ties the FTS
	// score, so the id-ascending tie-break applies; "chain/a-alpha" sorts first.
	const head = "chain/a-alpha"
	const attached = "chain/b-bridge"
	const excluded = "chain/c-gamma"

	// Contract: exactly 2 top-level rows survive — A as cluster head (with B
	// folded into its corroborating refs, freeing B's slot) and C as its own
	// independent hit. B must NEVER occupy a top-level slot of its own once
	// clustered. RED today: no implementation exists, so all 3 still show up
	// independently (len(rows) == 3 today).
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 top-level results (A as cluster head + C independent; "+
			"B must be folded into A's corroborating refs, not occupy its own slot), got %d: %v",
			len(rows), rows)
	}

	if got := rowID(t, rows[0]); got != head {
		t.Fatalf("measured tie-break drifted: expected %s to rank first, got %s (%v) — "+
			"re-measure and update this test's constants", head, got, rows)
	}

	var headRow, bridgeRow, excludedRow map[string]any
	for _, row := range rows {
		switch rowID(t, row) {
		case head:
			headRow = row
		case attached:
			bridgeRow = row
		case excluded:
			excludedRow = row
		}
	}
	if headRow == nil {
		t.Fatalf("head row %s missing from results: %v", head, rows)
	}
	if bridgeRow != nil {
		t.Fatalf("%s must not occupy its own top-level slot once clustered under %s: %v", attached, head, rows)
	}
	// C must still be reachable as its own independent top-level row — it does
	// not qualify with the head directly, so it must never silently vanish.
	if excludedRow == nil {
		t.Fatalf("%s (not in A's cluster) must remain an independent top-level result: %v", excluded, rows)
	}

	corr, ok := headRow["corroborating"].([]any)
	if !ok || len(corr) != 1 {
		t.Fatalf("head %s must have exactly 1 corroborating member (%s) — RED today, no "+
			"implementation exists yet: %#v", head, attached, headRow)
	}
	ref, _ := corr[0].(map[string]any)
	if got, _ := ref["id"].(string); got != attached {
		t.Fatalf("head %s corroborating member = %q, want %q (the pairwise-qualifying B, not "+
			"the transitively-reachable-only C)", head, got, attached)
	}
	// C must never appear inside the head's corroborating array (no transitive closure) —
	// B's citation-reachability is already checked above via the corroborating[0] equality.
	for _, raw := range corr {
		ref, _ := raw.(map[string]any)
		if got, _ := ref["id"].(string); got == excluded {
			t.Fatalf("transitive closure regression: %s (qualifies only via B, not directly "+
				"with head %s) wrongly appears in corroborating: %v", excluded, head, corr)
		}
	}
}

// ---------------------------------------------------------------------------
//  8. CONTRACT — the 24h window is STRICT (exclusive), not inclusive. Two
//     otherwise-identical record pairs share full person-identity overlap;
//     the only variable is the delta between their occurred_at instants.
//     Exactly 24h apart must NOT cluster (this already holds with zero
//     implementation, so it is a PASS-today guard against ever loosening the
//     boundary to <=); 23h59m apart — one minute inside — MUST cluster (RED
//     today: no implementation exists).
//
// ---------------------------------------------------------------------------
func TestClusterContractWindowBoundary(t *testing.T) {
	seedPair := func(t *testing.T, delta time.Duration) Config {
		t.Helper()
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		base, err := time.Parse(time.RFC3339, "2026-06-20T09:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
		seed := func(id, occurredAt string) {
			if err := writeMemory(cfg, Memory{
				ID: id, Scope: "personal", Type: "email", Title: "Boundary Window Regression",
				Source: "gmail", CreatedAt: occurredAt,
				Meta: map[string]any{
					"occurred_at": occurredAt,
					"from":        []string{"holly@example.com"},
					"to":          []string{"ivan@example.com"},
				},
				Text: "Boundary window regression fixture body text for the strict 24h window contract.",
			}); err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
		}
		seed("boundary/first", base.Format(time.RFC3339))
		seed("boundary/second", base.Add(delta).Format(time.RFC3339))

		ctx := context.Background()
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("ExactlyOnBoundary_24h_NotClustered", func(t *testing.T) {
		seedPair(t, 24*time.Hour)
		res := mcpResult(t, budgetCall("search_memory", `{"query":"Boundary Window Regression","limit":5}`))
		rows := resultRows(t, res)
		if len(rows) != 2 {
			t.Fatalf("exactly-24h-apart pair must NOT cluster (2 independent results), got %d: %v",
				len(rows), rows)
		}
		if envelopeContainsKey(t, res, "corroborating") {
			t.Fatal("exactly-24h-apart pair must not carry a corroborating key")
		}
	})

	t.Run("JustInsideBoundary_23h59m_Clustered", func(t *testing.T) {
		seedPair(t, 23*time.Hour+59*time.Minute)
		res := mcpResult(t, budgetCall("search_memory", `{"query":"Boundary Window Regression","limit":5}`))
		rows := resultRows(t, res)
		if len(rows) != 1 {
			t.Fatalf("23h59m-apart pair (inside the strict window) must cluster to 1 head, got %d: %v",
				len(rows), rows)
		}
		corr, ok := rows[0]["corroborating"].([]any)
		if !ok || len(corr) != 1 {
			t.Fatalf("23h59m-apart pair must produce a head with exactly 1 corroborating member — "+
				"RED today, no implementation exists yet: %#v", rows[0])
		}
	})
}

// ---------------------------------------------------------------------------
//  9. REGRESSION PIN — the refusal cap. 8 memories share one sender to 8
//     distinct recipients, all at the SAME occurred_at (a classic hub/fan-out
//     pattern — a broadcast, not one real-world event). 8 > the 5-member cap,
//     so the candidate cluster must be refused whole: NO clustering at all,
//     all 8 remain independent top-level hits. This is the distilled
//     seedBudgetFixture pathology (mora_mcp_budget_test.go's 200-same-sender
//     fixture) at a size (8) just past the cap, so the refusal itself — not
//     merely "no CreatedAt fallback" — is exercised (occurred_at is set
//     explicitly here, so Rule 2 would otherwise happily link all 8). PASS
//     today (nothing implemented, so nothing over-clusters); the guard is
//     against a FUTURE implementation that clusters the hub with no cap.
//
// ---------------------------------------------------------------------------
func TestClusterContractAntiHubRefusalCap(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	const occurredAt = "2026-06-25T12:00:00Z"
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("hub/thread-%02d", i)
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "personal", Type: "email",
			Title:     fmt.Sprintf("Hub Fanout Regression %02d", i),
			Source:    "gmail",
			CreatedAt: occurredAt,
			Meta: map[string]any{
				"occurred_at": occurredAt,
				"from":        []string{"zara@example.com"},
				"to":          []string{fmt.Sprintf("member%02d@example.com", i)},
			},
			Text: "Hub fanout regression body text for the anti-hub refusal cap contract.",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	ctx := context.Background()
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	res := mcpResult(t, budgetCall("search_memory", `{"query":"Hub Fanout Regression","limit":10}`))
	rows := resultRows(t, res)
	if len(rows) != 8 {
		t.Fatalf("refusal cap: candidate cluster of 8 (> 5-member cap) must refuse whole, "+
			"leaving 8 independent hits, got %d: %v", len(rows), rows)
	}
	if envelopeContainsKey(t, res, "corroborating") {
		t.Fatal("refusal cap: a refused over-cap candidate must carry no corroborating key at all")
	}
}

// ---------------------------------------------------------------------------
//  10. REGRESSION PIN — no CreatedAt fallback, ever. Two memories share full
//     person-identity overlap and CreatedAt values close together, but
//     NEITHER carries meta.occurred_at. Rule 2 must never link them: an
//     implementation that resurrects the old "occurred_at, else CreatedAt"
//     fallback would collapse this pair (and, at scale, the 200-thread
//     same-sender fixture the old fallback broke — see the AMENDMENT note at
//     the top of this file). PASS today (nothing implemented, so nothing
//     clusters); stays PASS after a correct implementation lands.
//
// ---------------------------------------------------------------------------
func TestClusterContractNoOccurredAtNoFallback(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	seed := func(id, createdAt string) {
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "personal", Type: "email", Title: "No Occurred At Regression",
			Source: "gmail", CreatedAt: createdAt,
			// Deliberately no "occurred_at" key at all.
			Meta: map[string]any{"from": []string{"karl@example.com"}, "to": []string{"lena@example.com"}},
			Text: "No occurred at regression body text for the CreatedAt-fallback-ban contract.",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("no-occurred-at/first", "2026-06-28T10:00:00Z")
	// 30m apart by CreatedAt — would fall inside the window under the banned fallback.
	seed("no-occurred-at/second", "2026-06-28T10:30:00Z")

	ctx := context.Background()
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	res := mcpResult(t, budgetCall("search_memory", `{"query":"No Occurred At Regression","limit":5}`))
	rows := resultRows(t, res)
	if len(rows) != 2 {
		t.Fatalf("no meta.occurred_at on either record: Rule 2 must never link via a CreatedAt "+
			"fallback, so both must remain independent, got %d: %v", len(rows), rows)
	}
	if envelopeContainsKey(t, res, "corroborating") {
		t.Fatal("no meta.occurred_at pair must not carry a corroborating key")
	}
}
