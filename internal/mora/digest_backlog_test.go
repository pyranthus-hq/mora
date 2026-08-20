package mora

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// seedCalEvent writes a calendar memory at an absolute instant, optionally tagged
// with a recurring-series id (meta.recurring_event_id) so the digest's
// series-collapse and forward-horizon logic can be exercised deterministically.
func seedCalEvent(t *testing.T, cfg Config, title string, at time.Time, recurringID string) {
	t.Helper()
	meta := map[string]any{}
	if recurringID != "" {
		meta["recurring_event_id"] = recurringID
	}
	m := Memory{
		ID:          "id-" + title,
		Scope:       "global",
		Type:        "note",
		Title:       title,
		Text:        title + " — event body for the digest snippet",
		Source:      "calendar_evt/" + title,
		Provider:    "calendar",
		ProviderID:  "calendar_evt/" + title,
		ContentHash: "h-" + title,
		CreatedAt:   at.UTC().Format(time.RFC3339),
		Meta:        meta,
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
}

// Bug A (card …liLE): a 48h window must NOT surface calendar events months out.
func TestWindowDigestForwardBoundsCalendar(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	seedCalEvent(t, cfg, "Near meeting", now.Add(12*time.Hour), "")
	seedCalEvent(t, cfg, "Hanukkah", now.Add(30*24*time.Hour), "")
	cfg = ungatedDigestConfig(cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 48, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	cal := digestSections(d)["calendar"]
	titles := sectionTitles(cal)
	if !contains(titles, "Near meeting") {
		t.Errorf("calendar section should include the near event; got %v", titles)
	}
	if contains(titles, "Hanukkah") {
		t.Errorf("48h window must forward-bound calendar; month-out event leaked: %v", titles)
	}
}

// Bug A: a past-oriented source's window has a future bound too — a future-dated
// gmail item (clock-skew/scheduled-send) is not part of "the last 48h".
func TestWindowDigestExcludesFutureNonCalendar(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	digestSeed(t, cfg, "gmail", "Real recent email", 2*time.Hour, now)
	digestSeed(t, cfg, "gmail", "Future email", -10*time.Hour, now) // now+10h

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 48, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	titles := sectionTitles(digestSections(d)["gmail"])
	if !contains(titles, "Real recent email") {
		t.Errorf("recent email should be present; got %v", titles)
	}
	if contains(titles, "Future email") {
		t.Errorf("future-dated non-calendar item must be excluded from the window; got %v", titles)
	}
}

// Bug A: within an upcoming (calendar) section, the NEAREST future event leads —
// not the farthest-out one (the old timestamp-DESC tie-break ranked Dec first).
func TestUpcomingSectionOrdersNearestFutureFirst(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	seedCalEvent(t, cfg, "Later meeting", now.Add(36*time.Hour), "")
	seedCalEvent(t, cfg, "Sooner meeting", now.Add(6*time.Hour), "")
	cfg = ungatedDigestConfig(cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 48, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	cal := digestSections(d)["calendar"]
	if len(cal.Items) == 0 || cal.Items[0].Title != "Sooner meeting" {
		t.Errorf("nearest-future event should lead; got %v", sectionTitles(cal))
	}
}

// Bug B (card …dnmM): a recurring series collapses to ONE digest line so it can't
// flood the section (and, via the byte budget, starve other sources).
func TestRecurringSeriesCollapsedInWindow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	for i := 1; i <= 5; i++ {
		seedCalEvent(t, cfg, "Sync up "+itoa(i), now.Add(time.Duration(i*6)*time.Hour), "series-sync")
	}
	seedCalEvent(t, cfg, "One-off review", now.Add(3*time.Hour), "")
	cfg = ungatedDigestConfig(cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 72, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	cal := digestSections(d)["calendar"]
	// 5 recurring instances → 1 collapsed line; plus the one-off → 2 items total.
	if len(cal.Items) != 2 {
		t.Fatalf("recurring series should collapse to one line (want 2 items: series + one-off); got %d: %v", len(cal.Items), sectionTitles(cal))
	}
	var seriesLine string
	for _, it := range cal.Items {
		if strings.Contains(it.Title, "Sync up") {
			seriesLine = it.Title
		}
	}
	if seriesLine == "" || !strings.Contains(seriesLine, "5") {
		t.Errorf("collapsed series line should carry the ×5 count; got %q", seriesLine)
	}
}

// Bug D (card …liO8): source_states count must reflect the in-window TOTAL
// (shown + more), not just the shown items, so the agent isn't misled.
func TestSourceStatesCountIncludesMore(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 12; i++ {
		digestSeed(t, cfg, "gmail", "Email "+itoa(i), time.Duration(i+1)*time.Hour, now)
	}
	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 48, perSourceCap: 8})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	gmail := digestSections(d)["gmail"]
	if len(gmail.Items)+gmail.MoreCount != 12 {
		t.Fatalf("precondition: want 12 in-window items (shown+more); got %d+%d", len(gmail.Items), gmail.MoreCount)
	}
	for _, ss := range buildSourceStates(cfg, d) {
		if ss.Instance == "gmail" {
			if ss.Count != 12 {
				t.Errorf("source_states count should be the in-window total 12; got %d", ss.Count)
			}
			return
		}
	}
	t.Fatalf("no gmail source_state found")
}

// Bug B / Codex P1: a single rescheduled instance (Change=="updated") must NOT be
// folded into the series representative — folding it would mark its update
// acknowledged in the delta watermark without ever surfacing the change.

// --- tiny local helpers (kept test-local to avoid colliding with prod helpers) --

func sectionTitles(s DigestSection) []string {
	out := make([]string, 0, len(s.Items))
	for _, it := range s.Items {
		out = append(out, it.Title)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
