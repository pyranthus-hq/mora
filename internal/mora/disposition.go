package mora

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/disposition"
	"github.com/pyranthus-hq/mora/internal/memory"
	searchpkg "github.com/pyranthus-hq/mora/internal/search"
)

// validateDispositionPublish is called after request shaping and before every
// publication/staging boundary. It never accepts a connector-shaped correction.
func validateDispositionPublish(cfg Config, m Memory) error {
	if m.Type != "correction" || m.Meta == nil {
		return nil
	}
	targetID, hasTarget := m.Meta["target"].(string)
	if !hasTarget || targetID == "" {
		return nil
	}
	if !disposition.IsLocalAuthored(m) {
		return newMoraError(errCodeUsageUnknownValue, "usage", nil, "typed corrections require a local authored source")
	}
	target, err := findMemory(cfg, targetID)
	if err != nil {
		return fmt.Errorf("correction target: %w", err)
	}
	if err := disposition.ValidateTarget(m, target); err != nil {
		return newMoraError(errCodeUsageUnknownValue, "usage", err, "%v", err)
	}
	return nil
}

// decorateDispositions overlays the latest visible local correction from the
// whole vault. It intentionally does not call listMemories: callers may be
// decorating listMemories results, and query/page subsets cannot decide latest.
func decorateDispositions(cfg Config, rows []Memory, now time.Time, filters ...searchFilters) ([]Memory, error) {
	return decorateCorrectionRows(cfg, rows, now, oneFilter(filters), false)
}

// decorateExplicitCorrections uses the same one-pass visible-vault projection
// as dispositions. It expands only the selected rows, never scans per result.
func decorateExplicitCorrections(cfg Config, rows []Memory, now time.Time, filter searchFilters) ([]Memory, error) {
	return decorateCorrectionRows(cfg, rows, now, filter, true)
}

const maxExplicitCorrections = 2
const explicitCorrectionExcerptRunes = 180

func decorateCorrectionRows(cfg Config, rows []Memory, now time.Time, filter searchFilters, includeLinks bool) ([]Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return nil, err
	}
	all := make([]Memory, 0, len(files))
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil || m.DeletedAt != "" || !g.memoryVisible(m.ID) {
			continue
		}
		all = append(all, decorateDecision(m, now))
	}
	all = suppressPendingDeletes(cfg, all)
	// Retrieval prepares disposition exclusions before ranking. Reconstruct that
	// boundary from this same vault snapshot so linked corrections cannot bypass
	// it without a second full-vault read.
	if len(filter.ExcludeDispositions) > 0 && len(filter.ExcludedMemoryIDs) == 0 {
		wanted := make(map[string]bool, len(filter.ExcludeDispositions))
		for _, value := range filter.ExcludeDispositions {
			wanted[value] = true
		}
		for key, d := range disposition.Project(all, now) {
			if wanted[d.Value] {
				filter.ExcludedMemoryIDs = append(filter.ExcludedMemoryIDs, key.ID)
			}
		}
	}
	targets := make(map[disposition.Key]Memory, len(all))
	for _, m := range all {
		targets[disposition.Key{Scope: m.Scope, ID: m.ID}] = m
	}
	filtered := make([]Memory, 0, len(all))
	for _, m := range all {
		if !filter.Active() || searchFilterPasses(filter, m) {
			filtered = append(filtered, m)
		}
	}
	projected := disposition.Project(filtered, now)
	links := map[disposition.Key][]Memory{}
	omitted := map[disposition.Key]bool{}
	if includeLinks {
		selectedTargets := make(map[disposition.Key]bool, len(rows))
		for _, row := range rows {
			if row.Owner == "" {
				selectedTargets[disposition.Key{Scope: row.Scope, ID: row.ID}] = true
			}
		}
		for _, c := range all {
			if c.Type != "correction" || c.Owner != "" || c.Provider != "" || c.ProviderID != "" || !disposition.IsLocalAuthored(c) || c.Meta == nil {
				continue
			}
			targetID, ok := c.Meta["target"].(string)
			if !ok || targetID == "" || disposition.ValidateFields(targetID, "") != nil {
				continue
			}
			key := disposition.Key{Scope: c.Scope, ID: targetID}
			if !selectedTargets[key] {
				continue
			}
			target, exists := targets[key]
			if !exists || disposition.ValidateTarget(c, target) != nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, c.CreatedAt)
			targetAt, targetErr := time.Parse(time.RFC3339, target.CreatedAt)
			if err != nil || targetErr != nil || at.After(now) || at.Before(targetAt) {
				continue
			}
			if filter.Active() && !searchFilterPasses(filter, c) {
				omitted[key] = true
				continue
			}
			links[key] = append(links[key], c)
		}
		for key := range links {
			sort.Slice(links[key], func(i, j int) bool {
				left, _ := time.Parse(time.RFC3339, links[key][i].CreatedAt)
				right, _ := time.Parse(time.RFC3339, links[key][j].CreatedAt)
				if !left.Equal(right) {
					return left.After(right)
				}
				return links[key][i].ID > links[key][j].ID
			})
		}
	}
	out := append([]Memory(nil), rows...)
	for i := range out {
		if out[i].Owner != "" {
			continue
		}
		out[i].Disposition = nil // clear a prior projection when its correction disappears
		key := disposition.Key{Scope: out[i].Scope, ID: out[i].ID}
		if d, ok := projected[key]; ok {
			dCopy := d
			out[i].Disposition = &dCopy
		}
		if !includeLinks {
			continue
		}
		out[i].ExplicitCorrections = nil
		out[i].CorrectionOmitted = omitted[key]
		selected := links[key]
		if len(selected) > maxExplicitCorrections {
			out[i].CorrectionOmitted = true
			selected = append([]Memory(nil), selected[:maxExplicitCorrections]...)
			if out[i].Disposition != nil {
				selectedHasDisposition := false
				for _, c := range selected {
					selectedHasDisposition = selectedHasDisposition || c.ID == out[i].Disposition.CorrectionID
				}
				if !selectedHasDisposition {
					for _, c := range links[key][maxExplicitCorrections:] {
						if c.ID == out[i].Disposition.CorrectionID {
							selected[len(selected)-1] = c
							break
						}
					}
				}
			}
		}
		for _, c := range selected {
			flat := strings.Join(strings.Fields(c.Text), " ")
			dispositionValue, _ := c.Meta["disposition"].(string)
			if !disposition.Valid(dispositionValue) {
				dispositionValue = ""
			}
			out[i].ExplicitCorrections = append(out[i].ExplicitCorrections, memory.ExplicitCorrection{
				ID: c.ID, CreatedAt: c.CreatedAt, Provenance: memory.DeriveProvenance(c),
				Text:        searchpkg.MatchSnippet(flat, "", explicitCorrectionExcerptRunes),
				Truncated:   utf8.RuneCountInString(flat) > explicitCorrectionExcerptRunes,
				Disposition: dispositionValue,
			})
		}
	}
	return out, nil
}
