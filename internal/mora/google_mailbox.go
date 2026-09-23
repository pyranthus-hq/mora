package mora

import (
	"sort"
	"strconv"
	"strings"
)

// A source owns the mailbox and the fetch selection it actually reads. Labels
// are order independent; display labels and calendar settings do not change a
// Gmail fetch. The first enabled claimant keeps running if old registry data
// contains duplicates, so the guard never disables both sides.
func gmailFetchSelection(s Source) string {
	labels := append([]string(nil), s.LabelIDs...)
	sort.Strings(labels)
	days := s.SinceDays
	if days == 0 {
		days = 90
	}
	return strings.Join(labels, "\x00") + "\x01" + strconv.Itoa(days)
}

func gmailMailboxOwner(sources []Source, this Source, email string) (Source, bool) {
	email = accountEmail(email)
	if email == "" {
		return Source{}, false
	}
	selection := gmailFetchSelection(this)
	for _, other := range sources {
		if other.Type != "gmail" || !other.IsEnabled() || accountEmail(other.Email) != email || gmailFetchSelection(other) != selection {
			continue
		}
		if other.Name == this.Name {
			return Source{}, false
		}
		return other, true
	}
	return Source{}, false
}
