package mora

import (
	"strings"
	"testing"
	"time"
)

// Regression tests for the "the brief is gibberish" report (2026-07-13). Every
// case below is a VERBATIM line the real brief surfaced, traced to its real
// source email. The brief is the user's unfinished business: a line that is not
// a complete, human-authored sentence stating something the USER must do or must
// not get wrong has no business being in it.

// RC1: evidence segmentation split on "." BEFORE URLs were stripped, so
// "https://meet.google.com/ctk-rdtz-jnx?hs=224" shredded into "com/ctk-rdtz-jnx?"
// — one slash, mostly letters, ends in "?" — which then read as a question. URLs
// must be removed from the text BEFORE it is cut into sentences.
func TestEvidenceSegmentsNeverShredURLsIntoQuestions(t *testing.T) {
	// The real body of the "Declined: Sync up meeting" calendar notification.
	body := "Join with Google Meet\nhttps://meet.google.com/ctk-rdtz-jnx?hs=224\n" +
		"Location\nSan Ramon, CA, USA\nhttps://www.google.com/maps/search/San+Ramon,+CA,+USA?hl=en\n"
	for _, seg := range meetingBriefEvidenceSegments(body) {
		if strings.Contains(seg, "ctk-rdtz-jnx") || strings.Contains(seg, "/maps/") || strings.Contains(seg, "hl=en") {
			t.Errorf("URL shard survived segmentation as a segment: %q", seg)
		}
		if actionableQuestion(seg) {
			t.Errorf("URL debris became an actionable question: %q", seg)
		}
	}
}

// RC2: "\n" was a sentence terminator, so a hard-wrapped Gmail body (plain text
// wraps at ~72 cols) was cut mid-clause. The real brief rendered "Please share
// the Ahrefs findings/report prior to the" — the line simply ran out.

// RC3: containsAnyPhrase was a raw substring test, so "depending on" matched the
// unresolved-thread phrase "pending". A vendor's price quote was filed as an
// unresolved decision on that alone.
func TestPhraseMatchIsWordBounded(t *testing.T) {
	if containsAnyPhrase("proper sem setup will cost $2,000+ depending on how many campaigns", unresolvedThreadPhrases) {
		t.Error(`"depending" must not match the phrase "pending" (substring false positive)`)
	}
	if !containsAnyPhrase("the contract is still pending on their side", unresolvedThreadPhrases) {
		t.Error(`a real "pending" must still match`)
	}
	if containsAnyPhrase("she joined the introduction call", stalenessGuardPhrases) &&
		!containsAnyPhrase("she joined acme last month", stalenessGuardPhrases) {
		t.Error("word-boundary matching regressed the genuine staleness signal")
	}
}

// RC4: the "unresolved decisions" path called the LOOSE bare-"?" gate, so PR
// #119's strict Gmail ask gate (a real interrogative opener or direct request)
// was never consulted there. "Need help?" — a Microsoft Teams invite footer —
// became an unresolved thread.
func TestGmailUnresolvedRequiresAGenuineAsk(t *testing.T) {
	for _, junk := range []string{"Need help?", "com/ctk-rdtz-jnx?", "Was this helpful?"} {
		m := Memory{Provider: "gmail", Type: "email", Text: junk}
		if endsInActionableQuestion(m) {
			t.Errorf("gmail: %q must not count as an actionable question (no interrogative opener / direct request)", junk)
		}
	}
	real := Memory{Provider: "gmail", Type: "email", Text: "Can you send the redlined contract before Friday?"}
	if !endsInActionableQuestion(real) {
		t.Error("a genuine Gmail ask must still count")
	}
	// iMessage deliberately keeps the looser bare-"?" rule (terse real conversation).
	terse := Memory{Provider: "imessage", Type: "imessage", Text: "Riya: the deck?"}
	if !endsInActionableQuestion(terse) {
		t.Error("iMessage must keep the looser bare-? rule")
	}
}

// RC5: calendar/meeting NOTIFICATION mail is machine-generated, not correspondence.
// The real brief mined "Declined: Sync up meeting" (a Google Calendar RSVP notice)
// and a Microsoft Teams invite for evidence. Nobody wrote those TO the user; they
// are event plumbing. They carry no unfinished business and must never be evidence.
func TestMeetingNotificationMailIsNotEvidence(t *testing.T) {
	cfg := Config{}
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	notifications := []Memory{
		{
			Provider: "gmail", Type: "email",
			Title: "Declined: Sync up meeting @ Weekly from 10pm to 11pm on weekdays (EDT) (Adit Karode)",
			Text:  "Gouri Karode has declined this invitation.\nSync up meeting\nJoin with Google Meet\nhttps://meet.google.com/ctk-rdtz-jnx?hs=224\n",
		},
		{
			Provider: "gmail", Type: "email",
			Title: "Invitation: GMG meet @ Tue Jul 14, 2026 (Adit Karode)",
			Text:  "You have been invited to the following event.\nView all guest info\n",
		},
		{
			Provider: "gmail", Type: "email",
			Title: "Introducing Pyranthus",
			Text:  "Microsoft Teams meeting\nJoin: https://teams.microsoft.com/meet/226094548165923\nMeeting ID: 226 094 548 165 923\nPasscode: zR7Fb9GW\nNeed help?\n",
		},
	}
	for _, m := range notifications {
		if kind := classifyMeetingBriefEvidence(m, cfg, at); kind != "" {
			t.Errorf("machine-generated meeting notification classified as %q: %q", kind, m.Title)
		}
	}
	// A genuine human email that merely MENTIONS a meeting must survive.
	real := Memory{
		Provider: "gmail", Type: "email",
		Title: "Contract redlines",
		Text:  "From: dan@example.com\n\nCan you send the redlined contract before our meeting?\n",
		Meta:  map[string]any{"from": []string{"dan@example.com"}},
	}
	if classifyMeetingBriefEvidence(real, cfg, at) == "" {
		t.Error("a genuine human ask must not be swept up by the notification filter")
	}
}

