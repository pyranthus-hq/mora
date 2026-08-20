package mora

import ()

// imsgNamed builds a 1:1 iMessage memory whose participant handle carries an
// address-book-resolved contact name (the trusted-name provenance P13's phone side
// relies on).

// candidateFor returns the proposed candidate joining handle<->email, or nil.

// TestP13CandidateProposedWithAddressBookAndSignature proves the CONFIRM-tier default
// path: a phone handle with a distinctive address-book contact name + an email that
// self-presents the same name AND whose local part echoes a name token is PROPOSED
// (queued) — never auto-merged.

// TestP13RefuseNoAddressSignature proves refuse-to-gap: a shared distinctive name
// with NO address echo (the email local part corroborates nothing) is NOT proposed.

// TestIssue219NearNamesBecomeCandidates pins the two conservative fuzzy forms
// observed in the live vault. They enter the human queue, but never auto-merge.

// TestP13RefuseSingleTokenName proves a single-token contact name ("Mom") is not
// distinctive enough to bridge channels and is never proposed.

// TestP13RefuseCommonName proves a distinctive name borne by too many identities is
// ambiguous — every candidate on it is refused (not queued as noise).

// TestP13RefuseServiceEmail proves an automated/service email is never proposed for
// unification even when it shares a name with a real phone contact.

// TestP13ConfirmedMergeUnifies is the DONE-criterion unification: a confirmed
// email<->phone pair collapses to ONE person entity carrying both identities.

// TestP13ConfirmAbsentIdentityIsInert proves a confirm for an identity not in this
// vault is a no-op (no ghost merge), so a stale/foreign confirm can't fabricate a
// person.

// TestP13ProvenanceForAutoRules proves the AUTO tiers also record provenance: a
// gmail dot/plus self-merge is tagged same-mailbox; a shared-name+echo merge is
// tagged name-echo.

// TestP13ConfirmedMergeDeterministic proves the confirm-applied build is byte-stable
// across runs (the ledger is part of vault state; determinism must hold).
