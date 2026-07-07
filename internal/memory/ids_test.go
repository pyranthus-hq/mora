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

func TestSanitizeWindowsBase(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "safe id untouched", in: "mem_20260101_120000_deadbeef", want: "mem_20260101_120000_deadbeef"},
		{name: "space is legal mid-name", in: "Dinner Friday?", want: "Dinner Friday_"},
		{name: "all reserved chars mapped", in: `a<b>c:d"e/f\g|h?i*j`, want: "a_b_c_d_e_f_g_h_i_j"},
		{name: "control chars mapped", in: "a\x01b\x1fc", want: "a_b_c"},
		{name: "trailing dot and space trimmed", in: "report.  ", want: "report"},
		{name: "only dots and spaces falls back", in: " . . ", want: "_"},
		{name: "empty falls back", in: "", want: "_"},
		{name: "reserved device name prefixed", in: "CON", want: "_CON"},
		{name: "reserved device name case-insensitive", in: "nul", want: "_nul"},
		{name: "reserved device name with extension", in: "COM1.md", want: "_COM1.md"},
		{name: "com0 is not reserved", in: "COM0", want: "COM0"},
		{name: "prnfoo is not reserved", in: "PRNfoo", want: "PRNfoo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeWindowsBase(tc.in); got != tc.want {
				t.Fatalf("SanitizeWindowsBase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeWindowsBaseIsDeterministic(t *testing.T) {
	in := `weird:name?*"<>|`
	if a, b := SanitizeWindowsBase(in), SanitizeWindowsBase(in); a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
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
