package mora

import (
	"testing"
)

// imsgNamed builds a 1:1 iMessage memory whose participant handle carries an
// address-book-resolved contact name (the trusted-name provenance P13's phone side
// relies on).
func imsgNamed(id, handle, name string) Memory {
	return Memory{
		ID: id, Scope: "personal", Type: "imessage", Title: id, CreatedAt: "2026-05-01T00:00:00Z", Text: "hi",
		Meta: map[string]any{"participants": []map[string]string{{"handle": handle, "name": name}}, "message_count": "1"},
	}
}

// candidateFor returns the proposed candidate joining handle<->email, or nil.
func candidateFor(cands []mergeCandidate, handle, email string) *mergeCandidate {
	for i := range cands {
		if personIdentity(cands[i].PhoneID) == handle && personIdentity(cands[i].EmailID) == email {
			return &cands[i]
		}
	}
	return nil
}

// TestP13CandidateProposedWithAddressBookAndSignature proves the CONFIRM-tier default
// path: a phone handle with a distinctive address-book contact name + an email that
// self-presents the same name AND whose local part echoes a name token is PROPOSED
// (queued) — never auto-merged.
func TestP13CandidateProposedWithAddressBookAndSignature(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "Riya Sharma"),
		senderEmail("t1", "riya@acme.com", "Riya Sharma", "me@x.com"),
	}
	res := buildGraphResult(mems, nil)
	c := candidateFor(res.candidates, "+14155550123", "riya@acme.com")
	if c == nil {
		t.Fatalf("expected a proposed email<->phone candidate; got %+v", res.candidates)
	}
	if !contains(c.Echoed, "riya") {
		t.Errorf("candidate should record the echoed name token; echoed=%v", c.Echoed)
	}
	// PROPOSAL ONLY: without a confirm the two identities stay SEPARATE.
	if a, b := entCarrying(res.entities, "+14155550123"), entCarrying(res.entities, "riya@acme.com"); a == "" || b == "" || a == b {
		t.Fatalf("a bare candidate must NOT auto-merge cross-channel: phone=%q email=%q", a, b)
	}
}

// TestP13RefuseNoAddressSignature proves refuse-to-gap: a shared distinctive name
// with NO address echo (the email local part corroborates nothing) is NOT proposed.
func TestP13RefuseNoAddressSignature(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "Riya Sharma"),
		senderEmail("t1", "rs2019@gmail.com", "Riya Sharma", "me@x.com"),
	}
	res := buildGraphResult(mems, nil)
	if c := candidateFor(res.candidates, "+14155550123", "rs2019@gmail.com"); c != nil {
		t.Fatalf("name match with no address signature must refuse-to-gap, got candidate %+v", c)
	}
}

// TestP13RefuseSingleTokenName proves a single-token contact name ("Mom") is not
// distinctive enough to bridge channels and is never proposed.
func TestP13RefuseSingleTokenName(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "Mom"),
		senderEmail("t1", "mom@acme.com", "Mom", "me@x.com"),
	}
	res := buildGraphResult(mems, nil)
	if len(res.candidates) != 0 {
		t.Fatalf("single-token name must not propose a merge, got %+v", res.candidates)
	}
}

// TestP13RefuseCommonName proves a distinctive name borne by too many identities is
// ambiguous — every candidate on it is refused (not queued as noise).
func TestP13RefuseCommonName(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "John Smith"),
		senderEmail("t1", "john@a.com", "John Smith", "me@x.com"),
		senderEmail("t2", "john@b.com", "John Smith", "me@x.com"),
		senderEmail("t3", "john@c.com", "John Smith", "me@x.com"),
		senderEmail("t4", "john@d.com", "John Smith", "me@x.com"),
	}
	res := buildGraphResult(mems, nil)
	if len(res.candidates) != 0 {
		t.Fatalf("a name borne by >maxNameMergeClusters identities must refuse, got %+v", res.candidates)
	}
}

// TestP13RefuseServiceEmail proves an automated/service email is never proposed for
// unification even when it shares a name with a real phone contact.
func TestP13RefuseServiceEmail(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "Neil Patel"),
		senderEmail("t1", "noreply@neilpatel.com", "Neil Patel", "me@x.com"),
	}
	res := buildGraphResult(mems, nil)
	if len(res.candidates) != 0 {
		t.Fatalf("service email must not be proposed, got %+v", res.candidates)
	}
}

