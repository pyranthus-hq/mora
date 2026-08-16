package mcp

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"unicode"
)

// Issue #242 — bounded match-centred read_memory.
//
// read_memory's parameter-free path is untouched: this file is consulted
// only when the caller supplies at least one of match/max_tokens/occurrence
// (mcpReadMemoryResult in mcp.go gates on BoundedReadRequested). Bounded mode
// replaces memory.text with a centred excerpt and adds a sibling "receipt"
// key — see internal/mora/read_memory_bounded_test.go for the frozen
// contract this file implements.

// BoundedReadReceipt is the #242 receipt object accompanying a bounded
// read_memory response. Field names/JSON tags are pinned by the frozen
// contract test (boundedReceipt in read_memory_bounded_test.go).
type BoundedReadReceipt struct {
	ID         string `json:"id"`
	Matched    bool   `json:"matched"`
	MatchCount int    `json:"match_count"`
	Occurrence int    `json:"occurrence"`
	Truncated  bool   `json:"truncated"`
	Budget     int    `json:"budget"`
	// EvidenceRef/Sender/At (issue #243, DQ6 §2) are stamped ONLY by the
	// evidence_ref read path (gmail_segments_read.go) — omitempty keeps
	// every #242 bounded-read caller byte-identical (composition, never a
	// forced new shape).
	EvidenceRef string `json:"evidence_ref,omitempty"`
	Sender      string `json:"sender,omitempty"`
	At          string `json:"at,omitempty"`
	Direction   string `json:"direction,omitempty"`
}

const (
	// defaultBoundedBudgetTokens is the effective excerpt budget applied when
	// a bounded call supplies match and/or occurrence but omits max_tokens —
	// generous enough for a useful excerpt, small enough to stay well inside
	// read_memory's own T0 envelope ceiling (mora_mcp_budget_test.go: 4000
	// tokens) without the caller needing to know that ceiling.
	defaultBoundedBudgetTokens = 800

	// boundedReadEnvelopeCeilingTokens mirrors read_memory's own T0 budget-test
	// ceiling. boundedExcerptCharCap hard-caps the excerpt regardless of a
	// caller-requested max_tokens, so a bounded read can never blow that
	// ceiling even when max_tokens is requested well above it (up to the
	// general contextMaxTokens=20000 knob ceiling). The divisor is a
	// conservative safety margin for the CallToolResult envelope doubling
	// (text content block + structuredContent mirror) plus JSON
	// escaping/indent overhead — see toCallToolResult.
	boundedReadEnvelopeCeilingTokens = 4000
	boundedEnvelopeOverheadDivisor   = 3
	boundedExcerptCharCap            = (boundedReadEnvelopeCeilingTokens * CharsPerToken) / boundedEnvelopeOverheadDivisor
)

// BoundedReadRequested reports whether args carry any of the three #242
// bounded-mode knobs. Bounded mode is strictly opt-in — the parameter-free
// path (no keys present) must stay byte-identical to pre-#242 behavior.
func BoundedReadRequested(args map[string]any) bool {
	_, hasMatch := args["match"]
	_, hasMaxTokens := args["max_tokens"]
	_, hasOccurrence := args["occurrence"]
	return hasMatch || hasMaxTokens || hasOccurrence
}

// occSpan is a rune-index [start,end) span of one literal match occurrence
// within a flattened text.
type occSpan struct{ start, end int }

// flattenForMatch collapses whitespace exactly like snippet/matchSnippet
// (think.go), so occurrence positions computed here line up with the excerpt
// text returned — the excerpt is a slice of this same flattened rune slice.
func flattenForMatch(text string) []rune {
	return []rune(strings.Join(strings.Fields(text), " "))
}

// isMatchWordRune mirrors earliestQueryMatch's word-boundary predicate
// (think.go), reused so a `match` phrase never matches mid-word (e.g. "cat"
// inside "category").
func isMatchWordRune(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }

