package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func contextCfg(t *testing.T) config.Config {
	t.Helper()
	v := t.TempDir()
	for _, name := range []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md"} {
		if err := os.WriteFile(filepath.Join(v, name), []byte("control"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{VaultDir: v}
}
func TestBuildContextOrderingAndBudget(t *testing.T) {
	cfg := contextCfg(t)
	if err := os.WriteFile(filepath.Join(cfg.VaultDir, "priority-map.md"), []byte(strings.Repeat("W", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}
	items := []memory.Memory{{Title: "QUERYHIT", Text: "relevant"}}
	query := BuildContext(cfg, items, 2000, true)
	if !strings.Contains(query, "QUERYHIT") || len(query) > 2000 {
		t.Fatalf("query context len=%d body=%q", len(query), query)
	}
	noQuery := BuildContext(cfg, items, 2000, false)
	wikiAt, itemAt := strings.Index(noQuery, "priority-map.md"), strings.Index(noQuery, "QUERYHIT")
	if wikiAt < 0 || (itemAt >= 0 && wikiAt > itemAt) {
		t.Fatalf("wiki must lead no-query context: %q", noQuery)
	}
	if BuildContext(cfg, items, 0, true) != "" {
		t.Fatal("zero budget must be empty")
	}
}
func TestBuildContextDecisionAndRuneSafety(t *testing.T) {
	cfg := contextCfg(t)
	items := []memory.Memory{{Title: "T", Text: strings.Repeat("🚀中", 50), Decision: &memory.DecisionValidity{AsOf: "2026-01-01T00:00:00Z", Durability: "working", FlipConditions: []string{"new evidence"}, ReviewBy: "2026-12-01T00:00:00Z"}, DecisionStatus: "current"}}
	full := BuildContext(cfg, items, 2000, true)
	for _, want := range []string{"Decision status: current", "Durability: working", "Flip conditions: new evidence", "Review by: 2026-12-01"} {
		if !strings.Contains(full, want) {
			t.Fatalf("context missing %q: %s", want, full)
		}
	}
	for _, budget := range []int{10, 17, 23, 42, 100} {
		out := BuildContext(cfg, items, budget, true)
		if len(out) > budget || !utf8.ValidString(out) {
			t.Fatalf("budget %d: len=%d valid=%v", budget, len(out), utf8.ValidString(out))
		}
	}
}
func TestBudgetResults(t *testing.T) {
	mems := []memory.Memory{{ID: "a", Text: strings.Repeat("x", 20)}, {ID: "b", Text: strings.Repeat("y", 20)}, {ID: "c", Text: strings.Repeat("z", 20)}}
	all, dropped := BudgetResults(mems, 0)
	if len(all) != 3 || dropped != 0 {
		t.Fatalf("disabled=(%d,%d)", len(all), dropped)
	}
	one, dropped := BudgetResults(mems, 1)
	if len(one) != 1 || dropped != 2 {
		t.Fatalf("forced first=(%d,%d)", len(one), dropped)
	}
	if got, d := BudgetResults(nil, 10); got != nil || d != 0 {
		t.Fatalf("nil=(%v,%d)", got, d)
	}
}

func TestBudgetResultsGenerous(t *testing.T) {
	mems := []memory.Memory{{ID: "a", Text: "a"}, {ID: "b", Text: "b"}}
	got, dropped := BudgetResults(mems, 1_000_000)
	if len(got) != 2 || dropped != 0 {
		t.Fatalf("got=(%d,%d)", len(got), dropped)
	}
}
