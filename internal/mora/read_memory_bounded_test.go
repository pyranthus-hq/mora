package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Issue #242 — bounded match-centred read_memory.
//
// read_memory gains three OPTIONAL args: match (string), max_tokens (int),
// occurrence (int, 1-indexed, default 1). This file is the FROZEN CONTRACT
// for that surface, pinned before implementation exists, so it is expected to
// fail RED until an implementer wires mcpReadMemory accordingly.
//
// Shape decisions this file pins (the implementer must match, not the other
// way around):
//
//  1. Parameter-free calls (no match/max_tokens/occurrence) are BYTE-IDENTICAL
//     to today: the top-level object has EXACTLY the keys {"memory","health"},
//     memory.text is the full untouched body, and no "receipt" key appears.
//     Bounded behavior must be additive and opt-in, never a default-path
//     regression.
//  2. Bounded calls (any one of the three new args present) reuse the SAME
//     top-level shape plus one sibling key: "receipt". The excerpt is NOT a
//     new "excerpt" field — it REPLACES memory.text, so every caller reads
//     the body from the same place (memory.text) whether bounded or not, and
//     the parameter-free path above never has to change to accommodate it.
//     receipt is {id, matched, match_count, occurrence, truncated, budget}
//     exactly as specified in #242.
//  3. occurrence is 1-INDEXED. Omitting it defaults to 1 (the first match).
//  4. An occurrence past the end (occurrence > match_count) is an EXPLICIT
//     UNMATCHED receipt — matched:false, match_count still reports the real
//     count so the caller can retry in range, occurrence echoes the request,
//     and memory.text carries NO fabricated body (never silently falls back
//     to occurrence 1 or to the full text).
//  5. A `match` that does not occur anywhere is likewise matched:false,
//     match_count:0, memory.text empty — never a silent full-body return.
//  6. receipt.budget is in the SAME unit as the max_tokens arg (tokens, not
//     bytes) and echoes the effective budget applied (the requested
//     max_tokens when supplied). receipt.truncated is true whenever
//     memory.text is not the complete original body.
//  7. health and the parent memory id are present on every bounded response,
//     matched or not, so a caller can always tell vault health apart from
//     match outcome.

// ---- fixtures --------------------------------------------------------

// boundedFillerWords returns n copies of a filler word joined by spaces (8
// bytes/word), never colliding with any marker token used below.
func boundedFillerWords(n int) string {
	return strings.TrimSpace(strings.Repeat("padding ", n))
}

const (
	boundedLongDocID      = "bounded-long-doc"
	boundedRepeatDocID    = "bounded-repeat-doc"
	boundedNoMatchDocID   = "bounded-no-match-doc"
	boundedMultiTopicID   = "bounded-multitopic-doc"
	boundedMatchPhrase    = "ZEBRAFISHMARKERALPHA"
	boundedRepeatPhrase   = "REPEATPHRASEBETA"
	boundedTopicBMarker   = "TOPICBSECRETMARKERXYZ"
	boundedRepeatCount    = 5
	boundedRepeatGapWords = 800 // ~6400B between occurrences — far outside any test window
)

// boundedRepeatNearMarker returns the unique token planted immediately beside
// the Nth (1-indexed) occurrence of boundedRepeatPhrase, so a centred excerpt
// can be proven to land on the RIGHT occurrence and not leak its neighbors.
func boundedRepeatNearMarker(occurrence int) string {
	return fmt.Sprintf("NEAROCC%d", occurrence)
}

