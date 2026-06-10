package mora

import (
	"strings"
	"testing"
)

// TestDigestSnippetIsTailBiased locks the digest's clipping direction: a digest
// item's snippet must keep the END of the body, not the start. Conversation
// memories (iMessage chats, gmail threads) append chronologically, so the
// user's own replies live at the tail — a head-clip systematically shows the
// other person's messages and drops the user's answer, and the agent then
// reports a replied-to thread as "unanswered" (the live false-unanswered misread,
// 2026-06-10). The digest is "what's new"; the newest content IS the tail.
func TestDigestSnippetIsTailBiased(t *testing.T) {
	old := strings.Repeat("Them: are you still building your startup? ", 10)
	body := old + "Me: yes — replied June 25, back in the bay permanently"

	m := Memory{ID: "x", Title: "Kai", Text: body, CreatedAt: "2026-06-04T23:10:41Z"}
	it := digestItemFor(Config{}, m, "imessage", "new")

	if !strings.Contains(it.Snippet, "Me: yes — replied June 25") {
		t.Fatalf("snippet must keep the tail (the user's reply), got: %q", it.Snippet)
	}
	if !strings.HasPrefix(it.Snippet, "…") {
		t.Fatalf("a tail-clipped snippet must mark the elision at the START, got: %q", it.Snippet)
	}
	if n := len([]rune(it.Snippet)); n > digestSnippetLen+1 {
		t.Fatalf("snippet exceeds digestSnippetLen+ellipsis: %d runes", n)
	}

	// Short bodies pass through whole — no ellipsis, no clipping.
	short := digestItemFor(Config{}, Memory{ID: "y", Title: "s", Text: "Me: see you at 3pm"}, "imessage", "new")
	if short.Snippet != "Me: see you at 3pm" {
		t.Fatalf("short body must pass through unclipped, got: %q", short.Snippet)
	}
}
