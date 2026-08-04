package mora

import (
	"strings"
	"unicode"
)

// supersessionTitleNoise removes grammar, generic bookkeeping words, and
// lifecycle-state words before two titles are compared. Removing state words
// is intentional: "Issue 62 implementation not yet PR'd" and "Issue 62
// implementation merged" should retain the same subject signature. The
// resulting relation is still only a hint, never an inferred closure.
var supersessionTitleNoise = map[string]bool{
	"a": true, "an": true, "and": true, "at": true, "for": true,
	"from": true, "in": true, "of": true, "on": true, "or": true,
	"re": true, "the": true, "to": true, "with": true,
	"issue": true, "project": true, "status": true, "update": true,
	"closed": true, "complete": true, "completed": true, "done": true,
	"draft": true, "landed": true, "merged": true, "not": true,
	"open": true, "pending": true, "pr": true, "yet": true,
}

func supersessionTitleTerms(title string) map[string]bool {
	terms := map[string]bool{}
	for _, term := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(term)) < 2 || supersessionTitleNoise[term] {
			continue
		}
		terms[term] = true
	}
	return terms
}

// stronglyRelatedTitles is precision-first. Two titles need at least two
// meaningful shared terms and those terms must cover at least 75% of the
// smaller title signature. A shared person's name alone is never enough to
// claim that their unrelated records belong to one lifecycle.
func stronglyRelatedTitles(a, b string) bool {
	at, bt := supersessionTitleTerms(a), supersessionTitleTerms(b)
	if len(at) < 2 || len(bt) < 2 {
		return false
	}
	if len(bt) < len(at) {
		at, bt = bt, at
	}
	shared := 0
	for term := range at {
		if bt[term] {
			shared++
		}
	}
	return shared >= 2 && shared*4 >= len(at)*3
}

// annotateLaterRelatedEvidence attaches, to each returned row, the newest
// strongly-related record found in the deeper pre-truncation candidate pool.
// It does not reorder results or assert closure. The pointer is derived after
// visibility filtering, so pending deletes, retracted memories, and explicitly
// superseded Teach revisions can never be cited as the newer evidence.
func annotateLaterRelatedEvidence(results, pool []Memory) []Memory {
	for i := range results {
		rowAt, rowOK := ingestRecencyOf(results[i])
		if !rowOK {
			continue
		}
		skip := map[string]bool{results[i].ID: true}
		for _, corroborating := range results[i].Corroborating {
			skip[corroborating.ID] = true
		}
		var newest Memory
		var newestAt string
		var newestInstant = rowAt
		for _, candidate := range pool {
			if skip[candidate.ID] || candidate.Scope != results[i].Scope || !stronglyRelatedTitles(results[i].Title, candidate.Title) {
				continue
			}
			candidateInstant, ok := ingestRecencyOf(candidate)
			if !ok || !candidateInstant.After(rowAt) {
				continue
			}
			candidateIndexedAt, _ := indexedAtOf(candidate)
			if candidateInstant.After(newestInstant) || (candidateInstant.Equal(newestInstant) && candidate.ID < newest.ID) {
				newest = candidate
				newestAt = candidateIndexedAt
				newestInstant = candidateInstant
			}
		}
		if newest.ID != "" {
			results[i].LaterRelatedEvidence = &LaterRelatedEvidence{
				ID: newest.ID, Title: newest.Title, Source: newest.Source, IndexedAt: newestAt,
			}
		}
	}
	return results
}
