package mora

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Wrong-person attribution is a severity-1 defect (FMB doctrine: it is a release
// blocker, never a ranking nit). These pin the two ways the meeting brief could
// attribute a cited line to a human it does not belong to:
//
//  1. SELF-AS-ATTENDEE — the user's own calendar identity is an alias that
//     sources.json has never seen (Google account adit@example.com, but the
//     calendar invites adit@adisam.com), so the user fails self-exclusion, becomes
//     an "attendee" of his own meeting, and his own records are cited back to him
//     as the counterparty's unfinished business.
//
//  2. MENTION-AS-OBLIGATION — the gazetteer emits a MENTIONS edge whenever a
//     person's name appears in a memory's body text and they are NOT a participant
//     (graph.go). Pooling that edge's evidence into the brief renders "someone else
//     wrote this person's name in a note" as "you owe this person a reply".

// writeConfigKeyForTest appends a raw key/value line to config.toml.
func writeConfigKeyForTest(t *testing.T, cfg Config, line string) {
	t.Helper()
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, []byte("\n"+line+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSelfEmailsIncludesConfiguredAliases pins the config seam: a user whose
// calendar identity differs from the mailbox that Google OAuth was granted on
// must be able to declare the alias, and every declared alias counts as self.
func TestSelfEmailsIncludesConfiguredAliases(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	writeConfigKeyForTest(t, cfg, `self_emails = "adit@adisam.com, Adit@Other.COM"`)

	reloaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	self := selfEmails(reloaded)
	for _, want := range []string{"adit@example.com", "adit@adisam.com", "adit@other.com"} {
		if !self[want] {
			t.Errorf("selfEmails missing %q; got %v", want, self)
		}
	}
}

// TestMeetingBriefDoesNotAttributeTheUserToTheirOwnMeeting is the severity-1
// regression. Reproduced live on the real vault: the "Dan - Adit sync up" event
// came back with attendees ["Adit Karode", "Daniel Rachev"] and 6 of 9 cited
// lines attributed to Adit — his own iMessages surfaced as unfinished business
// with Dan.
func TestMeetingBriefDoesNotAttributeTheUserToTheirOwnMeeting(t *testing.T) {
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
	// The user's calendar identity is an alias the Google source has never seen.
	writeConfigKeyForTest(t, cfg, `self_emails = "adit@adisam.com"`)
	cfg = mustConfig(t)

	event := eventMemFull(
		"calendar_event/dan-sync",
		"Dan - Adit sync up",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@adisam.com": "Adit Karode",
			"dan@example.com": "Daniel Rachev",
		},
		"adit@adisam.com", "dan@example.com",
	)
	event.Source = "calendar_event/dan-sync"
	event.Provider = "google"
	event.ProviderID = "calendar_event/dan-sync"
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}

	// A genuine open loop from Dan, and a record of the USER's own that must never
	// be cited back as Dan's unfinished business.
	fixtures := []Memory{
		meetingBriefEmail(
			"gmail_thread/dan-ask",
			"Contract redlines",
			"Can you send the redlined contract before Friday?",
			"dan@example.com",
			[]string{"adit@adisam.com"},
			at.Add(-24*time.Hour),
		),
		meetingBriefEmail(
			"gmail_thread/self-note",
			"Cell reminder",
			"I can call your cell at 8:30am if that works?",
			"adit@adisam.com",
			[]string{"someone-else@example.com"},
			at.Add(-72*time.Hour),
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

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}

	for _, a := range brief.Event.Attendees {
		if strings.Contains(strings.ToLower(a), "adit") {
			t.Errorf("wrong-person: the user is listed as an attendee of their own meeting: %v", brief.Event.Attendees)
		}
	}
	for _, sec := range brief.Sections {
		for _, line := range sec.Lines {
			if strings.Contains(strings.ToLower(line.Text), "adit karode") {
				t.Errorf("wrong-person: line attributed to the user themselves: %q", line.Text)
			}
			if line.Citation.MemoryID() == "gmail_thread/self-note" {
				t.Errorf("wrong-person: the user's own record cited as an attendee's evidence: %q", line.Text)
			}
		}
	}
}

// TestMeetingBriefRejectsMentionOnlyEvidenceAsObligation pins the second
// severity-1 class. graphGetEntity pools EVERY edge rel (MENTIONS included) into
// one evidence list, and the brief uses that list as its whole candidate pool. So
// a third party merely naming the attendee in a note became "your unfinished
// business with them" — the root cause of the mis-attributed-memory residual.
func TestMeetingBriefRejectsMentionOnlyEvidenceAsObligation(t *testing.T) {
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
		"calendar_event/neil-sync",
		"Neil sync",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@example.com": "Adit",
			"neil@example.com": "Neil Patel",
		},
		"adit@example.com", "neil@example.com",
	)
	event.Source = "calendar_event/neil-sync"
	event.Provider = "google"
	event.ProviderID = "calendar_event/neil-sync"
	event.Text += " Agenda: revised deck."
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}

	fixtures := []Memory{
		// Neil is a real participant: a genuine obligation the user owes HIM.
		meetingBriefEmail(
			"gmail_thread/neil-real",
			"Revised deck",
			"Can you send the revised deck by tomorrow?",
			"neil@example.com",
			[]string{"adit@example.com"},
			at.Add(-24*time.Hour),
		),
		// An AUTHORED/synthesized memory that merely names Neil in its body. It has
		// no participants at all, so the gazetteer emits MENTIONS -> person:neil.
		// PR #119's sender gate is scoped to isGmailMemory, so a non-Gmail memory
		// slips past it and gets attributed to Neil as HIS unfinished business.
		{
			ID:          "note/pilot-contract",
			Scope:       "project:acme",
			Type:        "note",
			Title:       "Pilot contract",
			Text:        "I spoke to Neil Patel about the pilot; can you follow up on the contract?",
			Source:      "authored",
			CreatedAt:   at.Add(-48 * time.Hour).UTC().Format(time.RFC3339),
			ContentHash: "hash-note-pilot-contract",
			Meta: map[string]any{
				"occurred_at": at.Add(-48 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}
	for _, m := range fixtures {
		if err := writeMemory(cfg, m); err != nil {
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

	var citedReal bool
	for _, sec := range brief.Sections {
		for _, line := range sec.Lines {
			switch line.Citation.MemoryID() {
			case "note/pilot-contract":
				t.Errorf("wrong-person: a MENTIONS-only memory surfaced as an obligation to the attendee: %q", line.Text)
			case "gmail_thread/neil-real":
				citedReal = true
			}
		}
	}
	if !citedReal {
		t.Fatal("guard is over-broad: the genuine participant-backed obligation was dropped too")
	}
}

// TestMeetingBriefExcludesSelfFromEventSelfEmail proves the ZERO-CONFIG path: with
// no self_emails declared and a Google mailbox that does not match the calendar
// alias, the event's own self_email (Google's Attendee.Self) is enough to keep the
// user out of their own attendee list. A new user must not have to know a config key
// to avoid a wrong-person brief.
func TestMeetingBriefExcludesSelfFromEventSelfEmail(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"calendar_event/dan-sync",
		"Dan - Adit sync up",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@adisam.com": "Adit Karode",
			"dan@example.com": "Daniel Rachev",
		},
		"adit@adisam.com", "dan@example.com",
	)
	event.Meta["self_email"] = "adit@adisam.com" // what the Google connector now records

	got := meetingBriefAttendees(event, selfEmails(cfg))
	if len(got) != 1 || got[0].identity != "dan@example.com" {
		t.Fatalf("attendees = %+v, want only dan@example.com (the user must be excluded via self_email)", got)
	}
}

// TestMeetingBriefGapsWhenSelfIsNotAmongAttendees closes the hole in the fail-closed
// guard, WITHOUT taking the brief down. Today assembly refuses only when NO self
// address is known at all. The dangerous case is subtler: self IS known and simply
// matches no attendee -- the user was invited under an alias -- so Mora cannot tell
// which invitee is the user and silently admits them as their own counterparty.
//
// The response must be refuse-to-GAP, not refuse-to-error: any invitee could BE the
// user, so attribute NOTHING, but still render the (cited) event and say exactly how
// to fix it. Erroring buys no extra safety -- zero lines are emitted either way --
// and would take down the whole next-meeting brief for one unresolvable event. The
// user's real Apple Calendar has 20 such events ("School Drop Off", "Dentist
// Apoointment") where he appears under an alias Mora has never seen.
func TestMeetingBriefGapsWhenSelfIsNotAmongAttendees(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

	// Self is known (the Google mailbox) but the invite used an alias Mora has never
	// seen, and the event predates the connector's self_email capture.
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	event := eventMemFull(
		"calendar_event/alias-sync",
		"Dan - Adit sync up",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@adisam.com": "Adit Karode",
			"dan@example.com": "Daniel Rachev",
		},
		"adit@adisam.com", "dan@example.com",
	)
	event.Source = "calendar_event/alias-sync"
	event.Provider = "google"
	event.ProviderID = "calendar_event/alias-sync"
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}
	// A real ask that WOULD be attributed if self were resolvable.
	if err := writeMemory(cfg, meetingBriefEmail(
		"gmail_thread/dan-ask", "Contract redlines",
		"Can you send the redlined contract before Friday?",
		"dan@example.com", []string{"adit@adisam.com"}, at.Add(-24*time.Hour),
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatalf("assembly must GAP, not error: one unresolvable event must not take down the brief: %v", err)
	}
	if !brief.SelfUnresolved {
		t.Error("SelfUnresolved must be set when no invitee matches a known self address")
	}
	if brief.Event == nil || brief.Event.Citation.MemoryID() != event.ID {
		t.Error("the artifact must survive: the cited event must still render")
	}
	// Attribute NOTHING: any of these addresses could be the user.
	for _, sec := range brief.Sections {
		if len(sec.Lines) > 0 {
			t.Errorf("wrong-person: attributed %d line(s) while self is unresolved: %q", len(sec.Lines), sec.Lines[0].Text)
		}
	}
	joined := strings.Join(brief.Gaps, " ")
	if !strings.Contains(joined, "self_emails") {
		t.Errorf("the gap must tell the user how to fix it (self_emails); got %v", brief.Gaps)
	}
}
