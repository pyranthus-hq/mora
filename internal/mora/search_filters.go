package mora

import (
	searchpkg "github.com/pyranthus-hq/mora/internal/search"
	"sort"
	"time"
)

// search_filters.go — issue #241: optional trusted-source ("source") and
// time-window ("since_hours") filters shared by search_memory and
// context_memory. See filters_contract_test.go's header comment for the
// FROZEN interface this file implements.
//
// searchFilters is threaded through search.go/hybrid.go/memfile.go's
// retrieval entry points as a trailing variadic parameter
// (`filters ...searchFilters`) rather than a new required argument, so every
// EXISTING call site (70+ across the package, CLI + eval + tests) keeps
// compiling unchanged and the zero value (no filters supplied) is byte-
// identical to pre-#241 behavior by construction — active() gates the one new
// branch each arm takes.

// searchFilters is the parsed, validated optional filter pair.
type searchFilters = searchpkg.Filter

// active reports whether either filter was actually supplied.

// receipt is the frozen "filters" response object: nil (no key at all) when
// neither filter was supplied, otherwise exactly the supplied filter(s).

// normalizedSource returns the source filter in its FULLY CANONICAL
// family[:instance] form — built from SourceFamily/SourceInstance (already
// providerToType-normalized and grammar-validated by parseSearchFilters),
// NOT the raw caller-supplied Source string. "" when no source filter is
// active.
//
// This is the ONLY safe input to digestSourceMatches anywhere outside
// sqlPredicate's own SQL-side comparison (which already compares against
// SourceFamily/SourceInstance directly, never through digestSourceMatches at
// all). providerToType only aliases a WHOLE provider token ("applecal" ->
// "applecalendar" via an exact catalog-Provider match) — it does NOT know how
// to normalize a family:instance COMPOSITE like "applecal:work", because no
// catalog entry's Provider ever equals the literal string "applecal:work".
// So digestSourceMatches(key, "applecal:work") — the RAW string — silently
// matches NOTHING, even against a real applecalendar:work memory, while
// sqlPredicate's SourceFamily="applecalendar"/SourceInstance="work" correctly
// matches it. Every Go-side/envelope matching site (passes,
// excludedByFilterSources, filteredMissingSources, compactHealthFiltered)
// MUST call this instead of reading f.Source directly, or the query path and
// the no-query/health/confidence paths silently disagree for any aliased
// family with an account suffix (today: applecalendar, aliased from
// "applecal"). Source itself is preserved ONLY for the "filters" response
// receipt, which echoes back exactly what the caller sent, unnormalized.

// sqlPredicate returns an " AND ..." WHERE-clause fragment (empty when
// inactive) plus its bind args, for the retrieval arms that filter INSIDE
// SQL, before ORDER BY/LIMIT — search.go's searchMemories, hybrid.go's
// ftsSearchIDs/vectorSearchIDs/graphExpandIDs, and share.go's
// searchShareIndex. Every one of those arms aliases the memories table `m`,
// so this is hardcoded to that alias rather than threading a table-prefix
// param through six call sites for zero real benefit.
//
// Source: memories.provider is stored CANONICALIZED at index-write time
// (providerToType(m.Provider) — see index.go), so `m.provider = ?` against
// SourceFamily (equally canonicalized by parseSearchFilters) is an exact
// match — no per-row alias resolution needed at query time. A bare-family
// filter (SourceInstance == "") intentionally omits the account predicate so
// it matches EVERY account of that family, mirroring digestSourceMatches'
// family-selects-every-instance rule; a family:instance filter adds the
// account equality so only that one instance matches.
//
// since_hours: memories.created_at_unix is a Unix-seconds INTEGER computed
// ONCE at index-write time by parsing CreatedAt as an RFC3339 instant
// (index.go) — never a lexical string compare, and never re-parsed per query.
// A memory whose CreatedAt failed to parse at index time is stamped
// created_at_unix=math.MinInt64 (createdAtUnix's sentinel), NOT 0/epoch: with
// maxSinceHours as large as ~2.56M hours, `now - since_hours` can itself land
// BEFORE 1970 (a negative cutoff), and 0 would then satisfy `>= cutoff` —
// letting a malformed row LEAK THROUGH the exact filter meant to exclude it.
// MinInt64 is below any cutoff this arithmetic can ever produce (bounded by
// maxSinceHours, which is itself derived from staying inside int64 range —
// see maxSinceHours' own doc comment), so it fails closed regardless of how
// negative the cutoff gets.

