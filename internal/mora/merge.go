package mora

import (
	"sort"
	"strings"
)

// P13 — tiered merge-confidence for person-entity unification.
//
// Mora unifies the many source-native identities of one human into a single
// canonical person. Precision is the first principle: a wrong-person merge is a
// severity-1 error, so identity clustering runs in three tiers, ordered by how
// PROVABLE the "same human" claim is:
//
//   - AUTO  — RULE 1 (same provider mailbox) and RULE 2 (echo-corroborated shared
//     name), both in graph.go. These are byte-provable same-identity within the
//     email channel and merge with no user action. NEVER loosened.
//   - CONFIRM (one-tap) — cross-channel email<->phone candidates. There is NO
//     byte-provable shared token across channels (a phone handle carries no address
//     to echo), so these are proposed to the user via the governance confirm-queue
//     and applied ONLY on an explicit merge_confirm(confirm). See emailPhoneCandidates.
//   - REFUSE (refuse-to-gap) — everything ambiguous: a too-common name, a name with
//     no address "signature", a single-token name, a service/org. Left UNMERGED.
//     Mora never guesses a merge; it gaps.
//
// The confirm-queue is keyed on SOURCE-NATIVE stable atoms (the #52 trap): an
// {imessage,handle,+1…} <-> {,address,a@b.com} pair, never a post-merge person: id.

// merge signal labels — the provenance recorded on every merge edge (why two
// identities were fused). Durable in the person_merges index table.
const (
	sigMailbox   = "same-mailbox" // RULE 1: provider-equivalent mailbox
	sigNameEcho  = "name-echo"    // RULE 2: distinctive shared name, address-echo corroborated
	sigConfirmed = "confirmed"    // RULE 3: user-confirmed via the one-tap confirm-queue
)

// confirmedMerge is a user-confirmed same-person pair, resolved from the governance
// ledger to the two PRE-MERGE graph person ids it unifies. GovID is the ledger entry
// that authorized it (provenance detail). Only this — an explicit human decision —
// unifies cross-channel identities.
type confirmedMerge struct {
	A, B  string // person ids (personID of each source atom); A<B not guaranteed
	GovID string
}

// mergeLink is one provenance edge: identity A and identity B were fused, justified
// by Signal (Detail carries the shared name for name-echo, or the gov entry id for a
// confirmed merge). Endpoints are SOURCE person ids (pre-merge), never the canonical —
// so the record stays truthful about which atoms were joined and by what evidence.
type mergeLink struct {
	A, B   string
	Signal string
	Detail string
}

// mergeCandidate is a proposed email<->phone unification awaiting one-tap confirm.
// It is only ever a PROPOSAL: a wrong candidate is queue noise the user rejects,
// never a merge. PhoneID/EmailID are pre-merge person ids; Name is the shared
// distinctive address-book contact name; Echoed are the name tokens the email
// address's local part corroborates (the "shared signature").
type mergeCandidate struct {
	PhoneID string
	EmailID string
	Name    string
	Echoed  []string
}

// personIdentity strips the "person:" prefix to the raw source identity.
func personIdentity(id string) string { return strings.TrimPrefix(id, "person:") }

// idIsEmail reports whether a person id is an email-addressed identity.
func idIsEmail(id string) bool { return strings.Contains(personIdentity(id), "@") }

// idIsPhoneHandle reports whether a person id is a non-email handle (an iMessage
// phone/handle identity). Combined with a person-kind check by the caller, this
// isolates real phone people from shortcodes/orgs/services.
func idIsPhoneHandle(id string) bool {
	ident := personIdentity(id)
	return ident != "" && !strings.Contains(ident, "@")
}

// echoTokens returns the name tokens whose text also appears as a local-part token
// of the email id — the "shared signature" that corroborates the address belongs to
// the named person (not just a spoofable display name). Empty => no corroboration.
func echoTokens(emailID string, nameToks []string) []string {
	toks := addrTokens(emailID)
	if len(toks) == 0 {
		return nil
	}
	var echoed []string
	for _, t := range nameToks {
		if toks[t] {
			echoed = append(echoed, t)
		}
	}
	return echoed
}

