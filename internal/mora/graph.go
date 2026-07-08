package mora

import (
	"fmt"
	"sort"
	"strings"
)

// maxParticipantFanout caps how many person edges a single memory emits. A
// 200-recipient blast would otherwise dominate the graph; past the cap the fan-out
// is truncated and a warning is emitted (the repo's honesty rule: never silently
// drop data — surface it).
const maxParticipantFanout = 64

// hubID is the canonical id of a memory's hub node. It uses the RAW StableID, not
// SafeFilename (which is lossy: '/', ':', ' ' all map to '_'), so distinct
// memories never collapse to one hub node. Hub ids are graph keys, not filenames.
func hubID(stableID string) string {
	return "memory:" + stableID
}

// graphEntity is a node in the materialized entity graph (I1). For S1 these are
// the structural entities already present in the Markdown (scopes / tags /
// [[wikilinks]] / - [categories]) plus a per-memory hub node. The canonical id
// is prefixed (scope:/tag:/link:/category:/memory:) so different kinds with the
// same display name never collide. person/company resolution is deferred (S3+).
type graphEntity struct {
	ID           string // canonical, prefixed id
	Kind         string // spec kind: project | topic | thread | event
	DisplayName  string
	Aliases      []string
	MentionCount int // distinct evidence memories
	FirstSeen    string
	LastSeen     string
	// Salience is the frozen person-ranking sort key (salience_micros, int64) from
	// the Phase 14 model (salience.go). It is set only on person-kind entities, in a
	// scoring pass that runs AFTER A3 merge so it scores the fully-merged human; a
	// service entity and every structural/hub entity keep 0. Integer + vault-relative
	// recency keep it byte-identical across rebuilds.
	Salience int64
}

// graphEdge is a hub(memory) -> entity relation carrying provenance (evidence_id
// = the source memory's raw StableID) and bi-temporal stamps populated from free
// signals only (there is no history source in I1; see the design spec §2).
type graphEdge struct {
	Src           string // hub: memory:<SafeFilename(id)>
	Rel           string // MENTIONS | ABOUT
	Dst           string // entity id
	EvidenceID    string // raw StableID of the source memory
	ValidFrom     string // occurred/created time ("" => NULL)
	ObservedAt    string // when mora learned it = last_synced, else created ("" => NULL)
	InvalidatedAt string // = deleted_at for tombstones, else "" (NULL)
}

// nullStr returns nil for an empty string so absent timestamps persist as SQL
// NULL rather than "" (the spec's bi-temporal columns are honestly NULL-or-set).
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// relForStructuralKind maps a structural extraction kind to a hub->entity rel.
func relForStructuralKind(kind string) string {
	if kind == "link" {
		return "MENTIONS"
	}
	return "ABOUT" // scope, tag, category
}

// specKindForStructural maps the legacy structural kind to the spec entity kind.
func specKindForStructural(kind, name string) string {
	if kind == "scope" && strings.HasPrefix(name, "project:") {
		return "project"
	}
	return "topic"
}

// hubKind classifies the per-memory hub node by its memory type.
func hubKind(m Memory) string {
	if m.Type == "event" {
		return "event"
	}
	return "thread"
}

// validFromOf is the true-in-world time of a memory's edges: the connector's
// occurred_at (event/email time) when present, else created_at. created_at is
// preserved across rewrites = first-seen, so it drifts after an update; occurred_at
// does not (spec §3 BLOCKER #3). Meta-less memories fall back to created_at, so
// existing structural edges are unaffected.
func validFromOf(m Memory) string {
	if m.Meta != nil {
		if s, ok := m.Meta["occurred_at"].(string); ok && s != "" {
			return s
		}
	}
	return m.CreatedAt
}

// observedAtOf is when mora learned the edge: last_synced, else created_at.
func observedAtOf(m Memory) string {
	if m.LastSynced != "" {
		return m.LastSynced
	}
	return m.CreatedAt
}

// personID is the canonical entity id for an email address or iMessage handle:
// "person:" + the lowercased identity. Two references to the same address/handle
// collapse to one row (exact-match self-merge); phone handles are case-stable.
func personID(identity string) string {
	return "person:" + strings.ToLower(strings.TrimSpace(identity))
}

// metaStrings coerces a Meta value to a string slice, tolerating both the
// connector-side []string and the post-JSON-round-trip []any.
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

