package meeting

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"reflect"
	"strings"
	"testing"
	"time"
)

type Memory = memory.Memory

func eventStart(m Memory) (time.Time, bool) { return EventStart(m) }
func selectNextEvent(mems []Memory, now time.Time, ids map[string]bool) *Event {
	return SelectNextEvent(mems, now, ids, func(m Memory, ids map[string]bool) bool {
		for _, a := range metaStrings(m.Meta["attendees"]) {
			if ids["person:"+strings.ToLower(a)] {
				return true
			}
		}
		return false
	}, DefaultGrace, DefaultHorizonDays)
}

// mpNow is the fixed "now" for the deterministic selection tests.
var mpNow = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

func eventMem(id, title, occurredAt string, attendees ...string) Memory {
	meta := map[string]any{"occurred_at": occurredAt}
	if len(attendees) > 0 {
		anyAtt := make([]any, len(attendees))
		for i, a := range attendees {
			anyAtt[i] = a
		}
		meta["attendees"] = anyAtt
	}
	return Memory{
		ID: id, Type: "event", Title: title, Provider: "google",
		ProviderID: "calendar_event/" + id, Source: "calendar_event/" + id,
		CreatedAt: occurredAt, Meta: meta,
	}
}

func TestSelectNextEventPicksEarliestUpcomingWithinHorizon(t *testing.T) {
	mems := []Memory{
		eventMem("past", "Past", "2026-06-14T10:00:00Z"),   // 2h ago, beyond grace → excluded
		eventMem("soon", "Soon", "2026-06-14T14:00:00Z"),   // +2h
		eventMem("later", "Later", "2026-06-16T10:00:00Z"), // +2d
		eventMem("far", "Far", "2026-07-20T10:00:00Z"),     // >14d → horizon excludes
	}
	ev := selectNextEvent(mems, mpNow, nil)
	if ev == nil || ev.StableID != "soon" {
		t.Fatalf("got %+v, want earliest upcoming 'soon'", ev)
	}
}

func TestSelectNextEventInProgressWithinGracePrefersCurrent(t *testing.T) {
	mems := []Memory{
		eventMem("inprog", "In progress", "2026-06-14T11:45:00Z"), // 15m ago, within 30m grace
		eventMem("soon", "Soon", "2026-06-14T14:00:00Z"),
	}
	ev := selectNextEvent(mems, mpNow, nil)
	if ev == nil || ev.StableID != "inprog" {
		t.Fatalf("got %+v, want the in-progress meeting", ev)
	}
}

func TestSelectNextEventStartedBeyondGraceTreatedAsPast(t *testing.T) {
	mems := []Memory{
		eventMem("stale", "Stale", "2026-06-14T11:15:00Z"), // 45m ago, beyond 30m grace
		eventMem("soon", "Soon", "2026-06-14T14:00:00Z"),
	}
	ev := selectNextEvent(mems, mpNow, nil)
	if ev == nil || ev.StableID != "soon" {
		t.Fatalf("got %+v, want 'soon' ('stale' is beyond grace)", ev)
	}
}

func TestSelectNextEventBoundaryAtGraceEdge(t *testing.T) {
	// Exactly now-30m counts as current; now-30m-1s does not.
	if ev := selectNextEvent([]Memory{eventMem("edge", "Edge", "2026-06-14T11:30:00Z")}, mpNow, nil); ev == nil || ev.StableID != "edge" {
		t.Fatalf("now-30m should count as current: %+v", ev)
	}
	if ev := selectNextEvent([]Memory{eventMem("over", "Over", "2026-06-14T11:29:59Z")}, mpNow, nil); ev != nil {
		t.Fatalf("now-30m-1s should be past (no other event): got %+v", ev)
	}
}

func TestSelectNextEventTwoInProgressPicksLatestStart(t *testing.T) {
	mems := []Memory{
		eventMem("early", "Early", "2026-06-14T11:40:00Z"),
		eventMem("late", "Late", "2026-06-14T11:50:00Z"), // closest to now
	}
	ev := selectNextEvent(mems, mpNow, nil)
	if ev == nil || ev.StableID != "late" {
		t.Fatalf("got %+v, want the latest-starting in-progress event", ev)
	}
}

