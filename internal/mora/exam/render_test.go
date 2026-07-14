package exam

import (
	"bytes"
	"testing"
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