// metaNames coerces a Meta "names" value (addr→display) to map[string]string.
func metaNames(v any) map[string]string {
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]string:
		for k, val := range t {
			out[k] = val
		}
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// metaPairs coerces a Meta "participants" value (iMessage handle↔name pairs) to a
// slice, tolerating the connector-side []map[string]string and the post-round-trip
// []any of map[string]any.
func metaPairs(v any) []struct{ handle, name string } {
	var out []struct{ handle, name string }
	switch items := v.(type) {
	case []map[string]string:
		for _, p := range items {
			out = append(out, struct{ handle, name string }{p["handle"], p["name"]})
		}
	case []any:
		for _, it := range items {
			switch p := it.(type) {
			case map[string]any:
				h, _ := p["handle"].(string)
				n, _ := p["name"].(string)
				out = append(out, struct{ handle, name string }{h, n})
			case map[string]string:
				out = append(out, struct{ handle, name string }{p["handle"], p["name"]})
			}
		}
	}
	return out
}

// personRef is one resolved person reference inside a memory.
type personRef struct {
	id, identity, name string
}

// personRefs resolves the person references in a memory's Meta into a sorted,
// deduped participant set, plus the sender/recipient id lists used for EMAILED
// edges, plus the participation relation (ATTENDED for events, else
// PARTICIPATED_IN). Pure: no I/O, deterministic for identical input.
func personRefs(m Memory) (parts []personRef, senders, recipients []string, rel string) {
	rel = "PARTICIPATED_IN"
	if m.Type == "event" {
		rel = "ATTENDED"
	}
	if m.Meta == nil {
		return nil, nil, nil, rel
	}
	names := metaNames(m.Meta["names"])
	seen := map[string]*personRef{}
	add := func(identity, name string) string {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			return ""
		}
		id := personID(identity)
		if name == "" {
			name = names[strings.ToLower(identity)]
		}
		if r, ok := seen[id]; ok {
			if r.name == "" && name != "" {
				r.name = name
			}
		} else {
			seen[id] = &personRef{id: id, identity: strings.ToLower(identity), name: name}
		}
		return id
	}
	for _, a := range metaStrings(m.Meta["from"]) {
		if id := add(a, ""); id != "" {
			senders = append(senders, id)
		}
	}
	for _, key := range []string{"to", "cc"} {
		for _, a := range metaStrings(m.Meta[key]) {
			if id := add(a, ""); id != "" {
				recipients = append(recipients, id)
			}
		}
	}
	for _, a := range metaStrings(m.Meta["attendees"]) {
		add(a, "")
	}
	if org, ok := m.Meta["organizer"].(string); ok {
		// The organizer owns/created the event — this is event-side self-presentation
		// (like an email From-name), so it counts as a sender and its display name is
		// a trusted alias. Attendees, who are labeled BY the organizer/calendar, are
		// not added here and stay recipient-side (untrusted).
		if id := add(org, ""); id != "" {
			senders = append(senders, id)
		}
	}
	for _, p := range metaPairs(m.Meta["participants"]) {
		add(p.handle, p.name)
	}

	for _, r := range seen {
		parts = append(parts, *r)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].id < parts[j].id })
	senders = sortedUnique(senders)
	recipients = sortedUnique(recipients)
	return parts, senders, recipients, rel
}

func sortedUnique(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0]
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// personAgg accretes a canonical person across memories (exact-match self-merge).
type personAgg struct {
	// aliases are the trusted match keys (gazetteer + get_entity resolution): the
	// identity itself plus only PROVENANCE-TRUSTED names (A2) — names someone
	// presented for THEMSELVES as an email sender, or an iMessage contact name from
	// the user's own address book. Inbound recipient-labels (a sender labeling the
	// recipient — spam mail-merge, others mislabeling you) are deliberately excluded
	// so they can't bleed into an identity.
	aliases  map[string]bool
	evidence map[string]bool
	// nameCounts tallies EVERY resolved name (any provenance) per distinct memory,
	// used only to pick a cosmetic display name (most-frequent). Matching never
	// trusts it.
	nameCounts  map[string]int
	first, last string
}