// passes is passes' Go-side twin for listMemories (memfile.go): the no-query
// context_memory "recency briefing" fallback walks vault Markdown files
// directly (no SQL layer, no pool/LIMIT-crowding concern — every file is
// already parsed and predicate-checked before the newest-first sort+limit
// truncate), so there is no SQL predicate to push into. Operates on the
// ALREADY-PARSED Memory (Provider/Account/CreatedAt from frontmatter) via the
// SAME sourceInstanceKey + digestSourceMatches semantics the SQL predicate's
// SourceFamily/SourceInstance were derived from, and the same parsed-RFC3339-
// instant time comparison — never lexical. Matches on normalizedSource(),
// NOT the raw f.Source — see normalizedSource's doc comment for why the raw
// string is unsafe to compare with for an aliased family:instance selector.
func searchFilterPasses(f searchFilters, m Memory) bool {
	if f.SourceFamily != "" {
		key, ok := sourceInstanceKey(m)
		if !ok || !digestSourceMatches(key, f.NormalizedSource()) {
			return false
		}
	}
	if f.SinceHours > 0 {
		ts, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			return false
		}
		cutoff := f.Now.Add(-time.Duration(f.SinceHours) * time.Hour)
		if ts.Before(cutoff) {
			return false
		}
	}
	return true
}

// oneFilter picks the first (only meaningful) element of a variadic
// searchFilters slice, defaulting to the zero value (no-op) when omitted —
// the shared boilerplate every retrieval entry point's optional trailing
// param needs.

// createdAtUnix parses createdAt as an RFC3339 instant and returns its Unix
// seconds, or math.MinInt64 on a parse failure. The SINGLE conversion every
// index-write path (index.go, index_upsert.go, share_gen.go) calls to
// populate created_at_unix, so the fail-closed-on-malformed-timestamp rule
// lives in exactly one place.
//
// The sentinel is MinInt64, NOT 0 (Unix epoch) — 0 looks like a safe "always
// in the past" choice, but it is not: sqlPredicate's cutoff is `now -
// since_hours`, and with since_hours as large as maxSinceHours (~2.56M
// hours, ~292 years) that cutoff itself goes NEGATIVE (before 1970). A
// malformed row stamped 0 would then satisfy `created_at_unix >= cutoff`
// and LEAK THROUGH the very filter meant to exclude it — the "0 as sentinel"
// bug this comment exists to warn future edits away from reintroducing.
// MinInt64 is below any cutoff sqlPredicate's bounded arithmetic can ever
// produce, so the malformed row is excluded regardless of how large
// since_hours (and therefore how negative the cutoff) gets.

// parseSearchFilters extracts and validates the optional source/since_hours
// MCP args, failing CLOSED (an error, never a silent no-filter) on an
// unrecognized/malformed source or a since_hours that is not a positive
// integer. now is the caller's captured clock (mcp.go handlers call
// briefClock() once per request and thread it through).

// parseSourceFilter validates s against the family:instance grammar and
// returns its normalized (family, instance) components, failing closed
// (never silently accepting or silently degrading to a family-only match) on
// anything structurally malformed:
//
//   - "gmail"            -> ("gmail", "")       — family only, any account
//   - "gmail:work"        -> ("gmail", "work")   — exact instance
//   - "gmail:"            -> error               — empty instance is not a
//     valid instance selector, never silently treated as family-only
//   - "gmail:work:extra"  -> error               — the grammar is a SINGLE
//     colon; multi-colon composites have no defined meaning
//   - ""                  -> error
//   - an unrecognized family (after providerToType normalization) -> error
//
// digestSourceMatches has no notion of "ambiguous" or "ignore extra parts" —
// only exact-key / family-prefix / no-match — so this validates the GRAMMAR
// SHAPE before any matching is attempted; a structurally valid but
// nonexistent instance (e.g. "gmail:doesnotexist") is NOT rejected here (it
// is well-formed, just a legitimate zero-match filter — there is no
// enumerable universe of valid account labels to validate an instance
// against, matching digestSourceMatches' own semantics exactly).

