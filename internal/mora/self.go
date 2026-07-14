package mora

import "strings"

// selfEmails returns the user's own lowercased addresses: Source.Email over enabled
// gmail/calendar sources, PLUS any aliases declared in config.toml `self_emails`.
// Used to exclude self from the attendee list. Empty when no Google account is
// connected (iMessage-only vault): self-exclusion becomes a no-op.
//
// The config aliases exist because the mailbox OAuth was granted on is frequently
// NOT the address a calendar invites (a Workspace alias, a custom domain). An
// unrecognized alias fails self-exclusion, so the user is admitted as an attendee of
// their own meeting and their own records are cited back as the counterparty's
// unfinished business — wrong-person attribution. Declared, never inferred.
func selfEmails(cfg Config) map[string]bool {
	out := map[string]bool{}
	for _, s := range loadSourcesOrEmpty(cfg) {
		if (s.Type == "gmail" || s.Type == "calendar") && s.Email != "" {
			out[strings.ToLower(s.Email)] = true
		}
	}
	for _, alias := range cfg.SelfEmails {
		if a := strings.ToLower(strings.TrimSpace(alias)); a != "" {
			out[a] = true
		}
	}
	return out
}
