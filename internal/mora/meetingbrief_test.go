package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"go/parser"
	"go/token"
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

func mustBriefCitationForTest(t *testing.T, memoryID, channel, source, date string) BriefCitation {
	t.Helper()
	c, err := newBriefCitation(memoryID, channel, source, date)
	if err != nil {
		t.Fatalf("newBriefCitation: %v", err)
	}
	return c
}

func TestMeetingBriefFixtureIsFullyCitedDeterministicAndActionable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
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
	event.Text += " Agenda: revised investor deck."
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
	if _, err := rebuildIndex(ctx, cfg); err != nil {
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
	var firstHuman, secondHuman bytes.Buffer
	if err := renderMeetingBrief(&firstHuman, first); err != nil {
		t.Fatal(err)
	}
	if err := renderMeetingBrief(&secondHuman, second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstHuman.Bytes(), secondHuman.Bytes()) {
		t.Fatalf("rebuild changed human brief bytes:\n%s\n%s", firstHuman.Bytes(), secondHuman.Bytes())
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
	if !kinds[meetingBriefOpenLoops] {
		t.Errorf("missing commitment section %q: %+v", meetingBriefOpenLoops, first.Sections)
	}
	if len(kinds) != 1 || lines != 1 {
		t.Fatalf("surfaced %d sections / %d lines, want the one typed relevant commitment only: %s", len(kinds), lines, firstJSON)
	}
}

func TestMeetingBriefRanksForgottenActionableEvidenceAboveRecentNoise(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-forgettability",
		"Portfolio commitments",
		at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"dana@example.com": "Dana", "me@example.com": "Me"},
		"me@example.com", "dana@example.com",
	)
	event.Text += " Agenda: portfolio introduction document."
	oldGem := meetingBriefEmail(
		"forgotten-gem",
		"Portfolio introduction commitment",
		"Can you send the portfolio introduction document you promised?",
		"dana@example.com",
		[]string{"me@example.com"},
		at.Add(-300*24*time.Hour),
	)
	recentNoise := meetingBriefEmail(
		"daily-noise",
		"Daily check-in",
		"Can you confirm today's routine check-in?",
		"dana@example.com",
		[]string{"me@example.com"},
		at.Add(-2*time.Hour),
	)
	for _, memory := range []Memory{event, oldGem, recentNoise} {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), oldGem.ID) {
		t.Fatalf("forgotten actionable gem did not win the single attendee slot: %s", payload)
	}
	if strings.Contains(string(payload), recentNoise.ID) {
		t.Fatalf("recent routine noise displaced the forgotten actionable gem: %s", payload)
	}
}

func TestMeetingBriefDropsAmbiguousSharedThread(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-shared-thread",
		"Contract",
		at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{
			"alice@example.com": "Alice",
			"bob@example.com":   "Bob",
			"me@example.com":    "Me",
		},
		"me@example.com", "alice@example.com",
		"bob@example.com",
	)
	event.Text += " Agenda: approve the contract."
	shared := meetingBriefEmail(
		"shared-request",
		"Contract approval",
		"Can you approve the contract before Friday?",
		"bob@example.com",
		[]string{"me@example.com", "alice@example.com"},
		at.Add(-30*24*time.Hour),
	)
	shared.Meta["names"] = map[string]string{
		"alice@example.com": "Alice",
		"bob@example.com":   "Bob",
	}
	for _, memory := range []Memory{event, shared} {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(brief) != 0 {
		t.Fatalf("multi-addressee request was assigned to an arbitrary attendee: %+v", brief.Sections)
	}
}

func TestMeetingBriefDropsAmbiguousOutboundGroupAttribution(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-ambiguous-outbound",
		"Contract review",
		at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{
			"alice@example.com": "Alice",
			"bob@example.com":   "Bob",
			"me@example.com":    "Me",
		},
		"me@example.com", "alice@example.com",
		"bob@example.com",
	)
	outbound := meetingBriefEmail(
		"outbound-group-promise",
		"Contract follow-up",
		"I will send the signed contract tomorrow.",
		"me@example.com",
		[]string{"alice@example.com", "bob@example.com"},
		at.Add(-30*24*time.Hour),
	)
	outbound.Meta["names"] = map[string]string{
		"alice@example.com": "Alice",
		"bob@example.com":   "Bob",
		"me@example.com":    "Me",
	}
	for _, memory := range []Memory{event, outbound} {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(brief) != 0 {
		t.Fatalf("ambiguous group promise was assigned to an arbitrary attendee: %+v", brief.Sections)
	}
}

