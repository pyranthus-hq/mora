package mora

import "testing"

// Selection-quality regression tests for the meeting brief. Run live on a real
// vault, the "Your open loops and obligations" section surfaced marketing /
// transactional email fragments and URL shards attributed to a co-recipient
// attendee (e.g. "Questions about your order?", "Is there a pdf export version
// from Ahrefs?", "com/maps/search/...+United+States?"). These pin the gates that
// exclude that noise while keeping genuine, user-owned asks.

func TestStripNoiseTokens(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// Pure URL / address shards collapse to "" (dropped from citation).
		{"maps url shard", "com/maps/search/California+Theatre%0A345+S+First+St,+San+Jose,+CA++95113,+United+States?", ""},
		{"address shard", ",+Dublin,+CA+94568?", ""},
		{"calendar path shard", "com/calendar/event?", ""},
		{"bare https token", "https://doordash.com/dashpass-redeem/?code=abc", ""},
		// A genuine ask that merely contains a link keeps its prose (P1 guard).
		{"ask with inline link kept", "can you review https://doc.com/x before Friday?", "can you review before Friday?"},
		{"utm token stripped, prose kept", "click here utm_source=newsletter", "click here"},
		// Slash pairs, dates, and normal prose are untouched.
		{"slash pair prose kept", "should we do A/B or C/D testing?", "should we do A/B or C/D testing?"},
		{"yes/no kept", "yes/no?", "yes/no?"},
		{"two states kept", "CA/NY?", "CA/NY?"},
		{"date question kept", "Can you meet 3/15 or 4/16?", "Can you meet 3/15 or 4/16?"},
		{"genuine statement kept", "We agreed to ship the pilot next week.", "We agreed to ship the pilot next week."},
	}
	for _, tc := range cases {
		if got := stripNoiseTokens(tc.in); got != tc.want {
			t.Errorf("%s: stripNoiseTokens(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
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
		if actionableQuestion(text) {
			t.Errorf("actionableQuestion(%q) = true, want false (transactional/bulk)", text)
		}
	}
	genuine := []string{"the deck?", "thoughts?", "Can you review this?"}
	for _, text := range genuine {
		if !actionableQuestion(text) {
			t.Errorf("actionableQuestion(%q) = false, want true (genuine)", text)
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
		if !gmailActionableAsk(text) {
			t.Errorf("gmailActionableAsk(%q) = false, want true (genuine email ask)", text)
		}
	}
	// Bare-"?" marketing/CTA with no interrogative opener and no direct request:
	// iMessage keeps these (real terse conversation) but Gmail must reject them.
	reject := []string{
		"Questions about your order?", // transactional (also fails actionableQuestion)
		"Ready to upgrade?",           // marketing CTA
		"Save big this weekend?",      // promo hook
		"Last chance?",                // urgency hook
	}
	for _, text := range reject {
		if gmailActionableAsk(text) {
			t.Errorf("gmailActionableAsk(%q) = true, want false (non-obligation email)", text)
		}
	}
}