// seedBoundedReadFixtures plants the four synthetic memories issue #242's
// contract needs: a single-occurrence document far over the read_memory
// token ceiling, a repeated-phrase document for occurrence selection, a
// document with no match at all, and a multi-topic document proving a
// bounded excerpt never exposes unrelated content.
func seedBoundedReadFixtures(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// (1) Long document, single occurrence, well over the 4000-token
	// (16000-byte) read_memory ceiling — ~48KB total.
	longText := boundedFillerWords(3000) + " " + boundedMatchPhrase + " " + boundedFillerWords(3000)
	if err := writeMemory(cfg, Memory{
		ID: boundedLongDocID, Scope: "global", Type: "note",
		Title: "Bounded long doc", CreatedAt: "2026-05-03T00:00:00Z", Text: longText,
	}); err != nil {
		t.Fatalf("seed long doc: %v", err)
	}

	// (2) Repeated phrase, boundedRepeatCount occurrences, each flanked by a
	// unique near-marker and separated by a wide filler gap.
	var b strings.Builder
	for i := 1; i <= boundedRepeatCount; i++ {
		fmt.Fprintf(&b, "%s %s %s %s ", boundedFillerWords(boundedRepeatGapWords), boundedRepeatNearMarker(i), boundedRepeatPhrase, boundedRepeatNearMarker(i))
	}
	if err := writeMemory(cfg, Memory{
		ID: boundedRepeatDocID, Scope: "global", Type: "note",
		Title: "Bounded repeat doc", CreatedAt: "2026-05-03T00:00:01Z", Text: b.String(),
	}); err != nil {
		t.Fatalf("seed repeat doc: %v", err)
	}

	// (3) No match anywhere.
	if err := writeMemory(cfg, Memory{
		ID: boundedNoMatchDocID, Scope: "global", Type: "note",
		Title: "Bounded no-match doc", CreatedAt: "2026-05-03T00:00:02Z",
		Text: boundedFillerWords(500),
	}); err != nil {
		t.Fatalf("seed no-match doc: %v", err)
	}

	// (4) Multi-topic: topic A carries the target phrase, topic B (far away)
	// carries a unique marker that a correctly-bounded excerpt must never
	// surface.
	multiText := "TOPIC A begins here. " + boundedFillerWords(300) + " " + boundedMatchPhrase + " " +
		boundedFillerWords(300) + " TOPIC A ends here. " +
		boundedFillerWords(4000) + // wide gap
		" TOPIC B begins here. " + boundedTopicBMarker + " " + boundedFillerWords(300) + " TOPIC B ends here."
	if err := writeMemory(cfg, Memory{
		ID: boundedMultiTopicID, Scope: "global", Type: "note",
		Title: "Bounded multi-topic doc", CreatedAt: "2026-05-03T00:00:03Z", Text: multiText,
	}); err != nil {
		t.Fatalf("seed multi-topic doc: %v", err)
	}

	return cfg
}

// boundedReceipt mirrors the #242 receipt object for JSON-key assertions.
type boundedReceipt struct {
	ID         string `json:"id"`
	Matched    bool   `json:"matched"`
	MatchCount int    `json:"match_count"`
	Occurrence int    `json:"occurrence"`
	Truncated  bool   `json:"truncated"`
	Budget     int    `json:"budget"`
}

type boundedReadEnvelope struct {
	Memory  Memory          `json:"memory"`
	Health  compactHealth   `json:"health"`
	Receipt *boundedReceipt `json:"receipt"`
}

func decodeBoundedRead(t *testing.T, raw any) (boundedReadEnvelope, map[string]any) {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal read_memory result: %v", err)
	}
	var env boundedReadEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal read_memory result: %v\n%s", err, b)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal read_memory result (generic): %v\n%s", err, b)
	}
	return env, generic
}

// ---- 1. parameter-free stays byte-identical ---------------------------

// TestMCPReadMemoryParameterFreeIsByteIdentical pins today's read_memory
// shape: exactly {"memory","health"}, memory.text untouched, no "receipt".
// Bounded mode must be strictly additive.
func TestMCPReadMemoryParameterFreeIsByteIdentical(t *testing.T) {
	cfg := seedBoundedReadFixtures(t)

	raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{"id": boundedLongDocID})
	if err != nil {
		t.Fatalf("mcpReadMemory: %v", err)
	}
	_, generic := decodeBoundedRead(t, raw)

	keys := make([]string, 0, len(generic))
	for k := range generic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"health", "memory"}
	if len(keys) != len(wantKeys) {
		t.Fatalf("parameter-free read_memory top-level keys = %v, want exactly %v", keys, wantKeys)
	}
	for i, k := range keys {
		if k != wantKeys[i] {
			t.Fatalf("parameter-free read_memory top-level keys = %v, want exactly %v", keys, wantKeys)
		}
	}

	memObj, _ := generic["memory"].(map[string]any)
	gotText, _ := memObj["text"].(string)
	wantText := boundedFillerWords(3000) + " " + boundedMatchPhrase + " " + boundedFillerWords(3000)
	if gotText != wantText {
		t.Fatalf("parameter-free memory.text was altered: got %d bytes, want %d bytes (full untouched body)", len(gotText), len(wantText))
	}

	// Determinism: two identical parameter-free calls must byte-match. A
	// bounded-mode change that leaks state across calls (or that varies the
	// parameter-free path by wall-clock content beyond health, which is
	// itself state-invariant for a healthy empty-connector vault) would show
	// up here.
	raw2, err := mcpReadMemory(context.Background(), cfg, map[string]any{"id": boundedLongDocID})
	if err != nil {
		t.Fatalf("mcpReadMemory (second call): %v", err)
	}
	b1, _ := json.Marshal(raw)
	b2, _ := json.Marshal(raw2)
	if string(b1) != string(b2) {
		t.Fatalf("parameter-free read_memory is not byte-stable across identical calls:\n%s\n---\n%s", b1, b2)
	}
}