// RC6: an obligation addressed to a THIRD PARTY is not the user's open loop. The
// real brief surfaced "*Action Item for Kim:* Please share the Ahrefs findings" —
// an item Gouri assigned to Kim — as unfinished business the USER owns. Doctrine:
// every line must answer "what must the USER do", never "what did someone say".
//
// Deciding this needs to know who the user IS, so the check takes the user's own
// name tokens (derived from their self addresses). An item assigned to a name that
// is not the user's is theirs to do, not ours to surface.
func TestThirdPartyActionItemIsNotTheUsersOpenLoop(t *testing.T) {
	self := map[string]bool{"adit": true, "karode": true}

	for _, text := range []string{
		"*Action Item for Kim:* Please share the Ahrefs findings report.",
		"Action item for Beth: please send the proposal.",
		"action items for gouri: draft the deck",
	} {
		if !assignedToThirdParty(text, self) {
			t.Errorf("must be recognised as assigned to someone else: %q", text)
		}
	}
	for _, text := range []string{
		"Can you send the redlined contract before Friday?",
		"Action item for Adit: send the deck.",
		"please share the findings when you get a chance",
		"next steps: we agreed to ship the pilot",
	} {
		if assignedToThirdParty(text, self) {
			t.Errorf("must NOT be treated as a third-party item: %q", text)
		}
	}
}

// selfNameTokens must derive the user's names from the addresses Mora already knows,
// so the third-party check has something to compare against without new config.
func TestSelfNameTokensFromAddresses(t *testing.T) {
	got := selfNameTokens(map[string]bool{"adit.karode@gmail.com": true, "adit@adisamconsulting.com": true})
	for _, want := range []string{"adit", "karode"} {
		if !got[want] {
			t.Errorf("selfNameTokens missing %q; got %v", want, got)
		}
	}
	if got["gmail"] || got["adisamconsulting"] {
		t.Errorf("domain must not become a name token: %v", got)
	}
}

// RC7: an inbound message with ANOTHER external recipient is not unfinished business
// between the user and the attendee — the ask may be aimed at the other recipient.
//
// The real brief surfaced "Is there a pdf export version from Ahrefs?" as an
// obligation between Adit and Gouri. Gouri sent it, and Adit is a recipient, so
// PR #119's sender gate passed it. But the mail also went to connect@bethmotta.com
// and opens "Hi Beth" — Gouri was asking BETH. Adit was a bystander on his parents'
// client thread.
//
// The codebase already refuses to guess in exactly this shape on the OUTBOUND side
// ("if a group record cannot be assigned to exactly one attendee, it is dropped
// rather than attributed arbitrarily"). Inbound must refuse for the same reason.
func TestInboundGroupThreadIsNotTwoPartyBusiness(t *testing.T) {
	self := map[string]bool{"adit@example.com": true}

	// Gouri -> {Adit, Beth}: who is being asked? Unknowable. Refuse.
	group := Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com", "beth@vendor.com"},
		},
	}
	if meetingBriefIsTwoPartyExchange(group, self, "gouri@example.com") {
		t.Error("a thread with another external recipient must not count as business between the user and the attendee")
	}

	// Gouri -> Adit: unambiguous.
	direct := Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com"},
		},
	}
	if !meetingBriefIsTwoPartyExchange(direct, self, "gouri@example.com") {
		t.Error("a direct message from the attendee to the user IS two-party business")
	}

	// Adit -> Gouri: the user's own promise to this attendee is still theirs to keep.
	outbound := Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"adit@example.com"},
			"to":   []string{"gouri@example.com"},
		},
	}
	if !meetingBriefIsTwoPartyExchange(outbound, self, "gouri@example.com") {
		t.Error("the user writing directly to the attendee IS two-party business")
	}

	// A thread among people who are ALL in this meeting is still the meeting's
	// business — the sender decides attribution. Only an OUTSIDER breaks it.
	inRoom := Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com", "abhi@example.com"},
		},
	}
	if !meetingBriefIsTwoPartyExchange(inRoom, self, "gouri@example.com", "abhi@example.com") {
		t.Error("a thread among the meeting's own attendees must survive")
	}

	// A cc'd outsider must not resurrect the ambiguity.
	ccd := Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com"},
			"cc":   []string{"sanjay@client.com"},
		},
	}
	if meetingBriefIsTwoPartyExchange(ccd, self, "gouri@example.com") {
		t.Error("another human on cc makes the addressee ambiguous")
	}
}

