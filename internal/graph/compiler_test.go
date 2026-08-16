package graph

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type Memory = memory.Memory

func hasEdgeSlice(edges []graphEdge, src, rel, dst string) bool {
	for _, e := range edges {
		if e.Src == src && e.Rel == rel && e.Dst == dst {
			return true
		}
	}
	return false
}
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
func senderEmail(id, from, name string, to ...string) Memory {
	meta := map[string]any{"from": []string{from}, "to": toAnySlice(to)}
	if name != "" {
		meta["names"] = map[string]string{from: name}
	}
	return Memory{ID: id, Scope: "personal", Type: "email", Title: id, CreatedAt: "2026-05-01T00:00:00Z", Text: "x", Meta: meta}
}
func entCarrying(ents []graphEntity, addr string) string {
	for _, e := range personEntities(ents) {
		if contains(e.Aliases, addr) {
			return e.ID
		}
	}
	return ""
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
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
func findGraphEntity(t *testing.T, ents []graphEntity, id string) graphEntity {
	t.Helper()
	for _, e := range ents {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("entity %q not emitted (have %d entities)", id, len(ents))
	return graphEntity{}
}
func imsgNamed(id, handle, name string) Memory {
	return Memory{
		ID: id, Scope: "personal", Type: "imessage", Title: id, CreatedAt: "2026-05-01T00:00:00Z", Text: "hi",
		Meta: map[string]any{"participants": []map[string]string{{"handle": handle, "name": name}}, "message_count": "1"},
	}
}
func candidateFor(cands []mergeCandidate, handle, email string) *mergeCandidate {
	for i := range cands {
		if personIdentity(cands[i].PhoneID) == handle && personIdentity(cands[i].EmailID) == email {
			return &cands[i]
		}
	}
	return nil
}
func hasSignal(links []mergeLink, sig string) bool {
	for _, l := range links {
		if l.Signal == sig {
			return true
		}
	}
	return false
}
func imsgMemory(id, handle, name, occurred string, msgCount int) Memory {
	return Memory{
		ID:        id,
		Type:      "imessage",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at":   occurred,
			"message_count": strconv.Itoa(msgCount),
			"participants": []any{
				map[string]any{"handle": handle, "name": name},
				map[string]any{"handle": "+10000000000", "name": "Me"},
			},
		},
	}
}
func emailMemory(id, from, to, occurred string, msgCount int) Memory {
	return Memory{
		ID:        id,
		Type:      "email",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at":   occurred,
			"message_count": strconv.Itoa(msgCount),
			"from":          []any{from},
			"to":            []any{to},
		},
	}
}
func eventMemory(id, organizer, attendee, occurred string) Memory {
	return Memory{
		ID:        id,
		Type:      "event",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at": occurred,
			"organizer":   organizer,
			"attendees":   []any{attendee},
		},
	}
}
func joinSorted(ss []string) string {
	sort.Strings(ss)
	out := ""
	for _, s := range ss {
		out += s + ","
	}
	return out
}
func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
func TestBuildGraphDeterministic(t *testing.T) {
	mems := []Memory{
		{ID: "a", Scope: "project:x", Tags: []string{"t2", "t1"}, Title: "[[Zeta]]", Text: "[[Alpha]] and [[Zeta]]\n- [Cat]", CreatedAt: "2026-01-02T00:00:00Z"},
		{ID: "b", Scope: "personal", Tags: []string{"t1"}, Text: "[[Alpha]]", CreatedAt: "2026-01-01T00:00:00Z", LastSynced: "2026-01-03T00:00:00Z"},
		{ID: "c", Scope: "project:x", Text: "no entities", CreatedAt: "2026-01-04T00:00:00Z"},
	}
	e1, g1, _ := buildGraph(mems)
	e2, g2, _ := buildGraph(mems)
	if !reflect.DeepEqual(e1, e2) {
		t.Fatalf("entities nondeterministic:\n%+v\n%+v", e1, e2)
	}
	if !reflect.DeepEqual(g1, g2) {
		t.Fatalf("edges nondeterministic:\n%+v\n%+v", g1, g2)
	}
}
func TestA2FanoutPreservesSender(t *testing.T) {
	// Sender id "person:zzz-sender@x.com" sorts AFTER all the "p###@x.com" recipients.
	var to []string
	for i := 0; i < maxParticipantFanout+20; i++ {
		to = append(to, fmt.Sprintf("p%03d@x.com", i))
	}
	m := Memory{
		ID: "gmail_thread/big", Type: "email", Title: "blast", CreatedAt: "2026-05-01T00:00:00Z",
		Meta: map[string]any{
			"from":  []any{"zzz-sender@x.com"},
			"to":    toAnySlice(to),
			"names": map[string]any{"zzz-sender@x.com": "Zed Sender"},
		},
	}
	ents, edges, warnings := buildGraph([]Memory{m})
	if len(warnings) == 0 {
		t.Fatal("expected a fan-out cap warning")
	}
	if !hasEdgeSlice(edges, "memory:gmail_thread/big", "PARTICIPATED_IN", "person:zzz-sender@x.com") {
		t.Fatal("sender was capped away — fan-out must retain self-presenters")
	}
	for _, e := range ents {
		if e.ID == "person:zzz-sender@x.com" {
			if !contains(e.Aliases, "Zed Sender") {
				t.Errorf("sender aliases = %v, want trusted self-presented name", e.Aliases)
			}
		}
	}
}
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

