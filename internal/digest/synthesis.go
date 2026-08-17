package digest

import (
	"fmt"
	"sort"
	"strings"
)

// SourceState is the bounded freshness fact used by the synthesis prompt.
type SourceState struct{ Instance, State string }

// SynthesisPrompt emits deterministic grounded instructions over already-budgeted items.
func SynthesisPrompt(urgent []Item, sections []Section, states []SourceState) string {
	var b strings.Builder

	// (1) Fixed grounding header — the unambiguous contract (D15-5). The trust
	// posture mirrors thinkPrompt's "say so plainly rather than guessing".
	b.WriteString("Write a brief grounded ONLY in the cited items below. ")
	b.WriteString("Cite each claim by its [id]. ")
	b.WriteString("You may `read_memory <id>` to verify any claim. ")
	b.WriteString("Do not invent facts not present in a cited item; if something is missing, say so plainly rather than guessing.\n")

	// (2) The cited items — one bounded line per item, in caller-given order. The
	// citation is the existing DigestItem.ID; the body is the already-budgeted
	// Snippet (do NOT re-snippet — the caller owns the budget). Urgent-shelf items
	// (issue #62 defect 2) lead and are flagged so the synthesized brief opens with
	// the deadline-bearing item, not a routine thread.
	b.WriteString("\nCITED ITEMS:\n")
	total := 0
	for _, it := range urgent {
		fmt.Fprintf(&b, "- [%s] (⚠ urgent · %s) %s — %s\n", it.ID, it.Source, it.Title, it.Snippet)
		total++
	}
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
