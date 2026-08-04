package mora

import (
	"os"
	"strings"
	"testing"
)

// TestClassifyMeetingNotesArtifact pins that Google's automated meeting-artifact
// senders (Gemini meeting notes, Google Meet) are classified "service" so a
// recurring meeting's auto-generated notes emails stop crowding the daily brief —
// while real humans at the same domain, and human local parts that merely contain
// "notes" as a substring, stay "person" (token-exact, precision-first).
func TestClassifyMeetingNotesArtifact(t *testing.T) {
	cases := []struct {
		identity, display, want string
	}{
		// the bug: Gemini meeting-notes emails surfaced as full brief items
		{"gemini-notes@google.com", "Gemini", "service"},
		// a notes sender at another host is service too (domain-agnostic, like "digest")
		{"meeting-notes@zoom.us", "", "service"},
		// regression guard: Google Meet notices already gated via the noreply family
		{"meetings-noreply@google.com", "Google Meet", "service"},
		// --- precision guards: must stay person ---
		{"riya@google.com", "Riya", "person"},  // real human at google.com
		{"keynotes@speaker.com", "", "person"}, // token "keynotes" != "notes" (no substring match)
		{"denotes@dict.com", "Sam", "person"},  // token "denotes" != "notes"
		// singular "note" is deliberately NOT a service token (more plausible as a
		// human handle/surname, no recall benefit) — guard against a future regression.
		{"john.note@personal.example", "John Note", "person"},
	}
	for _, c := range cases {
		if got := classifyIdentity(c.identity, c.display); got != c.want {
			t.Errorf("classifyIdentity(%q, %q) = %q, want %q", c.identity, c.display, got, c.want)
		}
	}
}

// TestClassifyIdentity is the A1 contract: automated/transactional senders are
// classified "service", real humans and named phone handles "person". The table
// is drawn from the real-data audit (2026-06-04) plus boundary cases.
func TestClassifyIdentity(t *testing.T) {
	cases := []struct {
		identity, display, want string
	}{
		// --- real humans -> person ---
		{"riya@gmail.com", "Riya Sharma", "person"},
		{"neil@example.com", "Neil Patel", "person"},
		{"alex.owner@gmail.com", "Alex Owner", "person"},
		{"sam@owner.dev", "Sam Owner", "person"},
		{"bob@y.com", "", "person"},
		// named phone handles are people; unnamed numbers are retained as artifacts
		{"+15551234567", "Mom", "person"},
		{"+447700900000", "", "artifact"},

		// --- automated local-part tokens -> service ---
		{"no-reply@medium.com", "Medium Daily Digest", "service"},
		{"noreply@github.com", "GitHub", "service"},
		{"donotreply@chase.com", "Chase", "service"},
		{"do-not-reply@notices.example.com", "", "service"},
		{"calendar-notification@google.com", "Google Calendar", "service"},
		{"drive-shares-dm-noreply@google.com", "", "service"},
		{"jobalerts-noreply@linkedin.com", "LinkedIn Job Alerts", "service"},
		{"receipts@uber.com", "Uber Receipts", "service"},
		{"news@info.equinox.com", "", "service"},
		{"alerts@t.equinox.com", "", "service"},
		{"support@stripe.com", "Stripe Support", "service"},
		{"info@meetup.com", "", "service"},
		{"billing@aws.amazon.com", "", "service"},
		{"team@figma.com", "The Figma Team", "service"},
		{"postmaster@x.com", "", "service"},
		{"mailer-daemon@googlemail.com", "Mail Delivery Subsystem", "service"},
		{"service@paypal.com", "", "service"},
		{"customer.service@shop.com", "", "service"},
		{"unsubscribe-4f19@customer.io", "", "service"},
		{"unsub@customeriomail.com", "", "service"},
		// concatenated / plus-tagged no-reply locals the token rule alone would miss
		{"noreplypatientbilling@onemedical.com", "One Medical", "service"},
		{"noreply+jobs@google.com", "Google Recruiting", "service"},
		{"donotreply2024@bank.com", "", "service"},

		// --- bulk-ESP host label -> service ---
		{"hello@email.uber.com", "Uber", "service"},
		{"x@mailer.linkedin.com", "LinkedIn", "service"},
		{"campaign@sendgrid.net", "", "service"},
		{"m@bounce.salesforce.com", "", "service"},

		// --- display-name patterns -> service (even with a benign address) ---
		{"venmo@person.venmo.com", "Venmo", "org"}, // address benign, display benign -> org (company, A4 defers)
		{"x@somehost.com", "Acme Receipts", "service"},
		{"y@somehost.com", "Weekly Alerts", "service"},
		{"z@somehost.com", "Foo Notifications", "service"},
		{"q@somehost.com", "Acme Job Alerts", "service"},
		{"r@somehost.com", "no-reply", "service"},

		// --- false-positive guards: human local parts that merely CONTAIN a
		// denylist substring but not as a whole delimiter token -> person ---
		{"gmail@gmail.com", "", "person"},             // "gmail" != token "mail"
		{"automotive@shop.com", "Joe", "person"},      // "automotive" != token "auto"
		{"teamus@x.com", "Pat", "person"},             // "teamus" != token "team"
		{"newsom@gov.ca", "Gavin", "person"},          // "newsom" != token "news"
		{"infante@x.com", "Maria", "person"},          // "infante" != token "info"
		{"adit+shopping@gmail.com", "Adit", "person"}, // human plus-addressing is fine
		{"jane+support@gmail.com", "Jane", "person"},  // +tag "support" is the user's label, not a service token
		{"adit+news@gmail.com", "Adit", "person"},     // +tag "news" likewise
		{"adit+receipts@gmail.com", "Adit", "person"}, // even a +receipts filter tag stays person
		{"servicenow@company.com", "Pat", "person"},   // "servicenow" != token "service"
		// real humans routed through a mail.* / email.* subdomain are NOT bulk-ESP
		{"andrea@mail.notte.cc", "Andrea Pinto", "person"},
		{"john@email.startup.io", "John Smith", "person"},
		// "mail"/"email"/"bot" are not bare local-part service tokens (mirror the host
		// exclusion; avoid hitting human handles)
		{"john.mail@x.com", "John", "person"},
		{"my.email@x.com", "Jane", "person"},
		{"the.bot@x.com", "Roberto", "person"},
		{"talbot@x.com", "Tal Bot", "person"}, // "talbot" != token "bot"
	}
	for _, c := range cases {
		if got := classifyIdentity(c.identity, c.display); got != c.want {
			t.Errorf("classifyIdentity(%q, %q) = %q, want %q", c.identity, c.display, got, c.want)
		}
	}
}

