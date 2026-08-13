package mora

import (
	"context"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type contextQueryIntent string

const (
	contextIntentGeneric      contextQueryIntent = "generic"
	contextIntentCurrentState contextQueryIntent = "current_state"
	contextIntentOpenLoops    contextQueryIntent = "open_loops"
)

// contextIntentOf recognizes the two broad daily-driver questions that need
// structured data instead of ordinary topic search. Keep this list narrow. A
// normal query must continue to use hybrid retrieval.
func contextIntentOf(query string) contextQueryIntent {
	lower := strings.ToLower(strings.Join(strings.Fields(query), " "))
	for _, phrase := range []string{
		"what am i waiting on",
		"what do i owe",
		"what do we owe",
		"what is still open",
		"what's still open",
		"what has closed",
		"what's closed",
		"open loops",
		"open commitments",
		"outstanding commitments",
		"unfinished commitments",
	} {
		if strings.HasPrefix(lower, phrase) {
			return contextIntentOpenLoops
		}
	}
	for _, phrase := range []string{
		"what materially changed",
		"what has materially changed",
		"what changed across",
		"active projects recently",
		"current state of my projects",
		"current state across my projects",
		"recent project changes",
	} {
		if strings.HasPrefix(lower, phrase) {
			return contextIntentCurrentState
		}
	}
	return contextIntentGeneric
}

func contextQueryData(ctx context.Context, cfg Config, query, scope string, limit int, filters searchFilters, now time.Time) ([]Memory, []Commitment, contextQueryIntent, error) {
	intent := contextIntentOf(query)
	switch intent {
	case contextIntentCurrentState:
		items, err := currentStateContextItems(cfg, query, scope, limit, filters)
		return items, nil, intent, err
	case contextIntentOpenLoops:
		commitments, err := openCommitmentsForContext(ctx, cfg, query, scope, filters, now)
		return nil, commitments, intent, err
	default:
		items, err := hybridSearch(ctx, cfg, query, scope, limit, filters)
		return items, nil, intent, err
	}
}

// currentStateContextItems starts from the complete newest-first browse list,
// then puts direct project evidence before bulk mail. It does not claim that a
// record changed anything. It only gives the caller recent evidence from which
// to make that judgment.
func currentStateContextItems(cfg Config, query, scope string, limit int, filters searchFilters) ([]Memory, error) {
	items, err := listMemories(cfg, scope, 0, filters)
	if err != nil {
		return nil, err
	}
	qualifiers := contextQualifierTerms(query, contextIntentCurrentState)
	if len(qualifiers) > 0 {
		filtered := items[:0]
		for _, item := range items {
			if contextTermsMatch(qualifiers, strings.Join([]string{item.Scope, item.Title, item.Text, strings.Join(item.Tags, " ")}, "\n")) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	sort.SliceStable(items, func(i, j int) bool {
		return currentStateRank(items[i]) < currentStateRank(items[j])
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func currentStateRank(m Memory) int {
	project := strings.HasPrefix(strings.ToLower(strings.TrimSpace(m.Scope)), "project:")
	service := memoryIsServiceOnly(m)
	switch {
	case project && !service:
		return 0
	case !service:
		return 1
	case project:
		return 2
	default:
		return 3
	}
}

// openCommitmentsForContext reads the typed commitment generation, keeps only
// canonical open rows, and applies the caller's scope/source/time filters to
// each opening memory. Closed rows never enter this surface.
func openCommitmentsForContext(ctx context.Context, cfg Config, query, scope string, filters searchFilters, now time.Time) ([]Commitment, error) {
	inventory, memories, err := readCommitmentInventoryWithMemories(ctx, cfg, now)
	if err != nil {
		return nil, err
	}
	qualifiers := contextQualifierTerms(query, contextIntentOpenLoops)
	var out []Commitment
	for memoryID, commitments := range inventory {
		m, ok := memories[memoryID]
		if !ok {
			continue
		}
		if scope != "" && m.Scope != scope {
			continue
		}
		if filters.Active() && !searchFilterPasses(filters, m) {
			continue
		}
		for _, commitment := range commitments {
			searchable := strings.Join([]string{
				m.Scope, m.Title, m.Text, commitment.Summary,
				commitment.Counterparty.Value, commitment.CounterpartyLabel,
			}, "\n")
			if commitment.State == commitOpen && commitment.DuplicateOf == "" && contextTermsMatch(qualifiers, searchable) {
				out = append(out, commitment)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		left, leftOK := rfc3339Instant(out[i].OpenedBy.OccurredAt)
		right, rightOK := rfc3339Instant(out[j].OpenedBy.OccurredAt)
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
	return out, nil
}

var currentStateIntentWords = map[string]bool{
	"what": true, "has": true, "materially": true, "changed": true,
	"change": true, "changes": true, "across": true, "my": true,
	"active": true, "projects": true, "project": true, "recently": true,
	"recent": true, "current": true, "state": true, "of": true,
}

var openLoopIntentWords = map[string]bool{
	"what": true, "am": true, "i": true, "we": true, "me": true,
	"my": true, "waiting": true, "on": true, "do": true, "owe": true,
	"is": true, "still": true, "open": true, "has": true, "closed": true,
	"and": true, "loops": true, "loop": true, "commitments": true,
	"commitment": true, "outstanding": true, "unfinished": true,
}

func contextQualifierTerms(query string, intent contextQueryIntent) []string {
	ignored := currentStateIntentWords
	if intent == contextIntentOpenLoops {
		ignored = openLoopIntentWords
	}
	var out []string
	seen := map[string]bool{}
	for _, word := range strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		word = strings.ToLower(word)
		if word == "" || utf8.RuneCountInString(word) < 2 || ignored[word] || ftsStopwords[word] || seen[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

func contextTermsMatch(terms []string, text string) bool {
	if len(terms) == 0 {
		return true
	}
	words := map[string]bool{}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		words[strings.ToLower(word)] = true
	}
	for _, term := range terms {
		if !words[term] {
			return false
		}
	}
	return true
}

func renderOpenCommitmentContext(commitments []Commitment, budget int) string {
	if budget <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Open commitments\n")
	if len(commitments) == 0 {
		b.WriteString("No open commitments matched this request.\n")
		return genericutil.TruncateRunes(b.String(), budget)
	}
	for _, commitment := range commitments {
		state := commitOpen
		if commitment.StateUncertain {
			state += "; source freshness uncertain"
		}
		fmt.Fprintf(&b, "- %s [%s; %s]", commitment.Summary, state, commitment.Direction)
		if due := commitDueValue(commitment.Due); due != commitDueNone {
			fmt.Fprintf(&b, " [due: %s]", due)
		}
		b.WriteString("\n")
		ref := commitment.OpenedBy.MessageRef
		if ref == "" {
			ref = commitment.OpenedBy.MemoryID
		}
		fmt.Fprintf(&b, "  Evidence: %s", ref)
		if commitment.OpenedBy.OccurredAt != "" {
			fmt.Fprintf(&b, " at %s", commitment.OpenedBy.OccurredAt)
		}
		b.WriteString("\n")
	}
	return genericutil.TruncateRunes(b.String(), budget)
}
