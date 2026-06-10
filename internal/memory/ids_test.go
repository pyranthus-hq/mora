package memory

import "testing"

func TestStableIDFromProvider(t *testing.T) {
	id1 := StableID(kindGmailThread, "thread-abc")
	id2 := StableID(kindGmailThread, "thread-abc")
	if id1 != id2 {
		t.Fatalf("StableID not deterministic: %s vs %s", id1, id2)
	}
	if id1 == StableID(kindGmailThread, "thread-xyz") {
		t.Fatal("different provider ids must produce different StableIDs")
	}
	if got := StableID(kindGmailThread, "thread-abc"); got != "gmail_thread/thread-abc" {
		t.Fatalf("unexpected id format: %s", got)
	}
}

func TestStableIDIncludesKindAndHandlesEmptyProviderID(t *testing.T) {
	if got := StableID(kindCalEvent, "thread-abc"); got == StableID(kindGmailThread, "thread-abc") {
		t.Fatalf("different kinds must produce different StableIDs, got %q", got)
	}
	if got := StableID(kindCalEvent, ""); got != "calendar_event/" {
		t.Fatalf("unexpected id for empty provider id: %q", got)
	}
}

func TestContentHashChangesWithContent(t *testing.T) {
	a := ContentHash("subject", "body one")
	b := ContentHash("subject", "body two")
	if a == b {
		t.Fatal("content hash must change when body changes")
	}
	if a != ContentHash("subject", "body one") {
		t.Fatal("content hash must be stable for identical content")
	}
}

func TestContentHashReturnsNonEmptyHex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []string
	}{
		{name: "zero parts"},
		{name: "one empty part", parts: []string{""}},
		{name: "one non-empty part", parts: []string{"body"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := ContentHash(tc.parts...)
			if hash == "" {
				t.Fatal("content hash must not be empty")
			}
			for _, r := range hash {
				if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
					t.Fatalf("content hash must be lowercase hex, got %q", hash)
				}
			}
		})
	}
}

func TestSafeFilename(t *testing.T) {
	for _, tc := range []struct {
		stableID string
		want     string
	}{
		{
			stableID: "gmail_thread/thread-abc",
			want:     "gmail_thread_thread-abc",
		},
		{
			stableID: "calendar_event/primary:event 123",
			want:     "calendar_event_primary_event_123",
		},
		{
			stableID: "gmail_thread//thread abc:part",
			want:     "gmail_thread__thread_abc_part",
		},
	} {
		t.Run(tc.stableID, func(t *testing.T) {
			if got := SafeFilename(tc.stableID); got != tc.want {
				t.Fatalf("SafeFilename(%q) = %q, want %q", tc.stableID, got, tc.want)
			}
		})
	}
}
