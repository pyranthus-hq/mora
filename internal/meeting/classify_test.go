package meeting

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"testing"
	"time"
)

func TestEvidenceSegmentsNeverShredURLsIntoQuestions(t *testing.T) {
	// The real body of the "Declined: Sync up meeting" calendar notification.
	body := "Join with Google Meet\nhttps://meet.google.com/ctk-rdtz-jnx?hs=224\n" +
		"Location\nSan Ramon, CA, USA\nhttps://www.google.com/maps/search/San+Ramon,+CA,+USA?hl=en\n"
	for _, seg := range EvidenceSegments(body) {
		if strings.Contains(seg, "ctk-rdtz-jnx") || strings.Contains(seg, "/maps/") || strings.Contains(seg, "hl=en") {
			t.Errorf("URL shard survived segmentation as a segment: %q", seg)
		}
		if ActionableQuestion(seg) {
			t.Errorf("URL debris became an actionable question: %q", seg)
		}
	}
}

func TestPhraseMatchIsWordBounded(t *testing.T) {
	if ContainsAnyPhrase("proper sem setup will cost $2,000+ depending on how many campaigns", unresolvedThreadPhrases) {
		t.Error(`"depending" must not match the phrase "pending" (substring false positive)`)
	}
	if !ContainsAnyPhrase("the contract is still pending on their side", unresolvedThreadPhrases) {
		t.Error(`a real "pending" must still match`)
	}
	if ContainsAnyPhrase("she joined the introduction call", stalenessGuardPhrases) &&
		!ContainsAnyPhrase("she joined acme last month", stalenessGuardPhrases) {
		t.Error("word-boundary matching regressed the genuine staleness signal")
	}
}

func TestGmailUnresolvedRequiresAGenuineAsk(t *testing.T) {
	for _, junk := range []string{"Need help?", "com/ctk-rdtz-jnx?", "Was this helpful?"} {
		m := memory.Memory{Provider: "gmail", Type: "email", Text: junk}
		if EndsInActionableQuestion(m, strings.ToLower(m.Title+" "+m.Text)) {
			t.Errorf("gmail: %q must not count as an actionable question (no interrogative opener / direct request)", junk)
		}
	}
	real := memory.Memory{Provider: "gmail", Type: "email", Text: "Can you send the redlined contract before Friday?"}
	if !EndsInActionableQuestion(real, strings.ToLower(real.Title+" "+real.Text)) {
		t.Error("a genuine Gmail ask must still count")
	}
	// iMessage deliberately keeps the looser bare-"?" rule (terse real conversation).
	terse := memory.Memory{Provider: "imessage", Type: "imessage", Text: "Riya: the deck?"}
	if !EndsInActionableQuestion(terse, strings.ToLower(terse.Title+" "+terse.Text)) {
		t.Error("iMessage must keep the looser bare-? rule")
	}
}

func TestMeetingNotificationMailIsNotEvidence(t *testing.T) {
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	notifications := []memory.Memory{
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
		if !IsMeetingNotification(m) {
			t.Errorf("machine-generated notification not recognized: %q", m.Title)
		}
		if kind := ClassifyEvidence(ClassifierInput{Memory: m, SignalText: strings.ToLower(m.Title + " " + m.Text), At: at}); kind != "" {
			t.Errorf("machine-generated meeting notification classified as %q: %q", kind, m.Title)
		}
	}
	// A genuine human email that merely MENTIONS a meeting must survive.
	real := memory.Memory{
		Provider: "gmail", Type: "email",
		Title: "Contract redlines",
		Text:  "From: dan@example.com\n\nCan you send the redlined contract before our meeting?\n",
		Meta:  map[string]any{"from": []string{"dan@example.com"}},
	}
	if ClassifyEvidence(ClassifierInput{Memory: real, SignalText: strings.ToLower(real.Title + " " + real.Text), At: at}) == "" {
		t.Error("a genuine human ask must not be swept up by the notification filter")
	}
}

