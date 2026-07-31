package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// Issue #243 — Gmail messages as derived evidence segments with stable
// sub-citations. Contract only; NO implementation lives in this file or
// anywhere else in this commit. Every "Contract" test below is expected to be
// RED today (it asserts a "gmail_segments" table, an "evidence" key, or an
// "evidence_ref" read behavior that does not exist in the codebase yet, and
// most fail loudly the moment they touch the missing table/column rather than
// skip); every "Pin" test is expected to be GREEN today (it captures CURRENT
// behavior — including the vacuous "no such key exists yet" case — as a
// regression guard the future implementation must not break). Each test's doc
// comment says which it is; §4 below is the audited red/green attribution for
// every test in the file (correction round 2).
//
// This is round 2 of the contract, revising round 1 (commit 64e00d6) after a
// supervisor audit against the LIVE issue body (`gh issue view 243 -R
// pyranthus-hq/mora --json title,body,url`; issue has no comments). §1
// restates the frozen interface. §2 records every design question as DECIDED
// — none may read as both open and frozen; where the audit overturned a
// round-1 choice, that override IS the decision. §3 quotes the governing
// issue wording verbatim. §4 maps every issue acceptance-criteria bullet to
// the test(s) pinning it.
//
// ---------------------------------------------------------------------------
// §1. FROZEN INTERFACE (integrator decisions, recapped from the DAG brief)
// ---------------------------------------------------------------------------
//
//  1. Derived projection: gmail_segments (base rows) + gmail_segments_fts
//     (FTS5), created/deleted/populated inside the SAME rebuildIndexWithPolicy
//     transaction; fully disposable; rebuild from the same vault is
//     deterministic.
//  2. Fail-closed alignment: segments are derived ONLY when
//     len(meta.messages) == the recovered body-block count AND every message's
//     MessageRef is well-formed; any mismatch => zero segments for that
//     memory (parent memory still indexed normally) + a structural
//     "unscorable" diagnostic reason recorded WITHOUT memory content
//     (counts/ids only). Never fabricate or guess a message_ref; never
//     attribute text to a ref it might not belong to.
//  3. Retrieval: segment-grain FTS is an ADDITIONAL candidate source mapping
//     hits to PARENT memory ids before fusion/slot accounting — one thread
//     consumes ONE parent result slot no matter how many segments hit; the
//     STRONGEST matching segment attaches to the parent's search_memory
//     result entry as an optional compact "evidence" object
//     {evidence_ref, sender, at, snippet}; absent when the best hit is not
//     segment-driven (see DQ5 — this "segment-driven" test is DECIDED
//     strictly in §2, overturning round 1's looser reading). A buried message
//     (rank-losing thread under parent-grain FTS) must be findable via its
//     segment.
//  4. read_memory extension: optional evidence_ref param — with id+
//     evidence_ref returns ONLY that message's segment content (bounded) + a
//     receipt identifying {id, evidence_ref, sender, at}; an evidence_ref that
//     does not belong to the given memory id => explicit error (fail closed,
//     no cross-memory read); composes with #242's bounded params (see DQ6 —
//     composition, not a forced new shape).
//  5. Byte-identity: no segment participation => outputs unchanged.
//  6. Determinism everywhere: segment ordering, strongest-segment tie-breaks
//     (best score, then lowest evidence_ref lexicographically), rebuild
//     determinism — PINNED to cover both segment rows AND resulting
//     rankings/receipts (DQ-determinism, round 2).
//
// ---------------------------------------------------------------------------
// §2. DESIGN QUESTIONS — every one DECIDED, none open. "(round 1)" marks this
// contract's own original proposal; "(round 2, integrator)" marks a decision
// the supervisor audit made or overturned explicitly; "(round 2, contract)"
// marks a NEW operational definition this revision had to pin to satisfy an
// integrator-mandated requirement (e.g. the ordering-mismatch signal) where
// the audit specified the REQUIREMENT but not the exact mechanism — recorded
// as decided, not left open, per the audit's hygiene rule.
// ---------------------------------------------------------------------------
//
//   - DQ1 (schema) — DECIDED (round 1, unchallenged): gmail_segments has
//     columns (evidence_ref TEXT PRIMARY KEY, memory_id TEXT, sender TEXT,
//     recipients TEXT [JSON array], at TEXT, block_refs TEXT [JSON array],
//     text TEXT) — evidence_ref is the MessageRef VERBATIM (the frozen
//     interface's own wording), so it is the natural primary key and a
//     UNIQUE-per-vault citation token. gmail_segments_fts is an FTS5 index
//     over (evidence_ref UNINDEXED, text) so a MATCH hit yields the
//     evidence_ref directly.
//   - DQ2 (diagnostics) — DECIDED (round 2, integrator: table shape ACCEPTED,
//     reason taxonomy EXTENDED; round 3, supervisor-authorized amendment:
//     taxonomy EXTENDED AGAIN to FIVE reasons — see reason 5 below, a
//     reviewer-reproduced P0 the original four never anticipated). A
//     dedicated gmail_segment_diagnostics table (memory_id TEXT PRIMARY KEY,
//     reason TEXT, meta_count INTEGER, body_count INTEGER) records the
//     fail-closed reason, counts/ids ONLY — never memory text. The refusal
//     taxonomy is FIVE reasons, each with its own distinct fixture (§4
//     requirement 5), checked in this PRIORITY ORDER (first match wins, so a
//     fixture built to trigger a later reason never gets mis-reported as an
//     earlier one it happens to also satisfy):
//       1. "truncated" — Memory.Truncated is true. Checked FIRST and
//          independently of the count comparison: the issue's own fail-closed
//          requirement ("Do not index metadata entries whose rendered text
//          was removed by body truncation") is about untrustworthy CONTENT,
//          which can coincidentally still produce a matching part count (this
//          contract's truncated fixture deliberately keeps meta_count ==
//          body_count to prove the check is independent, not a fallback of
//          the count check).
//       2. "count_mismatch" — len(meta.messages) != the recovered body-block
//          count (gmailBodyParts, DQ4).
//       3. "ordering_mismatch" — DECIDED (round 2, contract, REVISED after a
//          second audit pass rejected the round-2-first-draft signal). The
//          issue body names "a metadata/body count OR ordering mismatch" as
//          two DISTINCT fail-closed cases but does not specify the ordering
//          signal. The FIRST draft of this contract pinned "meta.messages[].At
//          not monotonically non-decreasing" — REJECTED: real providers can
//          legitimately emit non-monotonic timestamps (clock skew, delayed
//          delivery, a reply whose server-stamped time lands before the
//          message it replies to), so that signal could fail closed on a
//          VALID order, which is exactly the false-positive this contract
//          must not manufacture. The DECIDED signal instead: meta.messages
//          count equals the recovered body-block count (so this is never
//          confused with count_mismatch), AND meta.messages[].At IS
//          monotonically non-decreasing (so the ordering signal below is
//          isolated from the rejected timestamp heuristic), BUT
//          meta.messages[i].Sender does NOT match the sender address parsed
//          from the RAW joined body's own "From:" header at block position i
//          (net/mail address parsing, mirroring gmail.go's own header
//          handling) for at least one i. This is a DIRECT, positional,
//          content-grounded mismatch between what the metadata array claims
//          authored a position and what the rendered block at that position
//          actually says — never a proxy signal (like time ordering) that a
//          legitimate provider quirk could trip.
//       4. "malformed_ref" — counts and ordering are both fine, but at least
//          one MessageRef fails the DQ3 well-formedness check.
//       5. "duplicate_ref" — DECIDED (round 3, supervisor-authorized
//          amendment, reviewer-reproduced P0). Every MessageRef is
//          individually well-formed (DQ3), but two or more messages declare
//          the IDENTICAL MessageRef — which would otherwise collide on
//          gmail_segments' evidence_ref PRIMARY KEY, a real SQL
//          UNIQUE-constraint error that, unless caught HERE before any
//          INSERT runs, aborts the WHOLE rebuild transaction (not just this
//          one memory), taking the entire vault's index down until the one
//          offending file is hand-fixed — violating both the fail-closed
//          invariant (one bad memory must never cost every OTHER memory its
//          segments, or its own indexing) and the disposable-index
//          invariant. A DISTINCT reason from malformed_ref (an integrator
//          decision): a duplicated ref is not itself malformed — it fails on
//          a SET property (uniqueness across the memory's own messages), not
//          a per-ref one — so folding it into malformed_ref would blur two
//          different failure modes under one diagnostic. Checked LAST, after
//          every ref is already confirmed individually well-formed.
//   - DQ3 (well-formed MessageRef) — DECIDED (round 2, integrator: ACCEPTED
//     unchanged). A MessageRef is well-formed for parent memory m iff it has
//     the exact prefix m.ID+"#" and a non-empty suffix after that "#". This
//     reuses gmailMessageRef's own format ("gmail_thread/"+threadID+"#"+
//     messageID, gmail.go:161) and additionally PINS that the threadID
//     portion must equal the parent's OWN thread id (m.ID, since m.ID ==
//     "gmail_thread/"+threadID for every Gmail memory) — a MessageRef whose
//     thread portion names a DIFFERENT thread is refused even though it is
//     syntactically a valid MessageRef shape, because accepting it would let
//     one memory's segments claim identity in another's evidence space
//     (exactly the misattribution failure mode this issue exists to
//     prevent).
//   - DQ4 (body-block recovery) — DECIDED (round 1, unchallenged): reuse the
//     EXISTING split primitive (gmailBodyParts, commitment.go:548) rather
//     than inventing a second one — it is already the codebase's one
//     precedent for recovering per-message boundaries from the
//     "\n\n---\n\n"-joined body, and reusing it means a literal "---" line
//     inside a message produces the SAME extra-parts mismatch commitment.go's
//     own len(messages)==len(parts) guard already relies on
//     (commitment.go:814), not a second, possibly-divergent definition of
//     "matches".
//   - DQ5 (evidence attachment condition) — DECIDED (round 2, integrator:
//     REVISED to the issue-aligned rule, replacing BOTH the round-1 "any
//     co-match" reading and the round-2 "winning candidate-source ownership"
//     reading). Strict winning-arm ownership is DROPPED: it cannot be tested
//     without either an unproven BM25-magnitude assumption (silently
//     drift-able) or an arbitration seam that does not exist anywhere in the
//     codebase today (no production code computes or exposes "which arm
//     decided this rank"). The frozen rule is now purely a function of
//     MATCHING, not of who "won" a rank:
//       - A returned Gmail parent that has AT LEAST ONE query-matching
//         segment carries the evidence receipt of its STRONGEST matching
//         segment — the existing strongest/tie-break rule (best score, then
//         lowest evidence_ref lexicographically) is unchanged.
//       - A returned parent with NO query-matching segment carries no
//         "evidence" key at all.
//     This is fully deterministic and requires no arbitration: "does this
//     parent have a query-matching segment" is answerable directly from the
//     segment-grain FTS candidate set, independent of how the parent itself
//     was ranked or why it was returned. Three tests pin this (all under
//     seedGmailSegmentsSearchFixture):
//       - No segment match (evidence absent):
//         TestGmailSegmentsContractNoEvidenceWhenNoSegmentMatches
//         (gsSearchWellFormedID, gsTitleOnlyMarker present ONLY in the
//         title, absent from every message body) — the negative pin. This is
//         NOT an arbitration/ownership proof (round 2's second draft
//         over-claimed that framing); it is simply the "zero query-matching
//         segments" case of the rule above.
//       - Segment match, parent ALSO independently matches at parent-grain
//         (co-match, evidence present):
//         TestGmailSegmentsContractSearchEvidenceAttachesOnParentGrainCoMatch
//         (gsCoMatchID) — a NEW positive pin (round 2, fourth audit pass):
//         the query term sits in BOTH the title (an independent parent-grain
//         match) AND msg-1's body (a query-matching segment). Under the new
//         rule this is fully deterministic regardless of which arm "found"
//         the parent first: it has a query-matching segment, so it MUST
//         carry that segment's exact evidence receipt — no ownership
//         reasoning, no scoring assumption.
//       - Segment match without a title/parent-grain-obvious co-match
//         (evidence present): TestGmailSegmentsContractSearchEvidence
//         AttachesMatchingSegment and the buried-message pair
//         (TestGmailSegmentsContractBuriedMessagePin +
//         BuriedMessageFindableViaSegment) — unchanged from before (per the
//         audit's instruction: "buried-message tests stay as-is").
//     The semantic/embedder path must not disturb this: when hybrid fusion is
//     active (a semantic embedder opted in and reachable), the EXACT FTS
//     segment receipt (evidence_ref/sender/at/snippet) must survive unchanged
//     through fusion — pinned by
//     TestGmailSegmentsContractSemanticPathPreservesSegmentEvidence (a
//     fakeOllama seam, mirroring embed_ollama_test.go /
//     embedder_incident_replay_test.go / retrieval_rt_cover_test.go), which
//     compares the evidence object byte-for-byte between the static-embedder
//     run and the semantic-embedder run on the SAME buried-message query.
//   - DQ6 (read_memory evidence_ref composition with #242) — DECIDED (round
//     2, integrator: OVERTURNED — do NOT force an arbitrary eight-key
//     receipt; COMPOSE with #242's existing bounded/receipt machinery). The
//     issue body's own wording (§3 below) is: evidence_ref returns "the exact
//     message block with bounded adjacent context". This contract reads that
//     as: evidence_ref narrows the READ TARGET from the full thread body to
//     that ONE segment's text, and #242's EXISTING applyBoundedRead/
//     boundedReadReceipt pipeline (read_bounded.go) then runs UNCHANGED over
//     that narrowed text — never a bespoke new receipt struct. Concretely
//     pinned, without forcing every key on every call:
//       - The receipt/response ALWAYS carries the parent id, the requested
//         evidence_ref, and that segment's sender/at (the identity fields
//         #4's own read_memory-extension ask requires) —
//         TestGmailSegmentsContractReadMemoryEvidenceRefReturnsSegmentOnly
//         checks exactly these four, nothing more asserted about the rest of
//         the shape.
//       - "bounded adjacent context" is #242's OWN centered-excerpt mechanism
//         (centeredExcerptAt, read_bounded.go), reused as-is over the
//         narrowed segment text: a match phrase inside a long segment returns
//         a window of SURROUNDING text (not the bare phrase alone), the same
//         leading/trailing ellipsis-bounded behavior #242 already ships —
//         pinned by
//         TestGmailSegmentsContractReadMemoryEvidenceRefBoundedAdjacentContext.
//       - PRECEDENCE with #242's own params: evidence_ref narrows the target
//         FIRST; match/max_tokens/occurrence (if present) then apply WITHIN
//         that narrowed text exactly as #242 defines them — a match phrase
//         belonging to a DIFFERENT message of the same thread must not match
//         (TestGmailSegmentsContractReadMemoryEvidenceRefScopedNotWholeThread).
//   - DQ-recipients (To/Cc merge) — DECIDED (round 2, contract; requirement 3):
//     a segment's "recipients" column is the deterministic, sorted,
//     case-insensitive-deduped, LOWERCASED-output UNION of that message's To
//     and Cc address lists — never a plain concatenation (which could carry
//     duplicates when an address appears in both, in any casing) and never
//     independently-sorted-then-appended (which would not dedup across the
//     two fields). This reuses the SAME sorted/deduped/lowercased convention
//     gmail.go's own addrSet.list() already applies within each of To and Cc
//     individually (internal/google/identity.go) — recipients just applies
//     that convention one level up, across the two fields combined, and does
//     so defensively (the derivation must not assume its meta.messages input
//     already arrived pre-normalized). Pinned by
//     TestGmailSegmentsContractRecipientsMergeDeterministicDedup, whose
//     fixture deliberately mixes casing ACROSS To and Cc (e.g. "Bob@x.com" in
//     To, "bob@x.com" in Cc) so the test cannot pass on same-field-only
//     dedup.
//   - DQ-occurrence-time — DECIDED (round 2, contract; requirement 3): a
//     segment's "at" column is gmailMessageEvidence.At VERBATIM — the
//     message's Gmail InternalDate (gmail.go: `t := time.UnixMilli(msg.
//     InternalDate); evidence.At = t.UTC().Format(time.RFC3339)`), i.e. the
//     timestamp Gmail itself assigns the message (its inbox/thread ordering
//     instant — effectively send/receive time), RFC3339 UTC. It is copied,
//     never re-derived, re-parsed-and-reformatted, or defaulted from the
//     parent memory's CreatedAt. Pinned by the exact-value assertions in
//     TestGmailSegmentsContractWellFormedRowShape.
//
// ---------------------------------------------------------------------------
// §3. GOVERNING WORDING QUOTED FROM THE LIVE ISSUE BODY (gh issue view 243 -R
// pyranthus-hq/mora --json title,body,url; verbatim, no comments exist)
// ---------------------------------------------------------------------------
//
//	"Extend #242's bounded read contract to accept `evidence_ref` once the
//	segment projection exists, returning the exact message block with bounded
//	adjacent context." (Scope)
//
//	"Never fabricate a message ref when metadata/body alignment is missing or
//	malformed." / "Do not index metadata entries whose rendered text was
//	removed by body truncation." / "A metadata/body count or ordering
//	mismatch must produce an explicit unscorable/drop reason in rebuild
//	diagnostics, not an incorrectly attributed segment." / "SQLite remains a
//	disposable cache; deleting `index.db` and rebuilding from the vault must
//	reproduce every segment byte-for-byte." (Fail-closed requirements)
//
// ---------------------------------------------------------------------------
// §4. ACCEPTANCE CRITERIA (issue body, verbatim bullets) => TEST(S)
// ---------------------------------------------------------------------------
//
//	"A multi-message Gmail fixture where only message 2 matches returns the
//	thread once with message 2's evidence_ref, sender, timestamp, and
//	snippet." =>
//	  TestGmailSegmentsContractSearchEvidenceAttachesMatchingSegment (RED),
//	  TestGmailSegmentsContractOneParentSlotRegardlessOfSegmentHitCount
//	  (GREEN pin — see §2 DQ5, the "once" half),
//	  TestGmailSegmentsContractSearchEvidenceAttachesOnParentGrainCoMatch
//	  (RED — the deterministic co-match positive pin for DQ5's
//	  evidence-attachment rule, §2).
//
//	"Two messages with similar text remain independently addressable by
//	distinct stable refs." =>
//	  TestGmailSegmentsContractDistinctRefsForSimilarText (RED),
//	  TestGmailSegmentsContractWellFormedRowShape (RED, distinct evidence_ref
//	  rows).
//
//	"Segment hits from one thread consume one parent result slot while
//	retaining auditable member evidence." =>
//	  TestGmailSegmentsContractOneParentSlotRegardlessOfSegmentHitCount
//	  (GREEN pin).
//
//	"FTS-only ranking proves a buried message can win without the whole
//	thread first winning at parent grain." =>
//	  TestGmailSegmentsContractBuriedMessagePin (GREEN pin — the baseline
//	  proof parent-grain alone fails),
//	  TestGmailSegmentsContractBuriedMessageFindableViaSegment (RED).
//
//	"A semantic-embedder run preserves the existing embedder gate and cannot
//	lose the exact FTS evidence receipt." =>
//	  TestGmailSegmentsContractSemanticPathPreservesSegmentEvidence (RED).
//
//	"Malformed/truncated metadata fixtures drop/refuse rather than
//	misattribute." =>
//	  TestGmailSegmentsContractFailClosedCountMismatch (RED),
//	  TestGmailSegmentsContractFailClosedLiteralSeparatorInBody (RED),
//	  TestGmailSegmentsContractFailClosedOrderingMismatch (RED),
//	  TestGmailSegmentsContractFailClosedTruncatedBody (RED),
//	  TestGmailSegmentsContractFailClosedMalformedRef (RED),
//	  TestGmailSegmentsContractFailClosedDuplicateRef (round 3 addition,
//	  RED — the reviewer-reproduced P0: a duplicate MessageRef must fail
//	  closed for its OWN memory exactly like the other four reasons, never
//	  abort the whole rebuild),
//	  TestGmailSegmentsContractDuplicateRefBystanderSurvives (round 3
//	  addition, RED — the P0's blast-radius proof: an innocent bystander
//	  memory sharing a vault with a duplicate-ref offender must still be
//	  fully indexed and searchable),
//	  TestGmailSegmentsContractDiagnosticsSchemaHasNoContentColumns (RED —
//	  pins the diagnostics table's SCHEMA itself carries no content column,
//	  not merely that one wasn't selected).
//
//	"read_memory(id, evidence_ref=...) returns the cited block and rejects a
//	ref belonging to another memory." =>
//	  TestGmailSegmentsContractReadMemoryEvidenceRefReturnsSegmentOnly (RED),
//	  TestGmailSegmentsContractReadMemoryEvidenceRefBoundedAdjacentContext
//	  (RED),
//	  TestGmailSegmentsContractReadMemoryEvidenceRefScopedNotWholeThread
//	  (RED),
//	  TestGmailSegmentsContractReadMemoryEvidenceRefCrossMemoryFailsClosed
//	  (RED),
//	  TestGmailSegmentsContractReadMemoryEvidenceRefUnknownFailsClosed (RED),
//	  TestGmailSegmentsContractReadMemoryEvidenceRefOnFailClosedMemoryErrors
//	  (RED),
//	  TestGmailSegmentsContractReadMemoryEvidenceRefSharedFallbackFailsClosed
//	  (round 3 addition, RED — P2-1: the findSharedMemory fallback path must
//	  fail closed on evidence_ref exactly like the local path, never
//	  silently return the full shared body),
//	  TestGmailSegmentsContractToolsListExposesEvidenceRefParam (RED —
//	  schema discoverability, not behavior).
//
//	"Full index deletion/rebuild produces byte-identical segment rows and
//	rankings." =>
//	  TestGmailSegmentsContractDeterministicRebuild (RED — deletes index.db
//	  outright, then compares BOTH the WHOLE gmail_segments table (all
//	  parents, not one) AND an ORDERED multi-parent search_memory result/
//	  receipt sequence across the deletion+rebuild).
//
// The FTS5 shape pin (TestGmailSegmentsContractIndexTablesCreated's DDL
// assertions) and the diagnostics no-content-column pin
// (TestGmailSegmentsContractDiagnosticsSchemaHasNoContentColumns) are not
// themselves acceptance-criteria bullets but close the frozen interface's
// "disposable projection" (#1) and "content-free diagnostics" (#2)
// guarantees at the SCHEMA level, not just the query level.
//
//	"T0 MCP budget ceilings pass without increases." => not a new test in
//	this contract-only file (no new tool/param ships until implementation);
//	enforced by the EXISTING mora_mcp_budget_test.go gate suite, which this
//	worker's validation step runs unmodified. Once evidence_ref/segments ship,
//	the implementer adds the corresponding budgetCase row(s) there.
//
//	"Full CGO=0, race, vet, lint, and retrieval-eval gates remain green." =>
//	not this file's concern beyond `go build`/`go vet`/the required focused
//	gate re-run (see the worker report's validation section for this round's
//	status, which is DEFERRED under an active disk-space gate).
//
// Byte-identity for non-participating memories (frozen interface #5) is
// pinned by TestGmailSegmentsContractByteIdenticalWithoutSegmentParticipation
// (GREEN pin) — not itself an acceptance-criteria bullet, but the explicit
// integrator interface guarantee.
//
// ---------------------------------------------------------------------------