func TestSelectNextEventAllDayTodaySelectedYesterdayExcluded(t *testing.T) {
	mems := []Memory{
		eventMem("yday", "Yesterday all-day", "2026-06-13T00:00:00Z"),
		eventMem("today", "Today all-day", "2026-06-14T00:00:00Z"),
	}
	ev := selectNextEvent(mems, mpNow, nil)
	if ev == nil || ev.StableID != "today" {
		t.Fatalf("got %+v, want today's all-day event (calendar-day compare)", ev)
	}
	if !ev.AllDay {
		t.Errorf("today's midnight-UTC event should be flagged AllDay")
	}
}

func TestSelectNextEventAttendeeFilter(t *testing.T) {
	mems := []Memory{
		eventMem("with-riya", "Riya sync", "2026-06-14T14:00:00Z", "riya@a.com"),
		eventMem("with-bob", "Bob sync", "2026-06-14T13:00:00Z", "bob@z.com"), // earlier, but no riya
	}
	ev := selectNextEvent(mems, mpNow, map[string]bool{"person:riya@a.com": true})
	if ev == nil || ev.StableID != "with-riya" {
		t.Fatalf("got %+v, want the event WITH Riya (despite Bob's being earlier)", ev)
	}
	if ev := selectNextEvent(mems, mpNow, map[string]bool{"person:nobody@x.com": true}); ev != nil {
		t.Fatalf("no attendee match should select nil, got %+v", ev)
	}
}

func TestSelectNextEventStableTieBreak(t *testing.T) {
	mems := []Memory{
		eventMem("bbb", "B", "2026-06-14T14:00:00Z"),
		eventMem("aaa", "A", "2026-06-14T14:00:00Z"),
	}
	ev1 := selectNextEvent(mems, mpNow, nil)
	ev2 := selectNextEvent([]Memory{mems[1], mems[0]}, mpNow, nil) // reversed input
	if ev1 == nil || ev1.StableID != "aaa" || ev2 == nil || ev2.StableID != "aaa" {
		t.Fatalf("tie-break must be deterministic lowest StableID: %+v / %+v", ev1, ev2)
	}
}

func TestEventStartNoParseableCandidate(t *testing.T) {
	if ts, ok := eventStart(Memory{}); ok {
		t.Fatalf("eventStart(empty) ok=true (%v), want false", ts)
	}
	if _, ok := eventStart(Memory{Meta: map[string]any{"occurred_at": "nope"}}); ok {
		t.Fatal("eventStart with an unparseable occurred_at and no CreatedAt should be false")
	}
}

func TestSelectNextEventSkipsUnparseable(t *testing.T) {
	mems := []Memory{
		{ID: "bad", Type: "event", Meta: map[string]any{"occurred_at": "not-a-time"}, CreatedAt: "also-bad"},
		eventMem("good", "Good", "2026-06-14T14:00:00Z"),
	}
	ev := selectNextEvent(mems, mpNow, nil)
	if ev == nil || ev.StableID != "good" {
		t.Fatalf("got %+v, want 'good' (unparseable 'bad' skipped)", ev)
	}
}

func TestSelectNextEventCurrentTieBreak(t *testing.T) {
	a := eventMem("aaa", "A", "2026-06-14T11:45:00Z")
	b := eventMem("bbb", "B", "2026-06-14T11:45:00Z")
	ev1 := selectNextEvent([]Memory{a, b}, mpNow, nil)
	ev2 := selectNextEvent([]Memory{b, a}, mpNow, nil)
	if ev1 == nil || ev1.StableID != "aaa" || ev2 == nil || ev2.StableID != "aaa" {
		t.Fatalf("current-event tie-break must be lowest StableID 'aaa': %+v / %+v", ev1, ev2)
	}
}

func TestMetaStringsShapes(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{{[]string{"", "a"}, []string{"a"}}, {[]any{"b", "", 1}, []string{"b"}}, {"c", []string{"c"}}, {"", nil}, {1, nil}}
	for _, tc := range cases {
		if got := metaStrings(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("metaStrings(%v)=%v want %v", tc.in, got, tc.want)
		}
	}
}
