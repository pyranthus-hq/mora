package mora

import (
	"testing"
	"time"
)

func TestColdStartWindowAppleCalendarLooksForward(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	mems := []Memory{
		{ID: "future", Title: "Dentist", CreatedAt: now.Add(48 * time.Hour).Format(time.RFC3339), Provider: "applecal"},
		{ID: "past", Title: "Two days ago", CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339), Provider: "applecal"},
	}
	items, _, _, _, _ := deltaSectionItems(Config{}, briefDelta{ColdStart: true}, mems, now, "applecalendar", 8, nil)
	if len(items) != 1 || items[0].ID != "future" {
		t.Fatalf("cold-start applecalendar section = %+v; want exactly the upcoming event \"future\" (past events belong to the calendar's history, not its cold-start brief)", items)
	}
}
