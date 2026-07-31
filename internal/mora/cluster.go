package mora

import (
	"strings"
	"time"
)

// Issue #237 — corroborating-record clustering. Runs at result-assembly time,
// post-fusion/pre-truncate, on both the hybrid (hybrid.go) and FTS-only
// (search.go) search paths. See internal/mora/cluster_contract_test.go for the
// frozen (AMENDED) contract this file implements: two minimal OR'd anchor
// rules, a star topology anchored on each cluster's head (no transitive
// closure for Rule 2), and a whole-candidate refusal cap at >5 members.

// clusterWindow is Rule 2's (person-entity + time-window overlap) anchor
// window: two memories link via Rule 2 only when their real-world instants
// (clusterOccurredAt) fall within this of each other — STRICT/exclusive, so
// exactly clusterWindow apart does NOT link. Matches the contract test's
// clusterContractWindowHours (24h).
const clusterWindow = 24 * time.Hour

// clusterMaxMembers is the refusal cap: a candidate cluster (head +
// corroborating) exceeding this many total members is refused whole — every
// one of its records reverts to (and stays) independent. Precision-first: a
// hub/fan-out pattern is a sign the anchor is too weak to safely collapse.
const clusterMaxMembers = 5

// clusterOccurredAt returns m's explicit meta.occurred_at instant, parsed, and
// whether one was present at all. There is NO CreatedAt fallback, ever — a
// record missing meta.occurred_at can never participate in Rule 2, full stop
// (CreatedAt is ingest/write time, not event time, and treating it as an event
// anchor previously collapsed a 200-thread same-sender/same-CreatedAt fixture
// into one giant cluster). Unlike validFromOf/itemOccurredAt (graph.go /
// urgent.go), which intentionally DO fall back to CreatedAt for structural
// graph edges, clustering's real-world-event claim demands the stronger,
// narrower signal.
func clusterOccurredAt(m Memory) (time.Time, bool) {
	if m.Meta == nil {
		return time.Time{}, false
	}
	s, ok := m.Meta["occurred_at"].(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// clusterIdentitySet returns m's lowercased, trimmed participant-identity set —
// the union of Meta["from"], Meta["to"], Meta["cc"], Meta["attendees"],
// Meta["organizer"], and the "handle" of each Meta["participants"] pair.
// Exactly the fields personRefs (graph.go) already reads; no new extraction.
func clusterIdentitySet(m Memory) map[string]bool {
	set := map[string]bool{}
	if m.Meta == nil {
		return set
	}
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			set[s] = true
		}
	}
	for _, key := range []string{"from", "to", "cc", "attendees"} {
		for _, s := range metaStrings(m.Meta[key]) {
			add(s)
		}
	}
	if org, ok := m.Meta["organizer"].(string); ok {
		add(org)
	}
	for _, p := range metaPairs(m.Meta["participants"]) {
		add(p.handle)
	}
	return set
}

// identitySetsIntersect reports whether a and b share at least one identity.
func identitySetsIntersect(a, b map[string]bool) bool {
	// Iterate the smaller set for a cheap deterministic-result membership scan
	// (result is a bool, so map-iteration order cannot affect it).
	if len(b) < len(a) {
		a, b = b, a
	}
	for id := range a {
		if b[id] {
			return true
		}
	}
	return false
}

// clusterProviderLinked is Rule 1 (provider anchor equality): both memories
// carry a non-empty ProviderID and (Provider, ProviderID) is identical. Plain
// equality is already transitive, so no union-find machinery is needed for
// Rule 1 groups of >2 records to be found correctly by repeated pairwise
// checks against a single head.
func clusterProviderLinked(a, b Memory) bool {
	if a.ProviderID == "" || b.ProviderID == "" {
		return false
	}
	return a.Provider == b.Provider && a.ProviderID == b.ProviderID
}

