package mora

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const (
	calendarEventsDefaultLimit = 50
	calendarEventsMaxLimit     = 200
	calendarEventsBudgetBytes  = searchMemoryResultsBudgetBytes
)

// mcpCalendarEvents enumerates calendar events by their structured occurrence
// start. It deliberately reads the vault, rather than the search index: event
// start has no index column and this exact range surface must remain read-only.
func mcpCalendarEvents(_ context.Context, cfg Config, args map[string]any) (any, error) {
	zone := strArg(args, "timezone", "")
	loc := time.Local
	if zone != "" {
		var err error
		loc, err = time.LoadLocation(zone)
		if err != nil {
			return nil, fmt.Errorf("calendar_events: unknown timezone %q", zone)
		}
	}
	start, err := parseCalendarBoundary(args, "start", loc)
	if err != nil {
		return nil, err
	}
	end, err := parseCalendarBoundary(args, "end", loc)
	if err != nil {
		return nil, err
	}
	if !end.After(start) {
		return nil, fmt.Errorf("calendar_events: end must be after start")
	}
	limit := intArg(args, "limit", calendarEventsDefaultLimit)
	if limit <= 0 || limit > calendarEventsMaxLimit {
		return nil, fmt.Errorf("calendar_events: limit must be between 1 and %d", calendarEventsMaxLimit)
	}
	source := strArg(args, "source", "")
	if source != "" && source != "calendar" && source != "applecalendar" {
		return nil, fmt.Errorf("calendar_events: source must be calendar or applecalendar")
	}
	events, err := calendarEventsInRange(cfg, start, end, source)
	if err != nil {
		return nil, err
	}
	if len(events) > limit {
		events = events[:limit]
	}
	budgeted, dropped := budgetSearchResults(snippetMemories(events, ""), calendarEventsBudgetBytes)
	out := map[string]any{"events": budgeted, "health": compactHealthOf(cfg, briefClock())}
	if dropped > 0 {
		out["events_truncated"] = dropped
	}
	return out, nil
}

func parseCalendarBoundary(args map[string]any, key string, loc *time.Location) (time.Time, error) {
	raw, ok := args[key].(string)
	if !ok || raw == "" {
		return time.Time{}, fmt.Errorf("calendar_events: %s is required", key)
	}
	if len(raw) == len("2006-01-02") {
		day, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("calendar_events: invalid %s %q", key, raw)
		}
		return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc), nil
	}
	instant, ok := rfc3339Instant(raw)
	if !ok {
		return time.Time{}, fmt.Errorf("calendar_events: %s must be YYYY-MM-DD or RFC3339", key)
	}
	return instant, nil
}

func calendarEventsInRange(cfg Config, start, end time.Time, source string) ([]Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return nil, err
	}
	type row struct {
		memory Memory
		start  time.Time
	}
	var rows []row
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil || m.DeletedAt != "" || !g.memoryVisible(m.ID) {
			continue
		}
		if source == "calendar" && m.Provider != "calendar" {
			continue
		}
		if source == "applecalendar" && m.Provider != "applecal" {
			continue
		}
		stamp, ok := eventStartOf(m)
		if !ok {
			continue
		}
		at, ok := rfc3339Instant(stamp)
		if !ok || at.Before(start) || !at.Before(end) {
			continue
		}
		rows = append(rows, row{m, at})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].start.Equal(rows[j].start) {
			return rows[i].start.Before(rows[j].start)
		}
		return rows[i].memory.ID < rows[j].memory.ID
	})
	out := make([]Memory, len(rows))
	for i := range rows {
		out[i] = rows[i].memory
	}
	return out, nil
}
