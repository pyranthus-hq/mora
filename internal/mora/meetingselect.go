package mora

import (
	"sort"
	"time"
)

const (
	meetingPrepEvidenceCap = 24
	meetingPrepHorizonDays = 14
)

// prepClock is the meeting-brief wall clock used by the MCP next-event path.
// It is a var so tests can pin it; production never reassigns it.
var prepClock = time.Now

// MeetingGaps is the deterministic "what the vault does NOT know" analysis.
type MeetingGaps struct {
	UnknownAttendees []string `json:"unknown_attendees,omitempty"`
	ThinAttendees    []string `json:"thin_attendees,omitempty"`
	NoEvidence       []string `json:"no_evidence,omitempty"`
	NoAttendees      bool     `json:"no_attendees,omitempty"`
	SelfUnknown      bool     `json:"self_unknown,omitempty"`
}

// meetingPrepGrace is how long after an event's start it still counts as "current"
// (the meeting you just walked into). A var so tests can pin it; without a persisted
// end time, true end>now in-progress detection is deferred to a connector change
// (P1-F). 30 minutes covers the common "running a few minutes late" case.
var meetingPrepGrace = 30 * time.Minute

// MeetingEvent is the selected calendar event a meeting brief is built around.
type MeetingEvent struct {
	StableID   string   `json:"stable_id"`
	Title      string   `json:"title"`
	OccurredAt string   `json:"occurred_at"`
	Source     string   `json:"source"`
	AllDay     bool     `json:"all_day"`
	Attendees  []string `json:"attendees"`
}

// eventStart parses an event memory's start instant: Meta["occurred_at"] (RFC3339,
// written by both the google and applecal connectors), falling back to CreatedAt.
// ok is false if nothing parses.
func eventStart(m Memory) (time.Time, bool) {
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

// dayStartUTC truncates an instant to the start of its UTC calendar day.
func dayStartUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// selectNextEvent picks the meeting to brief: the in-progress event (started
// within the grace window) if any, else the earliest upcoming one, bounded to a
// 14-day horizon, optionally restricted to events whose attendees intersect
// attendeeFilterIDs (the resolved alias-id SET — P1-A). Pure over parsed memories +
// injected now; deterministic (StableID tie-break). Returns nil if none qualifies.
//
// All-day events are detected uniformly as a midnight-UTC start (both connectors —
// P1-B) and compared at calendar-day granularity to dodge the midnight boundary
// flake. Timed events use the grace-extended lower bound start >= now-grace (P1-F);
// true end>now detection awaits a persisted end time (deferred connector change).
func selectNextEvent(mems []Memory, now time.Time, attendeeFilterIDs map[string]bool) *MeetingEvent {
	type cand struct {
		m       Memory
		start   time.Time
		allDay  bool
		current bool // started/today (vs strictly future)
	}
	today := dayStartUTC(now)
	horizonDay := today.AddDate(0, 0, meetingPrepHorizonDays)
	graceFloor := now.Add(-meetingPrepGrace)

	var cands []cand
	for _, m := range mems {
		if m.DeletedAt != "" || m.Type != "event" {
			continue
		}
		start, ok := eventStart(m)
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
			inWindow = !start.Before(graceFloor) && !start.After(now.AddDate(0, 0, meetingPrepHorizonDays))
			current = !start.After(now)
		}
		if !inWindow {
			continue
		}
		if len(attendeeFilterIDs) > 0 && !memoryMentionsEntity(m, attendeeFilterIDs) {
			continue
		}
		cands = append(cands, cand{m: m, start: start, allDay: allDay, current: current})
	}
	if len(cands) == 0 {
		return nil
	}

	// Current-first: a started-within-grace event beats a future one. Among current
	// events pick the LATEST start (closest to now — the one you're in); among future
	// events pick the EARLIEST start. StableID (memory id) breaks ties deterministically.
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
				return current[i].start.After(current[j].start) // latest first
			}
			return current[i].m.ID < current[j].m.ID
		})
		pick = current[0]
	} else {
		sort.Slice(future, func(i, j int) bool {
			if !future[i].start.Equal(future[j].start) {
				return future[i].start.Before(future[j].start) // earliest first
			}
			return future[i].m.ID < future[j].m.ID
		})
		pick = future[0]
	}

	return &MeetingEvent{
		StableID:   pick.m.ID,
		Title:      pick.m.Title,
		OccurredAt: pick.start.UTC().Format(time.RFC3339),
		Source:     pick.m.Provider,
		AllDay:     pick.allDay,
		Attendees:  metaStrings(pick.m.Meta["attendees"]),
	}
}
