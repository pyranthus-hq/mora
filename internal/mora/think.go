package mora

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// I3 `think` is the synthesis envelope: a DETERMINISTIC floor (retrieve + gap
// analysis, pure Go, $0, works headless with no model) plus a ready-to-run
// synthesis prompt the calling agent's OWN model turns into a cited answer. Mora
// holds no API key and pays no synthesis bill — the agent that called `think`
// reads the evidence + gaps and writes the prose. The gap analysis is the
// trust feature: an honest "what the vault does NOT know," computed before any
// model runs, is the antidote to confidently-wrong RAG.

const (
	thinkStaleDays  = 30
	thinkThinK      = 2 // an entity with fewer than this many memories is "thin"
	thinkSparseK    = 2 // fewer retrieved records cannot independently corroborate an answer
	thinkSnippetLen = 240
)

// ThinkEvidence is one retrieved memory with provenance for citation.
type ThinkEvidence struct {
	StableID  string  `json:"stable_id"`
	Title     string  `json:"title"`
	Scope     string  `json:"scope"`
	CreatedAt string  `json:"created_at"`
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet"`
	// Owner marks evidence from a subscribed share corpus (subscription name).
	// Empty for the user's own memories — omitempty keeps local-only envelopes
	// byte-identical (MCP budget gate).
	Owner string `json:"owner,omitempty"`
	// Corroborating mirrors a cluster head's Memory.Corroborating (issue #237,
	// round-2 P1 scoping fix): buildThink retrieves via hybridSearchTrace, the
	// same shared primitive search_memory uses, so a head's folded members are
	// already known here — without this field they would be UNRECOVERABLE from
	// think's output (correctly absent as their own evidence rows, but never
	// cited anywhere else either). Empty/absent for non-head evidence.
	Corroborating []CorroboratingRef `json:"corroborating,omitempty"`
}

// ThinkGaps is the deterministic "what's missing" analysis (no model).
type ThinkGaps struct {
	Stale            []string `json:"stale,omitempty"`
	FreshnessUnknown []string `json:"freshness_unknown,omitempty"`
	SparseEvidence   []string `json:"sparse_evidence,omitempty"`
	SourceCoverage   []string `json:"source_coverage,omitempty"`
	TemporalState    []string `json:"temporal_state,omitempty"`
	ThinCoverage     []string `json:"thin_coverage,omitempty"`
	CoverageHoles    []string `json:"coverage_holes,omitempty"`
	RetrievalCaveats []string `json:"retrieval_caveats,omitempty"`
	ChecksApplied    []string `json:"checks_applied"`
}

func (g ThinkGaps) empty() bool {
	return len(g.Stale) == 0 && len(g.FreshnessUnknown) == 0 && len(g.SparseEvidence) == 0 &&
		len(g.SourceCoverage) == 0 && len(g.TemporalState) == 0 && len(g.ThinCoverage) == 0 &&
		len(g.CoverageHoles) == 0 && len(g.RetrievalCaveats) == 0
}

// ThinkResult is the synthesis envelope returned by `think`.
type ThinkResult struct {
	Query           string            `json:"query"`
	Evidence        []ThinkEvidence   `json:"evidence"`
	Gaps            ThinkGaps         `json:"gaps"`
	OpenLoops       []PersonOpenLoops `json:"open_loops,omitempty"`       // C1: unfinished tasks tied to people named in the query
	SharesUnhealthy []shareUnhealth   `json:"shares_unhealthy,omitempty"` // Packet H5: degraded/failed/never subscriptions surfaced as an explicit gap
	SynthesisPrompt string            `json:"synthesis_prompt"`
}

var capitalizedNameRe = regexp.MustCompile(`\b[A-Z][a-z]+(?:\s+[A-Z][a-z]+)+\b`)

