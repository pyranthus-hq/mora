package mora

import (
	"context"
	"github.com/pyranthus-hq/mora/internal/contextintent"
	"time"
)

type contextQueryIntent = contextintent.Intent

const (
	contextIntentGeneric      = contextintent.Generic
	contextIntentCurrentState = contextintent.CurrentState
	contextIntentOpenLoops    = contextintent.OpenLoops
)

// contextIntentOf recognizes the two broad daily-driver questions that need
// structured data instead of ordinary topic search. Keep this list narrow. A
// normal query must continue to use hybrid retrieval.
func contextIntentOf(query string) contextQueryIntent { return contextintent.Of(query) }

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
	return contextintent.CurrentItems(items, contextQualifierTerms(query, contextIntentCurrentState), limit, memoryIsServiceOnly), nil
}

// openCommitmentsForContext reads the typed commitment generation, keeps only
// canonical open rows, and applies the caller's scope/source/time filters to
// each opening memory. Closed rows never enter this surface.
func openCommitmentsForContext(ctx context.Context, cfg Config, query, scope string, filters searchFilters, now time.Time) ([]Commitment, error) {
	inventory, memories, err := readCommitmentInventoryWithMemories(ctx, cfg, now)
	if err != nil {
		return nil, err
	}
	accept := func(m Memory) bool { return !filters.Active() || searchFilterPasses(filters, m) }
	return contextintent.OpenItems(inventory, memories, contextQualifierTerms(query, contextIntentOpenLoops), scope, accept), nil
}

func contextQualifierTerms(query string, intent contextQueryIntent) []string {
	return contextintent.QualifierTerms(query, intent, ftsStopwords)
}

func renderOpenCommitmentContext(commitments []Commitment, budget int) string {
	return contextintent.RenderOpen(commitments, budget)
}
