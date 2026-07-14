package exam

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRenderDeterministic(t *testing.T) {
	l := validTestLedger()
	a, err := Render(l)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(l.Artifacts) || len(b) != len(a) {
		t.Fatalf("rendered files = %d and %d, want %d", len(a), len(b), len(l.Artifacts))
	}
	for path, bodyA := range a {
		bodyB, ok := b[path]
		if !ok || !bytes.Equal(bodyA, bodyB) {
			t.Fatalf("nondeterministic render at %q", path)
		}
	}
	for _, path := range []string{
		"vault/sources/gmail/gmail_thread_exam-thread.md",
		"vault/sources/imessage/imessage_chat_exam-chat.md",
		"vault/sources/calendar/calendar_event_exam-test.md",
		"vault/memories/exam/exam-note-closure.md",
	} {
		if _, ok := a[path]; !ok {
			t.Errorf("missing rendered channel path %q", path)
		}
	}
}

func TestRenderUsesPinnedTimezone(t *testing.T) {
	originalLocal := time.Local
	t.Cleanup(func() { time.Local = originalLocal })
	time.Local = time.FixedZone("exam+9", 9*60*60)

	l := validTestLedger()
	l.Artifacts[1].Messages[0].At = "2026-07-13T23:30:00Z"
	l.Artifacts[1].Messages[1].At = "2026-07-13T23:45:00Z"
	l.Artifacts[1].OccurredAt = "2026-07-13T23:45:00Z"

	files, err := Render(l)
	if err != nil {
		t.Fatal(err)
	}
	body := string(files["vault/sources/imessage/imessage_chat_exam-chat.md"])
	if !strings.Contains(body, "## 2026-07-13\n") {
		t.Fatalf("iMessage day was not rendered in the pinned timezone:\n%s", body)
	}
}