// buildThink assembles the envelope. now is injected for deterministic staleness
// in tests; callers pass time.Now().
func buildThink(ctx context.Context, cfg Config, query, scope string, limit int, now time.Time) (ThinkResult, error) {
	res := ThinkResult{Query: query}
	// Use the trace (tracePool=0 ⇒ production arms, zero extra work) so the gap
	// analysis can tell which evidence was a direct lexical/semantic hit vs only a
	// people-graph association (B3). The fused result is byte-identical to
	// hybridSearch — the trace is the per-arm lists it otherwise discards.
	local, tr, err := hybridSearchTrace(ctx, cfg, query, scope, limit, 0)
	if err != nil {
		return res, err
	}
	// Union subscribed share corpora in, owner-attributed (no-op without
	// subscriptions). The gap analysis below stays LOCAL-only: gaps report what
	// the user's own vault does not know, and shared ids must never be compared
	// against the personal index's retrieval trace or entity graph.
	mems, err := unionSharedResults(ctx, cfg, local, query, scope, limit)
	if err != nil {
		return res, err
	}
	for _, m := range mems {
		res.Evidence = append(res.Evidence, ThinkEvidence{
			StableID:      m.ID,
			Title:         m.Title,
			Scope:         m.Scope,
			CreatedAt:     m.CreatedAt,
			Score:         m.Score,
			Snippet:       matchSnippet(m.Text, query, thinkSnippetLen),
			Owner:         m.Owner,
			Corroborating: m.Corroborating,
		})
	}
	gaps, err := computeGaps(ctx, cfg, query, local, tr, now)
	if err != nil {
		return res, err
	}
	// The gap analysis ran on LOCAL results only; when shared corpora supplied
	// evidence the vault did not, the bare coverage-hole wording would
	// contradict the evidence right above it. Say precisely what is missing.
	if len(local) == 0 && len(mems) > 0 {
		for i, hole := range gaps.CoverageHoles {
			if hole == "No memory matched this query." {
				gaps.CoverageHoles[i] = "No memory in your own vault matched this query — the evidence comes entirely from shared corpora."
			}
		}
	}
	res.Gaps = gaps
	// C1: additively surface unfinished tasks tied to the people named in the
	// query ("what's still open with Sam"). Never partitions the evidence.
	loops, err := openLoopsForQuery(ctx, cfg, query)
	if err != nil {
		return res, err
	}
	res.OpenLoops = loops
	// Packet H5: surface any degraded/failed/never subscription as an explicit gap
	// so a suppressed-or-degraded share is visible, never silently dropped.
	res.SharesUnhealthy = sharesUnhealthy(cfg, now)
	res.SynthesisPrompt = thinkPrompt(query, res.Evidence, gaps, loops)
	return res, nil
}

