package mora

import "testing"

// Selection-quality regression tests for the meeting brief. Run live on a real
// vault, the "Your open loops and obligations" section surfaced marketing /
// transactional email fragments and URL shards attributed to a co-recipient
// attendee (e.g. "Questions about your order?", "Is there a pdf export version
// from Ahrefs?", "com/maps/search/...+United+States?"). These pin the gates that
// exclude that noise while keeping genuine, user-owned asks.

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
