package mora

import (
	"fmt"
	"sort"
	"strings"
)

// Phase 15 synthesis envelope: the digest analog of think.go's thinkPrompt. Mora
// makes ZERO model/network call here — exactly like `think`, it only EMITS a
// deterministic instruction STRING that the calling agent runs with its OWN
// model. The agent reads the cited items and writes the grounded, cited brief;
// Mora holds no API key and pays no synthesis bill (D15-1, zero egress).
//
// Citations are the EXISTING DigestItem.ID memory ids — no new scheme (D15-2) —
// so every synthesized claim traces to a memory the agent can `read_memory` back
// (SC#3).

// DigestEnvelope is the think-style projection the MCP `digest` tool and
// `pulse --digest --envelope` (wired in 15-02) return: the already-budgeted cited
// sections + source_states plus the emitted synthesis_prompt. Its JSON keys are a
// SUPERSET of digestMCPPayload's base map (generated / since_hours / sections /
// source_states / freshness / stale_tasks) plus synthesis_prompt — so the plain
// (non-envelope) payload stays a strict subset and envelope-OFF output is
// byte-for-byte unchanged (D15-3 backward-compat).
type DigestEnvelope struct {
	Generated       string            `json:"generated"`
	SinceHours      int               `json:"since_hours"`
	Sections        []DigestSection   `json:"sections"`
	SourceStates    []sourceState     `json:"source_states"`
	Freshness       map[string]string `json:"freshness,omitempty"`
	StaleTasks      []string          `json:"stale_tasks,omitempty"`
	SynthesisPrompt string            `json:"synthesis_prompt"`
}