// computeGaps derives staleness, thin-coverage, and coverage-hole signals — all
// deterministic and free, before any model is consulted.
func computeGaps(ctx context.Context, cfg Config, query string, mems []Memory, tr retrievalTrace, now time.Time) (ThinkGaps, error) {
	g := ThinkGaps{ChecksApplied: []string{
		"staleness", "evidence_density", "source_coverage", "temporal_state", "entity_coverage", "retrieval_support",
	}}

	if len(mems) == 0 {
		g.CoverageHoles = append(g.CoverageHoles, "No memory matched this query.")
	} else {
		// Pick the freshest evidence by PARSED instant — string compare would
		// misorder mixed RFC3339 offsets (e.g. local -07:00 vs UTC Z) (codex I3).
		var newest time.Time
		for _, m := range mems {
			if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil && t.After(newest) {
				newest = t
			}
		}
		if newest.IsZero() {
			g.FreshnessUnknown = append(g.FreshnessUnknown, "The matching evidence has no usable timestamp, so Mora cannot verify whether it is current.")
		} else if now.Sub(newest) > thinkStaleDays*24*time.Hour {
			g.Stale = append(g.Stale, fmt.Sprintf("The freshest matching memory is from %s — older than %d days; the answer may be out of date.", newest.UTC().Format("2006-01-02"), thinkStaleDays))
		}
		if len(mems) < thinkSparseK {
			g.SparseEvidence = append(g.SparseEvidence, fmt.Sprintf("Only %d matching memory was found; the answer lacks independent corroboration.", len(mems)))
		}
		sources := map[string]bool{}
		for _, m := range mems {
			sources[evidenceSource(m)] = true
		}
		if len(sources) == 1 {
			var source string
			for s := range sources {
				source = s
			}
			g.SourceCoverage = append(g.SourceCoverage, fmt.Sprintf("All matching evidence comes from %s; no other source corroborates it.", source))
		}
		if outcomeQuestion(query) && onlyProspectiveEvidence(mems) {
			g.TemporalState = append(g.TemporalState, "The evidence shows only invitation or scheduling state; Mora has no evidence that the event was completed or that an outcome/result was recorded.")
		}
	}

	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return g, err
	}
	defer db.Close()

	// Thin coverage: people the query names who have little evidence. Match ONLY
	// via the multi-token gazetteer (a distinctive full name) — matching loose
	// single tokens (first names, common words) against aliases would fire thin-
	// coverage noise on any query that happens to share a person's first name (codex I3).
	gaz, _, err := loadPersonGazetteer(ctx, db)
	if err != nil {
		return g, err
	}
	matched := map[string]bool{}
	for _, id := range gazetteerScan(gaz, query) {
		matched[id] = true
	}
	pids := make([]string, 0, len(matched))
	for id := range matched {
		pids = append(pids, id)
	}
	sort.Strings(pids)
	for _, pid := range pids {
		var display string
		var mc int
		if err := db.QueryRowContext(ctx, `SELECT display_name, mention_count FROM entities WHERE id = ?`, pid).Scan(&display, &mc); err != nil {
			continue
		}
		if mc < thinkThinK {
			g.ThinCoverage = append(g.ThinCoverage, fmt.Sprintf("Only %d memory about %s — coverage is thin.", mc, display))
		}
	}

	// Coverage holes: capitalized multi-word names in the query that resolve to NO
	// entity of any kind. Reuse the gazetteer's eligibility guards so a title-cased
	// question phrase ("What Did", "How Should We") is NOT mistaken for a name and
	// flagged as a false hole (codex I3) — only real-name-shaped phrases qualify.
	seenHole := map[string]bool{}
	for _, name := range capitalizedNameRe.FindAllString(query, -1) {
		if _, ok := normalizeGazName(name); !ok {
			continue
		}
		if seenHole[name] || entityExists(ctx, db, name) {
			continue
		}
		seenHole[name] = true
		g.CoverageHoles = append(g.CoverageHoles, fmt.Sprintf("The vault has no entity for %q.", name))
	}

	// B3 retrieval caveat: when the query named a person (the graph arm fired) and
	// EVERY returned memory came ONLY from people-graph association — present in the
	// graph arm but in neither the FTS (lexical), vector (semantic), nor Gmail
	// segment (direct message-text) arm — the
	// evidence proves "these memories are connected to <person>", NOT "they answer
	// the question". Flag it; do NOT drop — graph-only person expansion is the
	// showcased GraphRAG-lite recall feature (TestHybridGraphExpansion). Lowest
	// false-positive trigger: directly-supported count == 0 across ALL returned
	// evidence (codex). tr.FTS/tr.Vec/tr.Segment are the SAME call's production arms.
	if len(mems) > 0 && len(tr.Graph) > 0 {
		direct := make(map[string]bool, len(tr.FTS)+len(tr.Vec)+len(tr.Segment))
		for _, id := range tr.FTS {
			direct[id] = true
		}
		for _, id := range tr.Vec {
			direct[id] = true
		}
		for _, id := range tr.Segment {
			direct[id] = true
		}
		associationOnly := true
		for _, m := range mems {
			if direct[m.ID] {
				associationOnly = false
				break
			}
		}
		if associationOnly {
			who := "a person named in the question"
			if names := personDisplays(ctx, db, pids); names != "" {
				who = names
			}
			g.RetrievalCaveats = append(g.RetrievalCaveats, fmt.Sprintf(
				"The matches for %s come from people-graph association, not a direct lexical or semantic hit on the question — treat them as context and verify before relying.", who))
		}
	}
	return g, nil
}

