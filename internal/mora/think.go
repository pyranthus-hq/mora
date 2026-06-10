package mora

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
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
}

// ThinkGaps is the deterministic "what's missing" analysis (no model).
type ThinkGaps struct {
	Stale         []string `json:"stale,omitempty"`          // freshest evidence is old
	ThinCoverage  []string `json:"thin_coverage,omitempty"`  // named entity has little evidence
	CoverageHoles []string `json:"coverage_holes,omitempty"` // named entity has no page at all
}

func (g ThinkGaps) empty() bool {
	return len(g.Stale) == 0 && len(g.ThinCoverage) == 0 && len(g.CoverageHoles) == 0
}

// ThinkResult is the synthesis envelope returned by `think`.
type ThinkResult struct {
	Query           string          `json:"query"`
	Evidence        []ThinkEvidence `json:"evidence"`
	Gaps            ThinkGaps       `json:"gaps"`
	SynthesisPrompt string          `json:"synthesis_prompt"`
}

var capitalizedNameRe = regexp.MustCompile(`\b[A-Z][a-z]+(?:\s+[A-Z][a-z]+)+\b`)

// buildThink assembles the envelope. now is injected for deterministic staleness
// in tests; callers pass time.Now().
func buildThink(ctx context.Context, cfg Config, query, scope string, limit int, now time.Time) (ThinkResult, error) {
	res := ThinkResult{Query: query}
	mems, err := hybridSearch(ctx, cfg, query, scope, limit)
	if err != nil {
		return res, err
	}
	for _, m := range mems {
		res.Evidence = append(res.Evidence, ThinkEvidence{
			StableID:  m.ID,
			Title:     m.Title,
			Scope:     m.Scope,
			CreatedAt: m.CreatedAt,
			Score:     m.Score,
			Snippet:   snippet(m.Text, thinkSnippetLen),
		})
	}
	gaps, err := computeGaps(ctx, cfg, query, mems, now)
	if err != nil {
		return res, err
	}
	res.Gaps = gaps
	res.SynthesisPrompt = thinkPrompt(query, res.Evidence, gaps)
	return res, nil
}

// computeGaps derives staleness, thin-coverage, and coverage-hole signals — all
// deterministic and free, before any model is consulted.
func computeGaps(ctx context.Context, cfg Config, query string, mems []Memory, now time.Time) (ThinkGaps, error) {
	var g ThinkGaps

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
		if !newest.IsZero() && now.Sub(newest) > thinkStaleDays*24*time.Hour {
			g.Stale = append(g.Stale, fmt.Sprintf("The freshest matching memory is from %s — older than %d days; the answer may be out of date.", newest.UTC().Format("2006-01-02"), thinkStaleDays))
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
	return g, nil
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
func thinkPrompt(query string, ev []ThinkEvidence, gaps ThinkGaps) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Answer the question using ONLY the evidence below. Cite every claim with its [stable_id]. ")
	b.WriteString("If the evidence is insufficient, say so plainly rather than guessing.\n\n")
	fmt.Fprintf(&b, "QUESTION: %s\n\nEVIDENCE:\n", query)
	if len(ev) == 0 {
		b.WriteString("(none found)\n")
	}
	for _, e := range ev {
		fmt.Fprintf(&b, "- [%s] (%s, %s) %s — %s\n", e.StableID, e.Scope, e.CreatedAt, e.Title, e.Snippet)
	}
	if !gaps.empty() {
		b.WriteString("\nKNOWN GAPS (surface these honestly in a 'What the vault does not know' section):\n")
		for _, s := range gaps.Stale {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.ThinCoverage {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.CoverageHoles {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	return b.String()
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
