package search

import (
	"github.com/pyranthus-hq/mora/internal/memory"
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
func clusterOccurredAt(m memory.Memory) (time.Time, bool) {
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
func clusterIdentitySet(m memory.Memory) map[string]bool {
	set := map[string]bool{}
	if m.Meta == nil {
		return set
	}
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			return
		}
		// Reuse personRefs' (graph.go) structural-noise filter rather than forking
		// the list: a GitHub notification field label ("push"/"author"/…) is not a
		// real participant identity, so an identity-set intersection that only
		// coincides on a noise token must not count as Rule 2 overlap (round-2/P2
		// hardening, TestClusterContractRule2IdentityNoiseNearMiss).
		if isStructuralNoise(s) {
			return
		}
		set[s] = true
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
func clusterProviderLinked(a, b memory.Memory) bool {
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

// clusterAndTruncate collapses corroborating-record clusters into one result
// slot per cluster, then applies the frozen LEGACY SLOT DISCIPLINE (round-2
// backfill P1 fix, #237):
//
//	rawIDs is the FULL candidate pool's ids in rank order, BEFORE visibility
//	filtering (suppressPendingDeletes / currentMemories) removed anything —
//	exactly what a plain SQL-LIMIT-then-filter legacy query would have
//	fetched. Its first `limit` ids are THE WINDOW.
//
//	visible is rawIDs' pool AFTER visibility filtering, in the SAME relative
//	rank order (a subsequence of rawIDs) — this is what gets clustered.
//
// A window id that visibility filtering removed entirely (absent from
// visible) COSTS its slot outright — it is never backfilled, matching
// legacy SQL-LIMIT-then-filter semantics exactly. A window id that survives
// but gets folded into a cluster head — necessarily also a window id,
// because the greedy walk below only ever forms a head from an EARLIER
// (or equal) rank position than its members, so a member's head can never
// rank later than the member itself — frees its slot, backfilled from
// visible rows BEYOND the window, in rank order, skipping suppressed and
// already-absorbed rows. A pool with no suppression and no clustering
// degenerates to visible[:limit], byte-identical to pre-#237 output.
//
// GREEDY DETERMINISTIC FORMATION (per the frozen contract): walk visible in
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
// slot, freeing it per the window/backfill rule above.
func ClusterAndTruncate(rawIDs []string, visible []memory.Memory, limit int, annotate func([]memory.Memory, []memory.Memory) []memory.Memory) []memory.Memory {
	if limit <= 0 {
		return nil
	}
	n := len(visible)
	idents := make([]map[string]bool, n)
	occurred := make([]time.Time, n)
	occurredOK := make([]bool, n)
	for i, m := range visible {
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
			if clusterProviderLinked(visible[i], visible[j]) ||
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

	windowN := limit
	if windowN > len(rawIDs) {
		windowN = len(rawIDs)
	}
	inWindow := make(map[string]bool, windowN)
	for _, id := range rawIDs[:windowN] {
		inWindow[id] = true
	}

	buildRow := func(i int) memory.Memory {
		head := visible[i]
		if members := membersOfHead[i]; len(members) > 0 {
			corr := make([]memory.CorroboratingRef, 0, len(members))
			for _, mi := range members {
				mm := visible[mi]
				corr = append(corr, memory.CorroboratingRef{ID: mm.ID, Title: mm.Title, Source: mm.Source, CreatedAt: mm.CreatedAt})
			}
			head.Corroborating = corr
		}
		return head
	}

	// direct = window rows that survive as their own top-level row (head or
	// singleton), in rank order. backfillCandidates = beyond-window rows that
	// survive as their own top-level row, in rank order — the only pool a freed
	// window slot may draw from. backfillBudget counts window rows that were
	// folded into a (necessarily in-window) head: exactly the number of slots
	// legitimately freed for backfill. A window row removed by visibility
	// filtering is simply absent from `visible` and contributes to neither —
	// its slot is gone, full stop.
	//
	// direct is deliberately make()'d (non-nil) rather than a nil var: both
	// callers' legacy (pre-#237) pipelines always ran their pool through
	// currentMemories, whose filterCurrentMemories unconditionally builds a
	// fresh `make([]memory.Memory, 0, len(in))` slice — so a legacy zero-result
	// answer (e.g. every window row visibility-filtered away) was always a
	// non-nil empty slice, never nil, and marshals to JSON `[]`, never
	// `null`. A nil `direct` would flip that byte for the in-window-all-
	// suppressed case specifically; since append on a non-nil slice always
	// stays non-nil (even appending zero elements), building on top of a
	// make()'d slice here preserves that legacy byte exactly, without
	// touching the untouched-by-clustering nil/null paths (e.g. an
	// empty/punctuation-only query, which short-circuits before ever
	// reaching this function).
	direct := make([]memory.Memory, 0, windowN)
	var backfillCandidates []memory.Memory
	backfillBudget := 0
	for i := 0; i < n; i++ {
		if memberOf[i] != -1 {
			if inWindow[visible[i].ID] {
				backfillBudget++
			}
			continue // folded into a head's corroborating refs, never its own row
		}
		if inWindow[visible[i].ID] {
			direct = append(direct, buildRow(i))
		} else {
			backfillCandidates = append(backfillCandidates, buildRow(i))
		}
	}
	if backfillBudget > len(backfillCandidates) {
		backfillBudget = len(backfillCandidates)
	}
	out := append(direct, backfillCandidates[:backfillBudget]...)
	if len(out) > limit {
		out = out[:limit] // safety net; direct+backfillBudget <= windowN <= limit by construction
	}
	return annotate(out, visible)
}

func isStructuralNoise(handle string) bool {
	handle = strings.ToLower(strings.TrimSpace(handle))
	switch handle {
	case "push", "author", "mention", "ci activity", "state change", "ci-activity", "state-change":
		return true
	}
	return false
}
func metaStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, s := range t {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t != "" {
			return []string{t}
		}
	}
	return nil
}

type metaPair struct{ handle, name string }

func metaPairs(v any) []metaPair {
	var out []metaPair
	switch items := v.(type) {
	case []map[string]string:
		for _, p := range items {
			out = append(out, metaPair{p["handle"], p["name"]})
		}
	case []any:
		for _, it := range items {
			switch p := it.(type) {
			case map[string]any:
				h, _ := p["handle"].(string)
				n, _ := p["name"].(string)
				out = append(out, metaPair{h, n})
			case map[string]string:
				out = append(out, metaPair{p["handle"], p["name"]})
			}
		}
	}
	return out
}