// personDisplays joins the display names of the given canonical person ids, in
// their (sorted) input order, for an honest retrieval caveat message.
func personDisplays(ctx context.Context, db *sql.DB, pids []string) string {
	names := make([]string, 0, len(pids))
	for _, pid := range pids {
		names = append(names, entityDisplayName(ctx, db, pid))
	}
	return strings.Join(names, ", ")
}

// entityExists reports whether any entity matches name by display name or alias.
func entityExists(ctx context.Context, db *sql.DB, name string) bool {
	var one int
	if err := db.QueryRowContext(ctx, `SELECT 1 FROM entities WHERE lower(display_name) = lower(?) LIMIT 1`, name).Scan(&one); err == nil {
		return true
	}
	// Alias match (case-insensitive) over person entities.
	rows, err := db.QueryContext(ctx, `SELECT aliases FROM entities WHERE id LIKE 'person:%'`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var aj string
		if err := rows.Scan(&aj); err != nil {
			return false
		}
		if aliasMatches(aj, name) {
			return true
		}
	}
	return false
}

// thinkPrompt builds the instruction the calling agent's model runs to produce a
// cited answer plus an explicit "what's missing" section grounded in the gaps.
func thinkPrompt(query string, ev []ThinkEvidence, gaps ThinkGaps, loops []PersonOpenLoops) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Answer the question using ONLY the evidence below. Cite every claim with its [stable_id]. ")
	b.WriteString("If the evidence is insufficient, say so plainly rather than guessing.\n\n")
	fmt.Fprintf(&b, "QUESTION: %s\n\nEVIDENCE:\n", query)
	if len(ev) == 0 {
		b.WriteString("(none found)\n")
	}
	for _, e := range ev {
		if e.Owner != "" {
			// Shared evidence is labeled so the synthesis attributes claims to
			// the sharing party, never to the user's own vault.
			fmt.Fprintf(&b, "- [%s] (shared:%s, %s, %s) %s — %s\n", e.StableID, e.Owner, e.Scope, e.CreatedAt, e.Title, e.Snippet)
			continue
		}
		fmt.Fprintf(&b, "- [%s] (%s, %s) %s — %s\n", e.StableID, e.Scope, e.CreatedAt, e.Title, e.Snippet)
	}
	if !gaps.empty() {
		b.WriteString("\nKNOWN GAPS (surface these honestly in a 'What the vault does not know' section):\n")
		for _, s := range gaps.Stale {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.FreshnessUnknown {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.SparseEvidence {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.SourceCoverage {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.TemporalState {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.ThinCoverage {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.CoverageHoles {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.RetrievalCaveats {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	renderOpenLoops(&b, loops)
	return b.String()
}

func outcomeQuestion(query string) bool {
	words := wordSet(query)
	for _, term := range []string{"outcome", "result", "results", "accepted", "rejected", "offer", "decision", "completed", "happened"} {
		if words[term] {
			return true
		}
	}
	lower := strings.ToLower(query)
	return strings.Contains(lower, "how did") && (words["interview"] || words["meeting"] || words["event"])
}

func onlyProspectiveEvidence(mems []Memory) bool {
	prospective := 0
	for _, m := range mems {
		words := wordSet(m.Title + "\n" + m.Text)
		for _, term := range []string{"completed", "finished", "happened", "attended", "outcome", "result", "accepted", "rejected", "offer", "passed", "failed", "hired", "withdrew", "cancelled", "canceled"} {
			if words[term] {
				return false
			}
		}
		for _, term := range []string{"invite", "invited", "invitation", "schedule", "scheduled", "scheduling", "upcoming", "calendar", "confirmed", "confirmation"} {
			if words[term] {
				prospective++
				break
			}
		}
	}
	return prospective > 0 && prospective == len(mems)
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[word] = true
	}
	return out
}

// snippet returns a single-line, rune-safe prefix of text.
func snippet(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// snippetTermCap bounds how many query terms a snippet scan considers — a
// think query with pasted context must not turn every preview into a long scan.
const snippetTermCap = 12

// snippetTerms extracts the discriminative query terms used to center a
// snippet: ftsToken-normalized (case, edge punctuation, contraction tails),
// stopwords dropped with the SAME case-aware rule the FTS query uses (a
// capitalized "Will"/"WHO" is a name/acronym and survives), single-rune tokens
// dropped, de-duplicated. Longer terms sort first (stable on query order) so
// the window centers on the most discriminative match — "What did Dan say
// about polos" centers on polos, not an early "what".
func snippetTerms(query string) [][]rune {
	var out [][]rune
	seen := map[string]bool{}
	for _, f := range strings.Fields(query) {
		term, key := ftsToken(f)
		if key == "" || seen[key] || ftsIsStopword(term, key) {
			continue
		}
		kr := []rune(key)
		if len(kr) < 2 {
			continue
		}
		seen[key] = true
		out = append(out, kr)
		if len(out) == snippetTermCap {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// earliestQueryMatch returns the rune index of the highest-priority query-term
// match in r, or -1. Terms are tried in snippetTerms order (longest first);
// the first term that occurs anywhere wins with its EARLIEST occurrence —
// so a discriminative "polos" beats an early surviving "What". Matching is
// word-boundary and case-insensitive; lowercasing is per-rune so indexes stay
// aligned with r (a string-level ToLower can change rune counts for a handful
// of code points).
func earliestQueryMatch(r []rune, query string) int {
	terms := snippetTerms(query)
	if len(terms) == 0 {
		return -1
	}
	lower := make([]rune, len(r))
	for i, c := range r {
		lower[i] = unicode.ToLower(c)
	}
	isWord := func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }
	for _, t := range terms {
		for i := 0; i+len(t) <= len(lower); i++ {
			if i > 0 && isWord(lower[i-1]) {
				continue // mid-word — not a token start
			}
			hit := true
			for j, tc := range t {
				if lower[i+j] != tc {
					hit = false
					break
				}
			}
			if !hit {
				continue
			}
			if i+len(t) < len(lower) && isWord(lower[i+len(t)]) {
				continue // prefix of a longer word ("dan" in "abundant")
			}
			return i
		}
	}
	return -1
}

// matchSnippet returns a single-line, rune-safe window of text centered on the
// earliest query-term match, so a preview shows WHY a memory matched rather
// than its opening boilerplate — a hit deep in a long thread was previously
// found by FTS yet invisible in the head-clipped preview, making grounded
// answers look unsupported. Deterministic; with no usable term or no body
// match (a title/tag hit), it falls back to the head clip, byte-identical to
// snippet().
func matchSnippet(text, query string, n int) string {
	flat := strings.Join(strings.Fields(text), " ")
	r := []rune(flat)
	if len(r) <= n {
		return flat
	}
	pos := earliestQueryMatch(r, query)
	leadIn := n / 3 // context ahead of the match so the term doesn't open the window cold
	if pos < 0 || pos <= leadIn {
		// No body match, or the match already sits inside the head window.
		return strings.TrimSpace(string(r[:n])) + "…"
	}
	start := pos - leadIn
	if start+n > len(r) {
		start = len(r) - n
	}
	for start > 0 && start < pos && r[start-1] != ' ' {
		start++ // never open mid-word
	}
	end := start + n
	if end > len(r) {
		end = len(r)
	}
	out := "…" + strings.TrimSpace(string(r[start:end]))
	if end < len(r) {
		out += "…"
	}
	return out
}
