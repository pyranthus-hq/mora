package mora

import (
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"strings"
	"testing"
	"time"
)

// These four tests close the dated #139 holes in
// internal/mora/eval/obligations-v1/mutation-matrix.md Matrix 1: each of
// isForwardedSubject, isLeadInFragment, gmailActionableAsk, and unwrapHardWraps
// had only a synthetic consequence control (TestScorerRedTeam's
// s_gate_disable_sweep), which scores hand-built exam.Prediction values and
// never touches meetingbrief.go. Each test below drives the real
// buildEventMeetingBrief production path and is designed so the named gate is
// the SOLE thing standing between the current (correct) rendered output and a
// wrong one.

// TestExamForwardedSubjectNeverBecomesEvidence pins isForwardedSubject: a
// forwarded mail's body is entirely quoted (senderAuthoredBody drops it to
// ""), so the only candidate evidence left is the subject line itself. A
// forwarded subject is a stranger's words, never the sender's — surfacing it
// as the attendee's ask would be wrong-person evidence.
func TestExamForwardedSubjectNeverBecomesEvidence(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}

	event := eventMemFull(
		"calendar_event/jordan-sync",
		"Jordan sync",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@example.com":   "Adit",
			"jordan@example.com": "Jordan Diaz",
		},
		"adit@example.com", "jordan@example.com",
	)
	event.Source = "calendar_event/jordan-sync"
	event.Provider = "google"
	event.ProviderID = "calendar_event/jordan-sync"
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}

	// The subject alone reads as a direct request ("can you send"), but the
	// body is entirely a quoted forward — the sender wrote none of it.
	fixture := meetingBriefEmail(
		"gmail_thread/jordan-fwd",
		"Fwd: Can you send the updated pricing deck",
		"---------- Forwarded message ----------\nHi Adit, sharing this along.\nThanks!",
		"jordan@example.com",
		[]string{"adit@example.com"},
		at.Add(-24*time.Hour),
	)
	if err := writeMemory(cfg, fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range brief.Sections {
		for _, line := range sec.Lines {
			if line.Citation.MemoryID() == "gmail_thread/jordan-fwd" {
				t.Errorf("forwarded subject surfaced as evidence: %q", line.Text)
			}
		}
	}
}

// TestExamLeadInFragmentNeverBecomesEvidence pins isLeadInFragment: a body
// sentence that merely ANNOUNCES content ("...here are the next steps and
// deliverables:") and ends in a colon points at a list the sentence splitter
// already discarded. It must never stand alone as a rendered brief line.
func TestExamLeadInFragmentNeverBecomesEvidence(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}

	event := eventMemFull(
		"calendar_event/jordan-leadin",
		"Jordan sync",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@example.com":   "Adit",
			"jordan@example.com": "Jordan Diaz",
		},
		"adit@example.com", "jordan@example.com",
	)
	event.Source = "calendar_event/jordan-leadin"
	event.Provider = "google"
	event.ProviderID = "calendar_event/jordan-leadin"
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}

	// Empty title so the title fallback short-circuits on title=="" regardless
	// of isLeadInFragment, isolating the gate to the body-segment path alone.
	// The body contains "next steps" (an unresolvedThreadPhrases match) so the
	// memory classifies as unfinished business either way — the only question
	// is whether this lead-in sentence itself is allowed to stand as the line.
	fixture := meetingBriefEmail(
		"gmail_thread/jordan-leadin",
		"",
		"Based on our conversation, here are the next steps and deliverables:",
		"jordan@example.com",
		[]string{"adit@example.com"},
		at.Add(-24*time.Hour),
	)
	if err := writeMemory(cfg, fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range brief.Sections {
		for _, line := range sec.Lines {
			if line.Citation.MemoryID() == "gmail_thread/jordan-leadin" {
				t.Errorf("lead-in fragment surfaced as a standalone brief line: %q", line.Text)
			}
		}
	}
}

// TestExamGmailBareQuestionNeedsRealInterrogative pins gmailActionableAsk: a
// Gmail "?" counts as an open loop only when it carries a genuine
// interrogative opener or a direct request. A bare question with neither
// ("Does this schedule work for you?") must not surface at all.
func TestExamGmailBareQuestionNeedsRealInterrogative(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}

	event := eventMemFull(
		"calendar_event/jordan-bareq",
		"Jordan sync",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@example.com":   "Adit",
			"jordan@example.com": "Jordan Diaz",
		},
		"adit@example.com", "jordan@example.com",
	)
	event.Source = "calendar_event/jordan-bareq"
	event.Provider = "google"
	event.ProviderID = "calendar_event/jordan-bareq"
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}

	fixture := meetingBriefEmail(
		"gmail_thread/jordan-bareq",
		"Notes",
		"Does this schedule work for you?",
		"jordan@example.com",
		[]string{"adit@example.com"},
		at.Add(-24*time.Hour),
	)
	if err := writeMemory(cfg, fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range brief.Sections {
		for _, line := range sec.Lines {
			if line.Citation.MemoryID() == "gmail_thread/jordan-bareq" {
				t.Errorf("bare question with no interrogative opener or direct request surfaced as an open loop: %q", line.Text)
			}
		}
	}
}

// TestExamHardWrapJoinsBeforeSegmenting pins unwrapHardWraps: Gmail plain text
// hard-wraps at a fixed column, so "Please share the Ahrefs findings prior to
// the\nkickoff meeting." is one sentence, not two. Without the unwrap, the
// segmenter's own '\n' flush truncates the excerpt at "...prior to the",
// silently dropping the one clause ("kickoff meeting") that says what the ask
// is about.
func TestExamHardWrapJoinsBeforeSegmenting(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: at.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}

	event := eventMemFull(
		"calendar_event/jordan-wrap",
		"Jordan sync",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{
			"adit@example.com":   "Adit",
			"jordan@example.com": "Jordan Diaz",
		},
		"adit@example.com", "jordan@example.com",
	)
	event.Source = "calendar_event/jordan-wrap"
	event.Provider = "google"
	event.ProviderID = "calendar_event/jordan-wrap"
	event.Text += " Agenda: Ahrefs findings before the kickoff meeting."
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}

	fixture := meetingBriefEmail(
		"gmail_thread/jordan-wrap",
		"Notes",
		"Please share the Ahrefs findings prior to the\nkickoff meeting.",
		"jordan@example.com",
		[]string{"adit@example.com"},
		at.Add(-24*time.Hour),
	)
	if err := writeMemory(cfg, fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, sec := range brief.Sections {
		for _, line := range sec.Lines {
			if line.Citation.MemoryID() != "gmail_thread/jordan-wrap" {
				continue
			}
			found = true
			if !strings.Contains(line.Text, "kickoff meeting") {
				t.Errorf("hard-wrapped sentence was truncated before segmenting: %q", line.Text)
			}
		}
	}
	if !found {
		t.Fatal("expected the hard-wrapped ask to surface as a cited open loop")
	}
}