// findPhraseOccurrences returns the rune-index span of every case-insensitive,
// word-boundary occurrence of phrase within flat, in left-to-right order.
// This generalizes earliestQueryMatch's single-token word-boundary rule to a
// literal (possibly multi-word) phrase, and to ALL occurrences rather than
// just the earliest — #242 needs occurrence selection, search's snippet
// preview only ever needed the first.
func findPhraseOccurrences(flat []rune, phrase string) []occSpan {
	p := flattenForMatch(phrase)
	if len(p) == 0 {
		return nil
	}
	lowerFlat := make([]rune, len(flat))
	for i, c := range flat {
		lowerFlat[i] = unicode.ToLower(c)
	}
	lowerP := make([]rune, len(p))
	for i, c := range p {
		lowerP[i] = unicode.ToLower(c)
	}
	var spans []occSpan
	for i := 0; i+len(lowerP) <= len(lowerFlat); i++ {
		if i > 0 && isMatchWordRune(lowerFlat[i-1]) {
			continue // mid-word — not a phrase start
		}
		hit := true
		for j, pc := range lowerP {
			if lowerFlat[i+j] != pc {
				hit = false
				break
			}
		}
		if !hit {
			continue
		}
		end := i + len(lowerP)
		if end < len(lowerFlat) && isMatchWordRune(lowerFlat[end]) {
			continue // phrase is a prefix of a longer word
		}
		spans = append(spans, occSpan{start: i, end: end})
	}
	return spans
}

// centeredExcerptAt returns a word-boundary-safe window of flat, whose TOTAL
// rune length (including any leading/trailing ellipsis) never exceeds
// maxRunes, centered on [spanStart,spanEnd). This is the same
// leadIn/boundary-shift algorithm matchSnippet (think.go:395-419) uses to
// center on a freshly-searched query term, generalized to an explicit match
// position so #242 can center on any selected occurrence rather than only
// the earliest.
func centeredExcerptAt(flat []rune, spanStart, spanEnd, maxRunes int) string {
	if len(flat) <= maxRunes {
		return strings.TrimSpace(string(flat))
	}
	// Reserve room for up to two ellipsis runes (leading + trailing) so the
	// FINAL returned string — window plus ellipses — never exceeds maxRunes;
	// the caller's budget is a hard ceiling on what it gets back, not just
	// on the sliced window before decoration.
	windowLen := maxRunes - 2
	if windowLen < 1 {
		windowLen = 1
	}
	leadIn := windowLen / 3
	start := spanStart - leadIn
	if start < 0 {
		start = 0
	}
	if start+windowLen > len(flat) {
		start = len(flat) - windowLen
	}
	if start < 0 {
		start = 0
	}
	for start > 0 && start < spanStart && flat[start-1] != ' ' {
		start++ // never open mid-word
	}
	end := start + windowLen
	if end > len(flat) {
		end = len(flat)
	}
	out := ""
	if start > 0 {
		out += "…"
	}
	out += strings.TrimSpace(string(flat[start:end]))
	if end < len(flat) {
		out += "…"
	}
	return out
}

// ApplyBoundedRead computes the #242 bounded-mode excerpt + receipt for
// memory m. It never mutates m; it returns a shaped copy whose Text field
// carries the excerpt (empty when unmatched — never a silent fallback to the
// full body), plus the receipt object #242 pins.
func ApplyBoundedRead(m memory.Memory, args map[string]any) (memory.Memory, BoundedReadReceipt) {
	matchPhrase := StringArg(args, "match", "")
	occurrence := IntArg(args, "occurrence", 1)
	if occurrence < 1 {
		occurrence = 1
	}

	budgetTokens := IntArg(args, "max_tokens", 0)
	if budgetTokens <= 0 {
		budgetTokens = defaultBoundedBudgetTokens
	}
	excerptCap := budgetTokens * CharsPerToken
	if excerptCap > boundedExcerptCharCap {
		excerptCap = boundedExcerptCharCap
	}
	if excerptCap < 1 {
		excerptCap = 1
	}

	flat := flattenForMatch(m.Text)

	var spans []occSpan
	if matchPhrase == "" {
		// No literal phrase requested: max_tokens/occurrence alone still
		// opt into bounded mode. Treat the whole body as one trivial
		// occurrence so max_tokens gets a centered (here: head) excerpt
		// instead of the tool silently behaving parameter-free.
		if len(flat) > 0 {
			spans = []occSpan{{start: 0, end: len(flat)}}
		}
	} else {
		spans = findPhraseOccurrences(flat, matchPhrase)
	}

	receipt := BoundedReadReceipt{
		ID:         m.ID,
		MatchCount: len(spans),
		Occurrence: occurrence,
		Budget:     budgetTokens,
	}

	out := m
	out.Text = ""

	if occurrence > len(spans) {
		// Out-of-range occurrence (including "no spans at all") is an
		// explicit unmatched receipt — never an error, never a silent
		// fallback to occurrence 1 or the full text.
		receipt.Matched = false
		receipt.Truncated = true
		return out, receipt
	}

	span := spans[occurrence-1]
	excerptText := centeredExcerptAt(flat, span.start, span.end, excerptCap)
	out.Text = excerptText
	receipt.Matched = true
	receipt.Truncated = excerptText != string(flat)
	return out, receipt
}
