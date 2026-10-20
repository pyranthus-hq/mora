package mora

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// The actionable-service lane closes a specific hole: a service sender can hand
// the owner a REAL dated task ("Complete before September 23, 2026" from a
// background-check portal) that the digest deletes before anything gets to judge
// it. The artifact is service-only, so commitment extraction returns nothing
// (commitmentclassify.Classify short-circuits on ServiceOnly), and
// filterDigestInstancesByCommitments then drops the memory BEFORE urgency,
// ranking, and caps — so Today never sees it.
//
// The lane is the narrow fix. It admits such an artifact into assembly WITHOUT
// minting a synthetic commitment, because a portal task closes in a browser, not
// in a reply: a machine-counterparty obligation would sit open for the whole
// seven-day window with no reciprocal quote that could ever close it. It also
// leaves urgent.go strictly human-only; service mail lands as needs_attention on
// Today, never on the Urgent shelf.
//
// Admission is a CONJUNCTION, and each clause exists to kill a named leak:
//
//	service sender                   — human mail is already served by urgency
//	AND not bulk                     — List-Unsubscribe / Precedence / footer
//	AND no commercial tokens         — cart recovery, dunning, promo drip
//	AND an ABSOLUTE calendar date    — not "6 days left", not "before 9:00 PM"
//	    inside the next seven days
//	AND a recipient imperative in    — "complete before <date>", never a passive
//	    the same sentence as that        "service is paused" or a date that just
//	    date                             happens to appear in the body
//
// Read state is deliberately NOT a clause. UNREAD dies the moment the owner
// glances at the mail on a phone, and Gmail's auto-IMPORTANT fires on most
// transactional mail, so either one would be both leaky and lossy. An unresolved
// portal task stays visible until its deadline passes out of the window.
//
// Known, accepted misses: a high-stakes notice sent through a shared ESP that
// injects List-Unsubscribe (some DocuSign/Carta mail) is dropped by the bulk
// clause (a campaign footer, not a bare "unsubscribe"), and a purely relative
// deadline ("submit within 3 days") never anchors.
// Both are precision-first choices — a false Today card costs more than a miss
// the owner still has in the inbox.
const laneActionableService = "actionable_service"

// actionableServiceWindow is how far ahead a deadline may sit and still be
// today's problem. It mirrors commitmentDailyWindow so the lane and the
// commitment surface agree on what "soon" means.
const actionableServiceWindow = commitmentDailyWindow

// isActionableServiceMail reports whether a service-authored artifact carries a
// dated, recipient-actionable directive. Deterministic: regexp/strings/time only,
// no model call, and every time comparison runs against the injected now.
func isActionableServiceMail(m Memory, now time.Time) bool {
	if m.DeletedAt != "" || m.Provider != "gmail" {
		return false
	}
	if !memoryIsServiceOnly(m) {
		return false
	}
	blob := strings.ToLower(m.Title + "\n" + stripLinks(m.Text))
	if serviceMailIsBulk(m, blob) || hasCommercialTokens(blob) {
		return false
	}
	return hasImperativeDeadline(blob, now)
}

