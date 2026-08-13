package search

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// BuildContext assembles static vault controls and selected memories within a deterministic budget.
func BuildContext(cfg config.Config, items []memory.Memory, budget int, hasQuery bool) string {
	if budget <= 0 {
		return ""
	}
	var wiki strings.Builder
	for _, rel := range []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md"} {
		if body, err := os.ReadFile(filepath.Join(cfg.VaultDir, rel)); err == nil {
			fmt.Fprintf(&wiki, "\n# %s\n%s\n", rel, string(body))
		}
	}
	var its strings.Builder
	for _, m := range items {
		if m.Decision != nil {
			fmt.Fprintf(&its, "\n# %s\nDecision status: %s\nAs of: %s\nDurability: %s\nFlip conditions: %s\n", m.Title, m.DecisionStatus, m.Decision.AsOf, m.Decision.Durability, strings.Join(m.Decision.FlipConditions, "; "))
			if m.Decision.ReviewBy != "" {
				fmt.Fprintf(&its, "Review by: %s\n", m.Decision.ReviewBy)
			}
			fmt.Fprintf(&its, "%s\n", m.Text)
			continue
		}
		fmt.Fprintf(&its, "\n# %s\n%s\n", m.Title, m.Text)
	}
	first, second := wiki.String(), its.String()
	if hasQuery {
		first, second = its.String(), wiki.String()
	}
	var out strings.Builder
	out.WriteString(genericutil.TruncateRunes(first, budget))
	if rem := budget - out.Len(); rem > 0 {
		out.WriteString(genericutil.TruncateRunes(second, rem))
	}
	return out.String()
}

// BudgetResults keeps the largest whole-record prefix under a conservative JSON byte budget.
func BudgetResults(mems []memory.Memory, budgetBytes int) (kept []memory.Memory, dropped int) {
	if budgetBytes <= 0 || len(mems) == 0 {
		return mems, 0
	}
	const jsonSep = 2
	kept = make([]memory.Memory, 0, len(mems))
	used := 0
	for _, m := range mems {
		body, err := json.Marshal(m)
		cost := jsonSep
		if err == nil {
			cost += len(body)
		}
		if used+cost > budgetBytes && len(kept) > 0 {
			break
		}
		kept = append(kept, m)
		used += cost
	}
	return kept, len(mems) - len(kept)
}
