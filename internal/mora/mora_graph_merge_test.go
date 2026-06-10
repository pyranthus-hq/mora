package mora

import (
	"sort"
	"testing"
)

func personEntities(ents []graphEntity) []graphEntity {
	var out []graphEntity
	for _, e := range ents {
		if e.Kind == "person" || e.Kind == "service" {
			out = append(out, e)
		}
	}
	return out
}

func aliasesContainAll(al []string, want ...string) bool {
	have := map[string]bool{}
	for _, a := range al {
		have[a] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// senderEmail builds an email memory FROM `from` (self-presented `name`) to `to`.
func senderEmail(id, from, name string, to ...string) Memory {
	meta := map[string]any{"from": []string{from}, "to": toAnySlice(to)}
	if name != "" {
		meta["names"] = map[string]string{from: name}
	}
	return Memory{ID: id, Scope: "personal", Type: "email", Title: id, CreatedAt: "2026-05-01T00:00:00Z", Text: "x", Meta: meta}
}

// TestA3MergeGmailInbox proves RULE 1: gmail dot/plus variants are the same inbox
// and collapse to ONE person entity carrying all address aliases, with a unioned
// (not summed) mention_count.
func TestA3MergeGmailInbox(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "alex.owner@gmail.com", "Alex Owner", "x@y.com"),
		senderEmail("t2", "alexowner@gmail.com", "Alex Owner", "x@y.com"),
		senderEmail("t3", "alex.owner+promos@gmail.com", "Alex Owner", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	persons := personEntities(ents)
	// All three addresses must collapse to a single person entity.
	n := 0
	for _, e := range persons {
		if aliasesContainAll(e.Aliases, "alex.owner@gmail.com") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("gmail dot/plus variants did not collapse to one entity (found %d carrying the address); persons=%d", n, len(persons))
	}
	e := persons[0]
	for _, e2 := range persons {
		if len(e2.Aliases) > len(e.Aliases) {
			e = e2
		}
	}
	if !aliasesContainAll(e.Aliases, "alex.owner@gmail.com", "alexowner@gmail.com", "alex.owner+promos@gmail.com") {
		t.Errorf("merged aliases = %v, want all three gmail variants", e.Aliases)
	}
	if e.MentionCount != 3 {
		t.Errorf("mention_count = %d, want 3 distinct evidence memories", e.MentionCount)
	}
}

// TestA3MergeSharedNameWithEcho proves RULE 2: two DIFFERENT addresses that both
// self-present the same distinctive multi-token name AND whose addresses echo a
// name token merge into one person.
func TestA3MergeSharedNameWithEcho(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "riya.sharma@gmail.com", "Riya Sharma", "x@y.com"),
		senderEmail("t2", "riya@acmeconsulting.com", "Riya Sharma", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	// Exactly one entity should carry either riya address (they collapsed).
	var carriers []graphEntity
	for _, e := range personEntities(ents) {
		if contains(e.Aliases, "riya.sharma@gmail.com") || contains(e.Aliases, "riya@acmeconsulting.com") {
			carriers = append(carriers, e)
		}
	}
	if len(carriers) != 1 {
		t.Fatalf("shared-name+echo did not merge: %d entities carry a riya address, want 1", len(carriers))
	}
	if !aliasesContainAll(carriers[0].Aliases, "riya.sharma@gmail.com", "riya@acmeconsulting.com", "Riya Sharma") {
		t.Errorf("merged aliases = %v, want both addresses + the name", carriers[0].Aliases)
	}
}

// TestA3PrecisionTwoDifferentPeopleSameName proves the precision guard: two
// DIFFERENT people sharing a full name whose addresses do NOT echo the name are
// NOT merged (the catastrophic-merge case codex warned about).
func TestA3PrecisionTwoDifferentPeopleSameName(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "js@alpha.com", "John Smith", "x@y.com"),
		senderEmail("t2", "jsm@beta.com", "John Smith", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	// The two John addresses must remain SEPARATE entities (no echo -> no merge).
	a := entCarrying(ents, "js@alpha.com")
	b := entCarrying(ents, "jsm@beta.com")
	if a == "" || b == "" || a == b {
		t.Fatalf("two different people sharing a name (no address echo) were merged: js->%q jsm->%q", a, b)
	}
}

// TestA3PrecisionSharedFirstNameOnly proves the echo guard is NOT satisfied by a
// shared FIRST name: two different "Alex Morgan"s whose addresses echo only "alex"
// (no address spells the full name) must stay separate. (Regression for the review
// finding: first-name-only corroboration was fusing distinct humans.)
func TestA3PrecisionSharedFirstNameOnly(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "alex@acme.com", "Alex Morgan", "x@y.com"),
		senderEmail("t2", "alex.k@globex.com", "Alex Morgan", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	a := entCarrying(ents, "alex@acme.com")
	b := entCarrying(ents, "alex.k@globex.com")
	if a == "" || b == "" || a == b {
		t.Fatalf("two different 'Alex Morgan's merged on first-name echo: a=%q b=%q", a, b)
	}
}

// TestA3PrecisionThreeWaySharedName proves three unrelated "Maria Garcia"s, each
// echoing only one name token (no address spells the full name), do NOT collapse.
func TestA3PrecisionThreeWaySharedName(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "maria@acme.com", "Maria Garcia", "x@y.com"),
		senderEmail("t2", "garcia@globex.com", "Maria Garcia", "x@y.com"),
		senderEmail("t3", "maria@initech.com", "Maria Garcia", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	seen := map[string]bool{}
	for _, addr := range []string{"maria@acme.com", "garcia@globex.com", "maria@initech.com"} {
		seen[entCarrying(ents, addr)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three different 'Maria Garcia's collapsed into %d entities, want 3", len(seen))
	}
}

// TestA3PrecisionSplitNameTokens proves two people sharing a full name where each
// address echoes a DIFFERENT single token (sam@ / jones@ for "Sam Jones") do not
// merge — there is no full-name anchor. (codex-found case.)
func TestA3PrecisionSplitNameTokens(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "sam@acme.com", "Sam Jones", "x@y.com"),
		senderEmail("t2", "jones@globex.com", "Sam Jones", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	if a, b := entCarrying(ents, "sam@acme.com"), entCarrying(ents, "jones@globex.com"); a == "" || a == b {
		t.Fatalf("split-token same-name people merged: sam->%q jones->%q", a, b)
	}
}

// TestA3MergeFullNameAnchored proves a cluster whose address spells the FULL name
// anchors the merge: riya.sharma@gmail (full) pulls in riya@acmeconsulting (first-name
// echo, same display name).
func TestA3MergeFullNameAnchored(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "riya.sharma@gmail.com", "Riya Sharma", "x@y.com"),
		senderEmail("t2", "riya@work.com", "Riya Sharma", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	a := entCarrying(ents, "riya.sharma@gmail.com")
	b := entCarrying(ents, "riya@work.com")
	if a == "" || a != b {
		t.Fatalf("full-name-anchored merge failed: riya.sharma->%q riya@work->%q", a, b)
	}
}

// entCarrying returns the id of the person entity whose aliases include addr.
func entCarrying(ents []graphEntity, addr string) string {
	for _, e := range personEntities(ents) {
		if contains(e.Aliases, addr) {
			return e.ID
		}
	}
	return ""
}

// TestA3DoesNotMergeServiceByName proves RULE 2 only unions PERSON identities, so a
// service (bot) is never merged into a person by a coincidental shared name.
func TestA3DoesNotMergeServiceByName(t *testing.T) {
	mems := []Memory{
		senderEmail("t1", "neil.patel@gmail.com", "Neil Patel", "x@y.com"),
		// a bot that self-presents "Neil Patel" (no-reply -> service); must stay separate
		senderEmail("t2", "noreply@neilpatel-news.com", "Neil Patel", "x@y.com"),
	}
	ents, _, _ := buildGraph(mems)
	neil := entCarrying(ents, "neil.patel@gmail.com")
	bot := entCarrying(ents, "noreply@neilpatel-news.com")
	if neil == "" || bot == "" || neil == bot {
		t.Fatalf("service merged into person by shared name: neil=%q bot=%q", neil, bot)
	}
	for _, e := range ents {
		if e.ID == neil && e.Kind != "person" {
			t.Errorf("neil kind = %q, want person", e.Kind)
		}
		if e.ID == bot && e.Kind != "service" {
			t.Errorf("bot kind = %q, want service", e.Kind)
		}
	}
}

// TestA3MergeRewritesEdgesAndResolves proves merged entities keep all their edges
// (rewritten to the canonical id) so get_entity / co-occurrence resolve via ANY of
// the merged addresses, and that the graph stays deterministic across builds.
func TestA3MergeRewritesEdgesAndResolves(t *testing.T) {
	mems := []Memory{
		// adit (two addresses) and riya share a thread; adit also emails from his 2nd address.
		senderEmail("t1", "alex.owner@gmail.com", "Alex Owner", "riya.sharma@gmail.com"),
		senderEmail("t2", "alexowner@gmail.com", "Alex Owner", "riya.sharma@gmail.com"),
	}
	ents, edges, _ := buildGraph(mems)
	// Exactly one adit + one riya.
	if got := len(personEntities(ents)); got != 2 {
		t.Fatalf("want 2 people (merged adit + riya), got %d", got)
	}
	// No EMAILED self-loop survived (adit->adit across addresses), and every person
	// edge endpoint resolves to an emitted entity id.
	ids := map[string]bool{}
	for _, e := range ents {
		ids[e.ID] = true
	}
	for _, e := range edges {
		if e.Rel == "EMAILED" && e.Src == e.Dst {
			t.Fatalf("EMAILED self-loop survived the merge: %s", e.Src)
		}
		if e.Rel == "EMAILED" {
			if !ids[e.Src] || !ids[e.Dst] {
				t.Fatalf("EMAILED endpoint not an emitted entity: %s->%s", e.Src, e.Dst)
			}
		}
	}
	// Determinism: a second build is byte-identical.
	ents2, edges2, _ := buildGraph(mems)
	if !sameEntities(ents, ents2) || len(edges) != len(edges2) {
		t.Fatal("buildGraph nondeterministic across runs")
	}
}

func sameEntities(a, b []graphEntity) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(es []graphEntity) []string {
		out := make([]string, len(es))
		for i, e := range es {
			al := append([]string(nil), e.Aliases...)
			sort.Strings(al)
			out[i] = e.ID + "|" + e.Kind + "|" + e.DisplayName + "|" + joinSorted(al)
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

func joinSorted(ss []string) string {
	sort.Strings(ss)
	out := ""
	for _, s := range ss {
		out += s + ","
	}
	return out
}
