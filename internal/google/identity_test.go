package google

import (
	"testing"

	calendar "google.golang.org/api/calendar/v3"
	gmail "google.golang.org/api/gmail/v1"
)

func hdr(name, val string) *gmail.MessagePartHeader {
	return &gmail.MessagePartHeader{Name: name, Value: val}
}

func metaStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestGmailMetaIdentities proves Gmail emits normalized, lowercased From/To/Cc
// address lists (parsed with net/mail, so quoted display names and lists survive),
// a names map for alias accretion, and occurred_at for valid_from.
func TestGmailMetaIdentities(t *testing.T) {
	th := &gmail.Thread{Id: "t1", Messages: []*gmail.Message{
		{InternalDate: 1700000000000, Payload: &gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
			hdr("Subject", "Re: demo"),
			hdr("From", `"Neil Patel" <Neil@Example.com>`),
			hdr("To", "adit@x.com, Bob <bob@y.com>"),
			hdr("Cc", "carol@z.com"),
		}}},
		{InternalDate: 1700000100000, Payload: &gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
			hdr("From", "adit@x.com"),
			hdr("To", "neil@example.com"),
		}}},
	}}
	it := gmailThreadToItem(th)

	from := metaStrings(it.Meta["from"])
	if !has(from, "neil@example.com") || !has(from, "adit@x.com") {
		t.Fatalf("from = %v, want both senders lowercased", from)
	}
	to := metaStrings(it.Meta["to"])
	if !has(to, "bob@y.com") || !has(to, "adit@x.com") || !has(to, "neil@example.com") {
		t.Fatalf("to = %v", to)
	}
	if cc := metaStrings(it.Meta["cc"]); !has(cc, "carol@z.com") {
		t.Fatalf("cc = %v", cc)
	}
	names, _ := it.Meta["names"].(map[string]string)
	if names["neil@example.com"] != "Neil Patel" || names["bob@y.com"] != "Bob" {
		t.Fatalf("names = %v, want display-name aliases", names)
	}
	if s, _ := it.Meta["occurred_at"].(string); s == "" {
		t.Fatal("occurred_at missing from Gmail Meta")
	}
}

// TestAddrSetMalformedFallback proves that when ParseAddressList fails on a list
// (one bad address), the quote-aware fallback still recovers the valid addresses —
// including a quoted display name that itself contains a comma (codex S3 review).
func TestAddrSetMalformedFallback(t *testing.T) {
	s := newAddrSet()
	s.addHeader(`"Doe, Jane" <jane@example.com>, not-an-addr@@, Bob <bob@y.com>`)
	got := s.list()
	if !has(got, "jane@example.com") || !has(got, "bob@y.com") {
		t.Fatalf("fallback dropped valid addresses: %v", got)
	}
	if s.names["jane@example.com"] != "Doe, Jane" {
		t.Fatalf("quoted name with comma lost: %q", s.names["jane@example.com"])
	}
}

// TestCalendarMetaAttendees proves Calendar emits lowercased attendees + organizer
// and occurred_at, and that a non-recurring event carries NO recurring_event_id key
// (an empty value would be hash material and a pointless meta line — codex S2).
func TestCalendarMetaAttendees(t *testing.T) {
	ev := &calendar.Event{
		Id:      "e1",
		Summary: "Standup",
		Start:   &calendar.EventDateTime{DateTime: "2026-06-04T09:00:00Z"},
		Organizer: &calendar.EventOrganizer{
			Email: "Boss@Corp.com", DisplayName: "The Boss",
		},
		Attendees: []*calendar.EventAttendee{
			{Email: "adit@x.com"},
			{Email: "Neil@Example.com", DisplayName: "Neil Patel"},
		},
	}
	it := calEventToItem("primary", ev)

	att := metaStrings(it.Meta["attendees"])
	if !has(att, "adit@x.com") || !has(att, "neil@example.com") {
		t.Fatalf("attendees = %v", att)
	}
	if org, _ := it.Meta["organizer"].(string); org != "boss@corp.com" {
		t.Fatalf("organizer = %q, want lowercased", org)
	}
	if _, present := it.Meta["recurring_event_id"]; present {
		t.Fatal("non-recurring event must not carry an empty recurring_event_id")
	}
	if s, _ := it.Meta["occurred_at"].(string); s == "" {
		t.Fatal("occurred_at missing from Calendar Meta")
	}

	// A recurring instance keeps the key.
	ev.RecurringEventId = "series1"
	it2 := calEventToItem("primary", ev)
	if rid, _ := it2.Meta["recurring_event_id"].(string); rid != "series1" {
		t.Fatalf("recurring_event_id = %q", rid)
	}
}