// ---- shared body/meta construction helpers --------------------------------

// gmailSegJoinBody reproduces gmail.go's real join shape: one "From: %s\n\n%s"
// block per message, joined by "\n\n---\n\n" (gmail.go:104,122,153). Building
// fixtures this way — not a simplified stand-in — is what makes the
// count-mismatch and literal-separator fixtures below faithful reproductions
// of the real ambiguity, not synthetic test-only shapes.
func gmailSegJoinBody(messages ...[2]string) string {
	parts := make([]string, len(messages))
	for i, m := range messages {
		parts[i] = fmt.Sprintf("From: %s\n\n%s", m[0], m[1])
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// gmailSegMessages builds the meta["messages"] value using the SAME JSON
// shape gmailMessageEvidence emits (message_ref/sender/to/cc/at/block_refs) —
// commitmentMessageEvidence's tags are identical (commitment.go:148-156), so
// reusing it here means these fixtures round-trip through parseMemoryBytes
// exactly like a real connector-written vault file, with no new type needed.
func gmailSegMessages(msgs ...commitmentMessageEvidence) []commitmentMessageEvidence {
	return msgs
}

// ---- gmail_segments / gmail_segment_diagnostics query helpers -------------
//
// These open the index directly (the roIndexDSN/dbPath pattern already used
// by index_schema_test.go and embedder_incident_replay_test.go) because no
// MCP surface can inspect the derived projection's raw rows. Every one of
// them t.Fatalf's — never t.Skip's — when the table is absent: that IS the
// expected RED failure mode for a contract pinned before implementation, and
// a skip would hide it instead of reporting it (mora-testing-and-ci: "never
// t.Skip a red row — a skip is invisible").

type gmailSegRow struct {
	EvidenceRef string
	MemoryID    string
	Sender      string
	Recipients  string // raw JSON array text
	At          string
	BlockRefs   string // raw JSON array text
	Text        string
}

func gmailSegRowsFor(t *testing.T, cfg Config, memoryID string) []gmailSegRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT evidence_ref, memory_id, sender, recipients, at, block_refs, text
		 FROM gmail_segments WHERE memory_id = ? ORDER BY evidence_ref`, memoryID)
	if err != nil {
		t.Fatalf("gmail_segments query (table missing until #243 lands — see DQ1): %v", err)
	}
	defer rows.Close()
	var out []gmailSegRow
	for rows.Next() {
		var r gmailSegRow
		if err := rows.Scan(&r.EvidenceRef, &r.MemoryID, &r.Sender, &r.Recipients, &r.At, &r.BlockRefs, &r.Text); err != nil {
			t.Fatalf("scan gmail_segments row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("gmail_segments rows: %v", err)
	}
	return out
}

// gmailSegAllRows snapshots EVERY gmail_segments row across the whole vault,
// in a fully deterministic order (ORDER BY evidence_ref, the table's own
// primary key) — the whole-table counterpart to gmailSegRowsFor's
// single-memory filter, used by the rebuild-determinism test so ordering
// across MULTIPLE parents is actually exercised, not just one memory's rows
// (round 2, second audit pass).
func gmailSegAllRows(t *testing.T, cfg Config) []gmailSegRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT evidence_ref, memory_id, sender, recipients, at, block_refs, text
		 FROM gmail_segments ORDER BY evidence_ref`)
	if err != nil {
		t.Fatalf("gmail_segments query (table missing until #243 lands — see DQ1): %v", err)
	}
	defer rows.Close()
	var out []gmailSegRow
	for rows.Next() {
		var r gmailSegRow
		if err := rows.Scan(&r.EvidenceRef, &r.MemoryID, &r.Sender, &r.Recipients, &r.At, &r.BlockRefs, &r.Text); err != nil {
			t.Fatalf("scan gmail_segments row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("gmail_segments rows: %v", err)
	}
	return out
}

func gmailSegFTSTableExists(t *testing.T, cfg Config) bool {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='gmail_segments_fts'`).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("sqlite_master query: %v", err)
	}
	return name == "gmail_segments_fts"
}

// gmailSegFTSTableDDL returns the CREATE statement sqlite_master recorded for
// gmail_segments_fts — round 2, second audit pass: gmailSegFTSTableExists
// alone only proves a table NAME exists (it could be an ordinary empty
// TABLE); this proves the object's actual SHAPE — a genuine FTS5 virtual
// table over the frozen columns, not just a same-named placeholder.
func gmailSegFTSTableDDL(t *testing.T, cfg Config) (string, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	var ddl string
	err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='gmail_segments_fts'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("sqlite_master sql query: %v", err)
	}
	return ddl, true
}

// gmailSegFTSColumnSignature parses the exact column-definition list out of
// an "... fts5(...)" DDL string — round 2, fifth audit pass: a substring
// check for "evidence_ref" and "text" appearing ANYWHERE in the DDL cannot
// distinguish the frozen DQ1 shape (fts5(evidence_ref UNINDEXED, text)) from
// e.g. fts5(text, evidence_ref) (wrong order/indexing), fts5(evidence_ref,
// text, snippet) (an extra column), or a table/column comment that merely
// mentions the words. This finds "fts5(" case-insensitively, walks to its
// matching close paren (paren-depth aware, in case a column type ever adds
// nested parens), and splits the inner content on top-level commas — giving
// the EXACT, ordered column-definition list to assert against.
func gmailSegFTSColumnSignature(t *testing.T, ddl string) []string {
	t.Helper()
	lower := strings.ToLower(ddl)
	idx := strings.Index(lower, "fts5(")
	if idx == -1 {
		t.Fatalf("DDL does not contain fts5(...): %s", ddl)
	}
	start := idx + len("fts5(")
	depth := 1
	end := -1
	for i := start; i < len(ddl); i++ {
		switch ddl[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		t.Fatalf("DDL fts5(...) has no matching close paren: %s", ddl)
	}
	inner := ddl[start:end]
	parts := strings.Split(inner, ",")
	cols := make([]string, 0, len(parts))
	for _, p := range parts {
		cols = append(cols, strings.Join(strings.Fields(p), " ")) // collapse internal whitespace too
	}
	return cols
}

type gmailSegDiagnostic struct {
	MemoryID  string
	Reason    string
	MetaCount int
	BodyCount int
}

// gmailSegDiagnosticsSchemaColumns returns gmail_segment_diagnostics' column
// names via PRAGMA table_info — round 2, second audit pass: selecting only
// four known-safe columns (as gmailSegDiagnosticFor does) can never reveal an
// EXTRA content column even if the implementer added one; this inspects the
// actual table SCHEMA so a content column cannot exist undetected, rather
// than merely not being selected. PRAGMA table_info returns zero rows (not
// an error) for a table that does not exist, so an empty result here means
// "table missing", exactly like every other gmail_segment_* helper's
// missing-table RED signal.
func gmailSegDiagnosticsSchemaColumns(t *testing.T, cfg Config) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(gmail_segment_diagnostics)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(gmail_segment_diagnostics): %v", err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan PRAGMA table_info row: %v", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("PRAGMA table_info rows: %v", err)
	}
	return cols
}

// ---- typed, required-field JSON assertion helpers -------------------------
//
// Round 2, second audit pass: a bare `v, _ := obj[key].(T)` silently reads a
// MISSING key as T's zero value (false, 0, "") — which can coincidentally
// equal the "everything is fine" expectation and pass a test that should have
// failed loudly. These helpers require both PRESENCE and the correct type,
// t.Fatalf-ing otherwise, so a missing/mistyped field can never silently
// masquerade as a correct zero/false value.

func requireBoolField(t *testing.T, obj map[string]any, key string) bool {
	t.Helper()
	v, has := obj[key]
	if !has {
		t.Fatalf("missing required field %q: %#v", key, obj)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("field %q is not a bool: %#v (%T)", key, v, v)
	}
	return b
}

func requireNumberField(t *testing.T, obj map[string]any, key string) float64 {
	t.Helper()
	v, has := obj[key]
	if !has {
		t.Fatalf("missing required field %q: %#v", key, obj)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("field %q is not a number: %#v (%T)", key, v, v)
	}
	return n
}

func requireStringField(t *testing.T, obj map[string]any, key string) string {
	t.Helper()
	v, has := obj[key]
	if !has {
		t.Fatalf("missing required field %q: %#v", key, obj)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("field %q is not a string: %#v (%T)", key, v, v)
	}
	return s
}

func gmailSegDiagnosticFor(t *testing.T, cfg Config, memoryID string) (gmailSegDiagnostic, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	var d gmailSegDiagnostic
	err = db.QueryRow(
		`SELECT memory_id, reason, meta_count, body_count FROM gmail_segment_diagnostics WHERE memory_id = ?`,
		memoryID).Scan(&d.MemoryID, &d.Reason, &d.MetaCount, &d.BodyCount)
	if err == sql.ErrNoRows {
		return gmailSegDiagnostic{}, false
	}
	if err != nil {
		t.Fatalf("gmail_segment_diagnostics query (table missing until #243 lands — see DQ2): %v", err)
	}
	return d, true
}

// memoryStillIndexed checks the ordinary `memories` table directly — this
// table already exists today, so this helper is NOT expected to fail RED; it
// is the always-available half of the fail-closed contract (parent survival)
// that every fail-closed test below asserts BEFORE it hits the not-yet-built
// gmail_segments table.
func memoryStillIndexed(t *testing.T, cfg Config, memoryID string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, memoryID).Scan(&count); err != nil {
		t.Fatalf("memories query: %v", err)
	}
	return count > 0
}

// ---------------------------------------------------------------------------
// Fixture 1: schema/diagnostics fixture — one well-formed 2-message thread
// plus three malformed threads (count mismatch, literal-separator-in-body,
// malformed ref) and one unrelated thread for the cross-memory evidence_ref
// test. All in one vault, one rebuild, all synthetic (@example.com).
// ---------------------------------------------------------------------------

const (
	gsWellFormedID  = "gmail_thread/th-wellformed"
	gsWellMsg1Ref   = gsWellFormedID + "#msg-1"
	gsWellMsg2Ref   = gsWellFormedID + "#msg-2"
	gsAlphaMarker   = "GMLSEGALPHAMARKERONE"
	gsBetaMarker    = "GMLSEGBETAMARKERTWO"
	gsCountMismatch = "gmail_thread/th-count-mismatch"
	gsLiteralDash   = "gmail_thread/th-literal-dash"
	gsMalformedRef  = "gmail_thread/th-malformed-ref"
	gsCrossOtherID  = "gmail_thread/th-cross-other"
	gsCrossOtherRef = gsCrossOtherID + "#msg-1"

	// round 2 additions (audit corrections)
	gsOrderingMismatch  = "gmail_thread/th-ordering-mismatch"
	gsTruncatedID       = "gmail_thread/th-truncated"
	gsRecipientsID      = "gmail_thread/th-recipients-merge"
	gsRecipientsRef     = gsRecipientsID + "#msg-1"
	gsLongSegID         = "gmail_thread/th-long-segment"
	gsLongSegRef        = gsLongSegID + "#msg-1"
	gsLongSegMarker     = "GMLSEGLONGCONTEXTMARKER"
	gsLongSegNearBefore = "NEARCTXBEFOREMARKER"
	gsLongSegNearAfter  = "NEARCTXAFTERMARKER"

	// gmailSegRebuildOrderProbe is planted in THREE distinct memories'
	// bodies (gsWellFormedID, gsCountMismatch, gsCrossOtherID) so a
	// rebuild-determinism query against it returns a genuine multi-result,
	// multi-parent RANKED sequence — a single-hit query cannot exercise
	// "does ordering survive a rebuild" at all (round 2, second audit pass).
	gmailSegRebuildOrderProbe = "GMLSEGREBUILDORDERPROBE"
)

func seedGmailSegmentsFixture(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// (1) Well-formed: meta.messages count (2) == recovered body-block count
	// (2), both refs well-formed. The positive-path fixture for row-shape,
	// evidence attachment, and read_memory evidence_ref tests.
	wellBody := gmailSegJoinBody(
		[2]string{"alice@example.com", "Quick heads up, the " + gsAlphaMarker + " draft is ready for review. " + gmailSegRebuildOrderProbe + " noted for tracking."},
		[2]string{"bob@example.com", "Thanks — I will look at the " + gsBetaMarker + " numbers this afternoon."},
	)
	if err := writeMemory(cfg, Memory{
		ID: gsWellFormedID, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-wellformed",
		Title: "Draft review thread", CreatedAt: "2026-06-01T10:05:00Z", Text: wellBody,
		Meta: map[string]any{
			"from": []string{"alice@example.com", "bob@example.com"},
			"to":   []string{"alice@example.com", "bob@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{MessageRef: gsWellMsg1Ref, Sender: "alice@example.com", To: []string{"bob@example.com"}, At: "2026-06-01T10:00:00Z", BlockRefs: []string{"body"}},
				commitmentMessageEvidence{MessageRef: gsWellMsg2Ref, Sender: "bob@example.com", To: []string{"alice@example.com"}, At: "2026-06-01T10:05:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed well-formed thread: %v", err)
	}

	// (2) Count mismatch: meta declares 2 messages, but the body carries only
	// ONE joined block (no "\n\n---\n\n" separator at all) — meta_count=2,
	// body_count=1.
	mismatchBody := "From: carol@example.com\n\nOnly one message body actually made it into this file. " + gmailSegRebuildOrderProbe + " noted for tracking."
	if err := writeMemory(cfg, Memory{
		ID: gsCountMismatch, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-count-mismatch",
		Title: "Count mismatch thread", CreatedAt: "2026-06-02T09:00:00Z", Text: mismatchBody,
		Meta: map[string]any{
			"from": []string{"carol@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{MessageRef: gsCountMismatch + "#msg-1", Sender: "carol@example.com", At: "2026-06-02T09:00:00Z", BlockRefs: []string{"body"}},
				commitmentMessageEvidence{MessageRef: gsCountMismatch + "#msg-2", Sender: "carol@example.com", At: "2026-06-02T09:05:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed count-mismatch thread: %v", err)
	}

	// (3) Literal separator inside a message: meta declares 2 messages
	// (matching the REAL message count), but message 2's own body contains a
	// literal blank-line-dash-blank-line sequence (e.g. a signature block
	// using the same convention), so the naive split produces 3 parts, not
	// 2 — meta_count=2, body_count=3. This is the "must fail closed, not
	// misattribute" fixture: a buggy zip-by-index implementation would happily
	// pair message[0]<->part[0] and message[1]<->part[1] and silently drop
	// part[2], attributing message[1] only its own opening half.
	gsLiteralMarker := "GMLSEGLITERALDASHMARKER"
	literalBody := gmailSegJoinBody(
		[2]string{"dave@example.com", "First message, nothing unusual here."},
		[2]string{"erin@example.com", "Second message with an embedded rule.\n\n---\n\n" + gsLiteralMarker + " appears after a literal separator line inside ONE message body."},
	)
	if err := writeMemory(cfg, Memory{
		ID: gsLiteralDash, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-literal-dash",
		Title: "Literal separator thread", CreatedAt: "2026-06-03T09:00:00Z", Text: literalBody,
		Meta: map[string]any{
			"from": []string{"dave@example.com", "erin@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{MessageRef: gsLiteralDash + "#msg-1", Sender: "dave@example.com", At: "2026-06-03T09:00:00Z", BlockRefs: []string{"body"}},
				commitmentMessageEvidence{MessageRef: gsLiteralDash + "#msg-2", Sender: "erin@example.com", At: "2026-06-03T09:05:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed literal-dash thread: %v", err)
	}

	// (4) Malformed ref: meta.messages count (2) == body-block count (2) —
	// counts alone would pass — but message 2's MessageRef names a DIFFERENT
	// thread than this memory's own id (DQ3). Both messages must therefore be
	// refused together (whole-memory fail-closed, not a per-message partial).
	malformedBody := gmailSegJoinBody(
		[2]string{"frank@example.com", "First message, correctly ref'd."},
		[2]string{"grace@example.com", "Second message, ref points at the wrong thread."},
	)
	if err := writeMemory(cfg, Memory{
		ID: gsMalformedRef, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-malformed-ref",
		Title: "Malformed ref thread", CreatedAt: "2026-06-04T09:00:00Z", Text: malformedBody,
		Meta: map[string]any{
			"from": []string{"frank@example.com", "grace@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{MessageRef: gsMalformedRef + "#msg-1", Sender: "frank@example.com", At: "2026-06-04T09:00:00Z", BlockRefs: []string{"body"}},
				commitmentMessageEvidence{MessageRef: "gmail_thread/th-someone-elses-thread#msg-2", Sender: "grace@example.com", At: "2026-06-04T09:05:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed malformed-ref thread: %v", err)
	}

	// (5) An unrelated, otherwise well-formed single-message thread — used
	// only as the "other memory" in the cross-memory evidence_ref rejection
	// test, so that ref is a real, well-formed, DERIVED segment (not just a
	// syntactically odd string) belonging to a different parent.
	crossBody := "From: henry@example.com\n\nSingle self-contained message, unrelated to the wellformed thread. " + gmailSegRebuildOrderProbe + " noted for tracking."
	if err := writeMemory(cfg, Memory{
		ID: gsCrossOtherID, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-cross-other",
		Title: "Cross-memory other thread", CreatedAt: "2026-06-05T09:00:00Z", Text: crossBody,
		Meta: map[string]any{
			"from": []string{"henry@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{MessageRef: gsCrossOtherRef, Sender: "henry@example.com", At: "2026-06-05T09:00:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed cross-other thread: %v", err)
	}

	// (6) round 2 (REVISED after audit) — ordering mismatch: meta declares 2
	// messages, the recovered body-block count is ALSO 2 (counts match — this
	// must NOT be reported as count_mismatch), and meta.messages[].At IS
	// monotonically non-decreasing (09:00 < 09:05 — never confuse this with
	// the rejected non-monotonic-timestamp signal). The actual corruption:
	// block 0 of the RENDERED body is genuinely authored by kate (its own
	// "From: kate@example.com" header), block 1 by liam — but meta.messages
	// declares the SENDERS SWAPPED relative to those positions
	// (meta.messages[0].Sender=liam, meta.messages[1].Sender=kate) — a direct
	// positional mismatch between declared identity and the rendered block's
	// own header at that position (DQ2/§2's revised ordering_mismatch
	// signal), independent of timestamps entirely.
	orderingBody := gmailSegJoinBody(
		[2]string{"kate@example.com", "First rendered block, actually authored by kate per its own From: header."},
		[2]string{"liam@example.com", "Second rendered block, actually authored by liam per its own From: header."},
	)
	if err := writeMemory(cfg, Memory{
		ID: gsOrderingMismatch, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-ordering-mismatch",
		Title: "Ordering mismatch thread", CreatedAt: "2026-06-06T09:00:00Z", Text: orderingBody,
		Meta: map[string]any{
			"from": []string{"kate@example.com", "liam@example.com"},
			"messages": gmailSegMessages(
				// Declared senders are SWAPPED vs. the actual block-0/block-1
				// "From:" headers above — this is the fixture's only defect.
				commitmentMessageEvidence{MessageRef: gsOrderingMismatch + "#msg-1", Sender: "liam@example.com", At: "2026-06-06T09:00:00Z", BlockRefs: []string{"body"}},
				commitmentMessageEvidence{MessageRef: gsOrderingMismatch + "#msg-2", Sender: "kate@example.com", At: "2026-06-06T09:05:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed ordering-mismatch thread: %v", err)
	}

	// (7) round 2 — truncated body: meta declares 2 messages, the recovered
	// body-block count is ALSO 2 (deliberately matching, to prove the
	// truncated check runs independently of and BEFORE the count check — see
	// DQ2/§2's priority order), but the memory's own Truncated flag is set —
	// the same frontmatter field renderMemory/parseMemoryBytes already
	// round-trip for oversized connector bodies (memfile.go). A truncated
	// body's recovered "boundaries" cannot be trusted even when the count
	// happens to still line up.
	truncatedBody := gmailSegJoinBody(
		[2]string{"morgan@example.com", "First message of a body the connector truncated for size."},
		[2]string{"nina@example.com", "Second message, present in the truncated remainder."},
	)
	if err := writeMemory(cfg, Memory{
		ID: gsTruncatedID, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-truncated",
		Title: "Truncated body thread", CreatedAt: "2026-06-07T09:00:00Z", Text: truncatedBody,
		Truncated: true,
		Meta: map[string]any{
			"from": []string{"morgan@example.com", "nina@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{MessageRef: gsTruncatedID + "#msg-1", Sender: "morgan@example.com", At: "2026-06-07T09:00:00Z", BlockRefs: []string{"body"}},
				commitmentMessageEvidence{MessageRef: gsTruncatedID + "#msg-2", Sender: "nina@example.com", At: "2026-06-07T09:05:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed truncated thread: %v", err)
	}

	// (8) round 2 (REVISED after audit — mixed case ACROSS To and Cc, not just
	// a plain-lowercase overlap) — recipients merge: a single well-formed
	// message whose To and Cc overlap on one address in DIFFERENT casing,
	// pinning both the deterministic sorted-dedup union rule AND
	// case-insensitive dedup (DQ-recipients/§2): To=[Bob@example.com,
	// dave@example.com], Cc=[bob@example.com, Alice@EXAMPLE.com] must merge to
	// recipients=[alice@example.com, bob@example.com, dave@example.com]
	// (sorted, lowercased, bob deduped despite the casing mismatch — a
	// same-field-only or case-sensitive dedup would fail this).
	recipientsBody := "From: oscar@example.com\n\nSingle message with overlapping To/Cc recipients."
	if err := writeMemory(cfg, Memory{
		ID: gsRecipientsID, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-recipients-merge",
		Title: "Recipients merge thread", CreatedAt: "2026-06-08T09:00:00Z", Text: recipientsBody,
		Meta: map[string]any{
			"from": []string{"oscar@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{
					MessageRef: gsRecipientsRef, Sender: "oscar@example.com",
					To: []string{"Bob@example.com", "dave@example.com"},
					Cc: []string{"bob@example.com", "Alice@EXAMPLE.com"},
					At: "2026-06-08T09:00:00Z", BlockRefs: []string{"body"},
				},
			),
		},
	}); err != nil {
		t.Fatalf("seed recipients-merge thread: %v", err)
	}

	// (9) round 2 — a long single-message thread for the "bounded adjacent
	// context" read_memory test (DQ6/§2): the target marker sits between two
	// unique near-markers, themselves buried inside wide filler on both
	// sides, so a correctly-bounded evidence_ref+match+max_tokens read must
	// return SURROUNDING text (at least one near-marker), not just the bare
	// matched phrase. boundedFillerWords is the #242 contract's own helper
	// (read_memory_bounded_test.go), reused here rather than a second
	// filler-word generator.
	longSegBody := "From: pia@example.com\n\n" + boundedFillerWords(300) + " " + gsLongSegNearBefore + " " +
		gsLongSegMarker + " " + gsLongSegNearAfter + " " + boundedFillerWords(300)
	if err := writeMemory(cfg, Memory{
		ID: gsLongSegID, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-long-segment",
		Title: "Long segment thread", CreatedAt: "2026-06-09T09:00:00Z", Text: longSegBody,
		Meta: map[string]any{
			"from": []string{"pia@example.com"},
			"messages": gmailSegMessages(
				commitmentMessageEvidence{MessageRef: gsLongSegRef, Sender: "pia@example.com", At: "2026-06-09T09:00:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed long-segment thread: %v", err)
	}

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// 1. Index projection: tables exist, created inside the rebuild transaction.
// ---------------------------------------------------------------------------

// TestGmailSegmentsContractIndexTablesCreated — CONTRACT, RED today.
func TestGmailSegmentsContractIndexTablesCreated(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	if !gmailSegFTSTableExists(t, cfg) {
		t.Fatalf("gmail_segments_fts table does not exist after rebuildIndex")
	}
	// Round 2, second AND fifth audit passes: existence of a same-named
	// table is not proof of SHAPE — assert the sqlite_master DDL actually
	// declares a virtual FTS5 table (VIRTUAL TABLE + fts5 tokens), THEN parse
	// the exact column-definition list and assert it is EXACTLY the frozen
	// DQ1 shape — fts5(evidence_ref UNINDEXED, text) — not merely that both
	// column names occur somewhere in the DDL text (which a wrong-order,
	// wrong-indexing, or extra-column shape would also satisfy).
	ddl, ok := gmailSegFTSTableDDL(t, cfg)
	if !ok {
		t.Fatalf("gmail_segments_fts has no sqlite_master DDL entry")
	}
	upperDDL := strings.ToUpper(ddl)
	if !strings.Contains(upperDDL, "VIRTUAL TABLE") {
		t.Errorf("gmail_segments_fts DDL is not a VIRTUAL TABLE: %s", ddl)
	}
	if !strings.Contains(upperDDL, "FTS5") {
		t.Errorf("gmail_segments_fts DDL does not use fts5: %s", ddl)
	}
	cols := gmailSegFTSColumnSignature(t, ddl)
	if len(cols) != 2 {
		t.Fatalf("gmail_segments_fts column list = %v (%d columns), want EXACTLY 2: evidence_ref UNINDEXED, text — DDL: %s", cols, len(cols), ddl)
	}
	col0Fields := strings.Fields(cols[0])
	if len(col0Fields) == 0 || !strings.EqualFold(col0Fields[0], "evidence_ref") {
		t.Errorf("first fts5 column = %q, want it named evidence_ref (DQ1's frozen order)", cols[0])
	}
	if !strings.Contains(strings.ToUpper(cols[0]), "UNINDEXED") {
		t.Errorf("first fts5 column = %q, want it declared UNINDEXED (DQ1)", cols[0])
	}
	if !strings.EqualFold(strings.TrimSpace(cols[1]), "text") {
		t.Errorf("second fts5 column = %q, want EXACTLY %q (no qualifiers — DQ1's frozen shape)", cols[1], "text")
	}

	rows := gmailSegRowsFor(t, cfg, gsWellFormedID)
	if len(rows) == 0 {
		t.Fatalf("gmail_segments has zero rows for a well-formed 2-message thread")
	}
}

// TestGmailSegmentsContractDiagnosticsSchemaHasNoContentColumns — CONTRACT,
// RED today (round 2, second audit pass). gmailSegDiagnosticFor SELECTs only
// four known-safe columns, so it can never by itself prove the absence of a
// content column — it only proves one was not SELECTED. This asserts the
// TABLE SCHEMA itself (PRAGMA table_info) has EXACTLY {memory_id, reason,
// meta_count, body_count} — no fifth column (e.g. an accidental "text" or
// "snippet") can exist undetected, closing the frozen interface's
// content-free diagnostics requirement at the schema level, not the query
// level.
func TestGmailSegmentsContractDiagnosticsSchemaHasNoContentColumns(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)

	cols := gmailSegDiagnosticsSchemaColumns(t, cfg)
	if len(cols) == 0 {
		t.Fatalf("gmail_segment_diagnostics table missing (PRAGMA table_info returned zero columns)")
	}
	want := map[string]bool{"memory_id": true, "reason": true, "meta_count": true, "body_count": true}
	if len(cols) != len(want) {
		t.Fatalf("gmail_segment_diagnostics has %d columns %v, want exactly %d: memory_id, reason, meta_count, body_count", len(cols), cols, len(want))
	}
	for _, c := range cols {
		if !want[c] {
			t.Fatalf("gmail_segment_diagnostics has an unexpected column %q (content leak risk) — full column set: %v", c, cols)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Row shape for a well-formed thread.
// ---------------------------------------------------------------------------

// TestGmailSegmentsContractWellFormedRowShape — CONTRACT, RED today. Pins:
// exactly one row per message, evidence_ref == MessageRef verbatim, ordered
// by evidence_ref, sender/at carried through, each row's text contains ONLY
// its own message's marker (never the other message's).
func TestGmailSegmentsContractWellFormedRowShape(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	rows := gmailSegRowsFor(t, cfg, gsWellFormedID)
	if len(rows) != 2 {
		t.Fatalf("gmail_segments rows for %s = %d, want 2 (one per message)", gsWellFormedID, len(rows))
	}
	if rows[0].EvidenceRef != gsWellMsg1Ref || rows[1].EvidenceRef != gsWellMsg2Ref {
		t.Fatalf("evidence_ref values = [%s, %s], want [%s, %s] (verbatim MessageRef, sorted)",
			rows[0].EvidenceRef, rows[1].EvidenceRef, gsWellMsg1Ref, gsWellMsg2Ref)
	}
	for _, r := range rows {
		if r.MemoryID != gsWellFormedID {
			t.Errorf("row %s memory_id = %q, want %q", r.EvidenceRef, r.MemoryID, gsWellFormedID)
		}
	}
	if rows[0].Sender != "alice@example.com" || rows[1].Sender != "bob@example.com" {
		t.Errorf("sender values = [%s, %s], want [alice@example.com, bob@example.com]", rows[0].Sender, rows[1].Sender)
	}
	if !strings.Contains(rows[0].Text, gsAlphaMarker) {
		t.Errorf("msg-1 segment text missing its own marker: %q", rows[0].Text)
	}
	if strings.Contains(rows[0].Text, gsBetaMarker) {
		t.Errorf("msg-1 segment text leaked msg-2's marker (misattribution): %q", rows[0].Text)
	}
	if !strings.Contains(rows[1].Text, gsBetaMarker) {
		t.Errorf("msg-2 segment text missing its own marker: %q", rows[1].Text)
	}
	if strings.Contains(rows[1].Text, gsAlphaMarker) {
		t.Errorf("msg-2 segment text leaked msg-1's marker (misattribution): %q", rows[1].Text)
	}
	// DQ-occurrence-time (§2): "at" is gmailMessageEvidence.At VERBATIM — the
	// exact meta.messages[i].At string, never re-derived or reformatted.
	if rows[0].At != "2026-06-01T10:00:00Z" {
		t.Errorf("msg-1 segment at = %q, want the verbatim meta.messages[0].At %q", rows[0].At, "2026-06-01T10:00:00Z")
	}
	if rows[1].At != "2026-06-01T10:05:00Z" {
		t.Errorf("msg-2 segment at = %q, want the verbatim meta.messages[1].At %q", rows[1].At, "2026-06-01T10:05:00Z")
	}
	// Acceptance criterion "two messages ... remain independently addressable
	// by distinct stable refs" — the row-shape half: two rows, two DISTINCT
	// evidence_ref primary keys, never collapsed to one.
	if rows[0].EvidenceRef == rows[1].EvidenceRef {
		t.Fatalf("msg-1 and msg-2 collapsed onto the same evidence_ref %q", rows[0].EvidenceRef)
	}
}

// ---------------------------------------------------------------------------
// 3. Rebuild determinism.
// ---------------------------------------------------------------------------

// gmailSegStableRow is the STABLE, content-derived subset of one search_memory
// result row used by the rebuild-determinism comparison below — deliberately
// excludes health/freshness (indexed_at and friends legitimately advance
// between two real rebuilds even against an unchanged vault) so the
// comparison can never be flaky or vacuously loose from including a clock.
type gmailSegStableRow struct {
	ID       string         `json:"id"`
	Score    float64        `json:"score"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

// gmailSegStableSearchSnapshot runs a search_memory query and extracts ONLY
// id/score/evidence per row, IN RANKED ORDER, for a rebuild-determinism
// byte-diff — never the full envelope (which carries health/freshness).
func gmailSegStableSearchSnapshot(t *testing.T, cfg Config, query string) []gmailSegStableRow {
	t.Helper()
	_ = cfg
	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+query+`","limit":5}`))
	rows := resultRows(t, res)
	out := make([]gmailSegStableRow, 0, len(rows))
	for _, row := range rows {
		score, _ := row["score"].(float64)
		ev, _ := row["evidence"].(map[string]any)
		out = append(out, gmailSegStableRow{ID: rowID(t, row), Score: score, Evidence: ev})
	}
	return out
}

// TestGmailSegmentsContractDeterministicRebuild — CONTRACT, RED today (fails
// at the missing-table wall on the FIRST rebuild's row fetch, per the shared
// helper's Fatal-on-missing-table policy). Pins the FULL acceptance criterion
// ("Full index deletion/rebuild produces byte-identical segment rows and
// rankings", §4) — round 2, TWICE-REVISED after audit:
//   - first revision: (a) the index.db file (+ -wal/-shm sidecars) is REMOVED
//     outright between the two rebuilds — the issue's own wording is
//     "deleting index.db and rebuilding from the vault must reproduce every
//     segment byte-for-byte", which a within-process DELETE-then-reinsert
//     rebuild does not fully exercise (it never proves reconstruction from a
//     genuinely cold/absent index file); (b) the comparison is restricted to
//     STABLE fields only — never health/freshness, which legitimately vary
//     between rebuilds and would otherwise make a full-envelope diff flaky or
//     vacuously loose.
//   - second revision (this one): the first revision still only diffed ONE
//     memory's gmail_segments rows (gsWellFormedID) and a single-hit query
//     (gsBetaMarker matches exactly one parent) — a one-result comparison
//     cannot prove ranked ORDERING survives a rebuild at all (there is
//     nothing to order). Fixed: gmailSegAllRows snapshots EVERY segment row
//     across the WHOLE vault (multiple parents), and the search snapshot now
//     uses gmailSegRebuildOrderProbe — a term planted in THREE distinct
//     memories — so the comparison is a genuine ordered, multi-result,
//     multi-parent sequence both times.
func TestGmailSegmentsContractDeterministicRebuild(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t) // already rebuilt once by the seed helper

	// (a) the WHOLE gmail_segments table, not just one memory's rows — every
	// parent's segments must be identically reconstructed.
	first := gmailSegAllRows(t, cfg)
	if len(first) < 3 {
		t.Fatalf("baseline gmail_segments has only %d rows across the whole vault — too few to prove multi-parent determinism (want at least 3: gsWellFormedID contributes 2 rows alone, plus gsCrossOtherID/gsRecipientsID/gsLongSegID each contribute 1 more)", len(first))
	}

	// (b) a genuine multi-parent, multi-result RANKED query — a one-result
	// query cannot exercise "does ORDERING survive a rebuild" at all.
	// gmailSegRebuildOrderProbe is planted in three distinct memories'
	// bodies (see seedGmailSegmentsFixture).
	firstStable := gmailSegStableSearchSnapshot(t, cfg, gmailSegRebuildOrderProbe)
	if len(firstStable) < 3 {
		t.Fatalf("baseline probe query returned %d results, want at least 3 (gsWellFormedID, gsCountMismatch, gsCrossOtherID all carry the probe term) — cannot prove ranked ordering survives a rebuild with fewer than 2 comparable positions", len(firstStable))
	}

	if err := os.Remove(dbPath(cfg)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove index.db: %v", err)
	}
	_ = os.Remove(dbPath(cfg) + "-wal")
	_ = os.Remove(dbPath(cfg) + "-shm")

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuild after index.db deletion: %v", err)
	}
	second := gmailSegAllRows(t, cfg)
	secondStable := gmailSegStableSearchSnapshot(t, cfg, gmailSegRebuildOrderProbe)

	b1, _ := json.Marshal(first)
	b2, _ := json.Marshal(second)
	if string(b1) != string(b2) {
		t.Fatalf("gmail_segments (ALL rows, whole vault) are not byte-identical across index.db deletion+rebuild:\n%s\n---\n%s", b1, b2)
	}
	s1, _ := json.Marshal(firstStable)
	s2, _ := json.Marshal(secondStable)
	if string(s1) != string(s2) {
		t.Fatalf("search_memory ORDERED rankings/scores/evidence for a multi-parent query (health/freshness excluded) are not byte-identical across index.db deletion+rebuild:\n%s\n---\n%s", s1, s2)
	}
}

