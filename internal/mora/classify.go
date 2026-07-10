package mora

import "strings"

// classifyIdentity (A1) labels a person reference as "person" or "service".
// Automated/transactional senders (no-reply addresses, bulk-ESP hosts, and
// "... Receipts/Alerts" display names) are demoted to "service" so the People
// view stays human. Pure and deterministic: same input -> same label across
// graph rebuilds. Identities without an '@' (iMessage phone handles) are always
// real people.
//
// Conservatism is deliberate. Local-part matching is token-exact (a denylist
// token must BE a whole '.'/'-'/'_'-delimited token, or the entire local part) —
// never a substring — so "gmail", "automotive", "newsom" stay people. Host
// matching uses explicit bulk-ESP labels, not single letters like "t".
func classifyIdentity(identity, displayName string) string {
	id := strings.ToLower(strings.TrimSpace(identity))
	display := strings.ToLower(strings.TrimSpace(displayName))

	if isArtifact(id, display) {
		return "artifact"
	}
	if isRepo(id, display) {
		return "repo"
	}

	at := strings.LastIndexByte(id, '@')
	if at < 0 {
		// Phone-handle / non-email branch. D14-5: an all-digits handle of length
		// <=6 is an SMS shortcode (262966, 22395) -> service. The cut is safe
		// because no real phone number is <=6 digits — a 10/11-digit number or any
		// '+'-prefixed international number is a real phone and stays person. Scoped
		// strictly here; it never touches email classification.
		if isShortcode(id) {
			return "service"
		}
		if isOrg(id, display) {
			return "org"
		}
		return "person" // phone handle / non-email identity -> real person
	}
	local, host := id[:at], id[at+1:]
	if localPartIsService(local) || hostIsBulkESP(host) || displayIsService(displayName) {
		return "service"
	}
	if isOrg(id, display) {
		return "org"
	}
	return "person"
}

// serviceLocalTokens: a local part is a service if it equals one of these OR any
// of its '.'/'-'/'_'-delimited tokens does. (Both singular and plural spellings
// are listed explicitly — token matching is exact, not stemmed.)
var serviceLocalTokens = map[string]bool{
	"no-reply": true, "noreply": true, "no_reply": true,
	"donotreply": true, "do-not-reply": true,
	"notification": true, "notifications": true, "notify": true,
	"alert": true, "alerts": true,
	"receipt": true, "receipts": true,
	"news": true, "newsletter": true, "notes": true,
	"update": true, "updates": true,
	"mailer": true, "mailer-daemon": true,
	"bounce": true, "bounces": true,
	"info": true, "support": true, "service": true, "help": true, "sales": true,
	"billing": true, "marketing": true, "hello": true, "contact": true,
	"team": true, "automated": true, "auto": true,
	"account": true, "accounts": true,
	"security": true, "member": true, "members": true,
	"community": true, "digest": true, "postmaster": true,
	"robot": true, "daemon": true, "reply": true,
	// NOTE: "mail"/"email" are intentionally NOT here — they mirror the host-label
	// exclusion (mail.<domain> is ordinary routing; my.email@x is a real human).
	// "bot" is omitted too: as a bare token it hits human handles ("the.bot"); real
	// bot senders are caught by robot/daemon/no-reply markers and ESP host labels.
}

// serviceHostLabels: explicit bulk-ESP subdomain labels. A host whose '.'-split
// labels include any of these is a sending service. Deliberately EXCLUDES "mail"
// and "email": those are how ordinary mail is routed (mail.google.com,
// mail.<smalldomain>) and would false-positive real humans at small domains
// (e.g. a real person at mail.notte.cc, john@email.startup.io). The retained
// labels are unambiguous bulk-send infrastructure no human inbox lives behind.
//
// D14-5 brand-subdomain fix: "notify" and "alerts" are added here — they are
// unambiguous transactional-brand send labels (notify.acme.com, alerts.bank.com)
// and appear in NO real-human address in the false-positive corpus. The D14-5
// memo also proposed "email" as a brand label, but a 2026-06-08 precision-first
// cross-check rejected it: email.<x>.<tld> collides with real humans at small
// domains (email.startup.io), so demoting it would flip a real person — the
// worst error. We therefore accept the email.brand.com recall miss and keep only
// the two unambiguous labels. (TestClassifyBrandSubdomain pins both directions.)
var serviceHostLabels = map[string]bool{
	"noreply": true, "mailer": true, "bounce": true, "mktg": true,
	"marketing": true, "news": true, "notifications": true,
	"notify": true, "alerts": true,
	"em": true, "sendgrid": true, "mailgun": true, "send": true,
}