// TestClassifyShortcode (D14-5) pins the phone-handle branch: an all-digits
// handle of length <=6 is an SMS shortcode -> service. A real phone number with
// a trusted contact name stays person; an unnamed number is an artifact rather
// than a synthetic person.
func TestClassifyShortcode(t *testing.T) {
	cases := []struct {
		identity, display, want string
	}{
		// --- SMS shortcodes (all-digits, len <=6) -> service ---
		{"262966", "", "service"},     // Amazon
		{"22395", "", "service"},      // 5-digit shortcode
		{"456789", "", "service"},     // 6-digit boundary -> service
		{"99000", "USPS", "service"},  // display present, still service
		{"1", "", "service"},          // degenerate single digit
		{"  466453  ", "", "service"}, // whitespace trimmed before the digit scan

		// --- boundary: 6 digits -> service, unnamed 7 digits -> artifact ---
		{"123456", "", "service"},
		{"1234567", "", "artifact"},

		// --- named real phone numbers -> person; unnamed -> artifact ---
		{"4155550123", "Sam", "person"},
		{"15551234567", "Mom", "person"},
		{"+14155550123", "", "artifact"},
		{"+15551234567", "Mom", "person"},
		{"+447700900000", "", "artifact"},
		{"+1262966", "", "artifact"},

		// --- non-digit handles fall through to the existing person default ---
		{"+1-415-555-0123", "Sam", "person"}, // punctuation, not all-digits, has '+'
		{"colleague", "", "person"},          // alpha handle (e.g. an email username sans domain)
		{"", "", "person"},                   // empty -> person (no panic)
	}
	for _, c := range cases {
		if got := classifyIdentity(c.identity, c.display); got != c.want {
			t.Errorf("classifyIdentity(%q, %q) = %q, want %q", c.identity, c.display, got, c.want)
		}
	}
}

// TestClassifyBrandSubdomain (D14-5) pins the brand-mailing-subdomain rule:
// notify./alerts. host labels are unambiguous bulk-send infrastructure ->
// service. Per the precision-first resolution (D14-5 / 2026-06-08 cross-check),
// "email"/"mail" are deliberately NOT added as service labels — they false-
// positive real humans at small domains (mail.notte.cc, email.startup.io are in
// the corpus below and MUST stay person). So email.brand.com is a recall miss we
// accept rather than risk demoting a real person (the worst error).
func TestClassifyBrandSubdomain(t *testing.T) {
	cases := []struct {
		identity, display, want string
	}{
		// --- brand notify./alerts. subdomains -> service ---
		{"x@notify.acme.com", "", "service"},
		{"y@alerts.bank.com", "", "service"},
		{"hello@notify.sofi.com", "SoFi", "service"},
		{"noreply@alerts.chase.com", "Chase", "service"}, // belt-and-suspenders
		{"info@notify.uber.com", "", "service"},          // notify label fires (info also a local token)

		// --- 'mail'/'email' subdomains stay person (the precision-safe exclusion) ---
		{"andrea@mail.notte.cc", "Andrea Pinto", "person"}, // small-domain human
		{"john@email.startup.io", "John Smith", "person"},  // 'email' label NOT a service label
		{"sam@mail.google.com", "Sam", "person"},           // mail.google.com routing
		{"sofi@email.sofi.com", "Sofi Martinez", "person"}, // accepted recall miss; precision-safe
	}
	for _, c := range cases {
		if got := classifyIdentity(c.identity, c.display); got != c.want {
			t.Errorf("classifyIdentity(%q, %q) = %q, want %q", c.identity, c.display, got, c.want)
		}
	}
}