func TestThirdPartyActionItemIsNotTheUsersOpenLoop(t *testing.T) {
	self := map[string]bool{"adit": true, "karode": true}

	for _, text := range []string{
		"*Action Item for Kim:* Please share the Ahrefs findings report.",
		"Action item for Beth: please send the proposal.",
		"action items for gouri: draft the deck",
	} {
		if !AssignedToThirdParty(text, self) {
			t.Errorf("must be recognised as assigned to someone else: %q", text)
		}
	}
	for _, text := range []string{
		"Can you send the redlined contract before Friday?",
		"Action item for Adit: send the deck.",
		"please share the findings when you get a chance",
		"next steps: we agreed to ship the pilot",
	} {
		if AssignedToThirdParty(text, self) {
			t.Errorf("must NOT be treated as a third-party item: %q", text)
		}
	}
}

func TestSelfNameTokensFromAddresses(t *testing.T) {
	got := SelfNameTokens(map[string]bool{"adit.karode@gmail.com": true, "adit@adisamconsulting.com": true})
	for _, want := range []string{"adit", "karode"} {
		if !got[want] {
			t.Errorf("SelfNameTokens missing %q; got %v", want, got)
		}
	}
	if got["gmail"] || got["adisamconsulting"] {
		t.Errorf("domain must not become a name token: %v", got)
	}
}

func TestInboundGroupThreadIsNotTwoPartyBusiness(t *testing.T) {
	self := map[string]bool{"adit@example.com": true}

	// Gouri -> {Adit, Beth}: who is being asked? Unknowable. Refuse.
	group := memory.Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com", "beth@vendor.com"},
		},
	}
	if IsTwoPartyExchange(group, self, "gouri@example.com") {
		t.Error("a thread with another external recipient must not count as business between the user and the attendee")
	}

	// Gouri -> Adit: unambiguous.
	direct := memory.Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com"},
		},
	}
	if !IsTwoPartyExchange(direct, self, "gouri@example.com") {
		t.Error("a direct message from the attendee to the user IS two-party business")
	}

	// Adit -> Gouri: the user's own promise to this attendee is still theirs to keep.
	outbound := memory.Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"adit@example.com"},
			"to":   []string{"gouri@example.com"},
		},
	}
	if !IsTwoPartyExchange(outbound, self, "gouri@example.com") {
		t.Error("the user writing directly to the attendee IS two-party business")
	}

	// A thread among people who are ALL in this meeting is still the meeting's
	// business — the sender decides attribution. Only an OUTSIDER breaks it.
	inRoom := memory.Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com", "abhi@example.com"},
		},
	}
	if !IsTwoPartyExchange(inRoom, self, "gouri@example.com", "abhi@example.com") {
		t.Error("a thread among the meeting's own attendees must survive")
	}

	// A cc'd outsider must not resurrect the ambiguity.
	ccd := memory.Memory{
		Provider: "gmail", Type: "email",
		Meta: map[string]any{
			"from": []string{"gouri@example.com"},
			"to":   []string{"adit@example.com"},
			"cc":   []string{"sanjay@client.com"},
		},
	}
	if IsTwoPartyExchange(ccd, self, "gouri@example.com") {
		t.Error("another human on cc makes the addressee ambiguous")
	}
}

func TestLeadInSentencesAreNotEvidence(t *testing.T) {
	for _, leadIn := range []string{
		"Based on our conversation, here are the next steps and deliverables:",
		"Here are the key points for our intro meeting:",
		"Agenda:",
	} {
		if !IsLeadInFragment(leadIn) {
			t.Errorf("must be rejected as a content-free lead-in: %q", leadIn)
		}
	}
	for _, real := range []string{
		"Can you send the redlined contract before Friday?",
		"We agreed to ship the pilot next week.",
	} {
		if IsLeadInFragment(real) {
			t.Errorf("must NOT be rejected: %q", real)
		}
	}
}

