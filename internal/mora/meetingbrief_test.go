package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func meetingBriefEmail(id, title, body, from string, to []string, at time.Time) Memory {
	return Memory{
		ID:          id,
		Scope:       "project:acme",
		Type:        "email",
		Title:       title,
		Text:        "From: " + from + "\n\n" + body,
		Source:      id,
		Provider:    "gmail",
		ProviderID:  id,
		CreatedAt:   at.UTC().Format(time.RFC3339),
		ContentHash: "hash-" + id,
		Meta: map[string]any{
			"from":        []string{from},
			"to":          to,
			"occurred_at": at.UTC().Format(time.RFC3339),
			"names": map[string]string{
				"neil@example.com": "Neil Patel",
				"riya@example.com": "Riya Karode",
			},
		},
	}
}

func TestMeetingBriefFixtureIsFullyCitedDeterministicAndActionable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}

	event := eventMemFull(
		"calendar_event/founder-sync",
		"Founder sync",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@example.com": "Adit",
			"neil@example.com": "Neil Patel",
			"riya@example.com": "Riya Karode",
		},
		"adit@example.com", "riya@example.com", "neil@example.com",
	)
	event.Source = "calendar_event/founder-sync"
	event.Provider = "google"
	event.ProviderID = "calendar_event/founder-sync"
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}

	fixtures := []Memory{
		meetingBriefEmail(
			"gmail_thread/revised-deck",
			"Revised investor deck",
			"Can you send the revised deck by tomorrow?",
			"neil@example.com",
			[]string{"adit@example.com"},
			at.Add(-2*time.Hour),
		),
		meetingBriefEmail(
			"gmail_thread/pricing",
			"Pricing decision pending",
			"The pricing decision is still pending; next steps remain open.",
			"neil@example.com",
			[]string{"adit@example.com"},
			at.Add(-24*time.Hour),
		),
		meetingBriefEmail(
			"gmail_thread/new-role",
			"New role",
			"I joined Example Labs in a new role.",
			"neil@example.com",
			[]string{"adit@example.com"},
			at.Add(-48*time.Hour),
		),
		meetingBriefEmail(
			"gmail_thread/pilot",
			"Acme pilot roadmap",
			"The Acme pilot roadmap and launch milestone are attached.",
			"riya@example.com",
			[]string{"adit@example.com"},
			at.Add(-72*time.Hour),
		),
		meetingBriefEmail(
			"gmail_thread/trivia",
			"Family update",
			"My kid's name is Robin and their favorite food is pizza.",
			"neil@example.com",
			[]string{"adit@example.com"},
			at.Add(-96*time.Hour),
		),
	}
	for _, m := range fixtures {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	first, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("fixed (vault, --at) must be byte-identical:\n%s\n%s", firstJSON, secondJSON)
	}
	if first.EgressCalls != 0 {
		t.Fatalf("egress meter = %d, want 0", first.EgressCalls)
	}
	if first.Event == nil || len(first.Event.Attendees) != 2 {
		t.Fatalf("event/attendees = %+v", first.Event)
	}
	if strings.Contains(string(firstJSON), "Robin") || strings.Contains(string(firstJSON), "pizza") {
		t.Fatalf("attendee trivia leaked into unfinished-business brief: %s", firstJSON)
	}

	kinds := map[string]bool{}
	lines := 0
	for _, section := range first.Sections {
		kinds[section.Kind] = true
		for _, line := range section.Lines {
			lines++
			if err := line.validate(); err != nil {
				t.Fatalf("uncited line rendered: %+v: %v", line, err)
			}
		}
	}
	for _, want := range []string{
		meetingBriefOpenLoops,
		meetingBriefUnresolved,
		meetingBriefStaleness,
		meetingBriefSharedContext,
	} {
		if !kinds[want] {
			t.Errorf("missing unfinished-business section %q: %+v", want, first.Sections)
		}
	}
	if lines != 4 {
		t.Fatalf("surfaced %d evidence lines, want exactly four actionable cited lines: %s", lines, firstJSON)
	}
}

func TestRenderMeetingBriefFailsClosedOnUncitedLine(t *testing.T) {
	brief := MeetingBrief{
		AsOf: "2026-07-10T15:00:00Z",
		Event: &CitedMeetingEvent{
			ID:       "calendar_event/e1",
			Title:    "Sync",
			StartsAt: "2026-07-10T17:00:00Z",
			Citation: BriefCitation{
				MemoryID: "calendar_event/e1",
				Channel:  "calendar",
				Source:   "calendar_event/e1",
				Date:     "2026-07-10T17:00:00Z",
			},
		},
		Sections: []MeetingBriefSection{{
			Kind:  meetingBriefOpenLoops,
			Title: meetingBriefSectionTitles[meetingBriefOpenLoops],
			Lines: []CitedBriefLine{{
				Text: "Send the deck",
				Citation: BriefCitation{
					MemoryID: "gmail_thread/t1",
					Channel:  "gmail",
					Date:     "2026-07-10T13:00:00Z",
				},
			}},
		}},
	}
	var out bytes.Buffer
	err := renderMeetingBrief(&out, brief)
	if err == nil || !strings.Contains(err.Error(), "refusing to render uncited") {
		t.Fatalf("render error = %v, want fail-closed uncited error", err)
	}
	if out.Len() != 0 {
		t.Fatalf("fail-closed renderer wrote partial output: %q", out.String())
	}
}

func TestMeetingBriefRejectsWrongOrAmbiguousEventID(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/not-an-event", Type: "email", Title: "Not an event",
		CreatedAt: at.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := buildEventMeetingBrief(context.Background(), cfg, "gmail_thread/not-an-event", at, 0, 8); err == nil {
		t.Fatal("non-event id must fail rather than attribute an unrelated memory")
	}
}
