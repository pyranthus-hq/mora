package mora

import (
	"context"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	searchpkg "github.com/pyranthus-hq/mora/internal/search"
	"os"
	"path/filepath"
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

// searchMemoryObservation is the optional instrumentation returned only to
// defaultSearchForMCP; ordinary search callers keep the long-standing result.
type searchMemoryObservation struct {
	ScoreFused bool
	Trace      retrievalTrace
}

// searchMemories is the FTS-only retrieval arm. filters is an optional
// trailing #241 source/since_hours pair (searchFilters{}, the zero value,
// when omitted — every pre-#241 call site keeps compiling unchanged and gets
// a byte-identical query/result). The filter is a TRUE SQL WHERE predicate
// (searchFilters.sqlPredicate, search_filters.go) appended BEFORE ORDER
// BY/LIMIT, evaluated against the indexed memories.provider/account/
// created_at_unix columns (combined v5 schema, #241) — the SAME row bm25 already
// ranked, never a second parseMemory() disk read of the live vault file mid-
// ranking. Because the exclusion happens in the WHERE clause itself, a
// filtered-out row is never fetched, never ranked, and can never crowd a
// matching row out of `limit` (filters_contract_test.go's
// TestFiltersSearchMemoryPreRankSourceProof/SinceHoursProof) — LIMIT stays
// unconditional; there is no longer a separate "drop the SQL LIMIT" branch.
func searchMemories(ctx context.Context, cfg Config, query, scope string, limit int, filters ...searchFilters) ([]Memory, error) {
	return searchMemoriesObserved(ctx, cfg, query, scope, limit, nil, filters...)
}

// searchMemoriesObserved preserves searchMemories' public package signature but
// optionally exposes the actual score domain and arms to MCP confidence. Static
// retrieval is raw BM25 until the Gmail segment arm participates; at that point
// fuseGmailSegmentArm overwrites Memory.Score with positive RRF values.
func searchMemoriesObserved(ctx context.Context, cfg Config, query, scope string, limit int, observed *searchMemoryObservation, filters ...searchFilters) ([]Memory, error) {
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
	// Issue #237 — corroborating-record clustering needs to see PAST the raw
	// `limit` so a cluster's freed slots can backfill from the next-best
	// DISTINCT candidates: fetch a deeper pool (same limit*5-min-50 formula
	// hybrid.go's arms use), cluster, then truncate to `limit` below. A
	// non-positive limit keeps today's exact SQL LIMIT semantics (0 rows /
	// SQLite's "no limit" for negative) and skips clustering entirely — no
	// caller passes one, but this preserves the pre-#237 edge behavior.
	sqlLimit := limit
	if limit > 0 {
		sqlLimit = limit * 5
		if sqlLimit < 50 {
			sqlLimit = 50
		}
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
	args = append(args, sqlLimit)
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
		m.Tags = genericutil.SplitCSV(tags)
		if full, ferr := parseMemory(m.Path); ferr == nil {
			full.Score = m.Score
			m = full
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	parentIDs := make([]string, len(out))
	for i, m := range out {
		parentIDs[i] = m.ID
	}
	if observed != nil {
		observed.Trace.FTS = append([]string(nil), parentIDs...)
		observed.Trace.PreTruncPool = sqlLimit
	}
	// Issue #243 — segment-grain FTS as an ADDITIONAL candidate source,
	// mapped to PARENT ids, before fusion/slot accounting (frozen interface
	// #3). Admit segment-ranked parents even when parent-grain FTS ranked
	// them outside its widened pool; parentIDs stays separate so those rows do
	// not receive a fabricated parent-arm contribution. Best-effort: a
	// segment-arm/admission failure degrades retrieval but must never fail
	// search_memory outright.
	// Gated on a non-empty arm so a query with zero segment matches is a
	// complete no-op — the byte-identity guarantee for non-participating
	// memories (frozen interface #5).
	segIDs, gsegEvidence, segErr := gmailSegmentQueryArmBounded(ctx, db, query, scope, sqlLimit, f)
	if segErr == nil && len(segIDs) > 0 {
		if admitted, admitErr := admitGmailSegmentCandidates(ctx, db, out, segIDs, sqlLimit); admitErr == nil {
			out = admitted
		}
		out = fuseGmailSegmentArm(out, parentIDs, segIDs)
		if observed != nil {
			observed.ScoreFused = true
			observed.Trace.Segment = append([]string(nil), segIDs...)
		}
	}
	// Legacy slot discipline (#237 round-2 P1 fix): capture the pre-filter rank
	// order's ids BEFORE suppression/visibility filtering touches `out`, so
	// clusterAndTruncate can tell a row visibility-filtered out of the legacy
	// top-`limit` window (never backfilled) from a row folded into a cluster
	// (backfilled) — see cluster.go's clusterAndTruncate doc comment.
	rawIDs := make([]string, len(out))
	for i, m := range out {
		rawIDs[i] = m.ID
	}
	if observed != nil && observed.ScoreFused {
		observed.Trace.Fused = append([]string(nil), rawIDs...)
	}
	// B4: a memory with a pending delete op is suppressed from the search chokepoint
	// (the memories JOIN) while its rebuild is broken, so deleted content is never
	// served even when the index still carries the row.
	out = suppressPendingDeletes(cfg, out)
	filtered, err := currentMemories(cfg, out, time.Now())
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		if len(segIDs) > 0 {
			gsegEvidence = completeGmailSegmentEvidence(ctx, db, query, scope, filtered, gsegEvidence, f)
		}
		attachGmailSegmentEvidence(filtered, gsegEvidence)
		return filtered, nil // preserve pre-#237 SQL-LIMIT edge semantics; no clustering
	}
	// Issue #237 — cluster the (deeper-than-limit) candidate pool and truncate
	// to `limit`, collapsing corroborating records into one slot per cluster.
	result := clusterAndTruncate(rawIDs, filtered, limit)
	// Issue #243 — attach evidence AFTER slot accounting: a pure function of
	// "does this SURVIVING row's parent have a query-matching segment" (DQ5).
	if len(segIDs) > 0 {
		gsegEvidence = completeGmailSegmentEvidence(ctx, db, query, scope, result, gsegEvidence, f)
	}
	attachGmailSegmentEvidence(result, gsegEvidence)
	return result, nil
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
	out.WriteString(genericutil.TruncateRunes(first, budget))
	if rem := budget - out.Len(); rem > 0 {
		out.WriteString(genericutil.TruncateRunes(second, rem))
	}
	return out.String()
}

// ftsToken normalizes a raw field into its bare term and a lowercase key used
// for stopword lookup. The key takes the part before any apostrophe (straight
// or curly) so contractions collapse to their head ("what's"→"what",
// "it's"→"it", curly "what’s"→"what").

func ftsQuery(q string) string { return searchpkg.FTSQuery(q) }
func parseSearchArgs(args []string) (string, int, bool, []string, error) {
	return searchpkg.ParseArgs(args)
}
func ftsToken(f string) (string, string)  { return searchpkg.Token(f) }
func ftsIsStopword(term, key string) bool { return searchpkg.IsStopword(term, key) }
