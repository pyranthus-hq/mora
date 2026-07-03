package mora

import (
	"strings"
	"time"
	"unicode/utf8"
)

// urgent.go — issue #62 defect 2/4: the item-level "Urgent" lane. Ranking (salience.go)
// is relationship-volume × recency and has NO notion of urgency, so a one-off
// deadline email from a known-but-low-volume human sorts below routine threads and
// falls past the per-source cap / byte budget. The shelf is a SEPARATE lane above the
// sections keyed on actionability — deliberately NOT blended into the salience kernel
// (which must stay wall-clock-free and byte-stable); salience is only a tie-break.

const (
	// urgentRecencyWindow gates on WALL-CLOCK arrival: only an item whose occurred_at
	// is within this window of "now" can be urgent, so a months-old thread bumped by a
	// re-sync (or a stale "urgent" newsletter) never lands on the shelf. This is the
	// ONE place wall-clock enters ranking — kept out of salience by design.
	urgentRecencyWindow = 48 * time.Hour
	// urgentShelfCap bounds the shelf so it can never crowd the sections out of the
	// budget; overflow urgent items stay in their source sections.
	urgentShelfCap = 5
	// urgentSnippetLead is how many characters of context precede the deadline phrase
	// in a deadline-anchored snippet.
	urgentSnippetLead = 40
)

// urgentDeadlinePhrases are conservative deadline / time-pressure markers matched
// (case-insensitively) in an item's subject or body. The known-human-sender + recent-
// arrival gates keep marketing "urgent!"/"asap" blasts (service senders) off the
// shelf, so this list can be broad without flooding it. Order is the match priority
// (first hit wins) so detection is deterministic.
var urgentDeadlinePhrases = []string{
	"by end of day", "end of day today", "by eod", "by cob", "close of business",
	"by today", "by tomorrow", "by this afternoon", "by this evening",
	"same-day", "same day",
	"due today", "due tomorrow", "due by", "is due", "due date",
	"deadline", "time-sensitive", "time sensitive", "action required",
	"as soon as possible", "asap", "time critical", "urgently", "urgent",
	"needs your approval", "need your approval", "needs your sign-off", "sign-off",
	"needs your signature", "signature required", "please sign", "sign by",
	"respond by", "reply by", "confirm by", "rsvp by", "get back to me by",
	"expires today", "expires tomorrow", "final notice", "last chance",
	"immediately", "right away",
}

// isUrgent reports whether a surfaced memory belongs on the Urgent shelf and, if so,
// the matched deadline phrase (for the deadline-anchored snippet). Gate order:
//  1. a known-HUMAN sender (a service/no-reply From is never urgent — the spam guard);
//  2. a recent wall-clock arrival (occurred_at within urgentRecencyWindow of now);
//  3. a deadline / time-pressure phrase in the subject or body.
func isUrgent(m Memory, now time.Time) (bool, string) {
	if !hasHumanSender(m) {
		return false, ""
	}
	if !withinUrgentRecency(itemOccurredAt(m), now) {
		return false, ""
	}
	phrase := matchDeadlinePhrase(m.Title, m.Text)
	if phrase == "" {
		return false, ""
	}
	return true, phrase
}

// hasHumanSender reports whether a memory has at least one From/organizer that
// classifies as a real person (not a service/no-reply/bulk-ESP identity). It reuses
// the SAME classifyIdentity floor the salience kernel uses.
func hasHumanSender(m Memory) bool {
	_, senders, _, _ := personRefs(m)
	for _, id := range senders {
		if classifyIdentity(strings.TrimPrefix(id, "person:"), "") == "person" {
			return true
		}
	}
	return false
}

// itemOccurredAt is the item's true-in-world instant (occurred_at else created_at),
// parsed; zero time when unparseable.
func itemOccurredAt(m Memory) time.Time {
	if t, err := time.Parse(time.RFC3339, validFromOf(m)); err == nil {
		return t
	}
	return time.Time{}
}

// withinUrgentRecency reports whether an instant is a recent wall-clock arrival.
func withinUrgentRecency(t, now time.Time) bool {
	return !t.IsZero() && t.After(now.Add(-urgentRecencyWindow))
}

// matchDeadlinePhrase returns the first urgentDeadlinePhrases entry found in the
// normalized (lower-cased, whitespace-collapsed) subject+body, or "".
func matchDeadlinePhrase(title, body string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(title+" "+body), " "))
	for _, p := range urgentDeadlinePhrases {
		if strings.Contains(norm, p) {
			return p
		}
	}
	return ""
}

// urgentSnippet builds the shelf snippet (defect 4): a window centered on the deadline
// phrase (where the actual ask lives) rather than snippetTail's blind end-clip (which
// systematically showed sign-offs / P.S. lines). With no phrase it leads with the
// HEAD of the body. The leading "From: <addr>" envelope line is stripped.
func urgentSnippet(text string, n int, phrase string) string {
	if n <= 0 {
		n = digestSnippetLen
	}
	clean := stripFromPrefix(strings.Join(strings.Fields(text), " "))
	runes := []rune(clean)
	if phrase != "" {
		if bi := strings.Index(strings.ToLower(clean), strings.ToLower(phrase)); bi >= 0 {
			ri := len([]rune(clean[:snapRuneBoundary(clean, bi)]))
			start := ri - urgentSnippetLead
			if start < 0 {
				start = 0
			}
			end := start + n
			if end > len(runes) {
				end = len(runes)
			}
			seg := strings.TrimSpace(string(runes[start:end]))
			if start > 0 {
				seg = "…" + seg
			}
			if end < len(runes) {
				seg += "…"
			}
			return seg
		}
	}
	if len(runes) <= n {
		return clean
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}

// stripFromPrefix drops a leading "From: <addr> " envelope line (gmail bodies are
// stored as "From: <from>\n\n<body>"), so a head snippet leads with the body's ask.
func stripFromPrefix(s string) string {
	const p = "From: "
	if !strings.HasPrefix(s, p) {
		return s
	}
	rest := s[len(p):]
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		return strings.TrimSpace(rest[i+1:])
	}
	return ""
}

// snapRuneBoundary clamps a byte index to the nearest rune-start at or below it, so a
// byte offset from a case-folded search never splits a multibyte rune.
func snapRuneBoundary(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
