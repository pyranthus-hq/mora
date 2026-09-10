package mora

import (
	"encoding/json"
	"testing"
	"time"
)

func TestActivityListWindowAndExplicitUnknown(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	fixture := func(id, provider, at, synced string) Memory {
		return Memory{ID: id, Scope: "global", Type: "event", Title: id, Text: "fixture", Provider: provider, Source: provider, CreatedAt: at, LastSynced: synced, Meta: map[string]any{"occurred_at": at}}
	}
	cfg := seedRecencyVault(t,
		fixture("old-reimport", "calendar", "2025-09-09T11:00:00Z", "2026-09-10T12:00:00Z"),
		fixture("new-event", "calendar", "2026-09-10T11:00:00Z", "2026-09-01T12:00:00Z"),
		fixture("future", "calendar", "2026-09-11T11:00:00Z", "2026-09-10T12:00:00Z"),
		fixture("other-source", "gmail", "2026-09-10T12:00:00Z", "2026-09-10T12:00:00Z"))
	f, err := parseSearchFilters(map[string]any{"source": "calendar"}, now)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := listActivityMemories(cfg, "", 1, f, 24, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "new-event" || rows[0].EventAt != "2026-09-10T11:00:00Z" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	raw, _ := json.Marshal(rows[0])
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if v, ok := decoded["automated"]; !ok || v != nil {
		t.Fatalf("missing explicit unknown: %s", raw)
	}
	if _, ok := decoded["participation"]; ok {
		t.Fatalf("invented calendar participation: %s", raw)
	}
	legacy, err := listActivityMemories(cfg, "", 1, f, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if legacy[0].ID == "new-event" {
		t.Fatal("default write-time order changed")
	}
	raw, _ = json.Marshal(legacy[0])
	_ = json.Unmarshal(raw, &decoded)
	var plain map[string]any
	_ = json.Unmarshal(raw, &plain)
	if _, ok := plain["automated"]; ok {
		t.Fatalf("default output gained annotation: %s", raw)
	}
}

func TestActivityMCPListCanonicalAliasAndReceipts(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cfg := seedRecencyVault(t, Memory{ID: "event", Scope: "global", Type: "event", Title: "event", Text: "fixture", Provider: "calendar", Source: "calendar", CreatedAt: "2026-09-10T11:00:00Z", Meta: map[string]any{"occurred_at": "2026-09-10T11:00:00Z"}})
	old := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = old })
	for _, key := range []string{"event_since_hours", "since_hours"} {
		result, err := mcpListMemory(testCtx(t), cfg, map[string]any{"source": "calendar", key: 24, "limit": 1})
		if err != nil {
			t.Fatal(err)
		}
		out := result.(map[string]any)
		if out["order"] != "source-event" || out["source"] != "calendar" || out["event_since_hours"] != 24 {
			t.Fatalf("bad receipt: %+v", out)
		}
		rows := out["memories"].([]Memory)
		if len(rows) != 1 || rows[0].Automated == nil || rows[0].EventAt == "" {
			t.Fatalf("annotation lost through snippet: %+v", rows)
		}
	}
	sourceOnly, err := mcpListMemory(testCtx(t), cfg, map[string]any{"source": "calendar"})
	if err != nil {
		t.Fatal(err)
	}
	out := sourceOnly.(map[string]any)
	if out["source"] != "calendar" {
		t.Fatal("source filter not echoed")
	}
	if _, ok := out["order"]; ok {
		t.Fatal("default order mislabeled")
	}
}

func TestActivityMCPWindowRejectsInvalidAndConflictingInputs(t *testing.T) {
	now := time.Now()
	for _, value := range []any{0, -1, 8785, 1.5, "24", true, nil, json.Number("999999999999999999999999")} {
		if _, err := parseActivityHours(map[string]any{"event_since_hours": value}, true, now); err == nil {
			t.Fatalf("accepted invalid event window %#v", value)
		}
	}
	if _, err := parseActivityHours(map[string]any{"event_since_hours": 24, "since_hours": 24}, true, now); err == nil {
		t.Fatal("accepted two aliases")
	}
	if hours, err := parseActivityHours(map[string]any{}, true, now); err != nil || hours != 0 {
		t.Fatal(hours, err)
	}
	if hours, err := parseActivityHours(map[string]any{"event_since_hours": json.Number("8784")}, true, now); err != nil || hours != 8784 {
		t.Fatal(hours, err)
	}
	cfg := seedRecencyVault(t)
	for _, limit := range []any{0, -1, 1001, 1.5, "2", nil} {
		if _, err := mcpListMemory(testCtx(t), cfg, map[string]any{"event_since_hours": 24, "limit": limit}); err == nil {
			t.Fatalf("accepted invalid limit %#v", limit)
		}
	}
}
