package mora

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The head-clip bug: a query term deep in a long memory was found by FTS but
// invisible in the 240-char preview, so the answer looked unsupported. The
// snippet must center on the earliest query-term match instead.
func TestMatchSnippetCentersOnDeepMatch(t *testing.T) {
	filler := strings.Repeat("morning standup notes and assorted scheduling chatter ", 30)
	text := filler + "Dan said to wear polos for the party on Saturday. " + filler
	got := matchSnippet(text, "what did Dan say about polos", 240)

	if !strings.Contains(strings.ToLower(got), "polos") {
		t.Fatalf("snippet does not show the matched term:\n%q", got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("a mid-text window should open with an ellipsis, got:\n%q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a mid-text window should close with an ellipsis, got:\n%q", got)
	}
	if n := utf8.RuneCountInString(got); n > 240+2 {
		t.Fatalf("window blew the size budget: %d runes", n)
	}
	if got2 := matchSnippet(text, "what did Dan say about polos", 240); got2 != got {
		t.Fatalf("matchSnippet is not deterministic:\n%q\nvs\n%q", got, got2)
	}
}

// No body match (title/tag hit, or vocabulary mismatch) → identical to the
// head-clip snippet(): the opening is the honest preview when we can't center.
func TestMatchSnippetNoMatchFallsBackToHead(t *testing.T) {
	text := strings.Repeat("alpha beta gamma delta epsilon ", 40)
	want := snippet(text, 100)
	got := matchSnippet(text, "completely unrelated zebra", 100)
	if got != want {
		t.Fatalf("no-match case should equal snippet():\ngot  %q\nwant %q", got, want)
	}
}

// A stopword-only query carries no usable term — fall back to the head.
func TestMatchSnippetStopwordOnlyQuery(t *testing.T) {
	text := strings.Repeat("what did the meeting cover today ", 40)
	want := snippet(text, 80)
	if got := matchSnippet(text, "what did the", 80); got != want {
		t.Fatalf("stopword-only query should equal snippet():\ngot  %q\nwant %q", got, want)
	}
}

// A match already inside the head window keeps the plain head clip (no
// leading ellipsis) — same bytes a head reader would expect.
func TestMatchSnippetHeadMatchKeepsHead(t *testing.T) {
	text := "The invoice for Sam is due Friday. " + strings.Repeat("further unrelated detail ", 40)
	got := matchSnippet(text, "Sam invoice", 120)
	if strings.HasPrefix(got, "…") {
		t.Fatalf("head-window match must not open with an ellipsis: %q", got)
	}
	if !strings.Contains(got, "invoice") {
		t.Fatalf("head window should contain the term: %q", got)
	}
}

// A match at the very end clamps the window to the tail without overrunning.
func TestMatchSnippetEndMatchClamps(t *testing.T) {
	text := strings.Repeat("unrelated preamble text ", 40) + "final agreement: pricing locked at launch"
	got := matchSnippet(text, "pricing locked", 120)
	if !strings.Contains(got, "pricing locked") {
		t.Fatalf("tail window should contain the term: %q", got)
	}
	if strings.HasSuffix(got, "……") || !strings.HasPrefix(got, "…") {
		t.Fatalf("tail window shape wrong: %q", got)
	}
	if strings.HasSuffix(got, "…") {
		t.Fatalf("window reaching the end of text must not close with an ellipsis: %q", got)
	}
}

// Word boundaries: "dan" must not center on "abundant"; punctuation and case
// on the query side are normalized away.
func TestMatchSnippetWordBoundaryAndCase(t *testing.T) {
	text := strings.Repeat("abundant filler words here ", 30) + "then Dan replied yes. " + strings.Repeat("tail ", 30)
	got := matchSnippet(text, "Dan?", 100)
	if !strings.Contains(got, "Dan replied") {
		t.Fatalf("should center on the word Dan, not a substring inside abundant: %q", got)
	}
}

// Rune safety: multibyte text around the match must not be split into invalid
// UTF-8 (the byte-clean invariant extends to machine surfaces).
func TestMatchSnippetRuneSafe(t *testing.T) {
	text := strings.Repeat("🌊 समुद्र की लहरें और संदर्भ ", 30) + " budget approved 預算 " + strings.Repeat("🌊 और भी पाठ ", 30)
	got := matchSnippet(text, "budget", 120)
	if !utf8.ValidString(got) {
		t.Fatalf("snippet produced invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "budget") {
		t.Fatalf("snippet lost the match in multibyte text: %q", got)
	}
}

// Short text comes back whole, no ellipses.
func TestMatchSnippetShortText(t *testing.T) {
	if got := matchSnippet("short and sweet", "sweet", 100); got != "short and sweet" {
		t.Fatalf("short text should be returned whole: %q", got)
	}
}

// The MCP search surface: snippetMemories must center each preview on the
// query, flag the clip, and still drop Meta.
func TestSnippetMemoriesCentersOnQuery(t *testing.T) {
	long := strings.Repeat("weekly sync chatter about roadmaps ", 30) + "decision: polos confirmed for the party " + strings.Repeat("post-meeting notes ", 30)
	mems := []Memory{
		{ID: "m1", Text: long, Meta: map[string]any{"k": "v"}},
		{ID: "m2", Text: "tiny body"},
	}
	out := snippetMemories(mems, "polos party")
	if !strings.Contains(out[0].Text, "polos") {
		t.Fatalf("search preview hides the match again: %q", out[0].Text)
	}
	if !out[0].Truncated {
		t.Fatal("clipped preview must set Truncated")
	}
	if out[0].Meta != nil {
		t.Fatal("snippetMemories must still drop Meta")
	}
	if out[1].Text != "tiny body" || out[1].Truncated {
		t.Fatalf("short body must pass through untouched: %+v", out[1])
	}
}
