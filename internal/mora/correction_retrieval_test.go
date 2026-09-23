package mora

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedExplicitCorrectionFixture(t *testing.T, target Memory, corrections ...Memory) Config {
	t.Helper()
	cfg := coreBIngestInitCfg(t)
	for _, m := range append([]Memory{target}, corrections...) {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestExplicitCorrectionHiddenByGovernanceIsNotLinked(t *testing.T) {
	target := Memory{ID: "mem_prior", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget", Text: "widgetrun prior claim", CreatedAt: "2026-06-01T00:00:00Z"}
	correction := Memory{ID: "mem_hidden", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Hidden correction", Text: "private hidden correction", CreatedAt: "2026-07-01T00:00:00Z", Meta: map[string]any{"target": target.ID, "disposition": "outdated"}}
	cfg := seedExplicitCorrectionFixture(t, target, correction)
	if _, err := appendGovernanceEntry(cfg, govEntry{Kind: govKindTeachMemory, Action: govActionRecord, TargetID: correction.ID, Decision: teachMemoryRetract}); err != nil {
		t.Fatal(err)
	}
	assertExplicitCorrectionAbsent(t, cfg, target)
}

func TestExplicitCorrectionPendingDeleteIsNotLinked(t *testing.T) {
	target := Memory{ID: "mem_prior", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget", Text: "widgetrun prior claim", CreatedAt: "2026-06-01T00:00:00Z"}
	correction := Memory{ID: "mem_pending", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Pending correction", Text: "private pending correction", CreatedAt: "2026-07-01T00:00:00Z", Meta: map[string]any{"target": target.ID, "disposition": "outdated"}}
	cfg := seedExplicitCorrectionFixture(t, target, correction)
	if _, err := markIndexDirty(testCtx(t), cfg, pendingOp{Kind: opKindDelete, Path: filepath.Join(memoriesRoot(cfg), target.Scope, correction.ID+".md"), MemoryID: correction.ID}); err != nil {
		t.Fatal(err)
	}
	assertExplicitCorrectionAbsent(t, cfg, target)
}

func TestExplicitCorrectionTombstoneIsNotLinked(t *testing.T) {
	target := Memory{ID: "mem_prior", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget", Text: "widgetrun prior claim", CreatedAt: "2026-06-01T00:00:00Z"}
	correction := Memory{ID: "mem_dead", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Deleted correction", Text: "private deleted correction", CreatedAt: "2026-07-01T00:00:00Z", Meta: map[string]any{"target": target.ID, "disposition": "outdated"}}
	cfg := seedExplicitCorrectionFixture(t, target, correction)
	correction.DeletedAt = "2026-08-01T00:00:00Z"
	if err := writeMemory(cfg, correction); err != nil {
		t.Fatal(err)
	}
	assertExplicitCorrectionAbsent(t, cfg, target)
}

func assertExplicitCorrectionAbsent(t *testing.T, cfg Config, target Memory) {
	t.Helper()
	rows, err := decorateExplicitCorrections(cfg, []Memory{target}, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), searchFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].ExplicitCorrections) != 0 || rows[0].Disposition != nil || rows[0].CorrectionOmitted {
		t.Fatalf("hidden correction was linked or left a misleading marker: %+v", rows)
	}
}

func TestExplicitCorrectionSurvivesOffPageSearchAndContextBudget(t *testing.T) {
	target := Memory{ID: "mem_widget_running", Scope: "project:widget", Type: "fact", Source: "mcp",
		Title: "Widget status", Text: "widgetrun the pilot is running " + strings.Repeat("old claim ", 120),
		CreatedAt: "2026-06-01T10:00:00Z"}
	correction := Memory{ID: "mem_widget_cancelled", Scope: target.Scope, Type: "correction", Source: "mcp",
		Title: "Partner update", Text: "The partner cancelled the pilot. Follow up only to acknowledge the cancellation.",
		CreatedAt: "2026-07-01T10:00:00Z", Meta: map[string]any{"target": target.ID, "disposition": "outdated"}}
	cfg := seedExplicitCorrectionFixture(t, target, correction)
	search, err := mcpSearchMemory(testCtx(t), cfg, map[string]any{"query": "widgetrun", "scope": target.Scope, "limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	rows := search.(map[string]any)["results"].([]Memory)
	if len(rows) != 1 || rows[0].ID != target.ID || len(rows[0].ExplicitCorrections) != 1 {
		t.Fatalf("search lost the off-page correction: %+v", rows)
	}
	linked := rows[0].ExplicitCorrections[0]
	if linked.ID != correction.ID || linked.Provenance != "authored" || !strings.Contains(linked.Text, "cancelled the pilot") {
		t.Fatalf("linked correction = %+v", linked)
	}
	if rows[0].Disposition == nil || rows[0].Disposition.CorrectionID != correction.ID {
		t.Fatalf("disposition was not projected from the same correction: %+v", rows[0].Disposition)
	}
	contextResult, err := mcpContextMemory(testCtx(t), cfg, map[string]any{"query": "widgetrun", "scope": target.Scope, "max_tokens": float64(1000)})
	if err != nil {
		t.Fatal(err)
	}
	text := contextResult.(map[string]any)["context"].(string)
	if !strings.Contains(text, correction.ID) || !strings.Contains(text, "cancelled the pilot") || !strings.Contains(text, "Writer's disposition") {
		t.Fatalf("context lost the qualified correction: %q", text)
	}
	if strings.Index(text, correction.ID) > strings.Index(text, "the pilot is running") {
		t.Fatalf("old claim preceded its correction: %q", text)
	}
	// Under a smaller budget, the old assertion may disappear; it must never
	// survive alone without the explicit correction or an omission warning.
	for _, tokens := range []float64{40, 60, 90, 120} {
		result, err := mcpContextMemory(testCtx(t), cfg, map[string]any{"query": "widgetrun", "scope": target.Scope, "max_tokens": tokens})
		if err != nil {
			t.Fatal(err)
		}
		body := result.(map[string]any)["context"].(string)
		if strings.Contains(body, "the pilot is running") && !strings.Contains(body, correction.ID) {
			t.Fatalf("budget %.0f exposed the old claim without its correction: %q", tokens, body)
		}
	}
}

func TestExplicitCorrectionFilterNeverLeaksLinkedContent(t *testing.T) {
	target := Memory{ID: "gmail_thread/widget", Scope: "project:widget", Type: "email", Provider: "gmail", ProviderID: "widget",
		Source: "gmail", Title: "Widget status", Text: "widgetrun the pilot is running", CreatedAt: "2026-06-01T10:00:00Z"}
	correction := Memory{ID: "mem_widget_cancelled", Scope: target.Scope, Type: "correction", Source: "mcp",
		Title: "Private correction", Text: "the pilot ended in a secret note", CreatedAt: "2026-07-01T10:00:00Z",
		Meta: map[string]any{"target": target.ID, "disposition": "outdated"}}
	cfg := seedExplicitCorrectionFixture(t, target, correction)
	result, err := mcpSearchMemory(testCtx(t), cfg, map[string]any{"query": "widgetrun", "source": "gmail", "limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	rows := result.(map[string]any)["results"].([]Memory)
	if len(rows) != 1 || len(rows[0].ExplicitCorrections) != 0 || rows[0].Disposition != nil || !rows[0].CorrectionOmitted {
		t.Fatalf("source filter leaked or hid the omission: %+v", rows)
	}
	contextResult, err := mcpContextMemory(testCtx(t), cfg, map[string]any{"query": "widgetrun", "source": "gmail", "max_tokens": float64(1000)})
	if err != nil {
		t.Fatal(err)
	}
	text := contextResult.(map[string]any)["context"].(string)
	if strings.Contains(text, correction.ID) || strings.Contains(text, correction.Text) || !strings.Contains(text, "omitted") {
		t.Fatalf("filtered correction was exposed or omission concealed: %q", text)
	}
}

func TestExplicitCorrectionUsesSameScopeAndSurvivesAClusterFold(t *testing.T) {
	target := Memory{ID: "mem_prior", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget", Text: "widgetrun prior claim",
		CreatedAt: "2026-06-01T10:00:00Z", ContentHash: "same-event"}
	correct := Memory{ID: "mem_correction", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Widget", Text: "corrected statement",
		CreatedAt: "2026-06-02T10:00:00Z", ContentHash: "same-event", Meta: map[string]any{"target": target.ID}}
	otherScope := Memory{ID: "mem_other_scope", Scope: "project:other", Type: "correction", Source: "mcp", Title: "Widget", Text: "unrelated secret",
		CreatedAt: "2026-06-03T10:00:00Z", Meta: map[string]any{"target": target.ID}}
	cfg := seedExplicitCorrectionFixture(t, target, correct, otherScope)
	// The cluster folds the correction into a reference, so the actual text
	// would be absent unless the explicit link is projected after clustering.
	folded := clusterAndTruncate([]string{target.ID, correct.ID}, []Memory{target, correct}, 1)
	if len(folded) != 1 || len(folded[0].Corroborating) != 1 {
		t.Fatalf("fixture did not fold: %+v", folded)
	}
	rows, err := decorateExplicitCorrections(cfg, folded, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), searchFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0].ExplicitCorrections) != 1 || rows[0].ExplicitCorrections[0].ID != correct.ID {
		t.Fatalf("cluster fold lost explicit correction or crossed scopes: %+v", rows[0].ExplicitCorrections)
	}
}

func TestExplicitCorrectionCapUsesActualInstants(t *testing.T) {
	target := Memory{ID: "mem_prior", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget", Text: "widgetrun prior claim", CreatedAt: "2026-06-01T00:00:00Z"}
	// Lexical RFC3339 order differs from time order across offsets. The cap
	// must retain the two latest instants, not the two largest strings.
	earlier := Memory{ID: "mem_earlier", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Earlier", Text: "earlier correction", CreatedAt: "2026-07-03T00:00:00+14:00", Meta: map[string]any{"target": target.ID}}
	later := Memory{ID: "mem_later", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Later", Text: "later correction", CreatedAt: "2026-07-02T22:00:00-07:00", Meta: map[string]any{"target": target.ID}}
	latest := Memory{ID: "mem_latest", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Latest", Text: "latest correction", CreatedAt: "2026-07-03T06:00:00Z", Meta: map[string]any{"target": target.ID}}
	cfg := seedExplicitCorrectionFixture(t, target, earlier, later, latest)
	rows, err := decorateExplicitCorrections(cfg, []Memory{target}, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), searchFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0].ExplicitCorrections) != 2 || rows[0].ExplicitCorrections[0].ID != latest.ID || rows[0].ExplicitCorrections[1].ID != later.ID || !rows[0].CorrectionOmitted {
		t.Fatalf("instant ordering or cap omitted newest correction: %+v", rows[0])
	}
}

func TestExplicitCorrectionCapKeepsVisibleDispositionEvidence(t *testing.T) {
	target := Memory{ID: "mem_prior", Scope: "project:widget", Type: "fact", Source: "mcp", Title: "Widget", Text: "widgetrun prior claim", CreatedAt: "2026-06-01T00:00:00Z"}
	dispositionNote := Memory{ID: "mem_disposition", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Verdict", Text: "The pilot was cancelled", CreatedAt: "2026-07-01T00:00:00Z", Meta: map[string]any{"target": target.ID, "disposition": "outdated"}}
	note1 := Memory{ID: "mem_note1", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Note one", Text: "First follow up", CreatedAt: "2026-07-02T00:00:00Z", Meta: map[string]any{"target": target.ID}}
	note2 := Memory{ID: "mem_note2", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Note two", Text: "Second follow up", CreatedAt: "2026-07-03T00:00:00Z", Meta: map[string]any{"target": target.ID}}
	cfg := seedExplicitCorrectionFixture(t, target, dispositionNote, note1, note2)
	rows, err := decorateExplicitCorrections(cfg, []Memory{target}, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), searchFilters{})
	if err != nil {
		t.Fatal(err)
	}
	got := rows[0]
	if got.Disposition == nil || got.Disposition.CorrectionID != dispositionNote.ID || len(got.ExplicitCorrections) != 2 || got.ExplicitCorrections[0].ID != note2.ID || got.ExplicitCorrections[1].ID != dispositionNote.ID || !got.CorrectionOmitted {
		t.Fatalf("cap hid the correction behind a visible disposition: %+v", got)
	}
}

func TestExplicitCorrectionCLISourceFilterParity(t *testing.T) {
	target := Memory{ID: "gmail_thread/widget", Scope: "project:widget", Type: "email", Provider: "gmail", ProviderID: "widget", Source: "gmail", Title: "Widget status", Text: "widgetrun prior claim", CreatedAt: "2026-06-01T10:00:00Z"}
	correction := Memory{ID: "mem_private_correction", Scope: target.Scope, Type: "correction", Source: "mcp", Title: "Private update", Text: "private correction text", CreatedAt: "2026-07-01T10:00:00Z", Meta: map[string]any{"target": target.ID, "disposition": "outdated"}}
	seedExplicitCorrectionFixture(t, target, correction)
	for _, raw := range []string{
		run(t, "search", "widgetrun", "--source", "gmail", "--json"),
		run(t, "list", "--source", "gmail", "--json"),
	} {
		var receipt struct {
			Memories []Memory `json:"memories"`
		}
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			t.Fatal(err)
		}
		for _, row := range receipt.Memories {
			if row.ID == correction.ID || len(row.ExplicitCorrections) != 0 || row.Disposition != nil {
				t.Fatalf("CLI source filter leaked local correction: %+v", row)
			}
		}
	}
	unfiltered := run(t, "search", "widgetrun", "--json")
	var receipt struct {
		Memories []Memory `json:"memories"`
	}
	if err := json.Unmarshal([]byte(unfiltered), &receipt); err != nil {
		t.Fatal(err)
	}
	for _, row := range receipt.Memories {
		if row.ID == target.ID && len(row.ExplicitCorrections) == 1 && row.ExplicitCorrections[0].ID == correction.ID && row.ExplicitCorrections[0].Text == correction.Text {
			return
		}
	}
	t.Fatalf("CLI search lost unfiltered correction: %+v", receipt.Memories)
}
