package resource

import (
	"sort"
	"strings"
)

// Discover returns only the trusted integrator's eligible bindings for the
// exact project plus explicit global fallbacks. It has no logging side effect.
func Discover(root string, q Query) ([]Result, error) {
	if !strings.HasPrefix(q.ProjectScope, "project:") || len(q.ProjectScope) == len("project:") {
		return nil, ErrInvalidCandidate
	}
	selected := make(map[string]Binding)
	for _, b := range q.Bindings {
		if !b.Eligible || b.Retired || !validID(b.ResourceID) || !validDigest(b.RevisionDigest) {
			continue
		}
		if b.Scope != q.ProjectScope && b.Scope != "global" {
			continue
		}
		current, ok := selected[b.ResourceID]
		if !ok || (b.Scope == q.ProjectScope && current.Scope == "global") {
			selected[b.ResourceID] = b
		}
	}
	tokens := tokenSet(q.Task)
	if len(tokens) == 0 {
		return []Result{}, nil
	}
	rows := make([]scored, 0, len(selected))
	for _, binding := range selected {
		record, revision, err := ReadRevision(root, binding.ResourceID, binding.RevisionDigest)
		if err != nil {
			continue
		} // An invalid binding cannot reveal unrelated vault metadata.
		if revision.EffectiveScope != "" && revision.EffectiveScope != q.ProjectScope && revision.EffectiveScope != "global" {
			continue
		}
		if revision.Kind == KindSkill && (!binding.Active || !revision.Complete) {
			continue
		}
		score, reason := scoreRevision(revision, tokens)
		if score == 0 {
			continue
		}
		rows = append(rows, scored{score: score, result: Result{ResourceID: record.StableID, Kind: revision.Kind, Purpose: revision.Purpose, MatchReason: reason, Scope: binding.Scope, Source: revision.Source, RevisionDigest: revision.RevisionDigest, Availability: revision.Availability, KnownLimits: append([]string(nil), revision.KnownLimits...), ReviewState: reviewState(revision, binding)}})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return rows[i].score > rows[j].score
		}
		if rows[i].result.ResourceID != rows[j].result.ResourceID {
			return rows[i].result.ResourceID < rows[j].result.ResourceID
		}
		return rows[i].result.RevisionDigest < rows[j].result.RevisionDigest
	})
	if len(rows) > 5 {
		rows = rows[:5]
	}
	out := make([]Result, len(rows))
	for i := range rows {
		out[i] = rows[i].result
	}
	return out, nil
}

type scored struct {
	score  int
	result Result
}

func reviewState(r Revision, b Binding) string {
	if r.Kind == KindSkill && b.Active {
		return "approved"
	}
	return "unverified"
}
func scoreRevision(r Revision, tokens map[string]struct{}) (int, string) {
	var matched []string
	for _, field := range append([]string{r.Purpose}, r.Applicability...) {
		for _, token := range words(field) {
			if _, ok := tokens[token]; ok {
				matched = append(matched, token)
			}
		}
	}
	sort.Strings(matched)
	matched = compact(matched)
	if len(matched) == 0 {
		return 0, "metadata match unavailable"
	}
	return len(matched), "matches " + strings.Join(matched, ", ")
}
func tokenSet(s string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, word := range words(s) {
		set[word] = struct{}{}
	}
	return set
}
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
}
func compact(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