// TestMCPReadMemorySharedFallbackParameterFreeUnchanged proves the
// findSharedMemory fallback path (search returns a shared id; read_memory is
// the documented expansion) still returns the untouched, receipt-free shape
// when no bounded args are given.
func TestMCPReadMemorySharedFallbackParameterFreeUnchanged(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	longShared := "neil standardized on sqlite too " + boundedFillerWords(30)
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", longShared),
	})

	raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{"id": "mem_20260601_000000_aaaaaaaa"})
	if err != nil {
		t.Fatalf("mcpReadMemory shared fallback: %v", err)
	}
	env, generic := decodeBoundedRead(t, raw)
	if _, hasReceipt := generic["receipt"]; hasReceipt {
		t.Fatalf("parameter-free shared-fallback read must not carry a receipt: %v", generic)
	}
	if env.Memory.Owner != "neil" {
		t.Fatalf("shared fallback lost owner attribution: %+v", env.Memory)
	}
	if env.Memory.Text != longShared {
		t.Fatalf("shared fallback altered memory.text: got %q, want %q", env.Memory.Text, longShared)
	}
}

// ---- 2. bounded receipt shape + single-occurrence centring -----------

// TestMCPReadMemoryBoundedMatchReturnsReceiptAndCentredExcerpt drives the
// long, single-occurrence fixture (which is FAR over the 4000-token ceiling
// if returned whole) and pins: the receipt object appears with the exact
// #242 fields, occurrence defaults to 1, the excerpt contains the match, the
// excerpt is far smaller than the full body, and parent id + health survive.
func TestMCPReadMemoryBoundedMatchReturnsReceiptAndCentredExcerpt(t *testing.T) {
	cfg := seedBoundedReadFixtures(t)

	raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{
		"id": boundedLongDocID, "match": boundedMatchPhrase, "max_tokens": float64(300),
	})
	if err != nil {
		t.Fatalf("mcpReadMemory bounded: %v", err)
	}
	env, _ := decodeBoundedRead(t, raw)

	if env.Receipt == nil {
		t.Fatal("bounded read_memory did not return a receipt object")
	}
	r := *env.Receipt
	if r.ID != boundedLongDocID {
		t.Errorf("receipt.id = %q, want parent id %q", r.ID, boundedLongDocID)
	}
	if !r.Matched {
		t.Errorf("receipt.matched = false, want true (phrase is present)")
	}
	if r.MatchCount != 1 {
		t.Errorf("receipt.match_count = %d, want 1", r.MatchCount)
	}
	if r.Occurrence != 1 {
		t.Errorf("receipt.occurrence = %d, want 1 (default when omitted)", r.Occurrence)
	}
	if !r.Truncated {
		t.Errorf("receipt.truncated = false, want true (excerpt is not the whole ~48KB body)")
	}
	if r.Budget != 300 {
		t.Errorf("receipt.budget = %d, want the requested max_tokens (300)", r.Budget)
	}

	if !strings.Contains(env.Memory.Text, boundedMatchPhrase) {
		t.Fatalf("bounded excerpt does not contain the match phrase: %q", env.Memory.Text)
	}
	if got, max := len([]rune(env.Memory.Text)), 300*charsPerToken; got > max {
		t.Errorf("bounded excerpt is %d runes, exceeds the requested max_tokens budget (%d runes)", got, max)
	}
	if len(env.Memory.Text) >= len(boundedFillerWords(3000))*2 {
		t.Errorf("bounded excerpt (%d bytes) is not meaningfully smaller than the full body", len(env.Memory.Text))
	}
	if env.Health.State == "" {
		t.Errorf("bounded response missing health.state")
	}
}