// TestP13ConfirmedMergeUnifies is the DONE-criterion unification: a confirmed
// email<->phone pair collapses to ONE person entity carrying both identities.
func TestP13ConfirmedMergeUnifies(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "Riya Sharma"),
		senderEmail("t1", "riya@acme.com", "Riya Sharma", "me@x.com"),
	}
	confirmed := []confirmedMerge{{A: personID("+14155550123"), B: personID("riya@acme.com"), GovID: "gov_x"}}
	res := buildGraphResult(mems, confirmed)
	a := entCarrying(res.entities, "+14155550123")
	b := entCarrying(res.entities, "riya@acme.com")
	if a == "" || a != b {
		t.Fatalf("confirmed email<->phone pair did not unify: phone->%q email->%q", a, b)
	}
	// Provenance: the fusion is recorded as a confirmed merge referencing the ledger id.
	var found bool
	for _, m := range res.merges {
		if m.Signal == sigConfirmed && m.Detail == "gov_x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("confirmed merge must record provenance; merges=%+v", res.merges)
	}
}

// TestP13ConfirmAbsentIdentityIsInert proves a confirm for an identity not in this
// vault is a no-op (no ghost merge), so a stale/foreign confirm can't fabricate a
// person.
func TestP13ConfirmAbsentIdentityIsInert(t *testing.T) {
	mems := []Memory{senderEmail("t1", "riya@acme.com", "Riya Sharma", "me@x.com")}
	confirmed := []confirmedMerge{{A: personID("+19998887777"), B: personID("riya@acme.com"), GovID: "gov_x"}}
	res := buildGraphResult(mems, confirmed)
	for _, e := range personEntities(res.entities) {
		if e.ID == personID("+19998887777") {
			t.Fatalf("a confirm for an absent identity must not mint a person entity: %+v", e)
		}
	}
	for _, m := range res.merges {
		if m.Signal == sigConfirmed {
			t.Fatalf("no confirmed merge should apply when an endpoint is absent; got %+v", m)
		}
	}
}

// TestP13ProvenanceForAutoRules proves the AUTO tiers also record provenance: a
// gmail dot/plus self-merge is tagged same-mailbox; a shared-name+echo merge is
// tagged name-echo.
func TestP13ProvenanceForAutoRules(t *testing.T) {
	mailbox := buildGraphResult([]Memory{
		senderEmail("t1", "alex.owner@gmail.com", "Alex Owner", "me@x.com"),
		senderEmail("t2", "alexowner@gmail.com", "Alex Owner", "me@x.com"),
	}, nil)
	if !hasSignal(mailbox.merges, sigMailbox) {
		t.Errorf("gmail mailbox merge missing same-mailbox provenance: %+v", mailbox.merges)
	}
	name := buildGraphResult([]Memory{
		senderEmail("t1", "riya.sharma@gmail.com", "Riya Sharma", "me@x.com"),
		senderEmail("t2", "riya@work.com", "Riya Sharma", "me@x.com"),
	}, nil)
	if !hasSignal(name.merges, sigNameEcho) {
		t.Errorf("shared-name merge missing name-echo provenance: %+v", name.merges)
	}
}

func hasSignal(links []mergeLink, sig string) bool {
	for _, l := range links {
		if l.Signal == sig {
			return true
		}
	}
	return false
}

// TestP13ConfirmedMergeDeterministic proves the confirm-applied build is byte-stable
// across runs (the ledger is part of vault state; determinism must hold).
func TestP13ConfirmedMergeDeterministic(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "Riya Sharma"),
		senderEmail("t1", "riya@acme.com", "Riya Sharma", "me@x.com"),
	}
	confirmed := []confirmedMerge{{A: personID("+14155550123"), B: personID("riya@acme.com"), GovID: "gov_x"}}
	r1 := buildGraphResult(mems, confirmed)
	r2 := buildGraphResult(mems, confirmed)
	if !sameEntities(r1.entities, r2.entities) || len(r1.merges) != len(r2.merges) {
		t.Fatal("confirmed-merge build is nondeterministic across runs")
	}
}