// ---------------------------------------------------------------------------
// 4-6. Fail-closed alignment: three distinct malformed shapes, each must
// produce ZERO segments (never a partial/misattributed set), the parent
// memory must remain indexed normally, and a content-free diagnostic must
// name the reason.
// ---------------------------------------------------------------------------

// TestGmailSegmentsContractFailClosedCountMismatch — CONTRACT, RED today.
func TestGmailSegmentsContractFailClosedCountMismatch(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)

	if !memoryStillIndexed(t, cfg, gsCountMismatch) {
		t.Errorf("parent memory %s must remain indexed normally even when segments fail closed", gsCountMismatch)
	}

	rows := gmailSegRowsFor(t, cfg, gsCountMismatch)
	if len(rows) != 0 {
		t.Fatalf("count-mismatch thread produced %d segments, want 0 (meta declares 2 messages, body has 1 block)", len(rows))
	}

	diag, ok := gmailSegDiagnosticFor(t, cfg, gsCountMismatch)
	if !ok {
		t.Fatalf("no gmail_segment_diagnostics row for %s", gsCountMismatch)
	}
	if diag.Reason != "count_mismatch" {
		t.Errorf("diagnostic reason = %q, want %q", diag.Reason, "count_mismatch")
	}
	if diag.MetaCount != 2 || diag.BodyCount != 1 {
		t.Errorf("diagnostic counts = (meta=%d, body=%d), want (meta=2, body=1)", diag.MetaCount, diag.BodyCount)
	}
}

