package mora

// junkPattern names one known July 2026 meeting-brief failure signature and the
// frozen input that reproduces it. Patterns are regular expressions so the
// hard-wrap signature can match only the broken, sentence-ending fragment
// without rejecting the complete, legitimate sentence.
type junkPattern struct {
	pattern       string
	defectClass   string
	sourceFixture string
}

// sabotageJunkPatterns is the single scorer table for the frozen July 2026
// incident. Keep the signatures here rather than copying literals between the
// replay, self-check, and invariance gates.
var sabotageJunkPatterns = []junkPattern{
	{`(?i)ctk-rdtz-jnx`, "url-shard", "rsvp-meet-url.md"},
	{`(?i)prior to the\s*$`, "hard-wrap-fragment", "wrapped-third-party.md"},
	{`(?i)depending on`, "substring-pending", "vendor-quote.md"},
	{`(?i)need help\?`, "invite-footer", "teams-footer.md"},
	{`(?i)declined: sync up meeting`, "rsvp-notification", "rsvp-meet-url.md"},
	{`(?i)action item for kim`, "third-party-action", "wrapped-third-party.md"},
	{`(?i)is there a pdf export version from ahrefs\?`, "ambiguous-group-ask", "wrapped-third-party.md"},
	{`(?i)open to see how the loop works\?`, "forwarded-cta", "forwarded-marketing.md"},
	{`(?i)based on our conversation, here are the next steps and deliverables:`, "lead-in", "wrapped-third-party.md"},
	{`(?i)fwd: google ads account audit`, "forwarded-subject", "forwarded-marketing.md"},
	{`(?i)good morning leaving now`, "everyday-staleness", "imessage-self-line.md"},
	{`(?i)me:\s*good morning`, "self-spoken-imessage", "imessage-self-line.md"},
	{`(?i)was this helpful\?`, "generic-footer-question", "teams-footer.md"},
}