// ---- 3. occurrence selection + centring, no cross-occurrence leakage --

func TestMCPReadMemoryBoundedExcerptCentersOnRequestedOccurrence(t *testing.T) {
	cfg := seedBoundedReadFixtures(t)

	for _, occ := range []int{1, 2, 3, boundedRepeatCount} {
		t.Run(fmt.Sprintf("occurrence=%d", occ), func(t *testing.T) {
			raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{
				"id": boundedRepeatDocID, "match": boundedRepeatPhrase,
				"occurrence": float64(occ), "max_tokens": float64(300),
			})
			if err != nil {
				t.Fatalf("mcpReadMemory bounded occurrence=%d: %v", occ, err)
			}
			env, _ := decodeBoundedRead(t, raw)
			if env.Receipt == nil {
				t.Fatal("missing receipt")
			}
			r := *env.Receipt
			if !r.Matched {
				t.Fatalf("receipt.matched = false, want true (occurrence %d is in range)", occ)
			}
			if r.MatchCount != boundedRepeatCount {
				t.Errorf("receipt.match_count = %d, want %d", r.MatchCount, boundedRepeatCount)
			}
			if r.Occurrence != occ {
				t.Errorf("receipt.occurrence = %d, want %d (echoed request)", r.Occurrence, occ)
			}
			if env.Health.State == "" {
				t.Errorf("bounded response missing health.state")
			}

			wantMarker := boundedRepeatNearMarker(occ)
			if !strings.Contains(env.Memory.Text, wantMarker) {
				t.Fatalf("excerpt for occurrence=%d missing its own near-marker %q:\n%s", occ, wantMarker, env.Memory.Text)
			}
			for other := 1; other <= boundedRepeatCount; other++ {
				if other == occ {
					continue
				}
				leaked := boundedRepeatNearMarker(other)
				if strings.Contains(env.Memory.Text, leaked) {
					t.Fatalf("excerpt for occurrence=%d leaked neighbor marker %q (excerpt not centred/bounded):\n%s", occ, leaked, env.Memory.Text)
				}
			}
		})
	}
}

// TestMCPReadMemoryBoundedOutOfRangeOccurrenceIsExplicitlyUnmatched pins the
// #242 choice for an out-of-range occurrence: an EXPLICIT unmatched receipt
// (never an error, never a silent fallback to occurrence 1 or the full
// text).
func TestMCPReadMemoryBoundedOutOfRangeOccurrenceIsExplicitlyUnmatched(t *testing.T) {
	cfg := seedBoundedReadFixtures(t)

	const outOfRange = boundedRepeatCount + 7
	raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{
		"id": boundedRepeatDocID, "match": boundedRepeatPhrase, "occurrence": float64(outOfRange),
	})
	if err != nil {
		t.Fatalf("out-of-range occurrence must be an honest unmatched receipt, not an error: %v", err)
	}
	env, _ := decodeBoundedRead(t, raw)
	if env.Receipt == nil {
		t.Fatal("missing receipt")
	}
	r := *env.Receipt
	if r.Matched {
		t.Errorf("receipt.matched = true, want false (occurrence %d > match_count %d)", outOfRange, boundedRepeatCount)
	}
	if r.MatchCount != boundedRepeatCount {
		t.Errorf("receipt.match_count = %d, want the real total %d so the caller can retry in range", r.MatchCount, boundedRepeatCount)
	}
	if r.Occurrence != outOfRange {
		t.Errorf("receipt.occurrence = %d, want the echoed request %d", r.Occurrence, outOfRange)
	}
	if env.Memory.Text != "" {
		t.Errorf("out-of-range occurrence fabricated body content: %q, want empty (no silent fallback)", env.Memory.Text)
	}
	if env.Health.State == "" {
		t.Errorf("unmatched bounded response missing health.state")
	}
}

// ---- 4. no match anywhere ---------------------------------------------