// TestGmailSegmentsContractFailClosedLiteralSeparatorInBody — CONTRACT, RED
// today. The literal "---" line inside message 2's own body must not be
// silently absorbed by a naive zip-by-index; the whole memory fails closed,
// never a partial 2-of-3 attribution.
func TestGmailSegmentsContractFailClosedLiteralSeparatorInBody(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)

	if !memoryStillIndexed(t, cfg, gsLiteralDash) {
		t.Errorf("parent memory %s must remain indexed normally even when segments fail closed", gsLiteralDash)
	}

	rows := gmailSegRowsFor(t, cfg, gsLiteralDash)
	if len(rows) != 0 {
		t.Fatalf("literal-separator thread produced %d segments, want 0 (a literal \"---\" line must not silently misattribute)", len(rows))
	}

	diag, ok := gmailSegDiagnosticFor(t, cfg, gsLiteralDash)
	if !ok {
		t.Fatalf("no gmail_segment_diagnostics row for %s", gsLiteralDash)
	}
	if diag.Reason != "count_mismatch" {
		t.Errorf("diagnostic reason = %q, want %q (a literal separator widens the recovered block count past meta.messages)", diag.Reason, "count_mismatch")
	}
	if diag.MetaCount != 2 || diag.BodyCount != 3 {
		t.Errorf("diagnostic counts = (meta=%d, body=%d), want (meta=2, body=3)", diag.MetaCount, diag.BodyCount)
	}

	// The diagnostic must never carry memory content (frozen interface #2):
	// the unique marker planted inside the corrupted message body must not
	// leak into the diagnostic row's own text representation.
	diagJSON, _ := json.Marshal(diag)
	if strings.Contains(string(diagJSON), "GMLSEGLITERALDASHMARKER") {
		t.Fatalf("gmail_segment_diagnostics row leaked memory content: %s", diagJSON)
	}
}

