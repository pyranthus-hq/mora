// Package synthesis owns deterministic evidence and pre-model gap facts.
package synthesis

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/openloops"
	"github.com/pyranthus-hq/mora/internal/search"
	"strings"
	"time"
	"unicode"
)

const (
	StaleDays  = 30
	SnippetLen = 240
)

type Evidence struct {
	StableID         string                    `json:"stable_id"`
	Title            string                    `json:"title"`
	Scope            string                    `json:"scope"`
	CreatedAt        string                    `json:"created_at"`
	Score            float64                   `json:"score"`
	Snippet          string                    `json:"snippet"`
	Owner            string                    `json:"owner,omitempty"`
	Corroborating    []memory.CorroboratingRef `json:"corroborating,omitempty"`
	confidenceTitle  string
	confidenceText   string
	confidenceSource string
}

func (e Evidence) ConfidenceFacts() (string, string, string) {
	return e.confidenceTitle, e.confidenceText, e.confidenceSource
}

type Gaps struct {
	Stale            []string `json:"stale,omitempty"`
	FreshnessUnknown []string `json:"freshness_unknown,omitempty"`
	SparseEvidence   []string `json:"sparse_evidence,omitempty"`
	SourceCoverage   []string `json:"source_coverage,omitempty"`
	TemporalState    []string `json:"temporal_state,omitempty"`
	ThinCoverage     []string `json:"thin_coverage,omitempty"`
	CoverageHoles    []string `json:"coverage_holes,omitempty"`
	RetrievalCaveats []string `json:"retrieval_caveats,omitempty"`
	ChecksApplied    []string `json:"checks_applied"`
}

func (g Gaps) Empty() bool {
	return len(g.Stale) == 0 && len(g.FreshnessUnknown) == 0 && len(g.SparseEvidence) == 0 && len(g.SourceCoverage) == 0 && len(g.TemporalState) == 0 && len(g.ThinCoverage) == 0 && len(g.CoverageHoles) == 0 && len(g.RetrievalCaveats) == 0
}
func EvidenceFromMemories(mems []memory.Memory, query string) []Evidence {
	out := make([]Evidence, 0, len(mems))
	for _, m := range mems {
		source := m.Source
		if source == "" {
			source = m.Provider
		}
		if source == "" {
			source = m.Type
		}
		out = append(out, Evidence{StableID: m.ID, Title: m.Title, Scope: m.Scope, CreatedAt: m.CreatedAt, Score: m.Score, Snippet: search.MatchSnippet(m.Text, query, SnippetLen), Owner: m.Owner, Corroborating: m.Corroborating, confidenceTitle: m.Title, confidenceText: m.Text, confidenceSource: source})
	}
	return out
}
func BasicGaps(mems []memory.Memory, query string, now time.Time) Gaps {
	g := Gaps{ChecksApplied: []string{"staleness", "evidence_density", "source_coverage", "temporal_state", "entity_coverage", "retrieval_support"}}
	if len(mems) == 0 {
		g.CoverageHoles = append(g.CoverageHoles, "No memory matched this query.")
		return g
	}
	var newest time.Time
	for _, m := range mems {
		if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil && t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		g.FreshnessUnknown = append(g.FreshnessUnknown, "The matching evidence has no usable timestamp, so Mora cannot verify whether it is current.")
	} else if now.Sub(newest) > StaleDays*24*time.Hour {
		g.Stale = append(g.Stale, fmt.Sprintf("The freshest matching memory is from %s — older than %d days; the answer may be out of date.", newest.UTC().Format("2006-01-02"), StaleDays))
	}
	if len(mems) < 2 {
		g.SparseEvidence = append(g.SparseEvidence, fmt.Sprintf("Only %d matching memory was found; the answer lacks independent corroboration.", len(mems)))
	}
	sources := map[string]bool{}
	for _, m := range mems {
		source := m.Source
		if source == "" {
			source = m.Provider
		}
		if source == "" {
			source = m.Type
		}
		sources[source] = true
	}
	if len(sources) == 1 {
		var source string
		for s := range sources {
			source = s
		}
		g.SourceCoverage = append(g.SourceCoverage, fmt.Sprintf("All matching evidence comes from %s; no other source corroborates it.", source))
	}
	if OutcomeQuestion(query) && OnlyProspectiveEvidence(mems) {
		g.TemporalState = append(g.TemporalState, "The evidence shows only invitation or scheduling state; Mora has no evidence that the event was completed or that an outcome/result was recorded.")
	}
	return g
}
func OutcomeQuestion(query string) bool {
	words := wordSet(query)
	for _, term := range []string{"outcome", "result", "results", "accepted", "rejected", "offer", "decision", "completed", "happened"} {
		if words[term] {
			return true
		}
	}
	lower := strings.ToLower(query)
	return strings.Contains(lower, "how did") && (words["interview"] || words["meeting"] || words["event"])
}
func OnlyProspectiveEvidence(mems []memory.Memory) bool {
	prospective := 0
	for _, m := range mems {
		words := wordSet(m.Title + "\n" + m.Text)
		for _, term := range []string{"completed", "finished", "happened", "attended", "outcome", "result", "accepted", "rejected", "offer", "passed", "failed", "hired", "withdrew", "cancelled", "canceled"} {
			if words[term] {
				return false
			}
		}
		for _, term := range []string{"invite", "invited", "invitation", "schedule", "scheduled", "scheduling", "upcoming", "calendar", "confirmed", "confirmation"} {
			if words[term] {
				prospective++
				break
			}
		}
	}
	return prospective > 0 && prospective == len(mems)
}
func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		out[word] = true
	}
	return out
}

func Prompt(query string, ev []Evidence, gaps Gaps, loops []openloops.Person) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Answer the question using ONLY the evidence below. Cite every claim with its [stable_id]. ")
	b.WriteString("If the evidence is insufficient, say so plainly rather than guessing.\n\n")
	fmt.Fprintf(&b, "QUESTION: %s\n\nEVIDENCE:\n", query)
	if len(ev) == 0 {
		b.WriteString("(none found)\n")
	}
	for _, e := range ev {
		if e.Owner != "" {
			// Shared evidence is labeled so the synthesis attributes claims to
			// the sharing party, never to the user's own vault.
			fmt.Fprintf(&b, "- [%s] (shared:%s, %s, %s) %s — %s\n", e.StableID, e.Owner, e.Scope, e.CreatedAt, e.Title, e.Snippet)
			continue
		}
		fmt.Fprintf(&b, "- [%s] (%s, %s) %s — %s\n", e.StableID, e.Scope, e.CreatedAt, e.Title, e.Snippet)
	}
	if !gaps.Empty() {
		b.WriteString("\nKNOWN GAPS (surface these honestly in a 'What the vault does not know' section):\n")
		for _, s := range gaps.Stale {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.FreshnessUnknown {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.SparseEvidence {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.SourceCoverage {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.TemporalState {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.ThinCoverage {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.CoverageHoles {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.RetrievalCaveats {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	b.WriteString(openloops.Render(loops))
	return b.String()
}