// digestSynthesisPrompt builds the instruction the calling agent's model runs to
// turn the cited digest items into a grounded, cited brief. It mirrors
// thinkPrompt (think.go:184): a PURE function of its inputs — no time.Now, no map
// iteration, no DB, no network — so identical inputs yield a byte-identical
// string across calls.
//
// CRITICAL grounding invariant (T-15-01, no dangling citation): it cites EXACTLY
// the ids present in the PASSED sections. The caller passes the ALREADY-BUDGETED /
// truncated sections it is about to emit, so the prompt can never cite an id that
// was budget-dropped from the emitted items (which would force the agent to
// hallucinate or fail). Sections and items are ranged in the GIVEN order (the
// caller pre-sorts: sections by rank, items by capRecency) — they are NOT
// re-sorted here, so the prompt order matches the emitted items.
//
// Boundedness (for 15-02's space reservation): the output is a fixed header +
// one line per cited item (each line reuses the already-budgeted DigestItem.Snippet
// body — it is NOT re-snippeted here) + at most one bounded NOT-COVERED line. So
// the total length is predictable from the item count; 15-02 can reserve the
// prompt's space from the byte budget before budgeting items.
func digestSynthesisPrompt(sections []DigestSection, states []sourceState) string {
	var b strings.Builder

	// (1) Fixed grounding header — the unambiguous contract (D15-5). The trust
	// posture mirrors thinkPrompt's "say so plainly rather than guessing".
	b.WriteString("Write a brief grounded ONLY in the cited items below. ")
	b.WriteString("Cite each claim by its [id]. ")
	b.WriteString("You may `read_memory <id>` to verify any claim. ")
	b.WriteString("Do not invent facts not present in a cited item; if something is missing, say so plainly rather than guessing.\n")

	// (2) The cited items — one bounded line per item, in caller-given order. The
	// citation is the existing DigestItem.ID; the body is the already-budgeted
	// Snippet (do NOT re-snippet — the caller owns the budget).
	b.WriteString("\nCITED ITEMS:\n")
	total := 0
	for _, s := range sections {
		for _, it := range s.Items {
			fmt.Fprintf(&b, "- [%s] (%s) %s — %s\n", it.ID, it.Source, it.Title, it.Snippet)
			total++
		}
	}
	if total == 0 {
		b.WriteString("(no items)\n")
	}

	// (3) The cheap, bounded "what this brief does NOT cover" line (D15-5), derived
	// ONLY from the passed states — no DB, no deep per-item gap analysis. Collect
	// the stale/unavailable instances and SORT them for determinism (the one place
	// sort is needed: states are not guaranteed to arrive sorted by instance, and
	// map-order-free determinism is required by T-15-03).
	var uncovered []string
	for _, st := range states {
		if st.State == "stale" || st.State == "unavailable" {
			uncovered = append(uncovered, st.Instance)
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		fmt.Fprintf(&b, "\nWHAT THIS BRIEF DOES NOT COVER: %s (stale or unavailable since the last sync — do not assume they are up to date).\n", strings.Join(uncovered, ", "))
	}

	return b.String()
}

// envelopePromptReserve is the bounded byte floor reserved for the synthesis_prompt's
// FIXED template — the grounding header, the "CITED ITEMS:" label, and the bounded
// "WHAT THIS BRIEF DOES NOT COVER" line — measured generously so the instructions
// are NEVER budgeted away even when zero items are budgeted (15-02 prompt-first
// reservation). This covers ONLY the template; the per-item prompt lines are sized
// by the PROPORTIONAL reservation below (they scale with item count).
const envelopePromptReserve = 600

// envelopeItemBudgetNum/Den is the envelope-specific item-budget RATIO (15-03,
// D15-4). The synthesis_prompt is NOT a fixed-size addition: it re-emits one
// plain-text line per budgeted item (- [id] (Source) Title — Snippet), so it grows
// ~proportionally to the budgeted items. A flat byte reserve therefore cannot hold
// the ceiling — at max_tokens the additive prompt pushed the full CallToolResult to
// ~22.6k tokens, OVER the 20000 redline (the T0 digest_envelope row caught this).
//
// The fix, validated by the T0 gate: when assembling the envelope, budget the ITEMS
// against budgetChars × 2/3 — i.e. an effective envelope-inflation divisor of ~4.5
// (the base path's mcpDigestEnvelopeDivisor=3 × 3/2), reserving the remaining ~1/3
// of the compact budget for the additive per-item prompt + the fixed template. This
// is deterministic and keeps the envelope-ON CallToolResult comfortably under the
// SAME 20000-token ceiling as envelope-OFF (measured ~16.5k tok on the T0 fixture,
// ~17% headroom for snippet-content variance), without loosening the ceiling and
// without touching the envelope-OFF path (which still budgets against budgetChars).
const (
	envelopeItemBudgetNum = 2
	envelopeItemBudgetDen = 3
)

// envelopeItemsBudget is the item-budget the envelope path hands digestMCPPayload:
// the proportional 2/3 share, then a fixed-template floor subtracted, clamped ≥0.
// The proportional share is what holds the ceiling (the prompt grows per item); the
// envelopePromptReserve subtraction additionally guarantees the FIXED instructions
// survive even when budgetChars is tiny (items get 0 chars, the template still emits).
func envelopeItemsBudget(budgetChars int) int {
	return maxInt(0, budgetChars*envelopeItemBudgetNum/envelopeItemBudgetDen-envelopePromptReserve)
}

// maxInt returns the larger of a, b. Used to clamp the items budget to ≥0 after the
// prompt reserve is subtracted (a tiny budgetChars must not go negative — items get
// 0 chars, but the reserved prompt instructions still emit in full).
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// budgetEnvelopePayload is the ONE place the opt-in DigestEnvelope is assembled —
// shared by the MCP `digest` tool and `pulse --digest --envelope` (15-02). It
// enforces the two Gemini-validated invariants:
//
//	(1) Prompt-first PROPORTIONAL budget reservation (15-03/D15-4): it budgets the
//	    items against envelopeItemsBudget(budgetChars) — the 2/3 share minus the
//	    fixed-template floor — reserving the remaining ~1/3 for the additive
//	    per-item synthesis_prompt lines (which grow with item count) plus the fixed
//	    instructions. So a large digest squeezes the ITEMS, never the synthesis
//	    INSTRUCTIONS, AND the envelope-ON CallToolResult stays under the SAME
//	    20000-token ceiling as envelope-OFF (the T0 digest_envelope row proves it).
//	(2) No-dangling grounding (SC#3): it reads the ALREADY-BUDGETED sections back
//	    out of the payload and builds the prompt from THOSE, so the prompt can only
//	    cite ids the agent actually receives — never a budget-dropped id.
//
// The returned DigestEnvelope's base fields equal the plain payload exactly (the
// synthesis_prompt is the only addition), so envelope-OFF callers that return the
// plain map stay byte-identical (D15-3 / T-15-04). Model-free + zero-egress: it
// only calls digestSynthesisPrompt (a pure string builder) — no sampling, no
// model, no network (D15-1 / SC#2).
func budgetEnvelopePayload(cfg Config, d Digest, budgetChars int) DigestEnvelope {
	payload := digestMCPPayload(cfg, d, envelopeItemsBudget(budgetChars))

	// Read the budgeted sections + source_states back out — these are EXACTLY what
	// the caller emits, so the prompt built from them is no-dangling by construction.
	sections, _ := payload["sections"].([]DigestSection)
	states, _ := payload["source_states"].([]sourceState)
	prompt := digestSynthesisPrompt(sections, states)

	return DigestEnvelope{
		Generated:       asString(payload["generated"]),
		SinceHours:      asInt(payload["since_hours"]),
		Sections:        sections,
		SourceStates:    states,
		Freshness:       asStringMap(payload["freshness"]),
		StaleTasks:      asStringSlice(payload["stale_tasks"]),
		SynthesisPrompt: prompt,
	}
}

// asString / asInt / asStringMap / asStringSlice are the narrow type assertions
// that lift digestMCPPayload's map[string]any back into the typed DigestEnvelope
// fields. They mirror the exact types digestMCPPayload stores (d.Generated string,
// d.SinceHours int, d.Freshness map[string]string, d.StaleTasks []string), so a
// nil/absent value degrades to the zero value rather than panicking.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int {
	if i, ok := v.(int); ok {
		return i
	}
	return 0
}

func asStringMap(v any) map[string]string {
	if m, ok := v.(map[string]string); ok {
		return m
	}
	return nil
}

func asStringSlice(v any) []string {
	if s, ok := v.([]string); ok {
		return s
	}
	return nil
}