// TestGmailSegmentsContractFailClosedMalformedRef — CONTRACT, RED today. Both
// messages must be refused together (whole-memory fail-closed) even though
// the FIRST message's ref is perfectly well-formed — a partial derivation
// (segment 1 only) would be just as much a silent-misattribution risk as
// segment counting is, once a thread's ref integrity cannot be trusted.
func TestGmailSegmentsContractFailClosedMalformedRef(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)

	if !memoryStillIndexed(t, cfg, gsMalformedRef) {
		t.Errorf("parent memory %s must remain indexed normally even when segments fail closed", gsMalformedRef)
	}

	rows := gmailSegRowsFor(t, cfg, gsMalformedRef)
	if len(rows) != 0 {
		t.Fatalf("malformed-ref thread produced %d segments, want 0 (msg-2's ref names a different thread)", len(rows))
	}

	diag, ok := gmailSegDiagnosticFor(t, cfg, gsMalformedRef)
	if !ok {
		t.Fatalf("no gmail_segment_diagnostics row for %s", gsMalformedRef)
	}
	if diag.Reason != "malformed_ref" {
		t.Errorf("diagnostic reason = %q, want %q (counts matched; the ref itself is invalid)", diag.Reason, "malformed_ref")
	}
	if diag.MetaCount != 2 || diag.BodyCount != 2 {
		t.Errorf("diagnostic counts = (meta=%d, body=%d), want (meta=2, body=2) — counts alone are not the failure here", diag.MetaCount, diag.BodyCount)
	}
}

// TestGmailSegmentsContractFailClosedOrderingMismatch — CONTRACT, RED today
// (round 2, REVISED after audit). Counts match (meta_count=2, body_count=2)
// — this must be reported as "ordering_mismatch", NOT "count_mismatch" — and
// meta.messages[].At IS monotonically non-decreasing (09:00 < 09:05), so a
// timestamp-based signal is deliberately NOT what makes this fixture fail:
// the corruption is that meta.messages[0].Sender/[1].Sender are SWAPPED
// relative to which address actually authored the block at that position
// (block 0's own "From:" header is kate, block 1's is liam, but meta declares
// them the other way around) — DQ2/§2's revised, content-grounded
// ordering_mismatch signal.
func TestGmailSegmentsContractFailClosedOrderingMismatch(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)

	if !memoryStillIndexed(t, cfg, gsOrderingMismatch) {
		t.Errorf("parent memory %s must remain indexed normally even when segments fail closed", gsOrderingMismatch)
	}

	rows := gmailSegRowsFor(t, cfg, gsOrderingMismatch)
	if len(rows) != 0 {
		t.Fatalf("ordering-mismatch thread produced %d segments, want 0 (declared sender identity does not match the rendered block's own From: header at that position)", len(rows))
	}

	diag, ok := gmailSegDiagnosticFor(t, cfg, gsOrderingMismatch)
	if !ok {
		t.Fatalf("no gmail_segment_diagnostics row for %s", gsOrderingMismatch)
	}
	if diag.Reason != "ordering_mismatch" {
		t.Errorf("diagnostic reason = %q, want %q (counts matched; only the declared order is broken)", diag.Reason, "ordering_mismatch")
	}
	if diag.MetaCount != 2 || diag.BodyCount != 2 {
		t.Errorf("diagnostic counts = (meta=%d, body=%d), want (meta=2, body=2) — this is NOT a count failure", diag.MetaCount, diag.BodyCount)
	}
}

// TestGmailSegmentsContractFailClosedTruncatedBody — CONTRACT, RED today
// (round 2 addition). Issue fail-closed requirement: "Do not index metadata
// entries whose rendered text was removed by body truncation." Counts match
// (meta_count=2, body_count=2) — the truncated check must run independently
// of, and take priority over, the count check (DQ2/§2's priority order 1).
func TestGmailSegmentsContractFailClosedTruncatedBody(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)

	if !memoryStillIndexed(t, cfg, gsTruncatedID) {
		t.Errorf("parent memory %s must remain indexed normally even when segments fail closed", gsTruncatedID)
	}

	rows := gmailSegRowsFor(t, cfg, gsTruncatedID)
	if len(rows) != 0 {
		t.Fatalf("truncated-body thread produced %d segments, want 0 (Memory.Truncated=true — boundaries cannot be trusted)", len(rows))
	}

	diag, ok := gmailSegDiagnosticFor(t, cfg, gsTruncatedID)
	if !ok {
		t.Fatalf("no gmail_segment_diagnostics row for %s", gsTruncatedID)
	}
	if diag.Reason != "truncated" {
		t.Errorf("diagnostic reason = %q, want %q (a matching count must not mask a truncated body)", diag.Reason, "truncated")
	}
}

// TestGmailSegmentsContractRecipientsMergeDeterministicDedup — CONTRACT, RED
// today (round 2, REVISED after audit — mixed case ACROSS To and Cc).
// DQ-recipients/§2: recipients is the sorted, lowercased, case-INSENSITIVE
// deduped union of To and Cc — To=[Bob@example.com,dave@example.com] +
// Cc=[bob@example.com,Alice@EXAMPLE.com] must merge to exactly
// [alice@example.com,bob@example.com,dave@example.com]; a same-field-only or
// case-sensitive dedup would leave "Bob@example.com" and "bob@example.com" as
// two separate entries and fail this test.
func TestGmailSegmentsContractRecipientsMergeDeterministicDedup(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)

	rows := gmailSegRowsFor(t, cfg, gsRecipientsID)
	if len(rows) != 1 {
		t.Fatalf("gmail_segments rows for %s = %d, want 1", gsRecipientsID, len(rows))
	}
	var got []string
	if err := json.Unmarshal([]byte(rows[0].Recipients), &got); err != nil {
		t.Fatalf("recipients column is not a JSON array: %q (%v)", rows[0].Recipients, err)
	}
	want := []string{"alice@example.com", "bob@example.com", "dave@example.com"}
	if len(got) != len(want) {
		t.Fatalf("recipients = %v, want %v (sorted union, bob deduped across To/Cc)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recipients = %v, want %v (sorted union, bob deduped across To/Cc)", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 6b. Duplicate MessageRef (round 3, supervisor-authorized amendment —
// reviewer-reproduced P0). A memory whose meta.messages repeats the SAME
// MessageRef across two or more messages would otherwise collide on
// gmail_segments' evidence_ref PRIMARY KEY: a real SQL UNIQUE-constraint
// error that, unless caught BEFORE any INSERT runs, aborts the WHOLE
// rebuild transaction — not just this one memory's segments. That is a
// second, more severe failure mode than DQ2's original four reasons (which
// never touch the database: they all return before any INSERT), so this
// gets its OWN diagnostic reason, "duplicate_ref" (an integrator decision:
// distinct reason, not folded into malformed_ref, since a duplicate ref can
// be individually well-formed — it fails on a SET property, not a per-ref
// one). This is a genuinely isolated fixture (its own vault), deliberately
// NOT folded into seedGmailSegmentsFixture: until the #243 fix lands, a
// duplicate ref aborts rebuildIndex outright, which would take down every
// OTHER test sharing that fixture's single rebuild — not what this fixture
// exists to prove.
// ---------------------------------------------------------------------------

const (
	gsDuplicateRefID  = "gmail_thread/th-duplicate-ref"
	gsDuplicateRefRef = gsDuplicateRefID + "#msg-1" // repeated across BOTH messages below
	gsBystanderID     = "note/duplicate-ref-bystander"
	gsBystanderMarker = "GMLSEGBYSTANDERMARKEREIGHT"
)

// seedGmailSegmentsDuplicateRefFixture seeds ONE offending Gmail thread
// (both messages declare the identical MessageRef) alongside ONE innocent,
// ordinary, non-Gmail bystander memory, in the same vault, then rebuilds.
// Until the #243 duplicate-ref fail-closed fix lands, rebuildIndex itself
// fails here (the UNIQUE-constraint error) — this t.Fatalf IS the RED
// signal for every test below that calls this seed helper, exactly like
// every other gmail_segment_* helper's missing-table RED signal (never a
// t.Skip).
func seedGmailSegmentsDuplicateRefFixture(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	dupBody := gmailSegJoinBody(
		[2]string{"olga@example.com", "First message, correctly formed on its own."},
		[2]string{"peter@example.com", "Second message, but declares the SAME MessageRef as the first."},
	)
	if err := writeMemory(cfg, Memory{
		ID: gsDuplicateRefID, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-duplicate-ref",
		Title: "Duplicate ref thread", CreatedAt: "2026-06-10T09:00:00Z", Text: dupBody,
		Meta: map[string]any{
			"from": []string{"olga@example.com", "peter@example.com"},
			"messages": gmailSegMessages(
				// Both messages declare gsDuplicateRefRef — the fixture's only defect.
				commitmentMessageEvidence{MessageRef: gsDuplicateRefRef, Sender: "olga@example.com", At: "2026-06-10T09:00:00Z", BlockRefs: []string{"body"}},
				commitmentMessageEvidence{MessageRef: gsDuplicateRefRef, Sender: "peter@example.com", At: "2026-06-10T09:05:00Z", BlockRefs: []string{"body"}},
			),
		},
	}); err != nil {
		t.Fatalf("seed duplicate-ref thread: %v", err)
	}

	// Innocent bystander — an ordinary, well-formed, non-Gmail memory sharing
	// the SAME vault/rebuild as the offender, so the survival pin below can
	// prove the offender's failure never touches anything else.
	if err := writeMemory(cfg, Memory{
		ID: gsBystanderID, Scope: "personal", Type: "note", Source: "filesystem",
		Title: "Bystander note", CreatedAt: "2026-06-10T09:10:00Z",
		Text: gsBystanderMarker + " an ordinary note sharing the vault with the offender.",
	}); err != nil {
		t.Fatalf("seed bystander note: %v", err)
	}

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	return cfg
}

// TestGmailSegmentsContractFailClosedDuplicateRef — CONTRACT, RED today (the
// seed helper's own rebuildIndex fails before any assertion below even
// runs). Pins the diagnostic shape once the P0 fix lands: zero segments,
// reason="duplicate_ref", counts (meta=2, body=2) — this is explicitly NOT a
// count or ordering failure, only a duplicate-ref one.
func TestGmailSegmentsContractFailClosedDuplicateRef(t *testing.T) {
	cfg := seedGmailSegmentsDuplicateRefFixture(t)

	if !memoryStillIndexed(t, cfg, gsDuplicateRefID) {
		t.Errorf("parent memory %s must remain indexed normally even when segments fail closed", gsDuplicateRefID)
	}

	rows := gmailSegRowsFor(t, cfg, gsDuplicateRefID)
	if len(rows) != 0 {
		t.Fatalf("duplicate-ref thread produced %d segments, want 0 (both messages declare the SAME MessageRef)", len(rows))
	}

	diag, ok := gmailSegDiagnosticFor(t, cfg, gsDuplicateRefID)
	if !ok {
		t.Fatalf("no gmail_segment_diagnostics row for %s", gsDuplicateRefID)
	}
	if diag.Reason != "duplicate_ref" {
		t.Errorf("diagnostic reason = %q, want %q (a distinct reason from malformed_ref — an integrator decision, since a duplicated ref can be individually well-formed)", diag.Reason, "duplicate_ref")
	}
	if diag.MetaCount != 2 || diag.BodyCount != 2 {
		t.Errorf("diagnostic counts = (meta=%d, body=%d), want (meta=2, body=2) — this is NOT a count or ordering failure", diag.MetaCount, diag.BodyCount)
	}
}

// TestGmailSegmentsContractDuplicateRefBystanderSurvives — CONTRACT, RED
// today (the P0 itself: without the fix, seedGmailSegmentsDuplicateRefFixture's
// own rebuildIndex call fails with the UNIQUE-constraint error, so the WHOLE
// vault — offender AND bystander alike — never gets indexed at all). This is
// the reviewer's scratch repro, promoted to a named contract pin: a
// duplicate-ref offender sharing a vault with an ordinary memory must never
// take the bystander down with it. rebuildIndex must SUCCEED, and the
// bystander must be fully indexed and searchable via the real MCP
// search_memory surface — not merely present in the memories table.
func TestGmailSegmentsContractDuplicateRefBystanderSurvives(t *testing.T) {
	cfg := seedGmailSegmentsDuplicateRefFixture(t)

	if !memoryStillIndexed(t, cfg, gsBystanderID) {
		t.Fatalf("bystander memory %s did not survive a rebuild sharing its vault with a duplicate-ref offender", gsBystanderID)
	}

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsBystanderMarker+`","limit":5}`))
	rows := resultRows(t, res)
	var found bool
	for _, row := range rows {
		if rowID(t, row) == gsBystanderID {
			found = true
		}
	}
	if !found {
		t.Fatalf("bystander memory %s is not searchable via search_memory after a duplicate-ref offender shared its vault (rebuild must have partially or fully failed): %v", gsBystanderID, rows)
	}
}

// ---------------------------------------------------------------------------
// 7. Byte-identity: a memory with no segment participation is untouched.
// ---------------------------------------------------------------------------

// TestGmailSegmentsContractByteIdenticalWithoutSegmentParticipation — PIN,
// GREEN today (and must stay green post-implementation). A plain non-gmail
// memory and a legacy gmail memory with no meta.messages at all must produce
// search_memory / read_memory output with no "evidence"/"gmail_segments"
// vocabulary anywhere — the frozen interface's byte-identity guarantee.
func TestGmailSegmentsContractByteIdenticalWithoutSegmentParticipation(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	const plainID = "note/plain-memory"
	if err := writeMemory(cfg, Memory{
		ID: plainID, Scope: "personal", Type: "note", Source: "filesystem",
		Title: "GMLSEGBYTEIDENTITYMARKER plain note", CreatedAt: "2026-06-06T09:00:00Z",
		Text: "A perfectly ordinary note with no Gmail structure at all.",
	}); err != nil {
		t.Fatalf("seed plain memory: %v", err)
	}

	const legacyGmailID = "gmail_thread/th-legacy-no-messages"
	if err := writeMemory(cfg, Memory{
		ID: legacyGmailID, Scope: "personal", Type: "email", Source: "gmail",
		Provider: "gmail", ProviderID: "thread/th-legacy-no-messages",
		Title: "GMLSEGBYTEIDENTITYMARKER legacy thread", CreatedAt: "2026-06-06T09:05:00Z",
		Text: "From: irene@example.com\n\nA pre-PR1 gmail memory carrying no meta.messages at all.",
		Meta: map[string]any{"from": []string{"irene@example.com"}},
	}); err != nil {
		t.Fatalf("seed legacy gmail memory: %v", err)
	}

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}

	for _, id := range []string{plainID, legacyGmailID} {
		res := mcpResult(t, budgetCall("search_memory", `{"query":"GMLSEGBYTEIDENTITYMARKER","limit":5}`))
		rows := resultRows(t, res)
		var found map[string]any
		for _, row := range rows {
			if rowID(t, row) == id {
				found = row
			}
		}
		if found == nil {
			t.Fatalf("search_memory did not return %s at all", id)
		}
		if _, has := found["evidence"]; has {
			t.Errorf("%s: search_memory row carries an unexpected 'evidence' key: %#v", id, found)
		}
		if envelopeContainsKey(t, res, "gmail_segments") {
			t.Errorf("%s: search_memory envelope leaked 'gmail_segments' vocabulary: %v", id, res)
		}

		raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{"id": id})
		if err != nil {
			t.Fatalf("mcpReadMemory(%s): %v", id, err)
		}
		b, _ := json.Marshal(raw)
		var generic map[string]any
		if err := json.Unmarshal(b, &generic); err != nil {
			t.Fatalf("unmarshal read_memory result: %v", err)
		}
		keys := make([]string, 0, len(generic))
		for k := range generic {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) != 2 || keys[0] != "health" || keys[1] != "memory" {
			t.Errorf("%s: parameter-free read_memory top-level keys = %v, want exactly [health memory]", id, keys)
		}
	}
}

