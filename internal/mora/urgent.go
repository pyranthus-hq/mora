package mora

import (
	urgencypkg "github.com/pyranthus-hq/mora/internal/urgency"
	"strings"
	"time"
)

const urgentShelfCap = 5

func isUrgent(m Memory, now time.Time) (bool, string) {
	if isWhatsAppInformationalMemory(m) {
		return false, ""
	}
	_, _, starred := urgencypkg.Labels(m)
	return urgencypkg.Qualifies(hasHumanSender(m), itemOccurredAt(m), now, m.Title, m.Text, starred)
}
func hasHumanSender(m Memory) bool {
	_, senders, _, _ := personRefs(m)
	for _, id := range senders {
		if classifyIdentity(strings.TrimPrefix(id, "person:"), "") == "person" {
			return true
		}
	}
	return false
}
func itemOccurredAt(m Memory) time.Time {
	if t, err := time.Parse(time.RFC3339, validFromOf(m)); err == nil {
		return t
	}
	return time.Time{}
}
func urgencyScore(m Memory, phrase string) int { return urgencypkg.Score(m, phrase) }
func urgentSnippet(text string, n int, phrase string) string {
	return urgencypkg.Snippet(text, n, phrase)
}
func stripFromLine(text string) string { return urgencypkg.StripFromLine(text) }