func TestMeetingBriefRendersActionablePassageNotTriviaFromMixedThread(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-mixed-thread",
		"Deck review",
		at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"dana@example.com": "Dana", "me@example.com": "Me"},
		"me@example.com", "dana@example.com",
	)
	event.Text += " Agenda: revised deck."
	mixed := meetingBriefEmail(
		"mixed-thread",
		"Catch up",
		"My kid's name is Robin; can you send the revised deck before Friday?",
		"dana@example.com",
		[]string{"me@example.com"},
		at.Add(-90*24*time.Hour),
	)
	for _, memory := range []Memory{event, mixed} {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "Robin") || strings.Contains(string(payload), "kid's name") {
		t.Fatalf("personal trivia leaked from an otherwise actionable thread: %s", payload)
	}
	if !strings.Contains(string(payload), "revised deck") {
		t.Fatalf("actionable passage missing from mixed thread: %s", payload)
	}
}

func TestMeetingBriefMaxTokensBudgetsSerializedPayload(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-budgeted-brief",
		"Portfolio review",
		at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"dana@example.com": "Dana", "me@example.com": "Me"},
		"me@example.com", "dana@example.com",
	)
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		memory := meetingBriefEmail(
			fmt.Sprintf("budget-request-%02d", i),
			fmt.Sprintf("Portfolio request %02d", i),
			fmt.Sprintf("Can you send portfolio document %02d before the review?", i),
			"dana@example.com",
			[]string{"me@example.com"},
			at.Add(-time.Duration(30+i)*24*time.Hour),
		)
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	const maxTokens = 2000
	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, maxTokens, 15)
	if err != nil {
		t.Fatal(err)
	}
	_, budgetChars := resolveContextBudgetTokens(cfg, maxTokens)
	if got, wantMax := jsonLen(brief), budgetChars/mcpDigestEnvelopeDivisor; got > wantMax {
		t.Fatalf("serialized MeetingBrief = %d bytes, compact budget = %d", got, wantMax)
	}
	if meetingBriefLineCount(brief) == 0 {
		t.Fatal("real-size budget dropped every actionable line")
	}
	if _, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 1, 15); err == nil || !strings.Contains(err.Error(), "meeting brief event requires") {
		t.Fatalf("event-only payload exceeding max_tokens did not fail loudly: %v", err)
	}
}