// ---------------------------------------------------------------------------
// Fixture 2: search-behavior fixture — evidence attachment, one-slot-per-
// thread, tie-break, no-evidence, and the buried-message recall win.
// ---------------------------------------------------------------------------

const (
	gsSearchWellFormedID = "gmail_thread/th-search-wellformed"
	gsSearchMsg1Ref      = gsSearchWellFormedID + "#gs-1"
	gsSearchMsg2Ref      = gsSearchWellFormedID + "#gs-2"
	gsSearchAlpha        = "GMLSEGSEARCHALPHAMARKER"
	gsSearchBeta         = "GMLSEGSEARCHBETAMARKER"
	gsSearchShared       = "GMLSEGSEARCHSHAREDTERM"
	gsTitleOnlyMarker    = "GMLSEGTITLEONLYMARKERFIVE"

	gsTieID   = "gmail_thread/th-tie"
	gsTieRef1 = gsTieID + "#gt-1"
	gsTieRef2 = gsTieID + "#gt-2"
	gsTieMark = "GMLSEGTIEMARKERTHREE"

	gsBuriedID     = "gmail_thread/th-buried"
	gsBuriedMarker = "GMLSEGBURIEDMARKERFOUR"

	// round 2, fourth audit pass (DQ5, §2): the deterministic co-match
	// positive pin — the query term sits in BOTH the title (parent-grain
	// match) AND msg-1's body (a query-matching segment); msg-2 carries
	// neither, so the strongest — indeed the ONLY — matching segment is
	// unambiguously msg-1.
	gsCoMatchID      = "gmail_thread/th-co-match"
	gsCoMatchMsg1Ref = gsCoMatchID + "#cm-1"
	gsCoMatchMsg2Ref = gsCoMatchID + "#cm-2"
	gsCoMatchMarker  = "GMLSEGCOMATCHMARKER"
)

func seedGmailSegmentsSearchFixture(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	seedGmail := func(id, providerID, title, createdAt string, msgs []commitmentMessageEvidence, joined string, from []string) {
		t.Helper()
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "personal", Type: "email", Source: "gmail",
			Provider: "gmail", ProviderID: providerID,
			Title: title, CreatedAt: createdAt, Text: joined,
			Meta: map[string]any{"from": from, "messages": msgs},
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Evidence-attachment + one-slot-per-thread fixture: msg-1 carries ALPHA
	// + the shared term, msg-2 carries BETA + the shared term; the title
	// carries a marker that appears in NEITHER message body (for the
	// no-evidence-on-title-only-match pin).
	seedGmail(gsSearchWellFormedID, "thread/th-search-wellformed",
		"Project Kickoff Sync "+gsTitleOnlyMarker, "2026-06-10T10:00:00Z",
		[]commitmentMessageEvidence{
			{MessageRef: gsSearchMsg1Ref, Sender: "alice@example.com", At: "2026-06-10T10:00:00Z", BlockRefs: []string{"body"}},
			{MessageRef: gsSearchMsg2Ref, Sender: "bob@example.com", At: "2026-06-10T10:05:00Z", BlockRefs: []string{"body"}},
		},
		gmailSegJoinBody(
			[2]string{"alice@example.com", "Quick note about the kickoff. " + gsSearchAlpha + " " + gsSearchShared + " should be ready by Monday."},
			[2]string{"bob@example.com", "Following up here. " + gsSearchBeta + " " + gsSearchShared + " deck is attached for review."},
		),
		[]string{"alice@example.com", "bob@example.com"})

	// Tie-break fixture: two messages with BYTE-IDENTICAL bodies (same term,
	// same frequency, same length) so their segment-FTS scores tie exactly;
	// evidence must resolve to the lexicographically smaller evidence_ref
	// (gt-1 < gt-2).
	tieBody := "Status update. " + gsTieMark + " confirmed."
	seedGmail(gsTieID, "thread/th-tie", "Status Sync Thread", "2026-06-11T10:00:00Z",
		[]commitmentMessageEvidence{
			{MessageRef: gsTieRef1, Sender: "carol@example.com", At: "2026-06-11T10:00:00Z", BlockRefs: []string{"body"}},
			{MessageRef: gsTieRef2, Sender: "dave@example.com", At: "2026-06-11T10:05:00Z", BlockRefs: []string{"body"}},
		},
		gmailSegJoinBody([2]string{"carol@example.com", tieBody}, [2]string{"dave@example.com", tieBody}),
		[]string{"carol@example.com", "dave@example.com"})

	// Buried-message fixture: a 3-message thread whose overall joined body is
	// long (two ~150-word filler messages), with the marker appearing exactly
	// ONCE inside the short middle message. Four short decoy notes (NOT
	// gmail, no meta — never cluster, never carry segments) also contain the
	// marker once each, padded to be noticeably longer than the buried
	// message's own segment text but much shorter than the whole diluted
	// thread — this is what parent-grain BM25's length normalization buries:
	// the decoys outscore the long thread on the SAME single-occurrence term.
	filler := func(n int, word string) string {
		return strings.TrimSpace(strings.Repeat(word+" ", n))
	}
	buriedBody := gmailSegJoinBody(
		[2]string{"erin@example.com", "Weekly roundup. " + filler(150, "padding")},
		[2]string{"frank@example.com", "Reminder: " + gsBuriedMarker + " deadline is Friday."},
		[2]string{"grace@example.com", "Additional notes. " + filler(150, "padding")},
	)
	seedGmail(gsBuriedID, "thread/th-buried", "Weekly Roundup Thread", "2026-06-12T10:00:00Z",
		[]commitmentMessageEvidence{
			{MessageRef: gsBuriedID + "#msg-1", Sender: "erin@example.com", At: "2026-06-12T10:00:00Z", BlockRefs: []string{"body"}},
			{MessageRef: gsBuriedID + "#msg-2", Sender: "frank@example.com", At: "2026-06-12T10:05:00Z", BlockRefs: []string{"body"}},
			{MessageRef: gsBuriedID + "#msg-3", Sender: "grace@example.com", At: "2026-06-12T10:10:00Z", BlockRefs: []string{"body"}},
		},
		buriedBody, []string{"erin@example.com", "frank@example.com", "grace@example.com"})

	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("note/buried-decoy-%d", i)
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "personal", Type: "note", Source: "filesystem",
			Title: fmt.Sprintf("Decoy note %d", i), CreatedAt: "2026-06-12T09:00:00Z",
			Text: gsBuriedMarker + " " + filler(20, "decoyfiller"),
		}); err != nil {
			t.Fatalf("seed decoy %d: %v", i, err)
		}
	}

	// Co-match fixture (DQ5/§2, round 2, fourth audit pass): the query term
	// sits in the TITLE (an independent parent-grain match) AND in msg-1's
	// body (a query-matching segment); msg-2 carries neither. Under the
	// DECIDED rule ("any query-matching segment ⇒ evidence of the strongest
	// match"), this is fully deterministic — no arbitration between arms is
	// needed, since the rule only asks "does a segment match", not "who
	// ranked this parent".
	seedGmail(gsCoMatchID, "thread/th-co-match",
		"Co-match Test "+gsCoMatchMarker, "2026-06-14T10:00:00Z",
		[]commitmentMessageEvidence{
			{MessageRef: gsCoMatchMsg1Ref, Sender: "ivan@example.com", At: "2026-06-14T10:00:00Z", BlockRefs: []string{"body"}},
			{MessageRef: gsCoMatchMsg2Ref, Sender: "julia@example.com", At: "2026-06-14T10:05:00Z", BlockRefs: []string{"body"}},
		},
		gmailSegJoinBody(
			[2]string{"ivan@example.com", "Kickoff note mentioning " + gsCoMatchMarker + " once here."},
			[2]string{"julia@example.com", "No shared marker in this second message at all."},
		),
		[]string{"ivan@example.com", "julia@example.com"})

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	return cfg
}

// evidenceObject extracts row["evidence"] as a map, failing if absent.
func evidenceObject(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	ev, ok := row["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("row has no 'evidence' object: %#v", row)
	}
	return ev
}

// ---------------------------------------------------------------------------
// 8-11. Retrieval: evidence attachment, one-slot-per-thread, tie-break,
// no-evidence-when-not-segment-driven.
// ---------------------------------------------------------------------------

// TestGmailSegmentsContractSearchEvidenceAttachesMatchingSegment — CONTRACT,
// RED today. A query that hits ONLY msg-2's segment must attach evidence
// naming msg-2 (evidence_ref/sender/at), with a snippet drawn from that
// segment's own text.
func TestGmailSegmentsContractSearchEvidenceAttachesMatchingSegment(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsSearchBeta+`","limit":5}`))
	rows := resultRows(t, res)
	var found map[string]any
	for _, row := range rows {
		if rowID(t, row) == gsSearchWellFormedID {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("search_memory did not return %s for a query matching its msg-2 segment: %v", gsSearchWellFormedID, rows)
	}
	ev := evidenceObject(t, found)
	if evRef := requireStringField(t, ev, "evidence_ref"); evRef != gsSearchMsg2Ref {
		t.Errorf("evidence.evidence_ref = %q, want %q", evRef, gsSearchMsg2Ref)
	}
	if sender := requireStringField(t, ev, "sender"); sender != "bob@example.com" {
		t.Errorf("evidence.sender = %q, want %q", sender, "bob@example.com")
	}
	// Round 2, second audit pass: exact TYPED value, not mere presence — the
	// verbatim meta.messages[1].At this fixture seeded (DQ-occurrence-time).
	if at := requireStringField(t, ev, "at"); at != "2026-06-10T10:05:00Z" {
		t.Errorf("evidence.at = %q, want the verbatim segment At %q", at, "2026-06-10T10:05:00Z")
	}
	snippet, _ := ev["snippet"].(string)
	if !strings.Contains(snippet, gsSearchBeta) {
		t.Errorf("evidence.snippet = %q, want it to contain the matched term %q", snippet, gsSearchBeta)
	}
	if strings.Contains(snippet, gsSearchAlpha) {
		t.Errorf("evidence.snippet leaked msg-1's marker (wrong segment attributed): %q", snippet)
	}
	keys := make([]string, 0, len(ev))
	for k := range ev {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"at", "evidence_ref", "sender", "snippet"}
	if len(keys) != len(wantKeys) {
		t.Errorf("evidence object keys = %v, want exactly %v", keys, wantKeys)
	}
}

// TestGmailSegmentsContractOneParentSlotRegardlessOfSegmentHitCount — PIN,
// GREEN today (trivially: today's search never emits two rows for one
// memory id regardless of how many terms in it match, since a memory
// contributes at most one row from the `memories` table). It remains a
// REQUIRED guard once segment-grain fan-in exists: a query matching BOTH
// segments of the same thread (the shared term) must still produce exactly
// ONE top-level row for that parent — never two, however many of its
// segments the query hits.
func TestGmailSegmentsContractOneParentSlotRegardlessOfSegmentHitCount(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsSearchShared+`","limit":5}`))
	rows := resultRows(t, res)
	seen := 0
	for _, row := range rows {
		if rowID(t, row) == gsSearchWellFormedID {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("thread %s appeared %d times in results (both segments matched the shared term), want exactly 1", gsSearchWellFormedID, seen)
	}
}