// RC8: a forwarded or quoted block is SOMEONE ELSE'S words. The real brief mined
// "Fwd: Ai / AEO" — Gouri forwarding a marketing email — and surfaced the forwarded
// stranger's CTA ("Open to see how the loop works?") as Gouri's unfinished business
// with Adit. Only what the sender actually wrote, above the quote, is evidence.

// RC9: a sentence that ends in a colon is a LEAD-IN, not content. The real brief
// surfaced "Based on our conversation, here are the next steps and deliverables:" —
// which announces a list and states nothing. It tells the user nothing they must do.
func TestLeadInSentencesAreNotEvidence(t *testing.T) {
	for _, leadIn := range []string{
		"Based on our conversation, here are the next steps and deliverables:",
		"Here are the key points for our intro meeting:",
		"Agenda:",
	} {
		if !isLeadInFragment(leadIn) {
			t.Errorf("must be rejected as a content-free lead-in: %q", leadIn)
		}
	}
	for _, real := range []string{
		"Can you send the redlined contract before Friday?",
		"We agreed to ship the pilot next week.",
	} {
		if isLeadInFragment(real) {
			t.Errorf("must NOT be rejected: %q", real)
		}
	}
}

// RC10: when no sentence qualifies, the extractor falls back to the SUBJECT LINE —
// and that path skipped the forward filter, so "Fwd: Google Ads Account Audit &
// Recommendations - Ready for Your Review!" (a forwarded marketing subject) became
// shared context with an attendee. A forwarded subject is a stranger's subject.
func TestForwardedSubjectIsNotEvidence(t *testing.T) {
	cfg := Config{}
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	fwd := Memory{
		Provider: "gmail", Type: "email",
		Title: "Fwd: Google Ads Account Audit & Recommendations - Ready for Your Review!",
		Text:  "---------- Forwarded message ---------\nFrom: ads@vendor.com\n",
		Meta:  map[string]any{"from": []string{"gouri@example.com"}},
	}
	if got := meetingBriefActionableEvidenceText(fwd, cfg, at, meetingBriefSharedContext); got != "" {
		t.Errorf("a forwarded subject must not become evidence; got %q", got)
	}
	// A genuine subject still works as the fallback.
	real := Memory{
		Provider: "gmail", Type: "email",
		Title: "Acme pilot roadmap and launch milestone",
		Text:  "",
		Meta:  map[string]any{"from": []string{"gouri@example.com"}},
	}
	if got := meetingBriefActionableEvidenceText(real, cfg, at, meetingBriefSharedContext); got == "" {
		t.Error("a genuine subject must still be usable as the fallback excerpt")
	}
}

// RC11: staleness guards existed to stop the user asserting a fact that has since
// changed ("she moved to Berlin", "he's now at Acme"). The phrase list matched bare
// everyday verbs, so "Good morning leaving now" — the user walking out the door —
// became a staleness guard about an attendee. A staleness guard is an IDENTITY or
// ROLE change, not any sentence containing the word "leaving".
func TestStalenessGuardsRequireAnIdentityChange(t *testing.T) {
	for _, everyday := range []string{
		"good morning leaving now",
		"i left the keys on the table",
		"she joined the call late",
	} {
		if containsAnyPhrase(everyday, stalenessGuardPhrases) {
			t.Errorf("everyday speech must not be a staleness guard: %q", everyday)
		}
	}
	for _, real := range []string{
		"she moved to berlin last month",
		"he is now at acme as head of product",
		"i have a new role at stripe",
		"she is no longer at the company",
	} {
		if !containsAnyPhrase(real, stalenessGuardPhrases) {
			t.Errorf("a genuine identity/role change must still be a staleness guard: %q", real)
		}
	}
}

// RC12: an iMessage line the USER spoke is not evidence about the attendee, and the
// speaker prefix must never leak into the rendered line. The real brief rendered
// "Me: Good morning leaving now" as an attendee's staleness guard.
func TestUserSpokenIMessageIsNotAttendeeEvidence(t *testing.T) {
	cfg := Config{}
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	mine := Memory{
		Provider: "imessage", Type: "imessage",
		Title: "Gouri",
		Text:  "Me: Good morning leaving now",
	}
	got := meetingBriefActionableEvidenceText(mine, cfg, at, meetingBriefStaleness)
	if strings.Contains(got, "Me:") {
		t.Errorf("the speaker prefix must never render in a brief line: %q", got)
	}
	if got != "" {
		t.Errorf("the user's own passing remark is not an attendee staleness guard: %q", got)
	}
}
