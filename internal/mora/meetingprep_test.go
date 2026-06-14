package mora

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func runPrep(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	full := append([]string{"prep"}, args...)
	if err := Run(context.Background(), full, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run(prep %v): %v\n%s", args, err, out.String())
	}
	return out.String()
}

// TestCmdPrepCLI: `mora prep --at <now>` prints the next meeting, attendees, cited
// context, and the model-free synthesis prompt.
func TestCmdPrepCLI(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "riya@a.com", "Riya Karode", now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Acme sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	out := runPrep(t, "--at", now.Format(time.RFC3339))
	if !strings.Contains(out, "Next meeting: Acme sync") || !strings.Contains(out, "Riya Karode") {
		t.Fatalf("prep output missing meeting/attendee:\n%s", out)
	}
	if !strings.Contains(out, "To compose a grounded brief") {
		t.Fatalf("prep output missing synthesis prompt:\n%s", out)
	}
}

// TestCmdPrepJSON: `mora prep --json` emits the typed result with the event.
func TestCmdPrepJSON(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMemFull("evt", "Standup", now.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	out := runPrep(t, "--json", "--at", now.Format(time.RFC3339))
	if !strings.Contains(out, `"stable_id": "evt"`) || !strings.Contains(out, `"synthesis_prompt"`) {
		t.Fatalf("prep --json missing typed fields:\n%s", out)
	}
}

func eventMemFull(id, title, occurredAt string, names map[string]string, attendees ...string) Memory {
	m := eventMem(id, title, occurredAt, attendees...)
	if names != nil {
		m.Meta["names"] = names
	}
	return m
}

// TestBuildMeetingPrepBasic: the pack carries the event, a known attendee with
// cited evidence, a thin fresh attendee gap, and a non-fabrication prompt.
func TestBuildMeetingPrepBasic(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "riya@a.com", "Riya Karode", now.Add(-72*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMemNamed("e2", "gmail", "riya@a.com", "Riya Karode", now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Acme sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com", "newbob@x.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Event == nil || mp.Event.StableID != "evt" {
		t.Fatalf("event = %+v, want evt", mp.Event)
	}
	if !strings.Contains(mp.SynthesisPrompt, "MEETING: Acme sync") || !strings.Contains(mp.SynthesisPrompt, "Do NOT invent decisions") {
		t.Fatalf("prompt missing meeting frame / anti-fabrication clause:\n%s", mp.SynthesisPrompt)
	}
	var riya *PrepAttendee
	for i := range mp.Attendees {
		if mp.Attendees[i].PersonID == "person:riya@a.com" {
			riya = &mp.Attendees[i]
		}
	}
	if riya == nil || !riya.Known || riya.EvidenceCount < 2 {
		t.Fatalf("riya attendee = %+v, want known with >=2 evidence", riya)
	}
	if joined := strings.Join(mp.Gaps.ThinAttendees, " "); !strings.Contains(joined, "newbob@x.com") {
		t.Errorf("expected the fresh attendee newbob to be flagged thin: %v", mp.Gaps.ThinAttendees)
	}
}

// TestBuildMeetingPrepSelfExcluded: the user's own Google address is dropped from
// the attendee list.
func TestBuildMeetingPrepSelfExcluded(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Email: "adit@me.com", Enabled: ptr(true), CreatedAt: now.Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "1:1", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya"}, "adit@me.com", "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range mp.Attendees {
		if a.Identity == "adit@me.com" {
			t.Fatalf("self (adit@me.com) leaked into attendees: %+v", mp.Attendees)
		}
	}
	if mp.Gaps.SelfUnknown {
		t.Error("SelfUnknown should be false when a Google account is connected")
	}
}

// TestBuildMeetingPrepSelfUnknownGap: no Google source => no self-exclusion, gap set.
func TestBuildMeetingPrepSelfUnknownGap(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMemFull("evt", "sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !mp.Gaps.SelfUnknown {
		t.Error("SelfUnknown should be true with no Google account connected")
	}
}

// TestBuildMeetingPrepForgivingFallback (UX FORK 2): a named attendee with no
// upcoming meeting falls back to the next meeting, with an honest note.
func TestBuildMeetingPrepForgivingFallback(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMemFull("team", "Team sync", now.Add(3*time.Hour).Format(time.RFC3339),
		map[string]string{"bob@z.com": "Bob"}, "bob@z.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "Riya", map[string]bool{"person:riya@a.com": true}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Event == nil || mp.Event.StableID != "team" {
		t.Fatalf("event = %+v, want forgiving fallback to 'team'", mp.Event)
	}
	if !strings.Contains(mp.FallbackNote, "Riya") {
		t.Fatalf("FallbackNote = %q, want a note mentioning Riya", mp.FallbackNote)
	}
}

// TestBuildMeetingPrepNoUpcoming: only past events => Event nil, honest prompt.
func TestBuildMeetingPrepNoUpcoming(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMem("old", "Old", now.Add(-72*time.Hour).Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Event != nil {
		t.Fatalf("event = %+v, want nil (no upcoming)", mp.Event)
	}
	if !strings.Contains(mp.SynthesisPrompt, "No upcoming calendar event") {
		t.Fatalf("prompt = %q, want the honest no-event message", mp.SynthesisPrompt)
	}
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
	// No matching attendee → nil (the forgiving fallback is handled by buildMeetingPrep).
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