// TestGmailSegmentsContractEvidenceTieBreakLexicographicRef — CONTRACT, RED
// today. Two segments with byte-identical bodies produce an exact score tie;
// the frozen tie-break (lowest evidence_ref lexicographically) must pick
// gt-1, never gt-2, and never vary run to run.
func TestGmailSegmentsContractEvidenceTieBreakLexicographicRef(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	_ = cfg

	line := budgetCall("search_memory", `{"query":"`+gsTieMark+`","limit":5}`)
	var results []string
	for i := 0; i < 3; i++ {
		res := mcpResult(t, line)
		rows := resultRows(t, res)
		var found map[string]any
		for _, row := range rows {
			if rowID(t, row) == gsTieID {
				found = row
			}
		}
		if found == nil {
			t.Fatalf("run %d: search_memory did not return %s", i, gsTieID)
		}
		ev := evidenceObject(t, found)
		ref := requireStringField(t, ev, "evidence_ref")
		results = append(results, ref)
	}
	for i, ref := range results {
		if ref != gsTieRef1 {
			t.Errorf("run %d: evidence.evidence_ref = %q, want the lexicographically smaller %q (deterministic tie-break)", i, ref, gsTieRef1)
		}
	}
}

// TestGmailSegmentsContractNoEvidenceWhenNoSegmentMatches — PIN today
// (vacuously true: no implementation exists, so no row ever carries
// "evidence" yet), and remains a REQUIRED guard once implementation lands.
//
// This is the NO-SEGMENT-MATCH NEGATIVE PIN for DQ5's evidence-attachment
// rule (§2) — nothing more. It is NOT a candidate-source-ownership or
// arbitration proof (round 2's second draft over-claimed that framing; round
// 2 fourth audit pass corrected it: strict winning-arm ownership is DROPPED
// from the frozen rule entirely, see §2 DQ5). The rule this test pins is
// purely "does this parent have at least one query-matching segment": here
// gsTitleOnlyMarker appears ONLY in the parent's TITLE — not in msg-1's
// body, not in msg-2's body, not in ANY segment's text — so there are ZERO
// query-matching segments, and the rule says no "evidence" key attaches. The
// complementary POSITIVE co-match pin (a parent that matches at parent-grain
// AND has a query-matching segment ⇒ evidence attached with the exact
// strongest-segment receipt) is
// TestGmailSegmentsContractSearchEvidenceAttachesOnParentGrainCoMatch, right
// below.
func TestGmailSegmentsContractNoEvidenceWhenNoSegmentMatches(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsTitleOnlyMarker+`","limit":5}`))
	rows := resultRows(t, res)
	var found map[string]any
	for _, row := range rows {
		if rowID(t, row) == gsSearchWellFormedID {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("search_memory did not return %s for a title-only match: %v", gsSearchWellFormedID, rows)
	}
	if _, has := found["evidence"]; has {
		t.Errorf("title-only match must not carry an 'evidence' key (no segment could possibly have matched — the term is absent from every message body): %#v", found)
	}
}

// TestGmailSegmentsContractSearchEvidenceAttachesOnParentGrainCoMatch —
// CONTRACT, RED today (round 2, fourth audit pass). This is the DECIDED,
// deterministic POSITIVE co-match pin for DQ5's evidence-attachment rule
// (§2): the query term sits in BOTH the title (an independent parent-grain
// match — memories_fts(title,...)) AND msg-1's body (a query-matching
// segment). Under the frozen rule ("any query-matching segment ⇒ evidence of
// its strongest match"), evidence MUST attach here — the parent has at least
// one query-matching segment, full stop, regardless of the fact that it ALSO
// independently matches at parent-grain via the title. No arbitration
// between arms, no scoring-magnitude assumption: msg-1 is the ONLY matching
// segment (msg-2 carries neither the title marker nor any other match), so
// it is trivially both the "strongest" and the sole candidate.
func TestGmailSegmentsContractSearchEvidenceAttachesOnParentGrainCoMatch(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsCoMatchMarker+`","limit":5}`))
	rows := resultRows(t, res)
	var found map[string]any
	for _, row := range rows {
		if rowID(t, row) == gsCoMatchID {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("search_memory did not return %s for a term matching both its title and msg-1's segment: %v", gsCoMatchID, rows)
	}
	ev := evidenceObject(t, found)
	if evRef := requireStringField(t, ev, "evidence_ref"); evRef != gsCoMatchMsg1Ref {
		t.Errorf("evidence.evidence_ref = %q, want %q (the only query-matching segment)", evRef, gsCoMatchMsg1Ref)
	}
	if sender := requireStringField(t, ev, "sender"); sender != "ivan@example.com" {
		t.Errorf("evidence.sender = %q, want %q", sender, "ivan@example.com")
	}
	if at := requireStringField(t, ev, "at"); at != "2026-06-14T10:00:00Z" {
		t.Errorf("evidence.at = %q, want the verbatim segment At %q", at, "2026-06-14T10:00:00Z")
	}
	snippet := requireStringField(t, ev, "snippet")
	if !strings.Contains(snippet, gsCoMatchMarker) {
		t.Errorf("evidence.snippet = %q, want it to contain the matched term %q", snippet, gsCoMatchMarker)
	}
}

// TestGmailSegmentsContractDistinctRefsForSimilarText — CONTRACT, RED today
// (round 2 addition; today's read_memory ignores evidence_ref entirely, so
// both reads below currently return the identical, unscoped thread body
// instead of two independently addressable segments). Issue acceptance
// criterion: "Two messages with similar text remain independently
// addressable by distinct stable refs." Reuses the tie fixture (gt-1/gt-2
// carry byte-identical bodies) — the hardest case for independent
// addressability, since content alone cannot distinguish them.
func TestGmailSegmentsContractDistinctRefsForSimilarText(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	_ = cfg

	res1 := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q}`, gsTieID, gsTieRef1)))
	if isErr, _ := res1["isError"].(bool); isErr {
		t.Fatalf("evidence_ref=%s read isError: %v", gsTieRef1, res1)
	}
	p1 := structuredPayload(t, res1)
	r1, ok1 := p1["receipt"].(map[string]any)
	if !ok1 {
		t.Fatalf("evidence_ref=%s read has no receipt: %#v", gsTieRef1, p1)
	}
	if id := requireStringField(t, r1, "id"); id != gsTieID {
		t.Errorf("receipt.id = %q, want the requested parent memory id %q", id, gsTieID)
	}
	ref1 := requireStringField(t, r1, "evidence_ref")
	if ref1 != gsTieRef1 {
		t.Errorf("receipt.evidence_ref = %q, want %q", ref1, gsTieRef1)
	}
	if sender := requireStringField(t, r1, "sender"); sender != "carol@example.com" {
		t.Errorf("receipt.sender = %q, want %q", sender, "carol@example.com")
	}
	// Round 2, second audit pass: exact TYPED value, not mere presence.
	if at := requireStringField(t, r1, "at"); at != "2026-06-11T10:00:00Z" {
		t.Errorf("receipt.at = %q, want the verbatim segment At %q", at, "2026-06-11T10:00:00Z")
	}

	res2 := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q}`, gsTieID, gsTieRef2)))
	if isErr, _ := res2["isError"].(bool); isErr {
		t.Fatalf("evidence_ref=%s read isError: %v", gsTieRef2, res2)
	}
	p2 := structuredPayload(t, res2)
	r2, ok2 := p2["receipt"].(map[string]any)
	if !ok2 {
		t.Fatalf("evidence_ref=%s read has no receipt: %#v", gsTieRef2, p2)
	}
	if id := requireStringField(t, r2, "id"); id != gsTieID {
		t.Errorf("receipt.id = %q, want the requested parent memory id %q", id, gsTieID)
	}
	ref2 := requireStringField(t, r2, "evidence_ref")
	if ref2 != gsTieRef2 {
		t.Errorf("receipt.evidence_ref = %q, want %q", ref2, gsTieRef2)
	}
	if sender := requireStringField(t, r2, "sender"); sender != "dave@example.com" {
		t.Errorf("receipt.sender = %q, want %q", sender, "dave@example.com")
	}
	if at := requireStringField(t, r2, "at"); at != "2026-06-11T10:05:00Z" {
		t.Errorf("receipt.at = %q, want the verbatim segment At %q", at, "2026-06-11T10:05:00Z")
	}

	if ref1 == ref2 {
		t.Fatalf("both reads resolved to the same evidence_ref — refs are not independently addressable")
	}
}

// TestGmailSegmentsContractSemanticPathPreservesSegmentEvidence — CONTRACT,
// RED today (round 2 addition). Issue acceptance criterion: "A
// semantic-embedder run preserves the existing embedder gate and cannot lose
// the exact FTS evidence receipt." fakeOllama mirrors the seam already used
// by embed_ollama_test.go / embedder_incident_replay_test.go /
// retrieval_rt_cover_test.go: MORA_EMBEDDER/MORA_OLLAMA_URL/MORA_OLLAMA_MODEL
// set BEFORE withTempHome/init route defaultSearch through hybridSearch
// (embedderIsSemantic gate) instead of the FTS-only path. fakeOllama returns
// a FIXED vector for every Embed call, so the vector arm contributes an
// equal similarity to every candidate and does not itself favor any
// document — the buried-message query's outcome should be governed by the
// same segment-grain FTS signal either way. The evidence object attached to
// the buried thread's result row must be byte-identical whether the static
// or the semantic embedder produced the ranking.
func TestGmailSegmentsContractSemanticPathPreservesSegmentEvidence(t *testing.T) {
	staticCfg := seedGmailSegmentsSearchFixture(t)
	_ = staticCfg
	staticRes := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsBuriedMarker+`","limit":3}`))
	staticRows := resultRows(t, staticRes)
	var staticRow map[string]any
	for _, row := range staticRows {
		if rowID(t, row) == gsBuriedID {
			staticRow = row
		}
	}
	if staticRow == nil {
		t.Fatalf("static-embedder baseline did not return %s: %v", gsBuriedID, staticRows)
	}
	staticEv := evidenceObject(t, staticRow)
	staticEvJSON, _ := json.Marshal(staticEv)

	srv := fakeOllama(t, []float64{1, 0, 0, 0})
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	semCfg := seedGmailSegmentsSearchFixture(t)

	emb, embErr := chooseEmbedderFor(semCfg)
	if embErr != nil {
		t.Fatalf("chooseEmbedderFor under fakeOllama: %v", embErr)
	}
	if !embedderIsSemantic(emb) {
		t.Fatalf("embedder gate did not route semantic — fixture setup is broken, not the contract under test")
	}

	semRes := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsBuriedMarker+`","limit":3}`))
	semRows := resultRows(t, semRes)
	var semRow map[string]any
	for _, row := range semRows {
		if rowID(t, row) == gsBuriedID {
			semRow = row
		}
	}
	if semRow == nil {
		t.Fatalf("semantic-embedder path did not return %s: %v", gsBuriedID, semRows)
	}
	semEv := evidenceObject(t, semRow)
	semEvJSON, _ := json.Marshal(semEv)

	if string(staticEvJSON) != string(semEvJSON) {
		t.Fatalf("evidence receipt changed across the embedder gate:\nstatic=%s\nsemantic=%s", staticEvJSON, semEvJSON)
	}
}

// ---------------------------------------------------------------------------
// 12. The buried-message recall win — the contract's headline claim.
// ---------------------------------------------------------------------------

// TestGmailSegmentsContractBuriedMessagePin documents the PARENT-GRAIN-ONLY
// baseline (PIN, GREEN): under plain parent-grain BM25 — hybrid.go's
// ftsSearchIDs(ctx, db, query, scope, pool), the SAME memories_fts-only query
// hybridSearchTrace's FTS arm runs, which predates issue #243 and never
// calls the segment arm (gmailSegmentQueryArm/fuseGmailSegmentArm) — the
// long diluted thread does not survive a pool of 3 against four short
// single-occurrence decoys that outscore it on the identical rare term. This
// is the "miss" the contract test below proves segment-grain retrieval must
// fix.
//
// Supervisor-authorized amendment (round 2): the original version of this
// test called the full public search_memory MCP path directly. Once #243
// ships, search_memory itself ALWAYS composes the segment arm (gated only on
// the arm being non-empty, never a flag), so the buried thread the very next
// test (TestGmailSegmentsContractBuriedMessageFindableViaSegment) proves IS
// findable via search_memory — on the SAME query/fixture/limit this test
// used. A pin asserting search_memory's absence and a pin asserting
// search_memory's presence for the identical call cannot both hold once the
// feature is real, so this pin now targets ftsSearchIDs directly: a clean,
// pre-existing, UNTOUCHED-by-#243 seam (it is hybrid.go's own FTS arm
// builder, never modified by this issue — #243 only appends a fourth list to
// the RRF fusion call downstream of it), preserving the ORIGINAL
// parent-grain-alone-loses premise without a production bypass/flag and
// without contradicting the augmented public surface's own pin. If this test
// ever goes RED on its own (the thread starts appearing in ftsSearchIDs's
// own top-3 without any #243 change to hybrid.go), the fixture's burial
// premise needs re-tuning before the contract test below can be trusted.
func TestGmailSegmentsContractBuriedMessagePin(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)

	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()

	ids, err := ftsSearchIDs(context.Background(), db, gsBuriedMarker, "", 3)
	if err != nil {
		t.Fatalf("ftsSearchIDs: %v", err)
	}
	for _, id := range ids {
		if id == gsBuriedID {
			t.Fatalf("baseline premise broken: %s already appears in today's parent-grain-only ftsSearchIDs top-3 (%v) — retune the burial fixture before trusting the contract test below", gsBuriedID, ids)
		}
	}
}

// TestGmailSegmentsContractBuriedMessageFindableViaSegment — CONTRACT, RED
// today. The buried thread's own short segment (msg-2's few words vs. the
// thread's ~300-word diluted joined body) must win it a slot at limit=3 that
// parent-grain FTS alone denies it (see the PIN above), with evidence
// pointing at msg-2 specifically.
func TestGmailSegmentsContractBuriedMessageFindableViaSegment(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsBuriedMarker+`","limit":3}`))
	rows := resultRows(t, res)
	var found map[string]any
	for _, row := range rows {
		if rowID(t, row) == gsBuriedID {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("buried thread %s not found at limit=3 via segment-grain retrieval: %v", gsBuriedID, rows)
	}
	ev := evidenceObject(t, found)
	if evRef := requireStringField(t, ev, "evidence_ref"); evRef != gsBuriedID+"#msg-2" {
		t.Errorf("evidence.evidence_ref = %q, want %q", evRef, gsBuriedID+"#msg-2")
	}
	if sender := requireStringField(t, ev, "sender"); sender != "frank@example.com" {
		t.Errorf("evidence.sender = %q, want %q", sender, "frank@example.com")
	}
	// Round 2, second audit pass: exact TYPED value, not mere presence — this
	// is also the "segment wins ⇒ evidence attached with the exact segment
	// receipt" half of the DQ5 ownership proof (§2).
	if at := requireStringField(t, ev, "at"); at != "2026-06-12T10:05:00Z" {
		t.Errorf("evidence.at = %q, want the verbatim segment At %q", at, "2026-06-12T10:05:00Z")
	}
}

