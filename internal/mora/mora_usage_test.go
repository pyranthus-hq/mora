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

func TestUsageStripsQueryByDefault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// Default: the raw query text must NOT be retained in the local log.
	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "secret client merger", Scope: "personal", Results: 2})
	if raw := readUsageLog(t, cfg); strings.Contains(raw, "secret client merger") {
		t.Fatalf("query text must be stripped by default; leaked: %s", raw)
	}
	// ...but the useful metadata is still recorded.
	if raw := readUsageLog(t, cfg); !strings.Contains(raw, "search_memory") || !strings.Contains(raw, "personal") {
		t.Fatalf("tool + scope should still be logged; got: %s", raw)
	}

	// Opt in with `mora usage queries on`, and the query is then recorded.
	run(t, "usage", "queries", "on")
	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "opted in now", Results: 1})
	if raw := readUsageLog(t, cfg); !strings.Contains(raw, "opted in now") {
		t.Fatalf("after `usage queries on` the query should be recorded; got: %s", raw)
	}

	// Opt back out; new events are stripped again.
	run(t, "usage", "queries", "off")
	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "stripped again", Results: 1})
	if raw := readUsageLog(t, cfg); strings.Contains(raw, "stripped again") {
		t.Fatalf("after `usage queries off` the query should be stripped again; got: %s", raw)
	}
}

func readUsageLog(t *testing.T, cfg Config) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		t.Fatalf("usage log missing: %v", err)
	}
	return string(b)
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
