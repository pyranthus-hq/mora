package mora

import (
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"testing"
	"time"
)

func TestDispositionOverlayUsesWholeVisibleVaultAndSkipsShared(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir(), StateDir: t.TempDir()}
	target := Memory{ID: "target", Scope: "global", Type: "insight", Source: "manual", Title: "target", Text: "needle", CreatedAt: "2026-09-01T00:00:00Z"}
	correction := Memory{ID: "correction", Scope: "global", Type: "correction", Source: "manual", Title: "set aside", Text: "x", CreatedAt: "2026-09-02T00:00:00Z", Meta: map[string]any{"target": "target", "disposition": "not-context"}}
	if err := writeMemory(cfg, target); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, correction); err != nil {
		t.Fatal(err)
	}
	rows, err := decorateDispositions(cfg, []Memory{target, {ID: "target", Scope: "global", Owner: "share"}}, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Disposition == nil || rows[0].Disposition.Value != "not-context" {
		t.Fatalf("local=%+v", rows[0])
	}
	if rows[1].Disposition != nil {
		t.Fatalf("shared decorated=%+v", rows[1])
	}
}
func TestDispositionReadTargetReceiptFullAndBounded(t *testing.T) {
	m := Memory{Type: "correction", Meta: map[string]any{"target": "target"}, Text: "body"}
	full := mcpReadMemoryResult(Config{}, m, map[string]any{})
	if full["receipt"].(map[string]any)["target"] != "target" {
		t.Fatal(full)
	}
	bounded := mcpReadMemoryResult(Config{}, m, map[string]any{"max_tokens": 1})
	if bounded["receipt"].(mcppkg.BoundedReadReceipt).Target != "target" {
		t.Fatal(bounded)
	}
}

func TestDispositionPublishRequiresVisibleLocalSameScopeTarget(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir(), StateDir: t.TempDir()}
	target := Memory{ID: "target", Scope: "global", Source: "manual", Title: "target", Text: "x", CreatedAt: "2026-09-01T00:00:00Z"}
	if err := writeMemory(cfg, target); err != nil {
		t.Fatal(err)
	}
	correction := Memory{Scope: "global", Type: "correction", Source: "gmail", Meta: map[string]any{"target": "target", "disposition": "keep"}}
	if err := validateDispositionPublish(cfg, correction); err == nil {
		t.Fatal("connector correction accepted")
	}
	correction.Source = "manual"
	correction.Scope = "other"
	if err := validateDispositionPublish(cfg, correction); err == nil {
		t.Fatal("cross scope accepted")
	}
	correction.Scope = "global"
	if err := validateDispositionPublish(cfg, correction); err != nil {
		t.Fatal(err)
	}
}
