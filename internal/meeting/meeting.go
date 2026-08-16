package meeting

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"sort"
	"time"
)

const DefaultHorizonDays = 14
const DefaultGrace = 30 * time.Minute

type Event struct {
	StableID   string   `json:"stable_id"`
	Title      string   `json:"title"`
	OccurredAt string   `json:"occurred_at"`
	Source     string   `json:"source"`
	AllDay     bool     `json:"all_day"`
	Attendees  []string `json:"attendees"`
}

func EventStart(m memory.Memory) (time.Time, bool) {
	candidates := []string{}
	if m.Meta != nil {
		if s, _ := m.Meta["occurred_at"].(string); s != "" {
			candidates = append(candidates, s)
		}
	}
	candidates = append(candidates, m.CreatedAt)
	for _, s := range candidates {
		if s == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}
func dayStartUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

type AttendeeMatcher func(memory.Memory, map[string]bool) bool

func SelectNextEvent(mems []memory.Memory, now time.Time, attendeeIDs map[string]bool, match AttendeeMatcher, grace time.Duration, horizonDays int) *Event {
	type cand struct {
		m               memory.Memory
		start           time.Time
		allDay, current bool
	}
	today := dayStartUTC(now)
	horizonDay := today.AddDate(0, 0, horizonDays)
	graceFloor := now.Add(-grace)
	var cands []cand
	for _, m := range mems {
		if m.DeletedAt != "" || m.Type != "event" {
			continue
		}
		start, ok := EventStart(m)
		if !ok {
			continue
		}
		allDay := start.Equal(dayStartUTC(start))
		var inWindow, current bool
		if allDay {
			day := dayStartUTC(start)
			inWindow = !day.Before(today) && !day.After(horizonDay)
			current = day.Equal(today)
		} else {
			inWindow = !start.Before(graceFloor) && !start.After(now.AddDate(0, 0, horizonDays))
			current = !start.After(now)
		}
		if !inWindow {
			continue
		}
		if len(attendeeIDs) > 0 && (match == nil || !match(m, attendeeIDs)) {
			continue
		}
		cands = append(cands, cand{m: m, start: start, allDay: allDay, current: current})
	}
	if len(cands) == 0 {
		return nil
	}
	var current, future []cand
	for _, c := range cands {
		if c.current {
			current = append(current, c)
		} else {
			future = append(future, c)
		}
	}
	var pick cand
	if len(current) > 0 {
		sort.Slice(current, func(i, j int) bool {
			if !current[i].start.Equal(current[j].start) {
				return current[i].start.After(current[j].start)
			}
			return current[i].m.ID < current[j].m.ID
		})
		pick = current[0]
	} else {
		sort.Slice(future, func(i, j int) bool {
			if !future[i].start.Equal(future[j].start) {
				return future[i].start.Before(future[j].start)
			}
			return future[i].m.ID < future[j].m.ID
		})
		pick = future[0]
	}
	return &Event{StableID: pick.m.ID, Title: pick.m.Title, OccurredAt: pick.start.UTC().Format(time.RFC3339), Source: pick.m.Provider, AllDay: pick.allDay, Attendees: metaStrings(pick.m.Meta["attendees"])}
}
func metaStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, s := range t {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t != "" {
			return []string{t}
		}
	}
	return nil
}