// buildGraph derives the deterministic entity graph from the given memories: the
// structural entities (scopes/tags/[[links]]/categories), per-memory hub nodes,
// and the person graph (PARTICIPATED_IN/ATTENDED/EMAILED) compiled from Meta — all
// with provenance + bi-temporal stamps. Output is fully sorted so the same vault
// produces a byte-identical graph across rebuilds. The third return is the list of
// non-fatal warnings (e.g. fan-out caps) for the caller to surface.
func buildGraph(mems []Memory) ([]graphEntity, []graphEdge, []string) {
	byID := make(map[string]Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}

	var entities []graphEntity
	var edges []graphEdge
	var warnings []string

	// Structural entities + their hub->entity edges.
	for _, e := range extractEntities(mems) {
		id := e.Kind + ":" + e.Name
		rel := relForStructuralKind(e.Kind)
		first, last := "", ""
		liveCount := 0
		for _, mid := range e.MemoryIDs { // sorted by extractEntities
			m := byID[mid]
			edges = append(edges, graphEdge{
				Src:           hubID(mid),
				Rel:           rel,
				Dst:           id,
				EvidenceID:    mid,
				ValidFrom:     validFromOf(m),
				ObservedAt:    observedAtOf(m),
				InvalidatedAt: m.DeletedAt,
			})
			// Entity stats must mirror the live read path (invalidated_at IS NULL):
			// a tombstoned memory still emits its (invalidated) edge but must NOT
			// count toward mention_count / first_seen / last_seen.
			if m.DeletedAt != "" {
				continue
			}
			liveCount++
			if m.CreatedAt != "" {
				if first == "" || m.CreatedAt < first {
					first = m.CreatedAt
				}
				if m.CreatedAt > last {
					last = m.CreatedAt
				}
			}
		}
		entities = append(entities, graphEntity{
			ID:           id,
			Kind:         specKindForStructural(e.Kind, e.Name),
			DisplayName:  e.Name,
			MentionCount: liveCount,
			FirstSeen:    first,
			LastSeen:     last,
		})
	}

	// Per-memory hub nodes.
	for _, m := range mems {
		entities = append(entities, graphEntity{
			ID:           hubID(m.ID),
			Kind:         hubKind(m),
			DisplayName:  m.Title,
			MentionCount: 1,
			FirstSeen:    m.CreatedAt,
			LastSeen:     m.CreatedAt,
		})
	}

	// Person graph (S4): PARTICIPATED_IN/ATTENDED (memory hub -> person) for every
	// participant, EMAILED (sender -> recipient) for mail. Persons self-merge by
	// canonical id across memories. Co-occurrence is NOT materialized — it's a
	// query-time self-join (person -> memory -> person), so a 200-person thread is
	// O(N) edge rows, not O(N²).
	persons := map[string]*personAgg{}
	getP := func(id string) *personAgg {
		p := persons[id]
		if p == nil {
			p = &personAgg{aliases: map[string]bool{}, evidence: map[string]bool{}, nameCounts: map[string]int{}}
			persons[id] = p
		}
		return p
	}
	memParts := map[string]map[string]bool{} // memory id -> its metadata participant ids
	for _, m := range mems {
		parts, senders, recipients, rel := personRefs(m)
		// A2 provenance: a name is trusted (-> alias) when its bearer presented it
		// themselves (an email/event sender, incl. calendar organizer) or it's an
		// iMessage contact name (from the user's own address book). Recipient-labels
		// are not. Built before the fan-out cap so senders are never capped away.
		senderSet := make(map[string]bool, len(senders))
		for _, s := range senders {
			senderSet[s] = true
		}
		nameTrusted := m.Type == "imessage"
		if len(parts) > maxParticipantFanout {
			warnings = append(warnings, fmt.Sprintf(
				"graph: memory %s has %d participants; capping person fan-out to %d",
				m.ID, len(parts), maxParticipantFanout))
			parts = capParticipants(parts, senderSet, maxParticipantFanout)
		}
		capped := make(map[string]bool, len(parts))
		for _, p := range parts {
			capped[p.id] = true
		}
		memParts[m.ID] = capped
		vf, obs, inv := validFromOf(m), observedAtOf(m), m.DeletedAt
		for _, p := range parts {
			edges = append(edges, graphEdge{
				Src: hubID(m.ID), Rel: rel, Dst: p.id, EvidenceID: m.ID,
				ValidFrom: vf, ObservedAt: obs, InvalidatedAt: inv,
			})
			agg := getP(p.id)
			agg.aliases[p.identity] = true // the address/handle is always a trusted alias
			if p.name != "" {
				agg.nameCounts[p.name]++ // every name feeds the cosmetic display pick
				if nameTrusted || senderSet[p.id] {
					agg.aliases[p.name] = true // provenance-trusted -> a real match key
				}
			}
			if inv != "" { // tombstoned edges don't count toward live stats
				continue
			}
			agg.evidence[m.ID] = true
			if vf == "" {
				continue
			}
			if agg.first == "" || vf < agg.first {
				agg.first = vf
			}
			if vf > agg.last {
				agg.last = vf
			}
		}
		// EMAILED: sender -> each recipient (mail only), within the capped set.
		if rel == "PARTICIPATED_IN" && m.Type == "email" {
			for _, s := range senders {
				if !capped[s] {
					continue
				}
				for _, rcp := range recipients {
					if rcp == s || !capped[rcp] {
						continue
					}
					edges = append(edges, graphEdge{
						Src: s, Rel: "EMAILED", Dst: rcp, EvidenceID: m.ID,
						ValidFrom: vf, ObservedAt: obs, InvalidatedAt: inv,
					})
				}
			}
		}
	}
	// Gazetteer body-matching (S5): people are known from metadata; find them
	// mentioned in message/email BODIES too (e.g. "spoke with Sam Rivera") and emit
	// a MENTIONS edge. The gazetteer is built FROM the graph's own person aliases —
	// no NER, no model. Guarded for precision: word-boundary token matching,
	// min-length, a stoplist, multi-token names always eligible / single tokens only
	// when distinctive, and a deterministic tie-break on ambiguous names.
	// Provenance-trusted aliases (A2) were already accreted inline above, so the
	// gazetteer — built from p.aliases — body-matches only trusted names.
	gaz := buildGazetteer(persons)
	for _, m := range mems {
		if m.DeletedAt != "" || m.Text == "" {
			continue
		}
		vf, obs := validFromOf(m), observedAtOf(m)
		already := memParts[m.ID]
		for _, pid := range gazetteerScan(gaz, m.Text) {
			if already[pid] { // already a metadata participant — PARTICIPATED_IN covers it
				continue
			}
			edges = append(edges, graphEdge{
				Src: hubID(m.ID), Rel: "MENTIONS", Dst: pid, EvidenceID: m.ID,
				ValidFrom: vf, ObservedAt: obs,
			})
			agg := getP(pid)
			agg.evidence[m.ID] = true
			if vf != "" {
				if agg.first == "" || vf < agg.first {
					agg.first = vf
				}
				if vf > agg.last {
					agg.last = vf
				}
			}
		}
	}

	// A3: cluster same-human identities (Gmail-inbox normalization + echo-corroborated
	// shared-name) and collapse each cluster to one canonical entity, redirecting its
	// edges. Done after all per-address aggregates + edges (incl. gazetteer MENTIONS)
	// exist, so the merge sees complete aliases and rewrites every endpoint.
	canon := canonicalizePersons(persons)
	persons = mergePersonAggs(persons, canon)
	edges = rewritePersonEdges(edges, canon)

	// Salience pass (Phase 14, D14-1..D14-4): freeze each canonical person's ranking
	// score. It uses the SAME aggregatePersonSalience kernel the digest consumes
	// (salience.go) — one source of truth, never a re-implemented math here — so the
	// graph ordering and the digest ordering can never diverge. It runs HERE, after
	// the A3 merge (so it scores the fully-merged human, not pre-merge mailbox shards)
	// and before emission (so each emitted entity carries its frozen score). The
	// kernel keys by PRE-MERGE person id, so its output is remapped through `canon`
	// into canonical ids, max-folding any collision (two mailboxes of one human are
	// the strongest single signal, not the sum — the per-channel saturation already
	// caps volume, so summing would reward fragmentation past the [0,1] ceiling).
	// Determinism: the fold iterates sorted keys, stores an int64, and max is
	// order-independent; the kernel's recency is vault-relative (no time.Now), so the
	// score is byte-identical across rebuilds.
	sal := aggregatePersonSalience(mems)
	salKeys := make([]string, 0, len(sal))
	for k := range sal {
		salKeys = append(salKeys, k)
	}
	sort.Strings(salKeys)
	canonSal := make(map[string]int64, len(sal))
	for _, k := range salKeys {
		c := k
		if v, ok := canon[k]; ok {
			c = v
		}
		if sal[k] > canonSal[c] {
			canonSal[c] = sal[k]
		}
	}

	for _, id := range sortedPersonIDs(persons) {
		p := persons[id]
		identity := strings.TrimPrefix(id, "person:")
		// Trusted aliases (provenance) were already accreted into p.aliases above.
		// show drives the cosmetic display; classifyName is the trusted-only name fed
		// to A1 so an untrusted inbound label can't misclassify a real person.
		show, classifyName := resolvePersonName(p.nameCounts, p.aliases, identity)
		kind := classifyIdentity(identity, classifyName) // A1: person | service
		// The graph's classification is authoritative for the HumanGate: the kernel
		// gates on the address alone, but a trusted display-name suffix can flip an
		// address-only "person" to a service here — keep that service at 0 so a service
		// never carries a positive ranking score (D14-1/D14-6).
		salience := canonSal[id]
		if kind == "service" {
			salience = 0
		}
		entities = append(entities, graphEntity{
			ID:           id,
			Kind:         kind,
			DisplayName:  show,
			Aliases:      sortedKeys(p.aliases),
			MentionCount: len(p.evidence),
			FirstSeen:    p.first,
			LastSeen:     p.last,
			Salience:     salience,
		})
	}

	sort.Slice(entities, func(i, j int) bool { return entities[i].ID < entities[j].ID })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Src != edges[j].Src {
			return edges[i].Src < edges[j].Src
		}
		if edges[i].Rel != edges[j].Rel {
			return edges[i].Rel < edges[j].Rel
		}
		if edges[i].Dst != edges[j].Dst {
			return edges[i].Dst < edges[j].Dst
		}
		return edges[i].EvidenceID < edges[j].EvidenceID
	})
	return entities, edges, warnings
}