// ---------------------------------------------------------------------------
// 13-19. read_memory evidence_ref extension. Every test below drives the
// SAME real MCP dispatch path the rest of this file uses (mcpResult +
// budgetCall("read_memory", ...)) — round 2 correction: round 1 called
// mcpReadMemory directly, bypassing the tools/call transport/toCallToolResult
// envelope every other contract test exercises. A tool-level error surfaces
// as isError:true in the CallToolResult (toCallToolResult, mcp.go), NOT as a
// Go error from Run — so the fail-closed tests below check res["isError"],
// not a returned err.
// ---------------------------------------------------------------------------

// TestGmailSegmentsContractToolsListExposesEvidenceRefParam — CONTRACT, RED
// today (round 2 addition). read_memory's tools/list schema must advertise
// the new evidence_ref parameter so an MCP client can discover it without
// out-of-band docs — pins requirement 6 (MCP surface) independent of runtime
// behavior. evidence_ref is a STRING type (a MessageRef) and OPTIONAL (never
// listed in "required" — id alone stays required, exactly like match/
// max_tokens/occurrence from #242, so a parameter-free/plain-id call is
// unaffected).
func TestGmailSegmentsContractToolsListExposesEvidenceRefParam(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	res := mcpResult(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := res["tools"].([]any)
	var readMemoryTool map[string]any
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if tool["name"] == "read_memory" {
			readMemoryTool = tool
		}
	}
	if readMemoryTool == nil {
		t.Fatalf("tools/list has no read_memory entry: %v", tools)
	}
	schema, _ := readMemoryTool["inputSchema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	evidenceRefSchema, has := properties["evidence_ref"].(map[string]any)
	if !has {
		t.Fatalf("read_memory tools/list inputSchema.properties has no 'evidence_ref' key: %#v", properties)
	}
	if evidenceRefSchema["type"] != "string" {
		t.Errorf("evidence_ref schema type = %v, want %q", evidenceRefSchema["type"], "string")
	}
	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			if r == "evidence_ref" {
				t.Fatalf("evidence_ref must be OPTIONAL, but tools/list lists it in \"required\": %v", required)
			}
		}
	}
}

// TestGmailSegmentsContractReadMemoryEvidenceRefReturnsSegmentOnly —
// CONTRACT, RED today (round 2: real MCP dispatch, not a direct handler
// call). id+evidence_ref must return ONLY that message's segment content —
// never the other message's text — plus the identity fields DQ6/§2 pins:
// evidence_ref, sender, at (the parent id is checked separately below). This
// deliberately does NOT assert a fixed set of #242 fields (DQ6 round 2:
// compose with #242, never force an arbitrary key union) — see
// TestGmailSegmentsContractReadMemoryEvidenceRefBoundedAdjacentContext for
// the #242 excerpt-mechanism behavior itself.
func TestGmailSegmentsContractReadMemoryEvidenceRefReturnsSegmentOnly(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q}`, gsWellFormedID, gsWellMsg2Ref)))
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("evidence_ref read isError=true: %v", res)
	}
	payload := structuredPayload(t, res)

	memObj, _ := payload["memory"].(map[string]any)
	text, _ := memObj["text"].(string)
	if !strings.Contains(text, gsBetaMarker) {
		t.Fatalf("evidence_ref read text missing msg-2's own marker: %q", text)
	}
	if strings.Contains(text, gsAlphaMarker) {
		t.Fatalf("evidence_ref read text leaked msg-1's marker (whole-thread body returned instead of the segment): %q", text)
	}

	receipt, ok := payload["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("evidence_ref read has no 'receipt' object: %#v", payload)
	}
	if id := requireStringField(t, receipt, "id"); id != gsWellFormedID {
		t.Errorf("receipt.id = %q, want the requested parent memory id %q", id, gsWellFormedID)
	}
	if evRef := requireStringField(t, receipt, "evidence_ref"); evRef != gsWellMsg2Ref {
		t.Errorf("receipt.evidence_ref = %q, want %q", evRef, gsWellMsg2Ref)
	}
	if sender := requireStringField(t, receipt, "sender"); sender != "bob@example.com" {
		t.Errorf("receipt.sender = %q, want %q", sender, "bob@example.com")
	}
	// Round 2, second audit pass: exact TYPED value, not mere presence.
	if at := requireStringField(t, receipt, "at"); at != "2026-06-01T10:05:00Z" {
		t.Errorf("receipt.at = %q, want the verbatim segment At %q", at, "2026-06-01T10:05:00Z")
	}
}

// TestGmailSegmentsContractReadMemoryEvidenceRefBoundedAdjacentContext —
// CONTRACT, RED today (round 2 addition). Issue body wording (§3): evidence_ref
// returns "the exact message block with bounded adjacent context". Pins that
// this reuses #242's OWN centered-excerpt mechanism (centeredExcerptAt,
// read_bounded.go) over the narrowed segment text: a match phrase deep inside
// a long segment must come back with SURROUNDING text (at least one of the
// two unique near-markers planted immediately beside it), never the bare
// matched phrase alone, and receipt.truncated=true (the excerpt is smaller
// than the full ~600-word segment). Round 2 (second audit pass): the identity
// fields (evidence_ref, sender, at) must SURVIVE composition with #242's own
// bounded params (match/max_tokens) unchanged — this is the DQ6 "compose,
// don't replace" guarantee exercised concretely, not just the excerpt
// mechanism in isolation.
func TestGmailSegmentsContractReadMemoryEvidenceRefBoundedAdjacentContext(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q,"match":%q,"max_tokens":50}`, gsLongSegID, gsLongSegRef, gsLongSegMarker)))
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("evidence_ref+match+max_tokens read isError=true: %v", res)
	}
	payload := structuredPayload(t, res)
	memObj, _ := payload["memory"].(map[string]any)
	text, _ := memObj["text"].(string)
	if !strings.Contains(text, gsLongSegMarker) {
		t.Fatalf("bounded evidence_ref excerpt missing the matched phrase: %q", text)
	}
	if !strings.Contains(text, gsLongSegNearBefore) && !strings.Contains(text, gsLongSegNearAfter) {
		t.Fatalf("bounded evidence_ref excerpt has no adjacent context (missing both near-markers) — looks like the bare phrase, not a windowed excerpt: %q", text)
	}
	receipt, ok := payload["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("evidence_ref+match read has no 'receipt' object: %#v", payload)
	}
	if truncated := requireBoolField(t, receipt, "truncated"); !truncated {
		t.Errorf("receipt.truncated = false, want true (the ~600-word segment bounded to 50 tokens must be truncated)")
	}
	// Identity fields must survive composition with #242's bounded params —
	// evidence_ref/sender/id/at are not dropped just because match/max_tokens
	// were also supplied. Round 2, second audit pass: typed+present, and the
	// exact TYPED "at" value, not mere presence.
	if id := requireStringField(t, receipt, "id"); id != gsLongSegID {
		t.Errorf("receipt.id = %q, want the requested parent memory id %q", id, gsLongSegID)
	}
	if evRef := requireStringField(t, receipt, "evidence_ref"); evRef != gsLongSegRef {
		t.Errorf("receipt.evidence_ref = %q, want %q (must survive composition with #242's bounded params)", evRef, gsLongSegRef)
	}
	if sender := requireStringField(t, receipt, "sender"); sender != "pia@example.com" {
		t.Errorf("receipt.sender = %q, want %q (must survive composition with #242's bounded params)", sender, "pia@example.com")
	}
	if at := requireStringField(t, receipt, "at"); at != "2026-06-09T09:00:00Z" {
		t.Errorf("receipt.at = %q, want the verbatim segment At %q (must survive composition with #242's bounded params)", at, "2026-06-09T09:00:00Z")
	}
}

// TestGmailSegmentsContractReadMemoryEvidenceRefScopedNotWholeThread —
// CONTRACT, RED today (round 2, second audit pass: the receipt is MANDATORY
// here, not optional — a bounded read (match is supplied) already falls
// under #242's receipt contract, so its absence is itself a failure, not a
// skippable check). evidence_ref + a `match` phrase belonging to the OTHER
// message of the same thread must NOT match — the bounded-read excerpt
// machinery must operate strictly within the narrowed segment text (DQ6
// precedence), not the thread's full joined body: matched=false and
// match_count=0 (the phrase genuinely does not occur inside THIS segment),
// while the evidence identity fields (evidence_ref/sender/at) still
// correctly name the segment that was actually read, per DQ6's composition
// guarantee — an unmatched query must not also lose identity.
func TestGmailSegmentsContractReadMemoryEvidenceRefScopedNotWholeThread(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q,"match":%q}`, gsWellFormedID, gsWellMsg2Ref, gsAlphaMarker)))
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("evidence_ref+match read isError=true: %v", res)
	}
	payload := structuredPayload(t, res)
	receipt, ok := payload["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("evidence_ref+match read has no 'receipt' object (bounded reads REQUIRE the #242 receipt contract): %#v", payload)
	}
	// Round 2, second audit pass: require BOTH presence AND correct type
	// before comparing — a bare `v,_ := m["matched"].(bool)` silently reads a
	// MISSING key as false, which happens to equal the "unmatched" answer
	// this test expects and would pass even if the field were dropped
	// entirely.
	if matched := requireBoolField(t, receipt, "matched"); matched {
		t.Errorf("receipt.matched = true for a phrase belonging to a DIFFERENT message than evidence_ref scopes to: %#v", receipt)
	}
	if matchCount := requireNumberField(t, receipt, "match_count"); matchCount != 0 {
		t.Errorf("receipt.match_count = %v, want 0 (the phrase does not occur inside THIS segment's text)", matchCount)
	}
	if id := requireStringField(t, receipt, "id"); id != gsWellFormedID {
		t.Errorf("receipt.id = %q, want the requested parent memory id %q", id, gsWellFormedID)
	}
	if evRef := requireStringField(t, receipt, "evidence_ref"); evRef != gsWellMsg2Ref {
		t.Errorf("receipt.evidence_ref = %q, want %q (identity must survive even an unmatched scoped query)", evRef, gsWellMsg2Ref)
	}
	if sender := requireStringField(t, receipt, "sender"); sender != "bob@example.com" {
		t.Errorf("receipt.sender = %q, want %q (identity must survive even an unmatched scoped query)", sender, "bob@example.com")
	}
	if at := requireStringField(t, receipt, "at"); at != "2026-06-01T10:05:00Z" {
		t.Errorf("receipt.at = %q, want the verbatim segment At %q (identity must survive even an unmatched scoped query)", at, "2026-06-01T10:05:00Z")
	}
	memObj, _ := payload["memory"].(map[string]any)
	text, _ := memObj["text"].(string)
	if strings.Contains(text, gsAlphaMarker) {
		t.Fatalf("evidence_ref-scoped read matched text outside its own segment: %q", text)
	}
}

// TestGmailSegmentsContractReadMemoryEvidenceRefCrossMemoryFailsClosed —
// CONTRACT, RED today (round 2: real MCP dispatch — a tool-level error is
// isError:true on the CallToolResult, not a Go error). An evidence_ref that
// is a real, well-formed, DERIVED segment of a DIFFERENT memory must be an
// explicit error when read against the wrong parent id — never a silent
// cross-memory content leak.
func TestGmailSegmentsContractReadMemoryEvidenceRefCrossMemoryFailsClosed(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q}`, gsWellFormedID, gsCrossOtherRef)))
	isErr, _ := res["isError"].(bool)
	if !isErr {
		t.Fatalf("read_memory(id=%s, evidence_ref=%s) via real MCP dispatch did not error (isError=true), want a cross-memory rejection: %v", gsWellFormedID, gsCrossOtherRef, res)
	}
}

// TestGmailSegmentsContractReadMemoryEvidenceRefUnknownFailsClosed —
// CONTRACT, RED today (round 2: real MCP dispatch). An evidence_ref that is
// syntactically well-formed for the given memory (correct thread prefix) but
// does not correspond to any actually-derived segment must also fail closed,
// never silently fall back to the full body.
func TestGmailSegmentsContractReadMemoryEvidenceRefUnknownFailsClosed(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q}`, gsWellFormedID, gsWellFormedID+"#msg-999")))
	isErr, _ := res["isError"].(bool)
	if !isErr {
		t.Fatalf("read_memory with a nonexistent evidence_ref did not error via real MCP dispatch (isError=true), want an explicit rejection: %v", res)
	}
}

// TestGmailSegmentsContractReadMemoryEvidenceRefOnFailClosedMemoryErrors —
// CONTRACT, RED today (round 2: real MCP dispatch). A memory whose OWN
// segments failed closed at rebuild time (zero derived segments) must reject
// every evidence_ref against it, even one that is syntactically well-formed
// for that thread.
func TestGmailSegmentsContractReadMemoryEvidenceRefOnFailClosedMemoryErrors(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	_ = cfg

	res := mcpResult(t, budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"evidence_ref":%q}`, gsCountMismatch, gsCountMismatch+"#msg-1")))
	isErr, _ := res["isError"].(bool)
	if !isErr {
		t.Fatalf("read_memory evidence_ref against a fail-closed (zero-segment) memory did not error via real MCP dispatch (isError=true), want an explicit rejection: %v", res)
	}
}

// TestGmailSegmentsContractReadMemoryEvidenceRefSharedFallbackFailsClosed —
// CONTRACT, RED today (round 3, supervisor-authorized amendment, P2-1).
// mcpReadMemory falls back to findSharedMemory (a subscribed shared corpus)
// when the local vault has no such id — the documented expansion path a
// search_memory shared-corpus result uses (mcp.go's own comment: "search
// returns shared ids with 240-rune snippets, so read_memory is the
// documented expansion path for them too"). evidence_ref on THAT path must
// fail closed exactly like the local path — never silently ignore
// evidence_ref and return the full untouched shared body, which is what
// mcpReadMemoryResult does today (it only gates on #242's
// match/max_tokens/occurrence, never evidence_ref).
//
// Currently UNREACHABLE in production with a real evidence_ref (mora share
// export excludes Provider!="" and slash-bearing ids, so a genuine Gmail
// evidence_ref could never legitimately name a shared id today) — pinned
// anyway so the docs' never-silent-fallback promise is enforced if export
// invariants ever change, rather than silently rotting. Reuses
// share_test.go's own setupSubscription/fixtureMemory seam — the SAME one
// TestMCPReadMemorySharedFallbackParameterFreeUnchanged (#242,
// read_memory_bounded_test.go) already exercises — rather than inventing a
// second one; the shared memory itself need not be Gmail-shaped, since the
// fix is unconditional on evidence_ref presence for ANY shared-fallback read.
func TestGmailSegmentsContractReadMemoryEvidenceRefSharedFallbackFailsClosed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil shared note", "some shared content, not gmail-shaped at all"),
	})

	res := mcpResult(t, budgetCall("read_memory", `{"id":"mem_20260601_000000_aaaaaaaa","evidence_ref":"anything#msg-1"}`))
	isErr, _ := res["isError"].(bool)
	if !isErr {
		t.Fatalf("read_memory(evidence_ref=...) against a shared-fallback memory did not error via real MCP dispatch (isError=true), want an explicit fail-closed rejection — a shared memory must never silently ignore evidence_ref and return the full body: %v", res)
	}
}
