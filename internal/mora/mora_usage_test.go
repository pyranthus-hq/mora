package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageLoggedToStateNotVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	logUsage(mustConfig(t), usageEvent{Tool: "search_memory", Query: "secret query", Scope: "global", Results: 0})

	cfg := mustConfig(t)
	// raw log is in state dir, not vault
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		t.Fatalf("usage log missing: %v", err)
	}
	if !strings.Contains(string(raw), "search_memory") {
		t.Fatal("expected tool name in usage log")
	}
	// must not leak into the synced vault
	if _, err := os.Stat(filepath.Join(cfg.VaultDir, "usage")); err == nil {
		t.Fatal("usage log must NOT be inside the vault")
	}
}

func TestUsageDisabledByDoNotTrack(t *testing.T) {
	withTempHome(t)
	t.Setenv("DO_NOT_TRACK", "1")
	run(t, "init")
	logUsage(mustConfig(t), usageEvent{Tool: "search_memory"})
	if _, err := os.Stat(filepath.Join(mustConfig(t).StateDir, "usage", "events.jsonl")); err == nil {
		t.Fatal("DO_NOT_TRACK=1 must disable usage logging")
	}
}

func TestUsageDisabledByOffMarker(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg := mustConfig(t)
	usageDir := filepath.Join(cfg.StateDir, "usage")
	if err := os.MkdirAll(usageDir, 0o755); err != nil {
		t.Fatalf("create usage dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(usageDir, "OFF"), []byte(""), 0o644); err != nil {
		t.Fatalf("write OFF marker: %v", err)
	}

	logUsage(cfg, usageEvent{Tool: "search_memory"})
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "events.jsonl")); err == nil {
		t.Fatal("usage/OFF marker must disable usage logging")
	}
}

func mustConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestUsageReportIsContentFree(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "private secret", Results: 0, Millis: 5})
	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "other", Results: 3, Millis: 7})

	out := run(t, "usage", "report")
	if strings.Contains(out, "private secret") || strings.Contains(out, "other") {
		t.Fatalf("usage report must be content-free, leaked query: %s", out)
	}
	if !strings.Contains(out, "search_memory") || !strings.Contains(out, "empty") {
		t.Fatalf("report should aggregate tool counts + empty-rate: %s", out)
	}
}
