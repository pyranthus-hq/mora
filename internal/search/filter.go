package search

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

type Filter struct {
	// Source is the ORIGINAL, UNMODIFIED caller-supplied value (whitespace
	// included), e.g. "gmail" or "gmail:work" — kept byte-for-byte for the
	// "filters" response receipt only, which promises to echo back exactly
	// what the caller sent (docs/architecture/06-mcp-server.md's "Response
	// receipt" bullet). Parsing/validation runs on a TRIMMED copy
	// (parseSearchFilters), but Source itself is never trimmed or otherwise
	// normalized. Every matching comparison — sqlPredicate, passes,
	// excludedByFilterSources, filteredMissingSources, compactHealthFiltered
	// — MUST use normalizedSource()/SourceFamily/SourceInstance instead,
	// never Source directly (see normalizedSource's doc comment for why the
	// raw string is unsafe to compare with). "" means no source filter.
	Source string
	// SourceFamily/SourceInstance are Source's PARSED, NORMALIZED, VALIDATED
	// components — SourceFamily run through providerToType (the same
	// provider->catalog-type alias digestSourceMatches applies, e.g.
	// "applecal" -> "applecalendar"), so it matches the CANONICAL value every
	// retrieval arm's SQL predicate compares memories.provider against
	// (index-time-normalized, see index.go/index_upsert.go/share_gen.go).
	// SourceInstance is "" when Source has no ":account" suffix (any account
	// of that family matches); otherwise the exact account label. Set ONLY
	// when Source != "" (parseSearchFilters validates+populates both
	// together, failing closed on anything that doesn't parse).
	SourceFamily, SourceInstance string
	// SinceHours is a positive look-back window in hours; "" (0) means no
	// time filter.
	SinceHours int
	// EventSinceHours is a separate explicit source-event window, never CreatedAt.
	EventSinceHours     int
	ExcludeDispositions []string
	// ExcludedMemoryIDs is a trusted query-time projection of local, applied
	// corrections. It is never parsed from caller arguments or used for shares.
	ExcludedMemoryIDs []string
	// Now is the reference instant SinceHours is computed against. It is
	// captured ONCE per MCP call (mcp.go handlers) and threaded down, so a
	// single call sees one consistent clock across every retrieval arm —
	// never a fresh time.Now() call deep inside an arm.
	Now time.Time
}

func One(filters []Filter) Filter {
	if len(filters) > 0 {
		return filters[0]
	}
	return Filter{}
}
func CreatedAtUnix(createdAt string) int64 {
	ts, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return math.MinInt64
	}
	return ts.Unix()
}
func (f Filter) Active() bool {
	return f.Source != "" || f.SinceHours > 0 || f.EventSinceHours > 0 || len(f.ExcludeDispositions) > 0
}
func (f Filter) Receipt() map[string]any {
	if !f.Active() {
		return nil
	}
	out := map[string]any{}
	if f.Source != "" {
		out["source"] = f.Source
	}
	if f.SinceHours > 0 {
		out["since_hours"] = f.SinceHours
	}
	if f.EventSinceHours > 0 {
		out["event_since_hours"] = f.EventSinceHours
	}
	if len(f.ExcludeDispositions) > 0 {
		out["exclude_dispositions"] = f.ExcludeDispositions
	}
	return out
}
func (f Filter) NormalizedSource() string {
	if f.SourceFamily == "" {
		return ""
	}
	if f.SourceInstance == "" {
		return f.SourceFamily
	}
	return f.SourceFamily + ":" + f.SourceInstance
}
func (f Filter) SQLPredicate() (clause string, args []any) {
	var parts []string
	if f.SourceFamily != "" {
		parts = append(parts, "m.provider = ?")
		args = append(args, f.SourceFamily)
		if f.SourceInstance != "" {
			parts = append(parts, "m.account = ?")
			args = append(args, f.SourceInstance)
		}
	}
	if f.SinceHours > 0 {
		parts = append(parts, "m.created_at_unix >= ?")
		args = append(args, f.Now.Add(-time.Duration(f.SinceHours)*time.Hour).Unix())
	}
	if f.EventSinceHours > 0 {
		cutoff := f.Now.Add(-time.Duration(f.EventSinceHours) * time.Hour)
		parts = append(parts, `EXISTS (SELECT 1 FROM activity_stamps ast WHERE ast.memory_id=m.id AND ast.scope=m.scope AND (ast.event_at_unix > ? OR (ast.event_at_unix = ? AND ast.event_at_nanos >= ?)) AND (ast.event_at_unix < ? OR (ast.event_at_unix = ? AND ast.event_at_nanos <= ?)))`)
		args = append(args, cutoff.Unix(), cutoff.Unix(), cutoff.Nanosecond(), f.Now.Unix(), f.Now.Unix(), f.Now.Nanosecond())
	}
	if len(f.ExcludedMemoryIDs) > 0 {
		encoded, _ := json.Marshal(f.ExcludedMemoryIDs)
		parts = append(parts, "m.id NOT IN (SELECT value FROM json_each(?))")
		args = append(args, string(encoded))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(parts, " AND "), args
}
