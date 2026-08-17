// Package contextintent owns deterministic intent routing and structured context selection.
package contextintent

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/recency"
)

type Intent string

const (
	Generic      Intent = "generic"
	CurrentState Intent = "current_state"
	OpenLoops    Intent = "open_loops"
)

func Of(query string) Intent {
	lower := strings.ToLower(strings.Join(strings.Fields(query), " "))
	for _, phrase := range []string{"what am i waiting on", "what do i owe", "what do we owe", "what is still open", "what's still open", "what has closed", "what's closed", "open loops", "open commitments", "outstanding commitments", "unfinished commitments"} {
		if strings.HasPrefix(lower, phrase) {
			return OpenLoops
		}
	}
	for _, phrase := range []string{"what materially changed", "what has materially changed", "what changed across", "active projects recently", "current state of my projects", "current state across my projects", "recent project changes"} {
		if strings.HasPrefix(lower, phrase) {
			return CurrentState
		}
	}
	return Generic
}

var currentWords = map[string]bool{"what": true, "has": true, "materially": true, "changed": true, "change": true, "changes": true, "across": true, "my": true, "active": true, "projects": true, "project": true, "recently": true, "recent": true, "current": true, "state": true, "of": true}
var loopWords = map[string]bool{"what": true, "am": true, "i": true, "we": true, "me": true, "my": true, "waiting": true, "on": true, "do": true, "owe": true, "is": true, "still": true, "open": true, "has": true, "closed": true, "and": true, "loops": true, "loop": true, "commitments": true, "commitment": true, "outstanding": true, "unfinished": true}

func QualifierTerms(query string, intent Intent, stopwords map[string]bool) []string {
	ignored := currentWords
	if intent == OpenLoops {
		ignored = loopWords
	}
	var out []string
	seen := map[string]bool{}
	for _, word := range strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		word = strings.ToLower(word)
		if word == "" || utf8.RuneCountInString(word) < 2 || ignored[word] || stopwords[word] || seen[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}
func TermsMatch(terms []string, text string) bool {
	if len(terms) == 0 {
		return true
	}
	words := map[string]bool{}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words[strings.ToLower(word)] = true
	}
	for _, term := range terms {
		if !words[term] {
			return false
		}
	}
	return true
}
func Rank(m memory.Memory, serviceOnly bool) int {
	project := strings.HasPrefix(strings.ToLower(strings.TrimSpace(m.Scope)), "project:")
	switch {
	case project && !serviceOnly:
		return 0
	case !serviceOnly:
		return 1
	case project:
		return 2
	default:
		return 3
	}
}
func CurrentItems(items []memory.Memory, terms []string, limit int, isServiceOnly func(memory.Memory) bool) []memory.Memory {
	if len(terms) > 0 {
		filtered := items[:0]
		for _, item := range items {
			if TermsMatch(terms, strings.Join([]string{item.Scope, item.Title, item.Text, strings.Join(item.Tags, " ")}, "\n")) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	sort.SliceStable(items, func(i, j int) bool {
		return Rank(items[i], isServiceOnly(items[i])) < Rank(items[j], isServiceOnly(items[j]))
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}
func OpenItems(inventory map[string][]commitment.Record, memories map[string]memory.Memory, terms []string, scope string, accept func(memory.Memory) bool) []commitment.Record {
	var out []commitment.Record
	for memoryID, records := range inventory {
		m, ok := memories[memoryID]
		if !ok {
			continue
		}
		if scope != "" && m.Scope != scope {
			continue
		}
		if accept != nil && !accept(m) {
			continue
		}
		for _, record := range records {
			searchable := strings.Join([]string{m.Scope, m.Title, m.Text, record.Summary, record.Counterparty.Value, record.CounterpartyLabel}, "\n")
			if record.State == commitment.Open && record.DuplicateOf == "" && TermsMatch(terms, searchable) {
				out = append(out, record)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		left, leftOK := instant(out[i].OpenedBy.OccurredAt)
		right, rightOK := instant(out[j].OpenedBy.OccurredAt)
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && !left.Equal(right) {
			return left.After(right)
		}
		if out[i].OpenedBy.MemoryID != out[j].OpenedBy.MemoryID {
			return out[i].OpenedBy.MemoryID < out[j].OpenedBy.MemoryID
		}
		return out[i].ID < out[j].ID
	})
	return out
}
func instant(value string) (time.Time, bool) { return recency.Instant(value) }
func RenderOpen(records []commitment.Record, budget int) string {
	if budget <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Open commitments\n")
	if len(records) == 0 {
		b.WriteString("No open commitments matched this request.\n")
		return genericutil.TruncateRunes(b.String(), budget)
	}
	for _, record := range records {
		state := commitment.Open
		if record.StateUncertain {
			state += "; source freshness uncertain"
		}
		fmt.Fprintf(&b, "- %s [%s; %s]", record.Summary, state, record.Direction)
		if due := commitment.DueValue(record.Due); due != commitment.DueNone {
			fmt.Fprintf(&b, " [due: %s]", due)
		}
		b.WriteString("\n")
		ref := record.OpenedBy.MessageRef
		if ref == "" {
			ref = record.OpenedBy.MemoryID
		}
		fmt.Fprintf(&b, "  Evidence: %s", ref)
		if record.OpenedBy.OccurredAt != "" {
			fmt.Fprintf(&b, " at %s", record.OpenedBy.OccurredAt)
		}
		b.WriteString("\n")
	}
	return genericutil.TruncateRunes(b.String(), budget)
}