// capParticipants reduces an over-cap, id-sorted participant set to `cap` entries
// while guaranteeing the self-presenters (senders/organizer) are retained — they
// are the highest-value nodes and must never be dropped by the cap. The result is
// re-sorted by id so the graph stays byte-identical across rebuilds.
func capParticipants(parts []personRef, senderSet map[string]bool, limit int) []personRef {
	kept := make([]personRef, 0, limit)
	for _, p := range parts { // parts is sorted by id
		if senderSet[p.id] {
			kept = append(kept, p)
		}
	}
	if len(kept) > limit {
		kept = kept[:limit] // pathological: more senders than the cap
	}
	for _, p := range parts {
		if len(kept) >= limit {
			break
		}
		if !senderSet[p.id] {
			kept = append(kept, p)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].id < kept[j].id })
	return kept
}

// resolvePersonName returns (show, classify) names for a person.
//   - show: the cosmetic display — most-frequent TRUSTED name (so a high-volume spam
//     recipient-label can't hijack it), else most-frequent any-provenance name (a
//     name beats a raw address), else the identity.
//   - classify: ONLY the most-frequent trusted name (else empty). A1's display-suffix
//     rule must never see an untrusted inbound label, or a sender labeling a real
//     recipient "Acme Receipts" would flip that real person to "service".
func resolvePersonName(counts map[string]int, aliases map[string]bool, identity string) (show, classify string) {
	trusted := make(map[string]int, len(counts))
	for name, c := range counts {
		if aliases[name] {
			trusted[name] = c
		}
	}
	classify = mostFrequentName(trusted)
	show = classify
	if show == "" {
		show = mostFrequentName(counts)
	}
	if show == "" {
		show = identity
	}
	return show, classify
}

