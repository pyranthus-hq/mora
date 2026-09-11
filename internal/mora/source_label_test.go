package mora

import "testing"

func TestSourceLabel(t *testing.T) {
	for _, tc := range []struct {
		s    Source
		want string
	}{
		{Source{Type: "imessage"}, "Messages"},
		{Source{Type: "applecalendar"}, "Apple Calendar"},
		{Source{Type: "gmail", Account: "a@b"}, "Gmail · a@b"},
		{Source{Type: "calendar", Account: "a@b"}, "Google Calendar · a@b"},
		{Source{Type: "gmail"}, "Gmail"},
		{Source{Type: "calendar"}, "Google Calendar"},
		{Source{Type: "filesystem", Name: "notes"}, "Folder · notes"},
		{Source{Type: "github"}, "GitHub"},
		{Source{Type: "whatsapp"}, "WhatsApp"},
		{Source{Type: "custom", Name: "my source"}, "my source"},
	} {
		if got := sourceLabel(tc.s); got != tc.want {
			t.Errorf("sourceLabel(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}