// TestClassifyFalsePositiveCorpus is the precision-first regression guard
// (D14-5): a curated set of REAL-human addresses/handles that must ALL stay
// person after the shortcode + brand-subdomain tightening. A single flip here is
// the worst error this phase could introduce, so this corpus is the explicit
// invariant the threat register (T-14-04) demands.
func TestClassifyFalsePositiveCorpus(t *testing.T) {
	humans := []struct {
		identity, display string
	}{
		{"andrea@mail.notte.cc", "Andrea Pinto"},  // mail.<smalldomain>
		{"john@email.startup.io", "John Smith"},   // email.<smalldomain>
		{"first.last@gmail.com", "First Last"},    // ordinary gmail human
		{"someone@automotive.io", "Joe Driver"},   // 'automotive' != token 'auto'
		{"4155550123", "Sam"},                     // 10-digit phone (not a shortcode)
		{"+14155550123", "Sam"},                   // '+' intl phone
		{"newsom@gov.ca", "Gavin Newsom"},         // 'newsom' != token 'news'
		{"servicenow@company.com", "Pat"},         // 'servicenow' != token 'service'
		{"the.bot@x.com", "Roberto"},              // 'bot' is not a token
		{"j.smith@notes.example.com", "J. Smith"}, // 'notes' host label not a service label
		{"de.la.cruz@startup.io", "De La Cruz"},   // multi-token human local part
		{"q@somehost.com", "Patel, Anjali"},       // "Last, First" display stays person
	}
	for _, h := range humans {
		if got := classifyIdentity(h.identity, h.display); got != "person" {
			t.Errorf("FALSE POSITIVE: classifyIdentity(%q, %q) = %q, want person", h.identity, h.display, got)
		}
	}
}

// TestReciprocityOverrideDeferred documents (and grep-pins) that the D14-5
// is_from_me reciprocity-override is INTENTIONALLY not implemented in v1 — the
// direction signal isn't carried to the classify seam (conversationMeta emits
// only participants/message_count/occurred_at). The deferral rationale must live
// in classify.go so it's an explicit, reversible decision, not a forgotten gap
// (threat T-14-05). This test asserts the comment marker is present.
func TestReciprocityOverrideDeferred(t *testing.T) {
	src, err := os.ReadFile("classify.go")
	if err != nil {
		t.Fatalf("read classify.go: %v", err)
	}
	s := string(src)
	for _, marker := range []string{"reciprocity-override", "is_from_me", "conversationMeta"} {
		if !strings.Contains(s, marker) {
			t.Errorf("classify.go missing deferral marker %q (D14-5 reciprocity-override deferral must be documented in code)", marker)
		}
	}
}

// TestClassifyIdentityDeterministic proves the classifier is pure (same input ->
// same output across calls), a graph-rebuild invariant.
func TestClassifyIdentityDeterministic(t *testing.T) {
	for _, id := range []string{"no-reply@x.com", "riya@gmail.com", "+15551234567"} {
		first := classifyIdentity(id, "")
		for i := 0; i < 5; i++ {
			if got := classifyIdentity(id, ""); got != first {
				t.Fatalf("classifyIdentity(%q) nondeterministic: %q vs %q", id, got, first)
			}
		}
	}
}

func TestClassifyIdentityTypes(t *testing.T) {
	cases := []struct {
		identity, display, want string
	}{
		{"google.com", "", "org"},
		{"sofi@email.sofi.com", "SoFi", "org"},
		{"one-medical@something.com", "One Medical", "org"},
		{"chase@chase.com", "Chase Bank", "org"},
		{"pyranthus-hq/mora", "", "repo"},
		{"", "pyranthus-hq/anthos", "repo"},
		{"author", "", "artifact"},
		{"push", "", "artifact"},
		{"mention", "", "artifact"},
		{"ci activity", "", "artifact"},
		{"state change", "", "artifact"},
		{"ci-activity", "", "artifact"},
		{"state-change", "", "artifact"},
		{"iMessage;-;weird", "", "person"},
	}
	for _, c := range cases {
		if got := classifyIdentity(c.identity, c.display); got != c.want {
			t.Errorf("classifyIdentity(%q, %q) = %q, want %q", c.identity, c.display, got, c.want)
		}
	}
}