func TestStalenessGuardsRequireAnIdentityChange(t *testing.T) {
	for _, everyday := range []string{
		"good morning leaving now",
		"i left the keys on the table",
		"she joined the call late",
	} {
		if ContainsAnyPhrase(everyday, stalenessGuardPhrases) {
			t.Errorf("everyday speech must not be a staleness guard: %q", everyday)
		}
	}
	for _, real := range []string{
		"she moved to berlin last month",
		"he is now at acme as head of product",
		"i have a new role at stripe",
		"she is no longer at the company",
	} {
		if !ContainsAnyPhrase(real, stalenessGuardPhrases) {
			t.Errorf("a genuine identity/role change must still be a staleness guard: %q", real)
		}
	}
}

func TestActionableQuestion_RejectsTransactional(t *testing.T) {
	junk := []string{
		"Questions about your order?",
		"How did we do?",
		"Was this helpful?",
		"Manage your subscription?",
	}
	for _, text := range junk {
		if ActionableQuestion(text) {
			t.Errorf("ActionableQuestion(%q) = true, want false (transactional/bulk)", text)
		}
	}
	genuine := []string{"the deck?", "thoughts?", "Can you review this?"}
	for _, text := range genuine {
		if !ActionableQuestion(text) {
			t.Errorf("ActionableQuestion(%q) = false, want true (genuine)", text)
		}
	}
}

func TestGmailActionableAsk_StrictForEmail(t *testing.T) {
	pass := []string{
		"Can you send the deck?",
		"When can you review the doc?",
		"Would you mind introing me to Sam?",
		"Where did we land on pricing?",
		"Do you have the signed contract?",
	}
	for _, text := range pass {
		if !GmailActionableAsk(text) {
			t.Errorf("GmailActionableAsk(%q) = false, want true (genuine email ask)", text)
		}
	}
	// Bare-"?" marketing/CTA with no interrogative opener and no direct request:
	// iMessage keeps these (real terse conversation) but Gmail must reject them.
	reject := []string{
		"Questions about your order?", // transactional (also fails ActionableQuestion)
		"Ready to upgrade?",           // marketing CTA
		"Save big this weekend?",      // promo hook
		"Last chance?",                // urgency hook
	}
	for _, text := range reject {
		if GmailActionableAsk(text) {
			t.Errorf("GmailActionableAsk(%q) = true, want false (non-obligation email)", text)
		}
	}
}