// knownSourceFamily reports whether family (already providerToType-
// normalized) names a real catalog connector. The fail-closed gate: an
// unrecognized family is a tool error, never a silent no-filter/empty-match.
func searchCatalog() searchpkg.Catalog {
	return searchpkg.Catalog{Normalize: providerToType, Known: knownSourceFamily, Types: knownSourceTypes, Unsupported: func(family string) (string, bool) { reason, ok := unsupportedSourceFamilies[family]; return reason, ok }}
}
func parseSearchFilters(args map[string]any, now time.Time) (searchFilters, error) {
	return searchpkg.ParseFilter(args, now, searchCatalog())
}

func knownSourceFamily(family string) bool {
	for _, c := range connectorCatalog {
		if c.Type == family {
			return true
		}
	}
	return false
}

// unsupportedSourceFamilies lists catalog connector types (already
// providerToType-normalized) whose memories carry no per-item Provider
// identity, so a source filter for them can never match anything even
// though the family itself is real — REJECTED explicitly (fail closed)
// rather than silently accepted-but-permanently-empty.
//
// filesystem is the ONLY current case, and it is not incidental:
// ingestFilesystem (ingest.go) never sets Provider on the memories it
// writes, so the v4 index's memories.provider column is always "" for
// them. Critically, this is not a gap unique to search filters — digest
// has the IDENTICAL structural limitation today: sourceInstanceKey
// (connectors.go) returns ("", false) for an empty Provider, and
// buildDigest's byInstance loop (digest.go) skips non-groupable rows
// outright, so a filesystem memory never appears in ANY digest section
// either, and `digest --source filesystem` is equally silently empty.
// Issue #241's governing requirement is to REUSE digest's existing source
// semantics — "matchable in search filters but not in digest" would
// violate that parity, not extend it. (An alternative — deriving
// provider="filesystem"/account=<source name> at index time from the
// Tags:[]string{s.Type, s.Name} every filesystem row already carries —
// was considered and rejected for now: it would invent a NEW provenance
// invariant, coupling filter identity to a user-visible display field
// (Tags), and expand this change's blast radius into ingest.go, which is
// out of scope here. See the FOLLOW-UP note in
// docs/architecture/06-mcp-server.md's #241 section.)
//
// Every OTHER catalog family was audited the same way and confirmed to
// always set a real, non-empty Provider on every row, unconditionally: gmail
// and calendar via kindRegistry (internal/memory/mapped.go: gmail_thread ->
// "gmail", calendar_event -> "calendar"); imessage via an explicit
// `Provider: imessageProvider` assignment (internal/imessage/map.go);
// applecalendar via `memory.RegisterKind(KindAppleCalEvent, "event",
// "applecal")` (internal/applecal/applecal.go, normalized to
// "applecalendar" by providerToType); github via
// `memory.RegisterKind(KindIssue, "issue", "github")`. "manual"/"mcp"/other Source labels
// are not connector-catalog families at all — knownSourceFamily already
// rejects them as unknown before this map is ever consulted.
var unsupportedSourceFamilies = map[string]string{
	"filesystem": "filesystem memories carry no connector provider identity (ingestFilesystem never sets Provider) — family/instance filtering is unsupported for this source pending durable provenance (see docs/architecture/06-mcp-server.md's #241 section)",
}

// knownSourceTypes lists every catalog connector type, sorted — used only to
// compose the unknown-source error's guidance text.
func knownSourceTypes() []string {
	out := make([]string, 0, len(connectorCatalog))
	for _, c := range connectorCatalog {
		out = append(out, c.Type)
	}
	sort.Strings(out)
	return out
}

