package mora

import (
	"strings"
	"testing"
	"time"
)

func TestCompanionEventsEmpty(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	out := run(t, "companion", "events", "--json")
	doc := decodeCompanion(t, out, schemaCompanionEvents)

	events, ok := doc["events"].([]any)
	if !ok {
		t.Fatalf("events key is not a slice: %v", doc["events"])
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestCompanionEventsFlagsValidation(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	// Missing --json
	_, _, err := runSplit(t, "companion", "events")
	if err == nil || !strings.Contains(err.Error(), "--json is required") {
		t.Fatalf("expected missing --json error, got: %v", err)
	}

	// Invalid --days 0
	_, _, err = runSplit(t, "companion", "events", "--days", "0", "--json")
	if err == nil || !strings.Contains(err.Error(), "invalid --days") {
		t.Fatalf("expected invalid --days error for 0, got: %v", err)
	}

	// Invalid --days 91
	_, _, err = runSplit(t, "companion", "events", "--days", "91", "--json")
	if err == nil || !strings.Contains(err.Error(), "invalid --days") {
		t.Fatalf("expected invalid --days error for 91, got: %v", err)
	}

	// Unexpected positional argument
	_, _, err = runSplit(t, "companion", "events", "--json", "extra")
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("expected unexpected argument error, got: %v", err)
	}
}

func TestCompanionEventsParsingAndFiltering(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	pinOperationClockForTest(t, base)

	writeConfigKeyForTest(t, cfg, `self_emails = "adit@example.com"`)

	writeEvent := func(id, title, when, text string) {
		t.Helper()
		mem := Memory{
			ID:        id,
			Scope:     "global",
			Type:      "event",
			Title:     title,
			Text:      text,
			Source:    "calendar",
			Provider:  "calendar",
			CreatedAt: when,
			Meta: map[string]any{
				"occurred_at": when,
			},
		}
		if err := writeMemory(cfg, mem); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Google Meet with external attendee
	writeEvent("ev_meet", "External Sync", base.Add(2*time.Hour).Format(time.RFC3339),
		"When: Wed, 16 Sep 2026 14:00:00 UTC\nCalendar: Work\nLocation: Room 101\nAttendees: Adit <adit@example.com>, Partner Bob <bob@external.com>, Alice <alice@external.com>\nNotes: Join at https://meet.google.com/abc-defg-hij")

	// 2. Zoom meeting with only self attendee and local user
	writeEvent("ev_zoom", "Solo Planning", base.Add(4*time.Hour).Format(time.RFC3339),
		"When: Wed, 16 Sep 2026 16:00:00 UTC\nWhere: Virtual Room\nAttendees: adit@example.com, localdaemon\nJoin: <https://zoom.us/j/987654321>")

	// 3. Teams meeting with parenthesized email
	writeEvent("ev_teams", "Partner Review", base.Add(24*time.Hour).Format(time.RFC3339),
		"When: Thu, 17 Sep 2026 12:00:00 UTC\nAttendees: Marcus Vance (marcus.vance@northwind.com)\nLink: https://teams.microsoft.com/l/meetup-join/19123456")

	// 4. Past event within -2h window (-1h)
	writeEvent("ev_past_recent", "Recent Past Sync", base.Add(-1*time.Hour).Format(time.RFC3339),
		"When: Wed, 16 Sep 2026 11:00:00 UTC\nAttendees: adit@example.com")

	// 5. Past event outside -2h window (-3h)
	writeEvent("ev_past_old", "Expired Past Sync", base.Add(-3*time.Hour).Format(time.RFC3339),
		"When: Wed, 16 Sep 2026 09:00:00 UTC\nAttendees: adit@example.com")

	// 6. Future event at 8 days (outside default 7 days)
	writeEvent("ev_future_8d", "Next Week Event", base.AddDate(0, 0, 8).Format(time.RFC3339),
		"When: Thu, 24 Sep 2026 12:00:00 UTC\nAttendees: adit@example.com")

	// Default 7 days run
	out := run(t, "companion", "events", "--json")
	doc := decodeCompanion(t, out, schemaCompanionEvents)

	rawEvents, ok := doc["events"].([]any)
	if !ok {
		t.Fatalf("events is not a slice: %v", doc["events"])
	}

	// Should contain ev_past_recent, ev_meet, ev_zoom, ev_teams (4 events)
	// Should NOT contain ev_past_old (too old) or ev_future_8d (too far)
	if len(rawEvents) != 4 {
		t.Fatalf("expected 4 events with default 7 days, got %d", len(rawEvents))
	}

	eventsByID := make(map[string]map[string]any)
	for _, raw := range rawEvents {
		ev := raw.(map[string]any)
		eventsByID[ev["id"].(string)] = ev
	}

	// Verify ev_meet
	meet, ok := eventsByID["ev_meet"]
	if !ok {
		t.Fatalf("ev_meet missing from events")
	}
	if meet["title"] != "External Sync" {
		t.Errorf("ev_meet title = %v, want External Sync", meet["title"])
	}
	if meet["calendar"] != "Work" {
		t.Errorf("ev_meet calendar = %v, want Work", meet["calendar"])
	}
	if meet["location"] != "Room 101" {
		t.Errorf("ev_meet location = %v, want Room 101", meet["location"])
	}
	if meet["meet_url"] != "https://meet.google.com/abc-defg-hij" {
		t.Errorf("ev_meet meet_url = %v, want https://meet.google.com/abc-defg-hij", meet["meet_url"])
	}
	if meet["has_external"] != true {
		t.Errorf("ev_meet has_external = %v, want true", meet["has_external"])
	}
	if meet["dossier_query"] != "bob@external.com" {
		t.Errorf("ev_meet dossier_query = %v, want bob@external.com", meet["dossier_query"])
	}
	attendeesMeet, _ := meet["attendees"].([]any)
	if len(attendeesMeet) != 3 {
		t.Errorf("ev_meet attendees count = %d, want 3", len(attendeesMeet))
	}

	// Verify ev_zoom
	zoom, ok := eventsByID["ev_zoom"]
	if !ok {
		t.Fatalf("ev_zoom missing from events")
	}
	if zoom["location"] != "Virtual Room" {
		t.Errorf("ev_zoom location = %v, want Virtual Room", zoom["location"])
	}
	if zoom["meet_url"] != "https://zoom.us/j/987654321" {
		t.Errorf("ev_zoom meet_url = %v, want https://zoom.us/j/987654321", zoom["meet_url"])
	}
	if zoom["has_external"] != false {
		t.Errorf("ev_zoom has_external = %v, want false", zoom["has_external"])
	}
	if zoom["dossier_query"] != "Solo Planning" {
		t.Errorf("ev_zoom dossier_query = %v, want Solo Planning", zoom["dossier_query"])
	}

	// Verify ev_teams
	teams, ok := eventsByID["ev_teams"]
	if !ok {
		t.Fatalf("ev_teams missing from events")
	}
	if teams["meet_url"] != "https://teams.microsoft.com/l/meetup-join/19123456" {
		t.Errorf("ev_teams meet_url = %v, want https://teams.microsoft.com/l/meetup-join/19123456", teams["meet_url"])
	}
	if teams["has_external"] != true {
		t.Errorf("ev_teams has_external = %v, want true", teams["has_external"])
	}
	if teams["dossier_query"] != "marcus.vance@northwind.com" {
		t.Errorf("ev_teams dossier_query = %v, want marcus.vance@northwind.com", teams["dossier_query"])
	}

	// Now run with --days 10
	out10 := run(t, "companion", "events", "--days", "10", "--json")
	doc10 := decodeCompanion(t, out10, schemaCompanionEvents)
	rawEvents10, _ := doc10["events"].([]any)
	if len(rawEvents10) != 5 {
		t.Fatalf("expected 5 events with --days 10, got %d", len(rawEvents10))
	}
}

func TestCompanionEventsDropsFeedsAndDedupesMeetings(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	pinOperationClockForTest(t, base)
	writeConfigKeyForTest(t, cfg, `self_emails = "adit@example.com"`)

	writeEvent := func(id, title, when, text string) {
		t.Helper()
		mem := Memory{
			ID:        id,
			Scope:     "global",
			Type:      "event",
			Title:     title,
			Text:      text,
			Source:    "calendar",
			Provider:  "calendar",
			CreatedAt: when,
			Meta:      map[string]any{"occurred_at": when},
		}
		if err := writeMemory(cfg, mem); err != nil {
			t.Fatal(err)
		}
	}

	meetWhen := base.Add(48 * time.Hour).Format(time.RFC3339)
	writeEvent("applecal_event/client", "Client & Example Co Team Meeting", meetWhen,
		"When: Fri, 18 Sep 2026 10:00:00 PDT\nCalendar: work@example.com\nAttendees: adit@example.com, client@example.com\nhttps://meet.google.com/abc-defg-hij")
	writeEvent("calendar_event/client", "Client & Example Co Team Meeting", meetWhen,
		"When: Fri, 18 Sep 2026 10:00:00 PDT\nCalendar: primary\nAttendees: adit@example.com")
	writeEvent("applecal_event/yom", "Yom Kippur", base.AddDate(0, 0, 5).Format(time.RFC3339),
		"When: 2026-09-21 (all day)\nCalendar: US Holidays\nHoliday starts at sundown on the day before.")
	writeEvent("applecal_event/chelsea", "⚽️ Brentford - Chelsea", base.AddDate(0, 0, 2).Format(time.RFC3339),
		"When: 2026-09-18T19:00:00Z\nCalendar: Chelsea\nLocation: Gtech Community Stadium\nhttps://www.fotmob.com/match/5795456 Premier League")
	writeEvent("calendar_event/sync", "Sam - Alex sync up", base.AddDate(0, 0, 6).Format(time.RFC3339),
		"When: Tue, 22 Sep 2026 11:00:00 PDT\nCalendar: primary\nAttendees: adit@example.com, sam@example.com")

	out := run(t, "companion", "events", "--json")
	doc := decodeCompanion(t, out, schemaCompanionEvents)
	rawEvents, ok := doc["events"].([]any)
	if !ok {
		t.Fatalf("events is not a slice: %v", doc["events"])
	}
	if len(rawEvents) != 2 {
		t.Fatalf("expected 2 meetings (client once + sync), got %d: %#v", len(rawEvents), rawEvents)
	}

	byTitle := map[string]map[string]any{}
	for _, raw := range rawEvents {
		ev := raw.(map[string]any)
		byTitle[ev["title"].(string)] = ev
	}
	if _, ok := byTitle["Yom Kippur"]; ok {
		t.Fatal("Yom Kippur from US Holidays must not appear as a meeting")
	}
	if _, ok := byTitle["⚽️ Brentford - Chelsea"]; ok {
		t.Fatal("Chelsea sports feed must not appear as a meeting")
	}
	client, ok := byTitle["Client & Example Co Team Meeting"]
	if !ok {
		t.Fatal("client meeting missing after dedupe")
	}
	if client["meet_url"] != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("dedupe kept the copy without a meet URL: %#v", client)
	}
	if _, ok := byTitle["Sam - Alex sync up"]; !ok {
		t.Fatal("Sam sync with attendees must remain")
	}
}

func TestKeepCompanionMeeting(t *testing.T) {
	if keepCompanionMeeting(CompanionEvent{Title: "Yom Kippur", Calendar: "US Holidays"}) {
		t.Fatal("holiday calendar leaked")
	}
	if keepCompanionMeeting(CompanionEvent{Title: "Brentford - Chelsea", Calendar: "Chelsea", Location: "stadium"}) {
		t.Fatal("sports calendar leaked")
	}
	if !keepCompanionMeeting(CompanionEvent{Title: "Sync", Attendees: []string{"a@x.com"}}) {
		t.Fatal("attendee meeting dropped")
	}
	if !keepCompanionMeeting(CompanionEvent{Title: "Call", MeetURL: "https://meet.google.com/abc"}) {
		t.Fatal("meet-url meeting dropped")
	}
}
