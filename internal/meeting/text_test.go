package meeting

import (
	"strings"
	"testing"
	"time"
)

func TestEvidenceSegmentsDoNotTruncateMidClause(t *testing.T) {
	wrapped := "Please share the Ahrefs findings/report prior to the\nkickoff meeting so we can review it together.\n"
	for _, seg := range EvidenceSegments(wrapped) {
		if strings.HasSuffix(seg, "prior to the") {
			t.Fatalf("cut: %q", seg)
		}
	}
	if joined := strings.Join(EvidenceSegments(wrapped), " | "); !strings.Contains(joined, "kickoff meeting") {
		t.Fatalf("joined=%q", joined)
	}
}
func TestForwardedAndQuotedContentIsNotTheSendersWords(t *testing.T) {
	fwd := "Thanks, take a look when you can.\n---------- Forwarded message ---------\nFrom: Spammy <s@x>\nOpen to see how the loop works?\n"
	got := SenderAuthoredBody(fwd)
	if strings.Contains(got, "loop works") || !strings.Contains(got, "take a look") {
		t.Fatalf("got=%q", got)
	}
	reply := "Yes, Friday works.\nOn Tue, Mar 17, 2026 at 4:12 PM Beth <b@x> wrote:\n> can you send the deck?\n"
	got = SenderAuthoredBody(reply)
	if strings.Contains(got, "send the deck") || !strings.Contains(got, "Friday works") {
		t.Fatalf("got=%q", got)
	}
	sig := "Here is the plan.\n--\nName\nconfidential\n"
	got = SenderAuthoredBody(sig)
	if strings.Contains(got, "confidential") || !strings.Contains(got, "Here is the plan") {
		t.Fatalf("got=%q", got)
	}
}
func TestStripNoiseTokens(t *testing.T) {
	cases := []struct{ in, want string }{{"com/maps/search/California+Theatre%0A345+S+First+St?", ""}, {",+Dublin,+CA+94568?", ""}, {"com/calendar/event?", ""}, {"https://doordash.com/x?code=abc", ""}, {"can you review https://doc.com/x before Friday?", "can you review before Friday?"}, {"click here utm_source=newsletter", "click here"}, {"should we do A/B or C/D testing?", "should we do A/B or C/D testing?"}, {"yes/no?", "yes/no?"}, {"CA/NY?", "CA/NY?"}, {"Can you meet 3/15 or 4/16?", "Can you meet 3/15 or 4/16?"}, {"We agreed to ship the pilot next week.", "We agreed to ship the pilot next week."}}
	for _, tc := range cases {
		if got := StripNoiseTokens(tc.in); got != tc.want {
			t.Errorf("got=%q want=%q", got, tc.want)
		}
	}
}
func TestHistoricalTextAndRelativeAge(t *testing.T) {
	asOf := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct{ at, want string }{{"2026-08-01T10:00:00Z", "Earlier that day"}, {"2026-07-31T12:00:00Z", "~1 day ago"}, {"2026-07-01T12:00:00Z", "~31 days ago"}, {"2026-05-01T12:00:00Z", "~3 months ago"}, {"2023-08-01T12:00:00Z", "~3 years ago"}, {"2026-08-02T12:00:00Z", "Earlier that day"}}
	for _, tc := range cases {
		at, _ := time.Parse(time.RFC3339, tc.at)
		if got := RelativeAge(asOf, at); got != tc.want {
			t.Fatalf("at=%s got=%q want=%q", tc.at, got, tc.want)
		}
	}
	if HistoricalPrefix(asOf, "bad", "a") != "" {
		t.Fatal("bad date")
	}
	got := HistoricalText(asOf, "2026-07-31T12:00:00Z", " Sam ", "  sent   deck ")
	if got != "~1 day ago · Sam — “sent deck”" {
		t.Fatalf("got=%q", got)
	}
	if got := HistoricalPrefix(asOf, "2026-07-31T12:00:00Z", ""); got != "~1 day ago — " {
		t.Fatalf("got=%q", got)
	}
}
func TestEvidenceTextHelpers(t *testing.T) {
	body := "Header:\nA real sentence without ending\ncontinues here.\nhttps://x.com/a?q=b\n"
	segments := EvidenceSegments(body)
	if strings.Contains(strings.Join(segments, " "), "https") || !strings.Contains(strings.Join(segments, " "), "continues here") {
		t.Fatalf("segments=%v", segments)
	}
	if !IsForwardedSubject(" Fwd: x") || !IsForwardedSubject("fw:x") || IsForwardedSubject("re:x") {
		t.Fatal("forward")
	}
	if !IsLeadInFragment("Header:") || !IsLeadInFragment("two words") || IsLeadInFragment("three actual words") {
		t.Fatal("lead")
	}
	if StripSpeakerPrefix("Alex: hello there") != "hello there" || StripSpeakerPrefix("no label") != "no label" {
		t.Fatal("speaker")
	}
	if !ContainsPhrase("we sent it", "sent") || ContainsPhrase("resent it", "sent") || ContainsPhrase("x", "") {
		t.Fatal("phrase")
	}
	if !ContainsPersonalTrivia("favorite food") || ContainsPersonalTrivia("project plan") {
		t.Fatal("trivia")
	}
	if !PersonalTriviaOnly("birthday", []string{"project"}) || PersonalTriviaOnly("birthday project", []string{"project"}) {
		t.Fatal("trivia only")
	}
	if !IsQuotedReplyLine("On Tue, Person wrote:") {
		t.Fatal("quote")
	}
}

func TestTextBoundaryBranches(t *testing.T) {
	asOf := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if got := RelativeAge(asOf, asOf.Add(-60*24*time.Hour)); got != "~2 months ago" {
		t.Fatal(got)
	}
	if got := RelativeAge(asOf, asOf.Add(-730*24*time.Hour)); got != "~2 years ago" {
		t.Fatal(got)
	}
	got := SenderAuthoredBody(`own line
> quoted remnant
still own`)
	if strings.Contains(got, "quoted") || !strings.Contains(got, "still own") {
		t.Fatal(got)
	}
	if !ContainsPhrase("a ? b", "?") || !ContainsPhrase("resent then sent", "sent") {
		t.Fatal("later bounded occurrence")
	}
	if ContinuesSentence("done.", "next") || ContinuesSentence("", "next") || ContinuesSentence("open", "Next") || !ContinuesSentence("open", "next") {
		t.Fatal("continuation")
	}
}
