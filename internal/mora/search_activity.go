package mora

import (
	"context"
	"sort"
	"time"

	"github.com/pyranthus-hq/mora/internal/activity"
	"github.com/pyranthus-hq/mora/internal/disposition"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func prepareDispositionFilter(cfg Config, f searchFilters) (searchFilters, error) {
	if len(f.ExcludeDispositions) == 0 {
		return f, nil
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return f, err
	}
	governance, err := loadGovernance(cfg)
	if err != nil {
		return f, err
	}
	all := make([]Memory, 0, len(files))
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil || m.DeletedAt != "" || !governance.memoryVisible(m.ID) {
			continue
		}
		all = append(all, m)
	}
	wanted := map[string]bool{}
	for _, value := range f.ExcludeDispositions {
		wanted[value] = true
	}
	f.ExcludedMemoryIDs = nil
	for key, d := range disposition.Project(all, f.Now) {
		if wanted[d.Value] {
			f.ExcludedMemoryIDs = append(f.ExcludedMemoryIDs, key.ID)
		}
	}
	sort.Strings(f.ExcludedMemoryIDs)
	return f, nil
}

// defaultSearchForMCP applies exclusions before any retrieval pool. Its bounded
// counterfactual uses identical source/time/query/limit settings without the
// disposition predicate. The count describes that ranked page, not the corpus.
func defaultSearchForMCP(ctx context.Context, cfg Config, query, scope string, limit int, filters ...searchFilters) (mcpSearchResult, error) {
	f, err := prepareDispositionFilter(cfg, oneFilter(filters))
	if err != nil {
		return mcpSearchResult{}, err
	}
	out, err := defaultSearchForMCPFiltered(ctx, cfg, query, scope, limit, f)
	if err != nil {
		return out, err
	}
	if len(f.ExcludeDispositions) > 0 {
		baselineFilter := f
		baselineFilter.ExcludedMemoryIDs = nil
		baseline, err := defaultSearchForMCPFiltered(ctx, cfg, query, scope, limit, baselineFilter)
		if err != nil {
			return out, err
		}
		excluded := map[string]bool{}
		for _, id := range f.ExcludedMemoryIDs {
			excluded[id] = true
		}
		seen := map[string]bool{}
		for _, m := range baseline.Results {
			if m.Owner != "" || !excluded[m.ID] {
				continue
			}
			key := m.Scope + "\x00" + m.ID
			if !seen[key] {
				out.ExcludedByDisposition++
				seen[key] = true
			}
		}
	}
	if f.EventSinceHours > 0 || len(f.ExcludeDispositions) > 0 {
		out.Results = decorateActivitySearchRows(out.Results, f.Now)
	}
	return out, nil
}

func decorateActivitySearchRows(rows []Memory, now time.Time) []Memory {
	out := append([]Memory(nil), rows...)
	for i := range out {
		// Shared rows are hydrated from their owner's index and intentionally
		// lack raw Meta here. Keep that index projection, never replace it with
		// facts rederived from an empty local representation.
		if out[i].Owner != "" {
			continue
		}
		facts := activity.Derive(out[i], now)
		out[i].Participation = facts.Participation
		out[i].Automated = &memory.NullableBool{Value: facts.Automated}
		if facts.EventAt != nil {
			out[i].EventAt = facts.EventAt.UTC().Format(time.RFC3339Nano)
		}
	}
	return out
}