func TestMeetingBriefDatedHistoricalRailRejectsStalePresentTense(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-dated-rail",
		"Career update",
		at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"dana@example.com": "Dana", "me@example.com": "Me"},
		"me@example.com", "dana@example.com",
	)
	stale := meetingBriefEmail(
		"stale-role",
		"New role",
		"Dana is now at Denver Labs in a new role.",
		"dana@example.com",
		[]string{"me@example.com"},
		at.Add(-300*24*time.Hour),
	)
	for _, memory := range []Memory{event, stale} {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := renderMeetingBrief(&rendered, brief); err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(brief) != 0 {
		t.Fatalf("non-commitment historical context crossed the event eligibility gate: %+v", brief.Sections)
	}

	correction, err := newBriefLineCorrection(
		govAtom{Kind: atomStableID, Value: stale.ID},
		govAtom{Kind: atomAddress, Value: "dana@example.com"},
	)
	if err != nil {
		t.Fatal(err)
	}
	line, err := newCitedBriefLine(
		"Dana is now at Denver Labs.", "Dana",
		mustBriefCitationForTest(t, stale.ID, "gmail", stale.Source, stale.CreatedAt),
		correction, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	line.Text = "Dana is now at Denver Labs."
	brief.Sections = []MeetingBriefSection{{
		Kind:  meetingBriefStaleness,
		Title: meetingBriefSectionTitles[meetingBriefStaleness],
		Lines: []CitedBriefLine{line},
	}}
	rendered.Reset()
	err = renderMeetingBrief(&rendered, brief)
	if err == nil || !strings.Contains(err.Error(), "dated-historical rail") {
		t.Fatalf("present-tense stale fact crossed the rendering rail: %v", err)
	}
	if rendered.Len() != 0 {
		t.Fatalf("dated-historical rail wrote partial output: %q", rendered.String())
	}
}

func TestRenderMeetingBriefFailsClosedOnUncitedLine(t *testing.T) {
	brief := MeetingBrief{
		AsOf: "2026-07-10T15:00:00Z",
		Event: &CitedMeetingEvent{
			ID:       "calendar_event/e1",
			Title:    "Sync",
			StartsAt: "2026-07-10T17:00:00Z",
			Citation: mustBriefCitationForTest(t, "calendar_event/e1", "calendar", "calendar_event/e1", "2026-07-10T17:00:00Z"),
		},
		Sections: []MeetingBriefSection{{
			Kind:  meetingBriefOpenLoops,
			Title: meetingBriefSectionTitles[meetingBriefOpenLoops],
			Lines: []CitedBriefLine{{
				Text:     "Send the deck",
				Citation: BriefCitation{},
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

func TestBriefCitationRejectsUncitedJSON(t *testing.T) {
	var c BriefCitation
	err := json.Unmarshal([]byte(`{"memory_id":"gmail_thread/t1","channel":"gmail","date":"2026-07-10T13:00:00Z"}`), &c)
	if err == nil || !strings.Contains(err.Error(), "missing source") {
		t.Fatalf("partial citation JSON should fail closed with missing source, got: %v", err)
	}
}

func TestMeetingBriefLinesCarryOneActionCorrections(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-correction-actions", "Board prep", at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"neil@example.com": "Neil Patel", "adit@example.com": "Me"}, "adit@example.com", "neil@example.com",
	)
	event.Text += " Agenda: board deck."
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, meetingBriefEmail(
		"ask-correction-actions", "Board deck", "Please review the board deck by tomorrow.",
		"neil@example.com", []string{"adit@example.com"}, at.Add(-time.Hour),
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(brief) == 0 {
		t.Fatal("expected at least one cited line")
	}
	line := brief.Sections[0].Lines[0]
	if line.Correction.StableAtom.Kind != atomStableID || line.Correction.StableAtom.Value != "ask-correction-actions" {
		t.Fatalf("stable atom = %+v", line.Correction.StableAtom)
	}
	if line.Correction.AttendeeAtom.Kind != atomAddress || line.Correction.AttendeeAtom.Value != "neil@example.com" {
		t.Fatalf("attendee atom = %+v", line.Correction.AttendeeAtom)
	}
	for _, cmd := range []string{line.Correction.CorrectCommand, line.Correction.UnlinkCommand} {
		if !strings.Contains(cmd, "mora brief correct --memory-id ask-correction-actions --attendee neil@example.com") {
			t.Fatalf("missing one-action command wiring: %q", cmd)
		}
	}
}

func TestBriefCorrectUnlinkPersistsAcrossRebuildAndCanBeReconfirmed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-correction-persist", "Founder sync", at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"neil@example.com": "Neil Patel", "adit@example.com": "Me"}, "adit@example.com", "neil@example.com",
	)
	event.Text += " Agenda: revised deck."
	ask := meetingBriefEmail(
		"ask-correction-persist", "Deck follow-up", "Can you send the revised deck by tomorrow?",
		"neil@example.com", []string{"adit@example.com"}, at.Add(-2*time.Hour),
	)
	for _, m := range []Memory{event, ask} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	before, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(before) == 0 {
		t.Fatal("expected the line before correction")
	}
	if _, err := runErr(t, "brief", "correct", "--memory-id", ask.ID, "--attendee", "neil@example.com", "--unlink"); err == nil {
		t.Fatal("unlink without --yes must be rejected")
	}
	if out := run(t, "brief", "correct", "--memory-id", ask.ID, "--attendee", "neil@example.com", "--unlink", "--yes"); !strings.Contains(out, "unlinked citation") {
		t.Fatalf("unlink output = %q", out)
	}
	afterUnlink, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(afterUnlink) != 0 {
		t.Fatalf("unlink must suppress this cited line, got: %+v", afterUnlink.Sections)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	afterResync, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(afterResync) != 0 {
		t.Fatalf("unlink must persist across rebuild/re-sync, got: %+v", afterResync.Sections)
	}
	if out := run(t, "brief", "correct", "--memory-id", ask.ID, "--attendee", "neil@example.com", "--confirm"); !strings.Contains(out, "confirmed citation link") {
		t.Fatalf("confirm output = %q", out)
	}
	afterConfirm, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if meetingBriefLineCount(afterConfirm) == 0 {
		t.Fatalf("confirm should restore this source↔attendee line, got: %+v", afterConfirm.Sections)
	}
}

func TestBriefEventCLIAndMCPReturnSameShape(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"event-42", "Board prep", at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"neil@example.com": "Neil Patel", "adit@example.com": "Me"}, "adit@example.com", "neil@example.com",
	)
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, meetingBriefEmail(
		"ask-42", "Board deck", "Please review the board deck by tomorrow.",
		"neil@example.com", []string{"adit@example.com"}, at.Add(-time.Hour),
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	cliJSON := run(t, "brief", "--event-id", event.ID, "--at", at.Format(time.RFC3339), "--json")
	var cli MeetingBrief
	if err := json.Unmarshal([]byte(cliJSON), &cli); err != nil {
		t.Fatalf("decode CLI meeting brief: %v\n%s", err, cliJSON)
	}
	mcpValue, err := callMCPTool(ctx, "meeting_prep", map[string]any{
		"event_id": event.ID,
		"at":       at.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcp, ok := mcpValue.(MeetingBrief)
	if !ok {
		t.Fatalf("meeting_prep returned %T, want MeetingBrief", mcpValue)
	}
	cliBytes, _ := json.Marshal(cli)
	mcpBytes, _ := json.Marshal(mcp)
	if !bytes.Equal(cliBytes, mcpBytes) {
		t.Fatalf("CLI and MCP shapes differ:\nCLI %s\nMCP %s", cliBytes, mcpBytes)
	}

	human := run(t, "brief", "--event-id", event.ID, "--at", at.Format(time.RFC3339))
	for _, line := range strings.Split(human, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		for _, field := range []string{"memory-id:", "channel:", "source:", "date:"} {
			if !strings.Contains(line, field) {
				t.Fatalf("surfaced line lacks %s citation:\n%s", field, line)
			}
		}
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

func TestMeetingBriefUsesExactAttendeeIdentityNotSharedDisplayName(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	one := meetingBriefEmail(
		"ask-one", "One request", "Can you send the contract by tomorrow?",
		"one@example.com", []string{"me@example.com"}, at.Add(-time.Hour),
	)
	one.Meta["names"] = map[string]string{"one@example.com": "Jordan Lee"}
	two := meetingBriefEmail(
		"ask-two", "Wrong person's request", "Can you send the private deck by tomorrow?",
		"two@example.com", []string{"me@example.com"}, at.Add(-2*time.Hour),
	)
	two.Meta["names"] = map[string]string{"two@example.com": "Jordan Lee"}
	event := eventMemFull(
		"event-jordan", "Contract", at.Add(time.Hour).Format(time.RFC3339),
		map[string]string{"one@example.com": "Jordan Lee", "me@example.com": "Me"}, "me@example.com", "one@example.com",
	)
	event.Text += " Agenda: contract request."
	for _, memory := range []Memory{one, two, event} {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(brief)
	if !strings.Contains(string(payload), "ask-one") {
		t.Fatalf("exact attendee evidence missing: %s", payload)
	}
	if strings.Contains(string(payload), "ask-two") || strings.Contains(string(payload), "private deck") {
		t.Fatalf("same-name wrong-person evidence leaked: %s", payload)
	}
}

func TestMeetingBriefAssemblyHasNoNetworkImports(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "meetingbrief.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "net" || strings.HasPrefix(path, "net/") ||
			path == "github.com/pyranthus-hq/mora/internal/google" ||
			path == "github.com/pyranthus-hq/mora/internal/imessage" {
			t.Fatalf("meeting brief assembly imports %q; inference must remain zero-egress", path)
		}
	}
}