// mostFrequentName returns the name seen in the most distinct memories, breaking
// ties on the lexicographically smallest name (deterministic). Empty if no names.
func mostFrequentName(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names) // ascending -> lexicographic tie-break falls out of strict >
	best, bestCount := "", 0
	for _, name := range names {
		if counts[name] > bestCount {
			best, bestCount = name, counts[name]
		}
	}
	return best
}

// maxNameMergeClusters bounds how many distinct (RULE-1) mailbox clusters a single
// shared name may bridge (A3 RULE 2). A distinctive personal name borne by a few
// addresses is one human; a name borne by many is ambiguous (a common name) and is
// left unmerged — precision over recall.
const maxNameMergeClusters = 4

// mailboxKey collapses provider-equivalent addresses to one inbox identity (A3
// RULE 1). For Gmail-owned domains, dots and "+tags" in the local part are ignored
// and googlemail.com == gmail.com — these are provably the same mailbox. Every other
// provider is left byte-exact (only Gmail has these semantics), and phone handles
// key to themselves.
func mailboxKey(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return addr
	}
	local, host := addr[:at], addr[at+1:]
	if host == "gmail.com" || host == "googlemail.com" {
		if i := strings.IndexByte(local, '+'); i >= 0 {
			local = local[:i]
		}
		local = strings.ReplaceAll(local, ".", "")
		host = "gmail.com"
	}
	return local + "@" + host
}

