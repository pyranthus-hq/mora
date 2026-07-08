package mora

import (
	"testing"
	"time"
)

// Issue #62 defect 3 — calendar noise. Two connector-level noise sources flood the
// top of the budget and starve the Emails section: subscription/feed events (no human
// organizer) and stale events resurfacing only because a re-sync bumped their hash.

func lowSigEventMem(title, organizer string, occurred time.Time) Memory {
	meta := map[string]any{"occurred_at": occurred.UTC().Format(time.RFC3339)}
	if organizer != "" {
		meta["organizer"] = organizer
	}
	return Memory{ID: "id-" + title, Type: "event", Title: title, Text: title, Meta: meta}
}

func TestLowSignalCalendarSubscription(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	// A subscription/feed whose organizer is a bulk-send address collapses via the
	// existing service-only rule (no new heuristic needed).
	feed := lowSigEventMem("Lakers vs Celtics", "noreply@nba.com", now.Add(24*time.Hour))
	if !isLowSignalItem(feed, "new", now) {
		t.Fatalf("a service-organizer feed event must be low-signal")
	}
	// A meeting with a human organizer is NOT low-signal.
	meeting := lowSigEventMem("1:1 with boss", "boss@acme.com", now.Add(24*time.Hour))
	if isLowSignalItem(meeting, "new", now) {
		t.Fatalf("a meeting with a human organizer must NOT be low-signal")
	}
	// A personal event with NO organizer must NOT be collapsed — a real Apple Calendar
	// event can legitimately lack an organizer (false-positive guard).
	personal := lowSigEventMem("Dentist appointment", "", now.Add(24*time.Hour))
	if isLowSignalItem(personal, "new", now) {
		t.Fatalf("a personal no-organizer event must NOT be low-signal")
	}
}

func TestLowSignalStalePastUpdatedEvent(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	// Human organizer (so the subscription rule doesn't apply); PAST + updated => stale.
	pastUpdated := lowSigEventMem("Weekly sync", "boss@acme.com", now.Add(-72*time.Hour))
	if !isLowSignalItem(pastUpdated, "updated", now) {
		t.Fatalf("a PAST event resurfacing only as [updated] (re-sync bump) must be low-signal")
	}
	// A FUTURE event that was updated (genuinely rescheduled) is NOT stale.
	futureUpdated := lowSigEventMem("Rescheduled review", "boss@acme.com", now.Add(24*time.Hour))
	if isLowSignalItem(futureUpdated, "updated", now) {
		t.Fatalf("a FUTURE rescheduled event must NOT be low-signal")
	}
	// A PAST event that is NEW (first seen) is not the stale-resync case.
	pastNew := lowSigEventMem("Yesterday's standup", "boss@acme.com", now.Add(-12*time.Hour))
	if isLowSignalItem(pastNew, "new", now) {
		t.Fatalf("a past event that is genuinely NEW must not be treated as stale-updated noise")
	}
}

// TestBudgetFairFloorProtectsLowerRankSource (defect 3): a fat high-rank calendar
// section can no longer starve the Emails section — each source is guaranteed its
// budgetSourceFloor before any source is filled past it.
func TestBudgetFairFloorProtectsLowerRankSource(t *testing.T) {
	mk := func(src string, n int) DigestSection {
		var its []DigestItem
		for i := 0; i < n; i++ {
			its = append(its, DigestItem{ID: src + itoa(i), Title: src + " item " + itoa(i), Snippet: "some body text", Source: src})
		}
		return DigestSection{Source: src, State: stateDelta, Items: its}
	}
	// calendar sorts first (rank 0), gmail last (rank 2).
	d := Digest{Generated: "2026-07-02T00:00:00Z", Sections: []DigestSection{mk("calendar", 8), mk("gmail", 4)}}

	// A budget that reserves the chrome + exactly the two floors' items (2 each) and no
	// more — so WITHOUT a floor the rank-first greedy fill would give calendar all 4
	// (well past its floor) and gmail 0.
	reserve := len(renderDigestHeader(d)) + len(renderDigestFreshness(d)) +
		len(renderDigestUrgentShelf(d)) + len(renderDigestStaleTasks(d))
	for _, s := range d.Sections {
		reserve += len(renderDigestSectionHeading(s)) + len(renderDigestMoreLine(len(s.Items)+s.MoreCount))
	}
	floorBytes := 0
	for _, s := range d.Sections {
		for j := 0; j < budgetSourceFloor; j++ {
			floorBytes += len(renderDigestItemLine(s.Items[j]))
		}
	}
	budget := reserve + floorBytes

	bd, survived := budgetDigestForMarkdown(d, budget)
	gm := digestSections(bd)["gmail"]
	if len(gm.Items) < budgetSourceFloor {
		t.Fatalf("fair floor must protect the lower-rank Emails section; gmail got %d items (< floor %d)", len(gm.Items), budgetSourceFloor)
	}
	for _, it := range gm.Items {
		if !survived[it.ID] {
			t.Fatalf("kept gmail item %q must be reported as a survivor", it.ID)
		}
	}
}