func TestGraphFanoutCap(t *testing.T) {
	var to []string
	for i := 0; i < maxParticipantFanout+20; i++ {
		to = append(to, fmt.Sprintf("p%03d@x.com", i))
	}
	m := Memory{
		ID: "gmail_thread/big", Type: "email", Title: "blast", CreatedAt: "2026-05-01T00:00:00Z",
		Meta: map[string]any{"from": []any{"sender@x.com"}, "to": toAnySlice(to)},
	}
	ents, edges, warnings := buildGraph([]Memory{m})

	// Count PARTICIPATED_IN person edges from the hub.
	n := 0
	for _, e := range edges {
		if e.Rel == "PARTICIPATED_IN" {
			n++
		}
	}
	if n > maxParticipantFanout {
		t.Fatalf("fan-out not capped: %d PARTICIPATED_IN edges > cap %d", n, maxParticipantFanout)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a fan-out cap warning (honesty: no silent truncation)")
	}
	_ = ents
}
func TestBuildGraphHubIDsAreInjective(t *testing.T) {
	mems := []Memory{
		{ID: "x/y", Scope: "personal", Title: "A", Text: "[[Shared]]", CreatedAt: "2026-05-01T00:00:00Z"},
		{ID: "x_y", Scope: "personal", Title: "B", Text: "[[Shared]]", CreatedAt: "2026-05-02T00:00:00Z"},
	}
	ents, edges, _ := buildGraph(mems)
	hubs := map[string]bool{}
	for _, e := range ents {
		if strings.HasPrefix(e.ID, "memory:") {
			hubs[e.ID] = true
		}
	}
	if len(hubs) != 2 {
		t.Fatalf("distinct memories x/y and x_y collapsed to hub ids %v (want 2)", hubs)
	}
	srcs := map[string]bool{}
	for _, ed := range edges {
		if ed.Dst == "link:Shared" {
			srcs[ed.Src] = true
		}
	}
	if len(srcs) != 2 {
		t.Fatalf("edge srcs collapsed to %v (want 2 distinct hubs)", srcs)
	}
}
func TestBuildGraphSaliencePersonPositiveServiceZero(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	person := "friend@example.com"
	service := "no-reply@billing.example.com"
	mems := []Memory{
		emailMemory("em_person", person, "me@example.com", occ, 9),
		emailMemory("em_service", service, "me@example.com", occ, 9),
	}
	ents, _, _ := buildGraph(mems)

	p := findGraphEntity(t, ents, personID(person))
	if p.Kind != "person" {
		t.Fatalf("expected %q to classify person, got %q", person, p.Kind)
	}
	if p.Salience <= 0 {
		t.Fatalf("person Salience=%d, want > 0", p.Salience)
	}

	s := findGraphEntity(t, ents, personID(service))
	if s.Kind != "service" {
		t.Fatalf("expected %q to classify service, got %q", service, s.Kind)
	}
	if s.Salience != 0 {
		t.Fatalf("service Salience=%d, want exactly 0", s.Salience)
	}
}
func TestBuildGraphSalienceMatchesKernel(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	a := "alice@example.com"
	b := "+15550001111"
	mems := []Memory{
		emailMemory("em_a", a, "me@example.com", occ, 12),
		imsgMemory("im_b", b, "Bob Roberts", occ, 400),
	}
	ents, _, _ := buildGraph(mems)
	kernel := aggregatePersonSalience(mems)

	for _, id := range []string{personID(a), personID(b)} {
		e := findGraphEntity(t, ents, id)
		if e.Salience != kernel[id] {
			t.Fatalf("graph Salience for %q = %d, kernel = %d — graph must reuse the kernel, not re-implement it", id, e.Salience, kernel[id])
		}
		if e.Salience <= 0 {
			t.Fatalf("expected positive Salience for %q, got %d", id, e.Salience)
		}
	}
}
func TestBuildGraphSalienceMergedPersonNoDoubleCount(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	// dotted + tagged Gmail variants of the same mailbox → A3 RULE 1 merge.
	v1 := "sam.smith@gmail.com"
	v2 := "samsmith+news@gmail.com"
	mems := []Memory{
		emailMemory("em_v1", v1, "me@example.com", occ, 10),
		emailMemory("em_v2", v2, "me@example.com", occ, 10),
	}
	ents, _, _ := buildGraph(mems)

	// Exactly one person entity carries BOTH mailbox aliases — the two variants merged
	// to a single canonical human (the recipient "me" is a separate, expected person).
	var merged []graphEntity
	for _, e := range ents {
		if e.Kind != "person" {
			continue
		}
		hasV1, hasV2 := false, false
		for _, a := range e.Aliases {
			if a == v1 {
				hasV1 = true
			}
			if a == v2 {
				hasV2 = true
			}
		}
		if hasV1 || hasV2 {
			merged = append(merged, e)
		}
	}
	if len(merged) != 1 {
		t.Fatalf("expected the two mailbox variants to merge to 1 person entity, got %d: %+v", len(merged), merged)
	}
	if merged[0].Salience <= 0 {
		t.Fatalf("merged person Salience=%d, want > 0", merged[0].Salience)
	}
	// The canonical id's salience must be max-folded through canon, NOT summed across
	// the two mailboxes — assert it stays near a single saturated mailbox's ceiling
	// (well under 2× — a sum would roughly double it).
	single := aggregatePersonSalience([]Memory{emailMemory("em_one", v1, "me@example.com", occ, 10)})
	if merged[0].Salience > single[personID(v1)]*2 {
		t.Fatalf("merged Salience=%d looks summed (single=%d) — must be max-folded, not double-counted", merged[0].Salience, single[personID(v1)])
	}
}