// serviceMailIsBulk reports whether the artifact is bulk-sent. It reads the
// retained Gmail header facts first (the authoritative signal) and falls back to
// body footers, because mail ingested before automation_headers was retained has
// no header row at all and would otherwise look transactional.
func serviceMailIsBulk(m Memory, blob string) bool {
	for _, h := range memoryAutomationHeaders(m) {
		switch h {
		case "list-unsubscribe", "precedence: bulk", "precedence: list", "precedence: junk":
			return true
		}
	}
	for _, marker := range bulkBodyMarkers {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	return false
}

// bulkBodyMarkers are campaign-footer phrases, used only as a fallback for mail
// ingested before automation_headers was retained. A bare "unsubscribe" is NOT
// one of them: CAN-SPAM put that word in the footer of ordinary transactional
// mail too, and measured against the live 3,439-thread Gmail corpus it dropped a
// real background-check invitation while catching nothing else. These phrases are
// campaign machinery that transactional mail does not carry.
var bulkBodyMarkers = []string{
	"view in browser", "view this email in", "manage your preferences",
	"manage your subscription", "manage email preferences", "update your email preferences",
	"you are receiving this email because", "you're receiving this email because",
	"unsubscribe from all", "unsubscribe from these",
}

// memoryAutomationHeaders returns every normalized automation header retained
// across the artifact's messages, lowercased. A thread is bulk if ANY message in
// it is: a marketing message quoted into a transactional thread still marks the
// whole artifact as bulk-sent.
func memoryAutomationHeaders(m Memory) []string {
	if m.Meta == nil {
		return nil
	}
	b, err := json.Marshal(m.Meta["messages"])
	if err != nil {
		return nil
	}
	var rows []struct {
		Headers []string `json:"automation_headers"`
	}
	if json.Unmarshal(b, &rows) != nil {
		return nil
	}
	var out []string
	for _, row := range rows {
		for _, h := range row.Headers {
			out = append(out, strings.ToLower(strings.TrimSpace(h)))
		}
	}
	return out
}

// commercialTokens are shopping, dunning, and promotional phrases. They are
// PHRASES, not bare words, on purpose: a bare "order" hits "in order to", which
// appears in legitimate directives. Any hit disqualifies the artifact outright —
// the lane would rather miss a real deadline buried in a receipt than hand the
// owner a cart-recovery card.
var commercialTokens = []string{
	"your order", "order #", "order number", "your cart", "cart", "checkout",
	"free delivery", "your delivery", "shipping", "tracking number",
	"promo", "discount", "coupon", "% off", "on sale", "shop now",
	"your purchase", "reward", "gift card",
	"subscription", "payment method", "billing", "invoice", "past due",
	"renew your", "auto-renew", "free trial", "upgrade your plan",
}

func hasCommercialTokens(blob string) bool {
	for _, t := range commercialTokens {
		if strings.Contains(blob, t) {
			return true
		}
	}
	return false
}

// deadlinePrepositions bind a date to an obligation. "on" is deliberately absent:
// "generated on September 23" is a report timestamp, not a due date.
var deadlinePrepositions = []string{"before", "by ", "due", "no later than", "on or before", "deadline"}

// actionImperatives are verbs that ask the RECIPIENT to do something. Status
// words ("paused", "pending") and ambiguous nouns ("update", "review") are
// excluded: "Update: more information needed" is a subject line, not a directive,
// and admitting it would reopen the leak this lane exists to close.
var actionImperatives = []string{
	"complete", "submit", "verify", "upload", "provide", "confirm",
	"sign", "respond", "reply", "return", "activate",
	"register", "enroll", "authorize", "e-sign",
}

// forecastMarkers turn a directive into a prediction about what the SERVICE will
// do ("your background check should finish by Sep 24"). The verb and the deadline
// are both present, but nothing is being asked of the owner, so the sentence is a
// status report and the lane must not claim it.
var forecastMarkers = []string{
	"should ", "will ", "would ", "may ", "might ", "expected to ",
	"scheduled to ", "we'll ", "aims to ", "estimate",
}

// dateExpr matches the absolute calendar forms that appear in transactional mail:
// a named month with a day, an ISO date, and a numeric US slash date. A relative
// countdown ("6 days left") matches nothing here by design.
var dateExpr = regexp.MustCompile(`(?i)\b(?:` +
	`(jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?(?:,?\s+(\d{4}))?` +
	`|(\d{4})-(\d{2})-(\d{2})` +
	`|(\d{1,2})/(\d{1,2})/(\d{2,4})` +
	`)\b`)

var linkExpr = regexp.MustCompile(`https?://\S+`)

// stripLinks removes URLs before matching. Tracking links are long opaque blobs
// that can contain digit runs a date pattern would otherwise claim.
func stripLinks(text string) string { return linkExpr.ReplaceAllString(text, " ") }

// hasImperativeDeadline reports whether the blob contains an absolute date inside
// the window that is bound, IN THE SAME SENTENCE, to both a deadline preposition
// and a recipient imperative. Sentence scoping is what separates "Complete before
// September 23" from "Service is paused. Maintenance runs through September 23".
func hasImperativeDeadline(blob string, now time.Time) bool {
	for _, loc := range dateExpr.FindAllStringSubmatchIndex(blob, -1) {
		due, ok := parseAbsoluteCalendarDate(blob[loc[0]:loc[1]], now)
		if !ok || !deadlineInWindow(due, now) {
			continue
		}
		lead := sentenceLead(blob[:loc[0]])
		if !containsAny(lead, deadlinePrepositions) || !containsAny(lead, actionImperatives) {
			continue
		}
		if containsAny(lead, forecastMarkers) {
			continue // "your check should finish by Sep 24" is a status forecast.
		}
		return true
	}
	return false
}

// sentenceLead returns the tail of prefix back to the nearest sentence boundary,
// capped so a runaway unpunctuated paragraph cannot bind a distant verb to a date.
func sentenceLead(prefix string) string {
	if cut := strings.LastIndexAny(prefix, ".!?\n;:"); cut >= 0 {
		prefix = prefix[cut+1:]
	}
	const maxLead = 120
	if len(prefix) > maxLead {
		prefix = prefix[len(prefix)-maxLead:]
	}
	return prefix
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

var monthByPrefix = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
	"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}

// parseAbsoluteCalendarDate resolves one matched date expression to a UTC day. A
// missing year resolves to whichever candidate year sits nearest now, so a
// December notice naming "January 4" reads as next year rather than last.
func parseAbsoluteCalendarDate(raw string, now time.Time) (time.Time, bool) {
	g := dateExpr.FindStringSubmatch(raw)
	if g == nil {
		return time.Time{}, false
	}
	switch {
	case g[1] != "":
		month, ok := monthByPrefix[strings.ToLower(g[1])[:3]]
		if !ok {
			return time.Time{}, false
		}
		if g[3] != "" {
			return calendarDay(atoiSafe(g[3]), month, atoiSafe(g[2]))
		}
		return nearestYear(month, atoiSafe(g[2]), now)
	case g[4] != "":
		return calendarDay(atoiSafe(g[4]), time.Month(atoiSafe(g[5])), atoiSafe(g[6]))
	case g[7] != "":
		year := atoiSafe(g[9])
		if year < 100 {
			year += 2000
		}
		return calendarDay(year, time.Month(atoiSafe(g[7])), atoiSafe(g[8]))
	}
	return time.Time{}, false
}

// calendarDay rejects a date whose components are not a real calendar day.
// time.Date NORMALIZES out-of-range input — "February 31" becomes March 3,
// "2026-13-01" becomes January 2027 — so a round-trip check is the only way to
// tell a typo or an unrelated digit run from a genuine deadline. Admitting a
// normalized date would let an arbitrary "09/00/2026" in body text move a
// deadline into the window.
func calendarDay(year int, month time.Month, day int) (time.Time, bool) {
	if year < 1970 || year > 2200 || month < time.January || month > time.December || day < 1 || day > 31 {
		return time.Time{}, false
	}
	d := utcDay(year, month, day)
	if d.Year() != year || d.Month() != month || d.Day() != day {
		return time.Time{}, false
	}
	return d, true
}

func nearestYear(month time.Month, day int, now time.Time) (time.Time, bool) {
	var best time.Time
	for _, y := range []int{now.Year(), now.Year() - 1, now.Year() + 1} {
		cand, ok := calendarDay(y, month, day)
		if !ok {
			continue // Feb 29 in a common year: not a real day in THAT year.
		}
		if best.IsZero() || absDuration(cand.Sub(now)) < absDuration(best.Sub(now)) {
			best = cand
		}
	}
	return best, !best.IsZero()
}

func utcDay(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// deadlineInWindow keeps a deadline that is today or later and no more than
// actionableServiceWindow out. Today's own date still counts: a same-day deadline
// is the most actionable case there is.
//
// The lower bound carries a one-day grace because deadline DATES are parsed as
// UTC midnight while the owner lives in a local calendar day. Without it, a
// "complete before September 23" notice vanishes at 17:00 PDT on the 22nd — the
// instant UTC rolls over — which is precisely when it matters most. The grace
// costs one extra day of visibility on a just-expired deadline.
func deadlineInWindow(due, now time.Time) bool {
	now = now.UTC()
	floor := utcDay(now.Year(), now.Month(), now.Day()).AddDate(0, 0, -1)
	return !due.Before(floor) && !due.After(now.Add(actionableServiceWindow))
}

// stampActionableServiceLane marks a surfaced item as lane-admitted so the
// companion can promote it. The stamp is carried on the existing DigestItem.Lane
// field rather than a new one: Today's contract already reads lanes, so no
// schema, budget, or mobile payload changes.
func stampActionableServiceLane(it *DigestItem, m Memory, now time.Time) {
	if it.Lane != "" {
		return // an existing lane assignment (whatsapp relevance) wins.
	}
	if isActionableServiceMail(m, now) {
		it.Lane = laneActionableService
	}
}

// actionableServiceHoldIDs collects the memory IDs the brief watermark must NOT
// advance over. The delta watermark is a content-hash diff built for NEWS: once an
// artifact is committed, an unchanged copy reads as old and never surfaces again.
// A portal deadline is the opposite — its text is identical every day it stays
// undone, and the day it is finally acted on nothing about the email changes. So
// the lane keeps its members un-baselined: each run re-reads them as new until the
// deadline leaves the window, at which point the lane stops admitting them and the
// commitment gate drops them for good.
func actionableServiceHoldIDs(byInstance map[string][]Memory, now time.Time) map[string]bool {
	held := map[string]bool{}
	for _, memories := range byInstance {
		for _, m := range memories {
			if isActionableServiceMail(m, now) {
				held[m.ID] = true
			}
		}
	}
	return held
}