// unionFind is a tiny deterministic disjoint-set over string keys.
type unionFind struct{ parent map[string]string }

func newUnionFind(ids []string) *unionFind {
	p := make(map[string]string, len(ids))
	for _, id := range ids {
		p[id] = id
	}
	return &unionFind{parent: p}
}

func (u *unionFind) find(x string) string {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if ra < rb { // deterministic root choice (canonical is re-selected separately)
		u.parent[rb] = ra
	} else {
		u.parent[ra] = rb
	}
}

// trustedPersonNames returns the distinctive (multi-token, lowercased) trusted names
// of a person aggregate — its aliases that are display names (not the address/handle)
// and carry >= 2 tokens. These are the A3 RULE-2 merge keys.
func trustedPersonNames(p *personAgg) []string {
	var out []string
	for _, a := range sortedKeys(p.aliases) {
		if strings.ContainsRune(a, '@') || strings.HasPrefix(a, "+") {
			continue // an address/handle, not a name
		}
		if len(strings.Fields(a)) < 2 {
			continue // single token -> not distinctive enough to merge on
		}
		out = append(out, strings.ToLower(a))
	}
	return out
}

// addrTokens returns the lowercased local-part tokens of a person id's address
// (split on . - _), used for A3 RULE-2 echo corroboration. Empty for phone handles.
func addrTokens(id string) map[string]bool {
	addr := strings.TrimPrefix(id, "person:")
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return nil
	}
	out := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(addr[:at]), isDelim) {
		out[tok] = true
	}
	return out
}

// personKindOf computes the A1 classification for a single (pre-merge) identity.
func personKindOf(id string, p *personAgg) string {
	identity := strings.TrimPrefix(id, "person:")
	_, classifyName := resolvePersonName(p.nameCounts, p.aliases, identity)
	return classifyIdentity(identity, classifyName)
}

