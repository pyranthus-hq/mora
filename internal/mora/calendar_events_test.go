package mora

import (
	"context"
	"testing"
	"time"
)

func TestParseCalendarBoundary(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, value string
		want        time.Time
		wantErr     bool
	}{
		{"date only DST start", "2026-03-08", time.Date(2026, 3, 8, 0, 0, 0, 0, loc), false},
		{"date only DST end", "2026-11-01", time.Date(2026, 11, 1, 0, 0, 0, 0, loc), false},
		{"rfc3339", "2026-03-08T12:00:00Z", time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC), false},
		{"invalid date", "2026-02-30", time.Time{}, true},
		{"invalid timestamp", "2026-03-08 12:00", time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCalendarBoundary(map[string]any{"start": tc.value}, "start", loc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if !tc.wantErr && !got.Equal(tc.want) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCalendarEventsInRange(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), VaultDir: t.TempDir()}
	write := func(id, provider, when string) {
		t.Helper()
		if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "event", Title: id, Text: "event body", Source: provider, Provider: provider, CreatedAt: when, Meta: map[string]any{"occurred_at": when}}); err != nil {
			t.Fatal(err)
		}
	}
	write("end", "calendar", "2026-03-10T00:00:00Z")
	write("included-later", "applecal", "2026-03-09T12:00:00Z")
	write("included-earlier", "calendar", "2026-03-09T08:00:00Z")
	write("before", "calendar", "2026-03-08T23:59:59Z")
	got, err := calendarEventsInRange(cfg, time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "included-earlier" || got[1].ID != "included-later" {
		t.Fatalf("events = %#v", got)
	}
	got, err = calendarEventsInRange(cfg, time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), "applecalendar")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "included-later" {
		t.Fatalf("apple events = %#v", got)
	}
}

func TestMCPCalendarEventsValidation(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), VaultDir: t.TempDir()}
	cases := []map[string]any{
		{}, {"start": "2026-03-09", "end": "2026-03-09"},
		{"start": "2026-03-09", "end": "2026-03-10", "timezone": "No/Such_Zone"},
		{"start": "2026-03-09", "end": "2026-03-10", "source": "gmail"},
		{"start": "2026-03-09", "end": "2026-03-10", "limit": float64(201)},
	}
	for _, args := range cases {
		if _, err := mcpCalendarEvents(t.Context(), cfg, args); err == nil {
			t.Fatalf("args %#v succeeded", args)
		}
	}
}

func TestCalendarEventsMCPToolSchema(t *testing.T) {
	resp := handleMCP(context.Background(), jsonRPCRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/list"})
	tools := resp.Result.(map[string]any)["tools"].([]map[string]any)
	for _, tool := range tools {
		if tool["name"] != "calendar_events" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		for _, key := range []string{"start", "end", "timezone", "source", "limit"} {
			if _, ok := props[key]; !ok {
				t.Fatalf("calendar_events schema missing %q: %#v", key, props)
			}
		}
		required := schema["required"].([]string)
		if len(required) != 2 || required[0] != "start" || required[1] != "end" {
			t.Fatalf("required = %#v", required)
		}
		return
	}
	t.Fatal("calendar_events absent from tools/list")
}
