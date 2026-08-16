package mora

import (
	"strings"
	"testing"
	"time"
)

// Regression tests for the "the brief is gibberish" report (2026-07-13). Every
// case below is a VERBATIM line the real brief surfaced, traced to its real
// source email. The brief is the user's unfinished business: a line that is not
// a complete, human-authored sentence stating something the USER must do or must
// not get wrong has no business being in it.

// RC10: when no sentence qualifies, the extractor falls back to the SUBJECT LINE —
// and that path skipped the forward filter, so "Fwd: Google Ads Account Audit &
// Recommendations - Ready for Your Review!" (a forwarded marketing subject) became
// shared context with an attendee. A forwarded subject is a stranger's subject.
func TestForwardedSubjectIsNotEvidence(t *testing.T) {
	cfg := Config{}
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	fwd := Memory{
		Provider: "gmail", Type: "email",
		Title: "Fwd: Google Ads Account Audit & Recommendations - Ready for Your Review!",
		Text:  "---------- Forwarded message ---------\nFrom: ads@vendor.com\n",
		Meta:  map[string]any{"from": []string{"gouri@example.com"}},
	}
	if got := meetingBriefActionableEvidenceText(fwd, cfg, at, meetingBriefSharedContext); got != "" {
		t.Errorf("a forwarded subject must not become evidence; got %q", got)
	}
	// A genuine subject still works as the fallback.
	real := Memory{
		Provider: "gmail", Type: "email",
		Title: "Acme pilot roadmap and launch milestone",
		Text:  "",
		Meta:  map[string]any{"from": []string{"gouri@example.com"}},
	}
	if got := meetingBriefActionableEvidenceText(real, cfg, at, meetingBriefSharedContext); got == "" {
		t.Error("a genuine subject must still be usable as the fallback excerpt")
	}
}

// RC12: an iMessage line the USER spoke is not evidence about the attendee, and the
// speaker prefix must never leak into the rendered line. The real brief rendered
// "Me: Good morning leaving now" as an attendee's staleness guard.
func TestUserSpokenIMessageIsNotAttendeeEvidence(t *testing.T) {
	cfg := Config{}
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	mine := Memory{
		Provider: "imessage", Type: "imessage",
		Title: "Gouri",
		Text:  "Me: Good morning leaving now",
	}
	got := meetingBriefActionableEvidenceText(mine, cfg, at, meetingBriefStaleness)
	if strings.Contains(got, "Me:") {
		t.Errorf("the speaker prefix must never render in a brief line: %q", got)
	}
	if got != "" {
		t.Errorf("the user's own passing remark is not an attendee staleness guard: %q", got)
	}
}