// canonicalizePersons clusters same-human person identities (A3) and returns a map
// from every person id to its cluster's canonical id. Deterministic: groups are
// formed in sorted order and the canonical is chosen independently of union order
// (most evidence, then lexicographically smallest id).
//
// RULE 1: addresses that resolve to the same provider mailbox (mailboxKey).
// RULE 2: PERSON-classified identities sharing a distinctive multi-token trusted
// name, bridging at most maxNameMergeClusters mailbox clusters, AND corroborated —
// each bridged cluster must have a member address whose local part echoes a token of
// the shared name (so two unrelated people with the same name but unrelated addresses
// are never fused).
func canonicalizePersons(persons map[string]*personAgg) map[string]string {
	ids := sortedPersonIDs(persons)
	uf := newUnionFind(ids)

	// RULE 1 — same provider mailbox.
	byMailbox := map[string][]string{}
	for _, id := range ids {
		key := mailboxKey(strings.TrimPrefix(id, "person:"))
		byMailbox[key] = append(byMailbox[key], id)
	}
	for _, key := range sortedStringKeys(byMailbox) {
		grp := byMailbox[key]
		for i := 1; i < len(grp); i++ {
			uf.union(grp[0], grp[i])
		}
	}

	// RULE 2 — distinctive shared name, echo-corroborated, among person identities.
	personIDs := map[string]bool{}
	for _, id := range ids {
		if personKindOf(id, persons[id]) == "person" {
			personIDs[id] = true
		}
	}
	byName := map[string][]string{}
	for _, id := range ids {
		if !personIDs[id] {
			continue
		}
		for _, name := range trustedPersonNames(persons[id]) {
			byName[name] = append(byName[name], id)
		}
	}
	for _, name := range sortedStringKeys(byName) {
		nameToks := strings.Fields(name)
		// Per current cluster root, track its representative id and whether ANY member
		// address spells the FULL name (every name token echoed). Bridging requires a
		// full-name anchor: a shared first name alone must NOT fuse two people who
		// merely happen to share a full display name (review finding — the routine
		// "two different Alex Morgans" / three-way "Maria Garcia" false merge).
		type clusterEcho struct {
			rep  string
			full bool
		}
		echoing := map[string]*clusterEcho{} // root -> echo state
		for _, id := range byName[name] {
			toks := addrTokens(id)
			covered := 0
			for _, t := range nameToks {
				if toks[t] {
					covered++
				}
			}
			if covered == 0 {
				continue // address does not echo the name at all -> not corroborated
			}
			root := uf.find(id)
			c := echoing[root]
			if c == nil {
				c = &clusterEcho{rep: id}
				echoing[root] = c
			} else if id < c.rep {
				c.rep = id
			}
			if covered == len(nameToks) {
				c.full = true
			}
		}
		if len(echoing) < 2 || len(echoing) > maxNameMergeClusters {
			continue // nothing to bridge, or an ambiguous (too-common) name
		}
		anyFull := false
		for _, c := range echoing {
			if c.full {
				anyFull = true
				break
			}
		}
		if !anyFull {
			continue // no address spells the full name -> too weak to auto-merge
		}
		reps := make([]string, 0, len(echoing))
		for _, c := range echoing {
			reps = append(reps, c.rep)
		}
		sort.Strings(reps)
		for i := 1; i < len(reps); i++ {
			uf.union(reps[0], reps[i])
		}
	}

	// Choose the canonical id per cluster: most evidence, then smallest id.
	rep := map[string]string{}
	for _, id := range ids {
		root := uf.find(id)
		cur, ok := rep[root]
		if !ok || len(persons[id].evidence) > len(persons[cur].evidence) ||
			(len(persons[id].evidence) == len(persons[cur].evidence) && id < cur) {
			rep[root] = id
		}
	}
	canon := make(map[string]string, len(ids))
	for _, id := range ids {
		canon[id] = rep[uf.find(id)]
	}
	return canon
}

// sortedStringKeys returns the sorted keys of a map[string][]string.
func sortedStringKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mergePersonAggs folds every person aggregate into its canonical cluster, unioning
// aliases/name-counts/evidence and taking the min/max time bounds.
func mergePersonAggs(persons map[string]*personAgg, canon map[string]string) map[string]*personAgg {
	merged := map[string]*personAgg{}
	for _, id := range sortedPersonIDs(persons) {
		c := canon[id]
		dst := merged[c]
		if dst == nil {
			dst = &personAgg{aliases: map[string]bool{}, evidence: map[string]bool{}, nameCounts: map[string]int{}}
			merged[c] = dst
		}
		src := persons[id]
		for a := range src.aliases {
			dst.aliases[a] = true
		}
		for n, ct := range src.nameCounts {
			dst.nameCounts[n] += ct
		}
		for e := range src.evidence {
			dst.evidence[e] = true
		}
		if src.first != "" && (dst.first == "" || src.first < dst.first) {
			dst.first = src.first
		}
		if src.last > dst.last {
			dst.last = src.last
		}
	}
	return merged
}

// rewritePersonEdges redirects person edge endpoints to their canonical id, drops
// EMAILED self-loops created by the merge, and dedups identical edges.
func rewritePersonEdges(edges []graphEdge, canon map[string]string) []graphEdge {
	mapID := func(id string) string {
		if c, ok := canon[id]; ok {
			return c
		}
		return id
	}
	out := make([]graphEdge, 0, len(edges))
	seen := map[string]bool{}
	for _, e := range edges {
		e.Src = mapID(e.Src)
		e.Dst = mapID(e.Dst)
		if e.Rel == "EMAILED" && e.Src == e.Dst {
			continue // self-loop: two addresses of one person across a thread
		}
		key := e.Src + "\x00" + e.Rel + "\x00" + e.Dst + "\x00" + e.EvidenceID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// sortedPersonIDs returns the person aggregate ids in sorted order (deterministic).
func sortedPersonIDs(persons map[string]*personAgg) []string {
	out := make([]string, 0, len(persons))
	for id := range persons {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