func TestBuildGraphSalienceNoWallClock(t *testing.T) {
	build := func(occ, occ2 string) map[string]int64 {
		mems := []Memory{
			emailMemory("em1", "alice@example.com", "me@example.com", occ, 8),
			imsgMemory("im1", "+15550002222", "Carol", occ2, 600),
		}
		ents, _, _ := buildGraph(mems)
		out := map[string]int64{}
		for _, e := range ents {
			if e.Kind == "person" {
				out[e.ID] = e.Salience
			}
		}
		return out
	}
	// Recent vault vs a vault 5 years older — same internal deltas (30 days apart).
	recent := build("2026-06-01T00:00:00Z", "2026-05-02T00:00:00Z")
	old := build("2021-06-01T00:00:00Z", "2021-05-02T00:00:00Z")
	if !reflect.DeepEqual(recent, old) {
		t.Fatalf("salience depends on wall-clock — recent vault %v != old vault %v (must be vault-relative)", recent, old)
	}
}
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
func TestIssue219NearNamesBecomeCandidates(t *testing.T) {
	cases := []struct {
		handle, phoneName, email, emailName string
	}{
		{"+14155550123", "Dan Rachev", "daniel.rachev@example.com", "Daniel Rachev"},
		{"+14155550124", "Samika", "samika.karode@example.com", "Samika Karode"},
	}
	for _, tc := range cases {
		mems := []Memory{
			imsgNamed("c1", tc.handle, tc.phoneName),
			senderEmail("t1", tc.email, tc.emailName, "me@x.com"),
		}
		res := buildGraphResult(mems, nil)
		if c := candidateFor(res.candidates, tc.handle, tc.email); c == nil {
			t.Errorf("%q/%q should be a confirmation candidate; got %+v", tc.phoneName, tc.emailName, res.candidates)
		}
		if a, b := entCarrying(res.entities, tc.handle), entCarrying(res.entities, tc.email); a == b {
			t.Errorf("near names must not auto-merge: %q/%q", tc.phoneName, tc.emailName)
		}
	}
}
func TestIssue219NearNameStillRequiresEmailSignature(t *testing.T) {
	mems := []Memory{
		imsgNamed("c1", "+14155550123", "Dan Rachev"),
		senderEmail("t1", "dr42@example.com", "Daniel Rachev", "me@x.com"),
	}
	if got := buildGraphResult(mems, nil).candidates; len(got) != 0 {
		t.Fatalf("near-name proposal without an address signature must refuse-to-gap: %+v", got)
	}
}
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
func TestSalienceAggregateSaturationCapsRunaway(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z" // hold recency equal
	mega := "+15550001111"
	heavy := "+15550002222"
	mems := []Memory{
		imsgMemory("im_mega", mega, "Mega Texter", occ, 50000),
		imsgMemory("im_heavy", heavy, "Heavy Texter", occ, 5000),
	}
	scores := aggregatePersonSalience(mems)
	megaScore := scores[personID(mega)]
	heavyScore := scores[personID(heavy)]
	if megaScore == 0 || heavyScore == 0 {
		t.Fatalf("unexpected zero: mega=%d heavy=%d", megaScore, heavyScore)
	}
	// 10× the messages must NOT yield a meaningfully higher score (both saturate to ~1).
	if megaScore != heavyScore {
		t.Fatalf("saturation did not cap runaway volume: mega(50k)=%d != heavy(5k)=%d", megaScore, heavyScore)
	}
}
func TestSalienceAggregateMultiChannelWins(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z" // hold recency equal
	single := "+15550001111"
	multi := "multi@example.com"
	multiH := "+15559998888"
	mems := []Memory{
		// Single-channel contact, channel fully saturated.
		imsgMemory("im_single", single, "Single Chan", occ, 5000),
		// Multi-channel human: each channel ALSO saturated (high per-channel counts),
		// spanning email + calendar + iMessage.
		emailMemory("em_multi", multi, "me@example.com", occ, 500),
		eventMemory("ev_multi", multi, "me@example.com", occ),
		imsgMemory("im_multi", multiH, "Multi Phone", occ, 5000),
	}
	scores := aggregatePersonSalience(mems)
	singleScore := scores[personID(single)]
	multiScore := scores[personID(multi)] // email+event identity (multi-channel)
	if singleScore == 0 || multiScore == 0 {
		t.Fatalf("unexpected zero: single=%d multi=%d", singleScore, multiScore)
	}
	if multiScore <= singleScore {
		t.Fatalf("multi-channel did not win: multi=%d should beat single=%d (Breadth tiebreak)",
			multiScore, singleScore)
	}
}
func TestSalienceAggregateMessageCountVsFallback(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	withCount := emailMemory("em_big", "big@example.com", "me@example.com", occ, 100)
	// A memory with NO message_count -> fallback 1.
	noCount := Memory{
		ID: "em_small", Type: "email", CreatedAt: occ,
		Meta: map[string]any{
			"occurred_at": occ,
			"from":        []any{"small@example.com"},
			"to":          []any{"me@example.com"},
		},
	}
	scores := aggregatePersonSalience([]Memory{withCount, noCount})
	big := scores[personID("big@example.com")]
	small := scores[personID("small@example.com")]
	if big <= small {
		t.Fatalf("message_count not consumed: big(count=100)=%d should beat small(fallback=1)=%d", big, small)
	}
}
func TestSalienceAggregateTombstoneSkipped(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	live := emailMemory("em_live", "a@example.com", "me@example.com", occ, 5)
	dead := emailMemory("em_dead", "b@example.com", "me@example.com", occ, 5)
	dead.DeletedAt = "2026-06-02T00:00:00Z"
	scores := aggregatePersonSalience([]Memory{live, dead})
	if _, ok := scores[personID("a@example.com")]; !ok {
		t.Fatalf("live memory's person missing from scores")
	}
	if _, ok := scores[personID("b@example.com")]; ok {
		t.Fatalf("tombstoned memory's person should be skipped (live-stats rule)")
	}
}
func TestSalienceAggregateDeterminism(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	mems := []Memory{
		imsgMemory("im1", "+15550001111", "A", occ, 30),
		emailMemory("em1", "b@example.com", "me@example.com", occ, 9),
		eventMemory("ev1", "c@example.com", "me@example.com", occ),
	}
	a := aggregatePersonSalience(mems)
	b := aggregatePersonSalience(mems)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("aggregatePersonSalience non-deterministic:\n a=%v\n b=%v", a, b)
	}
}
func TestSalienceAggregateGarbageMeta(t *testing.T) {
	garbage := Memory{
		ID: "im_garbage", Type: "imessage", CreatedAt: "not-a-time",
		Meta: map[string]any{
			"occurred_at":   "also-not-a-time",
			"message_count": "🙃 not-a-number",
			"participants": []any{
				map[string]any{"handle": "+15551234567", "name": "Weird, Name"},
			},
		},
	}
	scores := aggregatePersonSalience([]Memory{garbage})
	got := scores[personID("+15551234567")]
	if got < 0 || got > 1_000_000 {
		t.Fatalf("garbage Meta produced out-of-range micros: %d", got)
	}
}
func TestSalienceAggregateRecencyVaultRelative(t *testing.T) {
	// Two people, both on the same single channel/volume; the one seen at vaultMax
	// must outrank the older one purely on vault-relative recency (no wall clock).
	recent := emailMemory("em_recent", "recent@example.com", "me@example.com", "2026-06-01T00:00:00Z", 5)
	old := emailMemory("em_old", "old@example.com", "me@example.com", "2024-06-01T00:00:00Z", 5)
	scores := aggregatePersonSalience([]Memory{recent, old})
	r := scores[personID("recent@example.com")]
	o := scores[personID("old@example.com")]
	if r <= o {
		t.Fatalf("vault-relative recency not applied: recent=%d should beat old=%d", r, o)
	}
}
