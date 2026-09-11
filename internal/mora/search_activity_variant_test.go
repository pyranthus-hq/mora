package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestActivitySearchCombinedVariantContract(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	old := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = old })
	for _, m := range []Memory{
		{ID: "excluded", Scope: "global", Type: "event", Provider: "calendar", Source: "calendar", Title: "excluded", Text: "windowprobe windowprobe windowprobe", CreatedAt: "2026-08-01T00:00:00Z", Meta: map[string]any{"occurred_at": "2026-09-10T11:00:00Z"}},
		{ID: "kept", Scope: "global", Type: "event", Provider: "calendar", Source: "calendar", Title: "kept", Text: "a passing windowprobe with unrelated words", CreatedAt: "2026-08-01T00:00:00Z", Meta: map[string]any{"occurred_at": "2026-09-10T10:00:00Z"}},
		{ID: "correction", Scope: "global", Type: "correction", Source: "manual", Title: "correction", Text: "set aside", CreatedAt: "2026-09-02T00:00:00Z", Meta: map[string]any{"target": "excluded", "disposition": "not-context"}},
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	mustRebuild(t, cfg)
	raw := run(t, "search", "windowprobe", "--source", "calendar", "--event-since-hours", "24", "--dispositions", "exclude:not-context", "--limit", "1", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	rows := got["memories"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "kept" || got["excluded_by_disposition"] != float64(1) {
		t.Fatalf("combined filter/refill/count failed: %s", raw)
	}
	for _, row := range rows {
		row.(map[string]any)["path"] = "<path>"
	}
	path := filepath.Join("testdata", "contracts", "variants", "mora.search.event-exclusions.json")
	if os.Getenv("MORA_UPDATE_ACTIVITY_GOLDENS") == "1" {
		data, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("combined search contract drift; Golden change reason required: %s", raw)
	}
}