func TestMCPReadMemoryBoundedNoMatchIsHonestNeverFullBody(t *testing.T) {
	cfg := seedBoundedReadFixtures(t)

	raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{
		"id": boundedNoMatchDocID, "match": boundedMatchPhrase,
	})
	if err != nil {
		t.Fatalf("mcpReadMemory no-match: %v", err)
	}
	env, _ := decodeBoundedRead(t, raw)
	if env.Receipt == nil {
		t.Fatal("missing receipt")
	}
	r := *env.Receipt
	if r.Matched {
		t.Errorf("receipt.matched = true, want false (phrase does not occur)")
	}
	if r.MatchCount != 0 {
		t.Errorf("receipt.match_count = %d, want 0", r.MatchCount)
	}
	if r.ID != boundedNoMatchDocID {
		t.Errorf("receipt.id = %q, want parent id %q", r.ID, boundedNoMatchDocID)
	}
	if r.Budget <= 0 {
		t.Errorf("receipt.budget = %d, want a positive effective budget even without an explicit max_tokens", r.Budget)
	}
	if env.Memory.Text != "" {
		t.Errorf("no-match bounded read fabricated/returned body content: %q, want empty (never silently return the full text)", env.Memory.Text)
	}
	if env.Health.State == "" {
		t.Errorf("no-match bounded response missing health.state")
	}
}

// ---- 5. requested budget AND the existing T0 ceiling both hold --------

// TestMCPReadMemoryBoundedRespectsCeilingAndRequestedBudget drives the
// bounded read through the FULL mcp-serve transport (mcpResult/measureEnvelope,
// shared with the T0 budget gate in mora_mcp_budget_test.go) so the same
// envelope-doubling math applies: a bounded read of a ~48KB document must
// land under the existing read_memory=4000-token ceiling even though a
// parameter-free read of the same document would blow far past it.
func TestMCPReadMemoryBoundedRespectsCeilingAndRequestedBudget(t *testing.T) {
	cfg := seedBoundedReadFixtures(t)
	run(t, "init") // no-op if already initialized; keeps this test self-sufficient under -run
	_ = cfg

	line := budgetCall("read_memory", fmt.Sprintf(`{"id":%q,"match":%q,"max_tokens":300}`, boundedLongDocID, boundedMatchPhrase))
	envBytes := measureEnvelope(t, line)
	if tok := (envBytes + charsPerToken - 1) / charsPerToken; tok > 4000 {
		t.Fatalf("bounded read_memory envelope = %d tokens, exceeds the existing 4000-token T0 ceiling (bytes=%d)", tok, envBytes)
	}

	raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{
		"id": boundedLongDocID, "match": boundedMatchPhrase, "max_tokens": float64(300),
	})
	if err != nil {
		t.Fatalf("mcpReadMemory: %v", err)
	}
	env, _ := decodeBoundedRead(t, raw)
	if got, max := len([]rune(env.Memory.Text)), 300*charsPerToken; got > max {
		t.Fatalf("bounded excerpt (%d runes) exceeds the REQUESTED max_tokens budget (%d runes)", got, max)
	}
	if env.Receipt == nil || !env.Receipt.Truncated {
		t.Fatalf("expected receipt.truncated=true for a ~48KB document bounded to 300 tokens")
	}
}

// ---- 6. multi-topic excerpt never exposes unrelated content -----------

func TestMCPReadMemoryBoundedExcerptDoesNotExposeUnrelatedTopic(t *testing.T) {
	cfg := seedBoundedReadFixtures(t)

	raw, err := mcpReadMemory(context.Background(), cfg, map[string]any{
		"id": boundedMultiTopicID, "match": boundedMatchPhrase, "max_tokens": float64(200),
	})
	if err != nil {
		t.Fatalf("mcpReadMemory multi-topic: %v", err)
	}
	env, _ := decodeBoundedRead(t, raw)
	if env.Receipt == nil || !env.Receipt.Matched {
		t.Fatalf("expected a matched receipt on the multi-topic fixture: %+v", env.Receipt)
	}
	if !strings.Contains(env.Memory.Text, boundedMatchPhrase) {
		t.Fatalf("multi-topic excerpt missing the requested match: %q", env.Memory.Text)
	}
	if strings.Contains(env.Memory.Text, boundedTopicBMarker) {
		t.Fatalf("multi-topic excerpt leaked unrelated Topic B content (%q) into a Topic A bounded read:\n%s", boundedTopicBMarker, env.Memory.Text)
	}
	if strings.Contains(env.Memory.Text, "TOPIC B begins here") {
		t.Fatalf("multi-topic excerpt leaked the Topic B section header:\n%s", env.Memory.Text)
	}
}