// DEFERRED (D14-5) — reciprocity-override. The design calls for force-keeping as
// "person" anyone the user demonstrably replied to (for iMessage, an is_from_me-
// derived direction signal that needs no `self` config). It is intentionally NOT
// implemented in v1 because the direction signal does not reach this seam:
// is_from_me is read per-message in internal/imessage/chatdb.go (the per-message
// fromMe flag), but internal/imessage/map.go:129 conversationMeta emits only
// participants / message_count / occurred_at — there is NO per-handle replied-to
// flag in Meta, so it never reaches package mora / classifyIdentity. Wiring the
// override would require a connector Meta change AND a re-ingest to populate
// existing vaults (which the phase's "defer reingest" constraint forbids). Per
// D14-5's own instruction ("if the direction signal isn't cleanly available at
// the classify seam, DEFER rather than guess — precision-first"), we defer it and
// record the rationale here so it lands cleanly once the connector carries
// direction. Do NOT guess at self/direction to fake it.

// serviceDisplaySuffixes: display names ending in one of these are role/automated
// senders ("Uber Receipts", "The Figma Team", "Medium Daily Digest").
var serviceDisplaySuffixes = []string{
	" receipts", " alerts", " notifications", " team",
	" support", " newsletter", " digest",
}

// serviceLocalSubstrings are no-reply markers so distinctive they're safe to match
// anywhere in a local part — catching concatenated forms ("noreplypatientbilling")
// and plus-tagged forms that the token rule alone would miss. Kept to the
// no-reply family only; no benign human local part contains these.
var serviceLocalSubstrings = []string{"noreply", "no-reply", "donotreply", "do-not-reply", "donotrespond"}

func isDelim(r rune) bool { return r == '.' || r == '-' || r == '_' }

// isShortcode reports whether handle is an SMS shortcode (D14-5): a whole run of
// 1..6 ASCII digits with no '+' prefix. Real phone numbers are 10+ digits (US is
// 10/11) and international numbers carry a '+', so both stay person — the length
// cut never collides with a real human. Byte-exact on the digit run (no regex):
// any non-digit rune disqualifies, so "+1262966" and "415-555-0123" are NOT
// shortcodes. The caller has already trimmed/lowercased; an empty handle returns
// false and falls through to the person default.
func isShortcode(handle string) bool {
	if handle == "" || len(handle) > 6 {
		return false
	}
	for _, r := range handle {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func localPartIsService(local string) bool {
	// Plus-addressing: the mailbox is the part before the first '+'; the tag after it
	// is a user-chosen label (jane+support@, adit+news@) and must NOT be read as a
	// service token. The base still catches real bots like noreply+jobs@ (base
	// "noreply") and jobalerts-noreply@.
	base := local
	if i := strings.IndexByte(base, '+'); i >= 0 {
		base = base[:i]
	}
	if serviceLocalTokens[base] {
		return true
	}
	for _, sub := range serviceLocalSubstrings {
		if strings.Contains(base, sub) {
			return true
		}
	}
	for _, tok := range strings.FieldsFunc(base, isDelim) {
		if serviceLocalTokens[tok] {
			return true
		}
	}
	return false
}

func hostIsBulkESP(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if serviceHostLabels[label] {
			return true
		}
	}
	return false
}

func displayIsService(display string) bool {
	d := strings.ToLower(strings.TrimSpace(display))
	if d == "" {
		return false
	}
	if strings.Contains(d, "job alerts") || strings.Contains(d, "no-reply") || strings.Contains(d, "do not reply") {
		return true
	}
	for _, suf := range serviceDisplaySuffixes {
		if strings.HasSuffix(d, suf) {
			return true
		}
	}
	return false
}

func isValidPhoneOrShortcode(handle string) bool {
	if handle == "" {
		return false
	}
	if handle[0] == '+' {
		handle = handle[1:]
	}
	if len(handle) == 0 {
		return false
	}
	for _, r := range handle {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isArtifact(id, display string) bool {
	artifacts := map[string]bool{
		"push": true, "author": true, "mention": true, "ci activity": true, "state change": true,
		"ci-activity": true, "state-change": true,
	}
	return artifacts[id] || artifacts[display]
}

func isRepo(id, display string) bool {
	if !strings.Contains(id, "@") && strings.Count(id, "/") == 1 {
		return true
	}
	if !strings.Contains(display, "@") && strings.Count(display, "/") == 1 {
		return true
	}
	return false
}

func isOrg(id, display string) bool {
	orgBrands := map[string]bool{
		"sofi": true, "one medical": true, "github": true, "google": true, "stripe": true, "chase": true,
		"apple": true, "slack": true, "zoom": true, "facebook": true, "meta": true, "amazon": true,
		"uber": true, "figma": true, "linkedin": true, "microsoft": true, "netflix": true, "paypal": true,
		"venmo": true, "airbnb": true, "lyft": true, "salesforce": true,
	}

	if orgBrands[display] {
		return true
	}

	orgKeywords := []string{
		" inc.", " inc", " corp.", " corp", " co.", " llc", " ltd.", " ltd", " company", " corporation",
		" association", " foundation", " university", " institute", " medical", " hospital",
		" clinic", " school", " college", " bank", " ventures", " capital", " partners", " labs",
	}
	for _, kw := range orgKeywords {
		if strings.HasSuffix(display, kw) {
			return true
		}
	}

	if !strings.Contains(id, "@") && strings.Contains(id, ".") && !strings.Contains(id, "/") && !isValidPhoneOrShortcode(id) {
		return true
	}
	return false
}
