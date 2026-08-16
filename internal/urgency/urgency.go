package urgency

import (
	"strings"

	"github.com/pyranthus-hq/mora/internal/memory"
	"time"
	"unicode"
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
// the matched deadline phrase (for the deadline-anchored snippet; "" when the item
// qualified via a STARRED label instead). Gate order:
//  1. a known-HUMAN sender (a service/no-reply From is never urgent — the spam guard);
//  2. a recent wall-clock arrival (occurred_at within urgentRecencyWindow of now);
//  3. a deadline / time-pressure phrase in the subject or body, OR a user-applied
//     STARRED Gmail label (an explicit "I flagged this" signal — issue #62 defect 2
//     enrichment). UNREAD/IMPORTANT alone do NOT qualify (too noisy as a gate); they
//     only BOOST ordering within the shelf via urgencyScore.

// gmailLabels reads the captured actionability labels (issue #62 defect 2 enrichment)
// off a memory's Meta. Absent on pre-#62 ingests / non-Gmail memories, so every
// caller degrades to the deadline-phrase behavior.

func metaStrings(v any) []string {
	switch value := v.(type) {
	case []string:
		out := make([]string, 0, len(value))
		for _, s := range value {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if value != "" {
			return []string{value}
		}
	}
	return nil
}

func gmailLabels(m memory.Memory) (unread, important, starred bool) {
	if m.Meta == nil {
		return false, false, false
	}
	for _, l := range metaStrings(m.Meta["labels"]) {
		switch l {
		case "UNREAD":
			unread = true
		case "IMPORTANT":
			important = true
		case "STARRED":
			starred = true
		}
	}
	return unread, important, starred
}

// urgencyScore ranks items WITHIN the shelf (never the gate). A user-explicit STARRED
// outweighs Gmail's auto IMPORTANT, which outweighs UNREAD; a matched deadline phrase
// adds on top. Salience and arrival time are the downstream tie-breaks
// (assembleUrgentShelf), keeping salience only a tie-breaker (issue #62).
func urgencyScore(m memory.Memory, phrase string) int {
	unread, important, starred := gmailLabels(m)
	score := 0
	if starred {
		score += 3
	}
	if important {
		score += 2
	}
	if unread {
		score++
	}
	if phrase != "" {
		score += 2
	}
	return score
}

// hasHumanSender reports whether a memory has at least one From/organizer that
// classifies as a real person (not a service/no-reply/bulk-ESP identity). It reuses
// the SAME classifyIdentity floor the salience kernel uses.

// itemOccurredAt is the item's true-in-world instant (occurred_at else created_at),
// parsed; zero time when unparseable.

// withinUrgentRecency reports whether an instant is a recent wall-clock arrival.
func withinUrgentRecency(t, now time.Time) bool {
	return !t.IsZero() && t.After(now.Add(-urgentRecencyWindow))
}

// matchDeadlinePhrase returns the first urgentDeadlinePhrases entry that occurs in the
// normalized subject+body as a whole word-run that is NOT immediately negated, or "".
// Word boundaries stop "urgent" from firing inside "insurgents"; the negation guard
// stops "not urgent" / "no signature required" from landing a reassuring email on the
// shelf.
func matchDeadlinePhrase(title, body string) string {
	// Space-pad so a phrase at the very start/end still has a boundary on both sides.
	norm := " " + strings.ToLower(strings.Join(strings.Fields(title+" "+body), " ")) + " "
	for _, p := range urgentDeadlinePhrases {
		if deadlinePhraseHit(norm, p) {
			return p
		}
	}
	return ""
}

// deadlinePhraseHit reports whether phrase occurs in the space-padded, lower-cased norm
// flanked by non-word bytes and not immediately preceded by a negator.
func deadlinePhraseHit(norm, phrase string) bool {
	for from := 0; ; {
		i := strings.Index(norm[from:], phrase)
		if i < 0 {
			return false
		}
		i += from
		if !isWordByte(norm[i-1]) && !isWordByte(norm[i+len(phrase)]) && !immediatelyNegated(norm[:i]) {
			return true
		}
		from = i + 1
	}
}

func isWordByte(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }

// deadlineNegators are the tokens that, immediately before a deadline phrase, flip its
// meaning ("not urgent", "no rush", "no signature required").
var deadlineNegators = map[string]bool{
	"not": true, "no": true, "never": true, "without": true, "non": true,
	"isn't": true, "aren't": true, "wasn't": true, "won't": true, "don't": true, "doesn't": true,
}

// immediatelyNegated reports whether the token right before the phrase is a negator.
func immediatelyNegated(prefix string) bool {
	prefix = strings.TrimRight(prefix, " ")
	last := prefix
	if j := strings.LastIndexByte(prefix, ' '); j >= 0 {
		last = prefix[j+1:]
	}
	return deadlineNegators[last]
}

// urgentSnippet builds the shelf snippet (defect 4): a window centered on the deadline
// phrase (where the actual ask lives) rather than snippetTail's blind end-clip (which
// systematically showed sign-offs / P.S. lines). With no phrase it leads with the HEAD
// of the body. The leading "From: <header>" envelope line is stripped in full.
func urgentSnippet(text string, n int, phrase string) string {
	if n <= 0 {
		n = 200
	}
	clean := strings.Join(strings.Fields(stripFromLine(text)), " ")
	runes := []rune(clean)
	if phrase != "" {
		if ri := runeIndexFold(runes, phrase); ri >= 0 {
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

// stripFromLine drops a leading "From: <header>" envelope line from a RAW connector body
// (gmail stores "From: <raw From header>\n\n<body>"), using the LINE boundary so a
// multi-word display name ("Jane Smith <jane@…>") is dropped in full. No such line =>
// returned unchanged.
func stripFromLine(text string) string {
	if !strings.HasPrefix(text, "From: ") {
		return text
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return "" // a single-line "From: ..." with no body has nothing to show.
}

// runeIndexFold returns the RUNE index of the first case-insensitive occurrence of
// phrase in runes, or -1. It folds per-rune, so it never suffers the byte-length drift
// a ToLower-then-byte-index would introduce for non-ASCII text before the phrase.
func runeIndexFold(runes []rune, phrase string) int {
	p := []rune(strings.ToLower(phrase))
	if len(p) == 0 {
		return 0
	}
	for i := 0; i+len(p) <= len(runes); i++ {
		ok := true
		for j := range p {
			if unicode.ToLower(runes[i+j]) != p[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func Labels(m memory.Memory) (bool, bool, bool)        { return gmailLabels(m) }
func Score(m memory.Memory, phrase string) int         { return urgencyScore(m, phrase) }
func Within(t, now time.Time) bool                     { return withinUrgentRecency(t, now) }
func MatchDeadline(title, body string) string          { return matchDeadlinePhrase(title, body) }
func Snippet(text string, n int, phrase string) string { return urgentSnippet(text, n, phrase) }
func StripFromLine(text string) string                 { return stripFromLine(text) }

func IsWordByte(b byte) bool { return isWordByte(b) }