// clusterPersonTimeLinked is Rule 2 (person-entity + time-window overlap,
// NARROWED): both records carry an explicit meta.occurred_at (no fallback),
// their participant-identity sets intersect, and their occurred_at instants
// are within a STRICT clusterWindow of each other.
func clusterPersonTimeLinked(aIdents, bIdents map[string]bool, aAt, bAt time.Time, aOK, bOK bool) bool {
	if !aOK || !bOK {
		return false
	}
	if !identitySetsIntersect(aIdents, bIdents) {
		return false
	}
	diff := aAt.Sub(bAt)
	if diff < 0 {
		diff = -diff
	}
	return diff < clusterWindow
}

// clusterAndTruncate collapses corroborating-record clusters in ranked (the
// full post-fusion/bm25 ranking, best match first, ties already broken
// deterministically by the caller) into one result slot per cluster, then
// truncates to limit.
//
// GREEDY DETERMINISTIC FORMATION (per the frozen contract): walk ranked in
// rank order; the strongest unprocessed record seeds a new candidate cluster
// as its head; scan the remaining unprocessed records in the same rank order
// and collect every one that qualifies against THAT head alone (Rule 1 OR
// Rule 2, pairwise-with-head — a STAR topology, no transitive closure for
// Rule 2). If head+qualifying would exceed clusterMaxMembers, the ENTIRE
// candidate is refused: the head AND every qualifying record are marked
// independent and permanently excluded from clustering with each other (or
// anyone else) — refusal is final, not merely "try a smaller subset next".
// Otherwise the candidate is committed: the qualifying records become the
// head's Corroborating refs (in rank order) and no longer occupy their own
// slot, freeing it for the next-best DISTINCT record further down ranked.
//
// A ranked list touching no cluster at all is returned byte-identical to
// ranked[:limit] (every group is a singleton, so the walk just takes the
// first `limit` entries in order) — clustering is a pure no-op off its own
// trigger condition.
func clusterAndTruncate(ranked []Memory, limit int) []Memory {
	if limit <= 0 || len(ranked) == 0 {
		return nil
	}
	n := len(ranked)
	idents := make([]map[string]bool, n)
	occurred := make([]time.Time, n)
	occurredOK := make([]bool, n)
	for i, m := range ranked {
		idents[i] = clusterIdentitySet(m)
		occurred[i], occurredOK[i] = clusterOccurredAt(m)
	}

	processed := make([]bool, n)         // finalized: either a committed head/member, a singleton, or refused
	memberOf := make([]int, n)           // index of the head this record was folded into, or -1
	membersOfHead := make(map[int][]int) // head index -> its Corroborating member indices, rank order
	for i := range memberOf {
		memberOf[i] = -1
	}

	for i := 0; i < n; i++ {
		if processed[i] {
			continue
		}
		var candidates []int
		for j := i + 1; j < n; j++ {
			if processed[j] {
				continue
			}
			if clusterProviderLinked(ranked[i], ranked[j]) ||
				clusterPersonTimeLinked(idents[i], idents[j], occurred[i], occurred[j], occurredOK[i], occurredOK[j]) {
				candidates = append(candidates, j)
			}
		}
		processed[i] = true
		if len(candidates) == 0 {
			continue // singleton
		}
		if len(candidates)+1 > clusterMaxMembers {
			// Refusal cap: the whole candidate stays independent, permanently —
			// none of these records may cluster with each other on this basis.
			for _, j := range candidates {
				processed[j] = true
			}
			continue
		}
		// Commit: i is the head, candidates are its corroborating members.
		membersOfHead[i] = candidates
		for _, j := range candidates {
			processed[j] = true
			memberOf[j] = i
		}
	}

	out := make([]Memory, 0, limit)
	for i := 0; i < n && len(out) < limit; i++ {
		if memberOf[i] != -1 {
			continue // folded into an earlier head's corroborating refs
		}
		head := ranked[i]
		if members := membersOfHead[i]; len(members) > 0 {
			corr := make([]CorroboratingRef, 0, len(members))
			for _, mi := range members {
				mm := ranked[mi]
				corr = append(corr, CorroboratingRef{ID: mm.ID, Title: mm.Title, Source: mm.Source, CreatedAt: mm.CreatedAt})
			}
			head.Corroborating = corr
		}
		out = append(out, head)
	}
	return out
}