// excludedByFilterSources returns the SORTED set of enabled connector
// instance keys (sourceHealthAll) an ACTIVE source filter excludes — the
// exact complement of filteredMissingSources'/compactHealthFiltered's kept
// population. Issue #241's acceptance criterion is that health/confidence
// output DISTINGUISHES excluded_by_filter from unavailable/unhealthy — mere
// omission from missing_sources (filteredMissingSources) leaves that
// ambiguous (is gmail absent because it's healthy, or because it was
// excluded?), so mcp.go surfaces this as an explicit top-level
// "excluded_by_filter" array, a sibling of "filters"/"confidence"/"health"
// — never nested inside confidence's frozen exact-field-set shape
// (confidence_contract_test.go's confidenceWantFields). Returns nil (no key
// at all, never an empty array) when the filter excludes nothing, mirroring
// this codebase's "no key when nothing to report" convention
// (results_truncated, shares_unhealthy). sourceHealthAll's own contract
// guarantees a Key-sorted slice, so filtering it preserves that order.
// filters MUST be the parsed searchFilters (never a raw source string) so
// this routes through filters.NormalizedSource() — see normalizedSource's
// doc comment for why the raw caller-supplied string is unsafe to compare
// with for an aliased family:instance selector.
func excludedByFilterSources(cfg Config, now time.Time, filters searchFilters) []string {
	source := filters.NormalizedSource()
	all := sourceHealthAll(cfg, now)
	var excluded []string
	for _, h := range all {
		if !digestSourceMatches(h.Key, source) {
			excluded = append(excluded, h.Key)
		}
	}
	return excluded
}

// filteredMissingSources is confidence.go's confidenceSourceGaps recomputed
// with the population narrowed to sources an ACTIVE source filter did not
// exclude — the #241/#238 interaction: a source excluded by the caller's OWN
// filter is a caller choice, not an incomplete-coverage signal, so it must
// never appear in confidence.missing_sources or move health_impact.
// confidence.go itself is UNTOUCHED (frozen): this is a standalone recompute
// over the same sourceHealthAll/worstSource primitives, called only from
// mcp.go's search_memory handler when a source filter is active, and used to
// OVERWRITE the frozen searchConfidence's MissingSources/HealthImpact fields
// after the fact — never by parameterizing confidenceSourceGaps itself.
// filters MUST be the parsed searchFilters (never a raw source string) —
// see excludedByFilterSources' identical note / normalizedSource's doc
// comment.
func filteredMissingSources(cfg Config, now time.Time, filters searchFilters) ([]string, string) {
	source := filters.NormalizedSource()
	all := sourceHealthAll(cfg, now)
	kept := make([]sourceHealth, 0, len(all))
	missing := make([]string, 0, len(all))
	for _, h := range all {
		if !digestSourceMatches(h.Key, source) {
			continue // excluded by the caller's own filter — not a coverage gap
		}
		kept = append(kept, h)
		if h.State != healthFresh {
			missing = append(missing, h.Key)
		}
	}
	impact := "none"
	if worst := worstSource(kept); worst != nil {
		impact = worst.State
	}
	return missing, impact
}

// compactHealthFiltered is compactHealthOf recomputed with h.Sources narrowed
// the SAME way filteredMissingSources narrows confidence's population: a
// source excluded by an active source filter must not drag the ALWAYS-PRESENT
// top-level "health" rollup into degraded/unhealthy either — both
// compactHealthOf and the confidence envelope project the SAME
// sourceHealthAll/worstSource signal, so excluding a source from one without
// the other would be an inconsistent half-fix (filters_contract_test.go's
// TestFilters*HealthExcludesFilteredSource). indexhealth.go/health_envelope.go
// are UNTOUCHED: this reuses healthOf + compactHealthFrom, the SAME
// composable primitives compactHealthOf itself is built from, filtering only
// the Sources slice and re-deriving State via the existing
// aggregateHealthState so Index/Producers/Shares contributions are unaffected.
// filters MUST be the parsed searchFilters (never a raw source string) —
// see excludedByFilterSources' identical note / normalizedSource's doc
// comment.
func compactHealthFiltered(cfg Config, now time.Time, filters searchFilters) compactHealth {
	source := filters.NormalizedSource()
	h := healthOf(cfg, now)
	kept := make([]sourceHealth, 0, len(h.Sources))
	for _, s := range h.Sources {
		if digestSourceMatches(s.Key, source) {
			kept = append(kept, s)
		}
	}
	h.Sources = kept
	h.State = aggregateHealthState(h)
	return compactHealthFrom(h)
}

func oneFilter(filters []searchFilters) searchFilters { return searchpkg.One(filters) }
func createdAtUnix(createdAt string) int64            { return searchpkg.CreatedAtUnix(createdAt) }
