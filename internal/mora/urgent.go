package mora

import (
	"encoding/json"

	"github.com/pyranthus-hq/mora/internal/evidencetext"
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
	if !starred && isBulkMail(m) {
		return false, ""
	}
	title, body := urgentText(m)
	return urgencypkg.Qualifies(hasHumanSender(m), itemOccurredAt(m), now, title, body, starred)
}

type gmailRow struct {
	Headers []string `json:"automation_headers"`
}

// gmailRows returns the per-message rows of a Gmail memory's Meta "messages".
func gmailRows(m Memory) []gmailRow {
	if m.Meta == nil {
		return nil
	}
	b, err := json.Marshal(m.Meta["messages"])
	if err != nil {
		return nil
	}
	var rows []gmailRow
	if json.Unmarshal(b, &rows) != nil {
		return nil
	}
	return rows
}

// isBulkMail: at least one message row and every row carries automation headers
// (list-unsubscribe etc). One human reply in the thread makes it not bulk.
func isBulkMail(m Memory) bool {
	rows := gmailRows(m)
	for _, r := range rows {
		if len(r.Headers) == 0 {
			return false
		}
	}
	return len(rows) > 0
}

// urgentText returns the title and body that phrase-based urgency may read: the
// newest message's own text with quoted and forwarded blocks removed (the same
// stripper the commitment extractor uses). The thread title counts only when the
// thread is a single message, since only then it is the newest message's subject.
// A memory with no per-message structure keeps the whole text (quotes still
// stripped) and its title, as before.
func urgentText(m Memory) (title, body string) {
	text, title := m.Text, m.Title
	if n := len(gmailRows(m)); n > 0 {
		if n > 1 {
			title = ""
		}
		parts := strings.Split(text, "\n\n---\n\n")
		text = parts[len(parts)-1]
	}
	return title, evidencetext.SenderAuthoredBody(urgencypkg.StripFromLine(text))
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
