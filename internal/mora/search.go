package mora

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// snippetMemories returns copies of the results with each body flattened to a
// single line and clipped to searchSnippetLen (Truncated flags the clip), and
// drops the Meta map so a row's total size is bounded (Meta is entity-graph
// frontmatter — agents get it via get_entity/read_memory, not a search preview).
// The clip window is centered on the earliest query-term match (matchSnippet),
// so a preview shows the evidence for the hit, not the memory's opening lines.
// Only the token-budgeted MCP surface calls this; the CLI keeps full bodies+meta.
func snippetMemories(mems []Memory, query string) []Memory {
	if mems == nil {
		return nil
	}
	out := make([]Memory, len(mems))
	for i, m := range mems {
		full := strings.Join(strings.Fields(m.Text), " ")
		if utf8.RuneCountInString(full) > searchSnippetLen {
			m.Text = matchSnippet(m.Text, query, searchSnippetLen)
			m.Truncated = true
		} else {
			m.Text = full
		}
		m.Meta = nil // unbounded graph frontmatter — not part of a search preview
		out[i] = m
	}
	return out
}

// budgetSearchResults caps the aggregate JSON size of a (snippeted) search result
// slice, keeping a whole-Memory prefix — never a half-record. It mirrors
// budgetSections' greedy item-by-item fill with the SAME conservative jsonSep
// over-count, but for a flat slice: snippetMemories bounds each row, this bounds
// the total so a large `limit` arg can't blow the MCP search envelope. A
// budgetBytes ≤ 0 disables it. The first result is always kept so a matched query
// never returns empty purely for budget; this is sound because snippetMemories has
// already capped the body (searchSnippetLen) and dropped Meta, so a single row is
// O(1KB) — far under searchMemoryResultsBudgetBytes (11K), and the forced row can
// never itself breach the ceiling. Returns the kept prefix and the number dropped.
func budgetSearchResults(mems []Memory, budgetBytes int) (kept []Memory, dropped int) {
	if budgetBytes <= 0 || len(mems) == 0 {
		return mems, 0
	}
	const jsonSep = 2 // per-element comma/bracket glue (conservative over-count), matching budgetSections.
	kept = make([]Memory, 0, len(mems))
	used := 0
	for _, m := range mems {
		cost := jsonLen(m) + jsonSep
		if used+cost > budgetBytes && len(kept) > 0 {
			break
		}
		kept = append(kept, m)
		used += cost
	}
	return kept, len(mems) - len(kept)
}

// searchMemories is the FTS-only retrieval arm. filters is an optional
// trailing #241 source/since_hours pair (searchFilters{}, the zero value,
// when omitted — every pre-#241 call site keeps compiling unchanged and gets
// a byte-identical query/result). The filter is a TRUE SQL WHERE predicate
// (searchFilters.sqlPredicate, search_filters.go) appended BEFORE ORDER
// BY/LIMIT, evaluated against the indexed memories.provider/account/
// created_at_unix columns (v4 schema, #241) — the SAME row bm25 already
// ranked, never a second parseMemory() disk read of the live vault file mid-
// ranking. Because the exclusion happens in the WHERE clause itself, a
// filtered-out row is never fetched, never ranked, and can never crowd a
// matching row out of `limit` (filters_contract_test.go's
// TestFiltersSearchMemoryPreRankSourceProof/SinceHoursProof) — LIMIT stays
// unconditional; there is no longer a separate "drop the SQL LIMIT" branch.
func searchMemories(ctx context.Context, cfg Config, query, scope string, limit int, filters ...searchFilters) ([]Memory, error) {
	f := oneFilter(filters)
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			return nil, err
		}
	}
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	match := ftsQuery(query)
	if strings.TrimSpace(match) == "" {
		// An empty / all-punctuation query has no terms to MATCH. FTS5 errors on
		// an empty MATCH string ("fts5: syntax error near \"\""), so short-circuit
		// to zero results rather than crashing the search command.
		return nil, nil
	}
	sqlq := `SELECT m.id, m.scope, m.type, m.title, m.tags, m.source, m.created_at, m.path, m.text, bm25(memories_fts) AS score
		FROM memories_fts JOIN memories m ON m.id = memories_fts.id WHERE memories_fts MATCH ?`
	args := []any{match}
	if scope != "" {
		sqlq += ` AND m.scope = ?`
		args = append(args, scope)
	}
	if pc, pargs := f.sqlPredicate(); pc != "" {
		sqlq += pc
		args = append(args, pargs...)
	}
	sqlq += ` ORDER BY score, m.id LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text, &m.Score); err != nil {
			return nil, err
		}
		m.Tags = splitCSV(tags)
		if full, ferr := parseMemory(m.Path); ferr == nil {
			full.Score = m.Score
			m = full
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// B4: a memory with a pending delete op is suppressed from the search chokepoint
	// (the memories JOIN) while its rebuild is broken, so deleted content is never
	// served even when the index still carries the row.
	out = suppressPendingDeletes(cfg, out)
	return currentMemories(cfg, out, time.Now())
}
func buildContext(cfg Config, items []Memory, budget int, hasQuery bool) string {
	if budget <= 0 {
		return ""
	}
	var wiki strings.Builder
	for _, rel := range []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md"} {
		if body, err := os.ReadFile(filepath.Join(cfg.VaultDir, rel)); err == nil {
			fmt.Fprintf(&wiki, "\n# %s\n%s\n", rel, string(body))
		}
	}
	var its strings.Builder
	for _, m := range items {
		if m.Decision != nil {
			fmt.Fprintf(&its, "\n# %s\nDecision status: %s\nAs of: %s\nDurability: %s\nFlip conditions: %s\n",
				m.Title, m.DecisionStatus, m.Decision.AsOf, m.Decision.Durability,
				strings.Join(m.Decision.FlipConditions, "; "))
			if m.Decision.ReviewBy != "" {
				fmt.Fprintf(&its, "Review by: %s\n", m.Decision.ReviewBy)
			}
			fmt.Fprintf(&its, "%s\n", m.Text)
			continue
		}
		fmt.Fprintf(&its, "\n# %s\n%s\n", m.Title, m.Text)
	}
	// Ordering by intent: when there IS a query, the caller already filtered
	// items to the most relevant memories — surface them first so the static
	// wiki preamble can never starve them out of the budget. With no query
	// (session-start briefing), the wiki preamble leads and items fill the rest.
	first, second := wiki.String(), its.String()
	if hasQuery {
		first, second = its.String(), wiki.String()
	}
	var out strings.Builder
	out.WriteString(truncateRunes(first, budget))
	if rem := budget - out.Len(); rem > 0 {
		out.WriteString(truncateRunes(second, rem))
	}
	return out.String()
}

// ftsToken normalizes a raw field into its bare term and a lowercase key used
// for stopword lookup. The key takes the part before any apostrophe (straight
// or curly) so contractions collapse to their head ("what's"→"what",
// "it's"→"it", curly "what’s"→"what").
func ftsToken(f string) (term, key string) {
	term = strings.Trim(f, `"':;,.!?()[]{}<>-`)
	if term == "" {
		return "", ""
	}
	key = strings.ToLower(term)
	if i := strings.IndexAny(key, "'’"); i > 0 {
		key = key[:i]
	}
	return term, key
}