func TestClassifyEvidencePolicy(t *testing.T) {
	at := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   ClassifierInput
		want string
	}{
		{"service", ClassifierInput{Memory: memory.Memory{Source: "manual", Type: "task"}, SignalText: "i will send the contract", ServiceOnly: true, At: at}, ""},
		{"open loop", ClassifierInput{Memory: memory.Memory{Source: "manual", Type: "task"}, SignalText: "send the contract", At: at}, OpenLoops},
		{"unresolved", ClassifierInput{Memory: memory.Memory{Source: "manual"}, SignalText: "the decision is still pending", At: at}, Unresolved},
		{"staleness", ClassifierInput{Memory: memory.Memory{Source: "manual"}, SignalText: "she moved to berlin", At: at}, Staleness},
		{"context", ClassifierInput{Memory: memory.Memory{Source: "manual"}, SignalText: "the pilot roadmap", At: at}, SharedContext},
		{"future context", ClassifierInput{Memory: memory.Memory{Source: "manual"}, SignalText: "the pilot roadmap", OccurredAt: at.Add(time.Hour), At: at}, ""},
		{"noise", ClassifierInput{Memory: memory.Memory{Source: "manual"}, SignalText: "hello there", At: at}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyEvidence(tc.in); got != tc.want {
				t.Fatalf("ClassifyEvidence() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUserOwnedOpenLoopPolicy(t *testing.T) {
	self := map[string]bool{"me@example.com": true}
	cases := []struct {
		name   string
		m      memory.Memory
		signal string
		self   map[string]bool
		want   bool
	}{
		{"manual task", memory.Memory{Type: "task", Source: "manual"}, "", self, true},
		{"provider task", memory.Memory{Type: "task", Provider: "github", Source: "github"}, "", self, false},
		{"imessage empty", memory.Memory{Provider: "imessage", Text: "# heading\n* bullet"}, "", self, false},
		{"imessage trivia", memory.Memory{Provider: "imessage", Text: "Sam: my dog is cute"}, "", self, false},
		{"imessage my promise", memory.Memory{Provider: "imessage", Text: "Me: I'll send it"}, "", self, true},
		{"imessage my question", memory.Memory{Provider: "imessage", Text: "Me: the deck?"}, "", self, true},
		{"imessage request", memory.Memory{Provider: "imessage", Text: "Sam: can you send it"}, "", self, true},
		{"imessage question", memory.Memory{Provider: "imessage", Text: "Sam: the deck?"}, "", self, true},
		{"imessage chat", memory.Memory{Provider: "imessage", Text: "Sam: hello there"}, "", self, false},
		{"gmail no identity", memory.Memory{Provider: "gmail"}, "can you send it", nil, false},
		{"gmail outbound promise", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"me@example.com"}}}, "i will send it", self, true},
		{"gmail outbound ask", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"me@example.com"}}}, "can you review this?", self, true},
		{"gmail outbound chat", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"me@example.com"}}}, "hello", self, false},
		{"gmail inbound request", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"sam@example.com"}, "to": []string{"me@example.com"}}}, "please review it", self, true},
		{"gmail inbound ask", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"sam@example.com"}, "to": []string{"me@example.com"}}}, "where is the deck?", self, true},
		{"gmail inbound chat", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"sam@example.com"}, "to": []string{"me@example.com"}}}, "hello", self, false},
		{"gmail mixed senders", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"me@example.com", "sam@example.com"}, "to": []string{"me@example.com"}}}, "can you send it?", self, false},
		{"gmail not addressed", memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"sam@example.com"}, "to": []string{"other@example.com"}}}, "can you send it?", self, false},
		{"manual promise", memory.Memory{Source: "manual"}, "i will send it", self, true},
		{"mcp chat", memory.Memory{Source: "mcp"}, "hello", self, false},
		{"unknown", memory.Memory{Source: "connector"}, "i will send it", self, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UserOwnedOpenLoop(tc.m, tc.signal, tc.self); got != tc.want {
				t.Fatalf("UserOwnedOpenLoop() = %v, want %v", got, tc.want)
			}
		})
	}
}
func TestClassificationHelperBranches(t *testing.T) {
	if !IsIMessage(memory.Memory{ProviderID: "imessage/chat/1"}) || IsIMessage(memory.Memory{Type: "imessage"}) {
		t.Fatal("iMessage provider identity drift")
	}
	if DirectRequest("hello") || !DirectRequest("please send it") {
		t.Fatal("direct-request boundary")
	}
	if PersonalTriviaOnlyMeeting("the pilot roadmap") {
		t.Fatal("material context marked as trivia")
	}
	if got := SelfNameTokens(map[string]bool{"xx@example.com": true, "ada": true}); !got["xx"] || !got["ada"] {
		t.Fatal(got)
	}
	if AssignedToThirdParty("action item for", map[string]bool{}) {
		t.Fatal("empty assignee")
	}
	if AssignedToThirdParty("plain text", map[string]bool{}) {
		t.Fatal("plain text")
	}
	for _, text := range []string{"", "# heading", "* bullet", "no colon", "Sam:"} {
		if speaker, body := LastConversationLine(text); speaker != "" || body != "" {
			t.Fatalf("LastConversationLine(%q)=(%q,%q)", text, speaker, body)
		}
	}
}