// emailPhoneCandidates proposes CONFIRM-tier email<->phone unifications from the
// pre-merge person aggregates. A candidate needs BOTH signals of the default path:
//
//   - address-book corroboration: a phone handle carries a distinctive (multi-token)
//     TRUSTED contact name — resolved from the user's own AddressBook — that an email
//     PERSON also self-presents (the name bridges the two channels); AND
//   - shared-signature: the email address's local part echoes >=1 token of that name
//     (echoTokens), so the address structurally corroborates the identity.
//
// Precision guards (all -> REFUSE, i.e. not proposed):
//   - single-token names are not distinctive (trustedPersonNames drops them);
//   - a name borne by more than maxNameMergeClusters distinct identities is a common
//     name — ambiguous, so asking would be noise;
//   - no address echo -> the address gives zero corroboration.
//
// Pure and deterministic (sorted output). Proposal only: nothing here merges.
func emailPhoneCandidates(persons map[string]*personAgg) []mergeCandidate {
	ids := sortedPersonIDs(persons)

	kindOf := make(map[string]string, len(ids))
	names := make(map[string][]string, len(ids)) // id -> distinctive trusted names
	carriers := map[string]map[string]bool{}     // name -> ids carrying it
	for _, id := range ids {
		p := persons[id]
		kindOf[id] = personKindOf(id, p)
		ns := trustedPersonNames(p)
		names[id] = ns
		for _, n := range ns {
			if carriers[n] == nil {
				carriers[n] = map[string]bool{}
			}
			carriers[n][id] = true
		}
	}

	var out []mergeCandidate
	for _, phoneID := range ids {
		if kindOf[phoneID] != "person" || !idIsPhoneHandle(phoneID) {
			continue
		}
		for _, n := range names[phoneID] {
			if len(carriers[n]) > maxNameMergeClusters {
				continue // too-common a name -> refuse (ambiguous)
			}
			nameToks := strings.Fields(n)
			emailIDs := make([]string, 0, len(carriers[n]))
			for eid := range carriers[n] {
				emailIDs = append(emailIDs, eid)
			}
			sort.Strings(emailIDs)
			for _, eid := range emailIDs {
				if eid == phoneID || kindOf[eid] != "person" || !idIsEmail(eid) {
					continue
				}
				echoed := echoTokens(eid, nameToks)
				if len(echoed) == 0 {
					continue // no address signature -> refuse-to-gap
				}
				out = append(out, mergeCandidate{PhoneID: phoneID, EmailID: eid, Name: n, Echoed: echoed})
			}
		}
	}

	// Near-name forms are confirmation candidates, never automatic merges. This
	// covers address-book variants such as "Dan Rachev"/"Daniel Rachev" and a
	// distinctive first-name-only contact such as "Samika"/"Samika Karode". The
	// shared anchor must be rare, the email must still echo its own trusted name,
	// and a human must still confirm the source-native pair.
	type namedIdentity struct{ id, name string }
	buckets := map[string][]namedIdentity{}
	for _, id := range ids {
		if kindOf[id] != "person" {
			continue
		}
		for _, name := range trustedCandidateNames(persons[id]) {
			for _, key := range candidateNameAnchors(name) {
				buckets[key] = append(buckets[key], namedIdentity{id: id, name: name})
			}
		}
	}
	bucketKeys := make([]string, 0, len(buckets))
	for key := range buckets {
		bucketKeys = append(bucketKeys, key)
	}
	sort.Strings(bucketKeys)
	for _, key := range bucketKeys {
		bucket := buckets[key]
		unique := map[string]bool{}
		for _, item := range bucket {
			unique[item.id] = true
		}
		if len(unique) > maxNameMergeClusters {
			continue
		}
		for _, phone := range bucket {
			if !idIsPhoneHandle(phone.id) {
				continue
			}
			for _, email := range bucket {
				if !idIsEmail(email.id) || phone.name == email.name || !compatibleNearName(phone.name, email.name) {
					continue
				}
				echoed := echoTokens(email.id, strings.Fields(email.name))
				if len(echoed) == 0 {
					continue
				}
				out = append(out, mergeCandidate{
					PhoneID: phone.id, EmailID: email.id,
					Name: phone.name + " / " + email.name, Echoed: echoed,
				})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].PhoneID != out[j].PhoneID {
			return out[i].PhoneID < out[j].PhoneID
		}
		if out[i].EmailID != out[j].EmailID {
			return out[i].EmailID < out[j].EmailID
		}
		return out[i].Name < out[j].Name
	})
	// Dedup identical (phone,email,name) rows (a phone may carry the same name twice
	// across aliases; a candidate is uniquely a phone<->email<->name triple).
	dedup := out[:0]
	var last mergeCandidate
	for i, c := range out {
		if i > 0 && c.PhoneID == last.PhoneID && c.EmailID == last.EmailID && c.Name == last.Name {
			continue
		}
		dedup = append(dedup, c)
		last = c
	}
	return dedup
}

// trustedCandidateNames is intentionally a little broader than the auto-merge
// key: a single alphabetic name of at least four characters can be useful in a
// human confirmation proposal, but can never authorize a merge by itself.
func trustedCandidateNames(p *personAgg) []string {
	var out []string
	for _, alias := range sortedKeys(p.aliases) {
		if strings.ContainsRune(alias, '@') || strings.HasPrefix(alias, "+") {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(alias))
		fields := strings.Fields(name)
		if len(fields) == 0 || len(fields) == 1 && !distinctiveSingleName(fields[0]) {
			continue
		}
		out = append(out, name)
	}
	return out
}

func distinctiveSingleName(name string) bool {
	if len([]rune(name)) < 4 {
		return false
	}
	for _, r := range name {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func candidateNameAnchors(name string) []string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return nil
	}
	out := []string{"first:" + fields[0]}
	if len(fields) > 1 {
		out = append(out, "last:"+fields[len(fields)-1])
	}
	return out
}

func compatibleNearName(a, b string) bool {
	aa, bb := strings.Fields(a), strings.Fields(b)
	if len(aa) == 0 || len(bb) == 0 {
		return false
	}
	if len(aa) == 1 || len(bb) == 1 {
		single, full := aa, bb
		if len(bb) == 1 {
			single, full = bb, aa
		}
		return len(full) > 1 && single[0] == full[0]
	}
	if aa[len(aa)-1] != bb[len(bb)-1] {
		return false
	}
	firstA, firstB := aa[0], bb[0]
	return len(firstA) >= 3 && len(firstB) >= 3 &&
		(strings.HasPrefix(firstA, firstB) || strings.HasPrefix(firstB, firstA))
}