// ftsIsStopword decides whether a token is a droppable function word. It is
// deliberately case-aware: a function word is dropped only when written in
// lowercase. An explicit capital or all-caps form (Will, WHO, IT, CAN, AM)
// signals a proper noun or acronym that is discriminative in a real query, so
// it survives — this generalizes past a hand-picked collision list to protect
// every name/acronym (Mora, Neil, GEO, MFA, IP, SF, …). Single-character
// function words ("a", "i") are pure noise and always dropped regardless of case.
func ftsIsStopword(term, key string) bool {
	if !ftsStopwords[key] {
		return false
	}
	if utf8.RuneCountInString(term) == 1 {
		return true
	}
	return term == strings.ToLower(term)
}
func ftsQuery(q string) string {
	// Build an OR of quoted content tokens. Space-joining (the original behavior)
	// made FTS5 treat the query as an implicit AND of every token, so a
	// natural-language query like "what did neil say about the offsite" matched
	// nothing (it required every word, stopwords included). OR-joining lets any
	// term match while bm25 ranks the best matches first.
	//
	// But a pure OR of *every* token dilutes ranking: stopwords ("the/with/what")
	// match nearly everything, ballooning the candidate pool and letting docs that
	// hit several common words (while missing the rare, meaningful ones) outrank
	// the true match. Measured on Adit's real-query golden set, dropping function
	// words lifts FTS recall@5 0.591→0.667 (and the hybrid surface 0.394→0.439),
	// with no query regressing inside the top-5 cutoff. So we OR only the
	// content terms; if a query is ALL stopwords we fall back to every token so we
	// never emit an empty MATCH (FTS5 errors on ""). Each term is double-quoted so
	// operators/specials (AND, OR, NOT, *, :, -) inside a term can't raise
	// "fts5: syntax error".
	type tok struct{ term, key string }
	var toks []tok
	for _, f := range strings.Fields(q) {
		term, key := ftsToken(f)
		if term == "" {
			continue
		}
		toks = append(toks, tok{term, key})
	}
	content := make([]tok, 0, len(toks))
	for _, t := range toks {
		if ftsIsStopword(t.term, t.key) {
			continue
		}
		content = append(content, t)
	}
	if len(content) == 0 {
		content = toks // all-stopword query: keep everything rather than match nothing
	}
	terms := make([]string, 0, len(content))
	for _, t := range content {
		terms = append(terms, `"`+strings.ReplaceAll(t.term, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}
func parseSearchArgs(args []string) (string, int, bool, []string, error) {
	scope := ""
	limit := 10
	jsonOut := false
	var query []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--scope":
			if i+1 >= len(args) {
				return "", 0, false, nil, errors.New("--scope requires value")
			}
			i++
			scope = args[i]
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case a == "--limit":
			if i+1 >= len(args) {
				return "", 0, false, nil, errors.New("--limit requires value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return "", 0, false, nil, err
			}
			limit = n
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return "", 0, false, nil, err
			}
			limit = n
		default:
			query = append(query, a)
		}
	}
	return scope, limit, jsonOut, query, nil
}
