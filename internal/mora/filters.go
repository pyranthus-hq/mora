package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/previewfilter"
	"sort"
	"strings"
	"time"
)

// humanizeIndexBusy turns a raw SQLITE_BUSY ("database is locked") into an
// actionable message. The hourly ingest rebuilds the whole index inside one
// transaction; a read that outlasts the busy_timeout (e.g. during a large commit
// flush) should tell the user to retry, not surface a driver code. Non-busy errors
// pass through unchanged.
func humanizeIndexBusy(err error) error {
	if err == nil {
		return nil
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "sqlite_busy") || strings.Contains(s, "database is locked") {
		return fmt.Errorf("the index is busy (the hourly ingest is rebuilding it) — retry in a few seconds: %w", err)
	}
	return err
}

// resolveEntityFilter resolves a --entity/name value to its alias-id set, returning
// a user-facing error on no-match or ambiguity so every surface (brief/pulse/prep,
// CLI + MCP) reports the same guidance instead of a silently-empty result.
func resolveEntityFilter(ctx context.Context, cfg Config, name string) (map[string]bool, error) {
	_, idSet, ok, ambiguous, err := resolveEntityID(ctx, cfg, name)
	if err != nil {
		return nil, humanizeIndexBusy(err)
	}
	if len(ambiguous) > 0 {
		return nil, fmt.Errorf("%q is ambiguous — matches: %s. Re-run with the email/handle (e.g. --entity name@example.com)", name, strings.Join(ambiguous, ", "))
	}
	if !ok {
		return nil, fmt.Errorf("no entity matches %q — try `mora graph` to list known people", name)
	}
	return idSet, nil
}

// filterByInstance drops memories failing the entity/scope/since-days filters,
// per instance, returning a NEW map (never mutates input). Empty filters are the
// identity (byte-identical output). Pure over parsed memories + injected now, so
// it is deterministic. Source filtering is NOT done here — it stays section-level
// (digestSourceMatches). Placed in buildDigest AFTER the whole-vault memSal
// computation (P1-C) so salience ranks stay whole-vault while the surfaced set
// narrows. A since-days cutoff of <=0 is a no-op (a negative is never a future
// cutoff — P1-D); an unparseable CreatedAt is dropped under an active cutoff.
func filterByInstance(byInstance map[string][]Memory, opts briefOpts, now time.Time) map[string][]Memory {
	return previewfilter.FilterByInstance(byInstance, previewfilter.Options{EntityIDs: opts.entityIDSet, Scope: opts.scope, SinceDays: opts.sinceDays}, now)
}

// memoryMatchesPreviewFilters is the per-row predicate shared by digest input
// filtering and the empty-result evidence counter. Source remains instance-level
// and is deliberately handled by digestSourceMatches at the caller. Keeping one
// predicate prevents the evidence from claiming a match for a row the actual
// filter later drops (or vice versa).
func memoryMatchesPreviewFilters(m Memory, opts briefOpts, now time.Time) bool {
	return previewfilter.Matches(m, previewfilter.Options{EntityIDs: opts.entityIDSet, Scope: opts.scope, SinceDays: opts.sinceDays}, now)
}

// filters.go — entity/scope/since-days filtering for the brief/digest surfaces
// and the meeting-prep attendee filter. All membership logic is pure over parsed
// memories; entity resolution (DB-backed) lives in resolveEntityID.

// memoryMentionsEntity reports whether memory m references any person in idSet.
//
// idSet is the resolved alias-id SET for one entity: the canonical person id plus
// every address/handle alias id that the A3 merge (graph.go) folded under it.
// personRefs(m) emits RAW pre-merge ids (one per address/handle as it appears on
// the message), so testing SET membership — not scalar equality against a single
// canonical id — is what keeps a memory referencing a merged-away alias from being
// silently dropped (P1-A). An empty set matches nothing; callers gate "no entity
// filter" on len(idSet)==0 upstream. Pure: no I/O.
func memoryMentionsEntity(m Memory, idSet map[string]bool) bool {
	return previewfilter.MentionsEntity(m, idSet)
}

// resolveEntityID resolves a name/email/handle to one entity and returns its
// canonical person id PLUS the full alias-id SET (canonical ∪ every address/handle
// alias id) for membership testing. It mirrors graphGetEntity's resolution (same
// entities query + display-name/alias predicate) but, unlike graphGetEntity, it
// exposes the canonical id and applies a STRICT ambiguity contract:
//
//   - unique match            → (canonical, idSet, true, nil, nil)
//   - no match                → ("", nil, false, nil, nil)
//   - >1 distinct entity match → ("", nil, false, candidates, nil)
//
// An exact address/handle input (contains '@' or starts with '+') resolves
// directly and unambiguously to the cluster carrying that address, even if a
// display name is shared by others. The idSet is what callers thread into
// briefOpts.entityIDSet / the meeting-prep attendee filter; the canonical id is
// the edges key for evidence assembly. One indexed query; buildDigest stays DB-free.
func resolveEntityID(ctx context.Context, cfg Config, name string) (canonical string, idSet map[string]bool, ok bool, ambiguous []string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, false, nil, nil
	}
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return "", nil, false, nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT id, display_name, aliases, mention_count FROM entities WHERE id NOT LIKE 'memory:%'`)
	if err != nil {
		return "", nil, false, nil, err
	}
	type cand struct {
		id, display, aliasesJSON string
		aliases                  []string
	}
	isAddr := strings.Contains(name, "@") || strings.HasPrefix(name, "+")
	wantID := personID(name)
	var aliasHit *cand
	var byName []cand
	for rows.Next() {
		var c cand
		var mention int
		if err := rows.Scan(&c.id, &c.display, &c.aliasesJSON, &mention); err != nil {
			rows.Close()
			return "", nil, false, nil, err
		}
		if c.aliasesJSON != "" {
			_ = json.Unmarshal([]byte(c.aliasesJSON), &c.aliases)
		}
		if isAddr && aliasIDSet(c.id, c.aliases)[wantID] {
			cc := c
			aliasHit = &cc // exact address/handle match is unique to a cluster
		}
		if strings.EqualFold(c.display, name) || aliasMatches(c.aliasesJSON, name) {
			byName = append(byName, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, false, nil, err
	}

	if aliasHit != nil {
		return aliasHit.id, aliasIDSet(aliasHit.id, aliasHit.aliases), true, nil, nil
	}
	uniq := map[string]cand{}
	for _, c := range byName {
		uniq[c.id] = c
	}
	switch len(uniq) {
	case 0:
		return "", nil, false, nil, nil
	case 1:
		for _, c := range uniq {
			return c.id, aliasIDSet(c.id, c.aliases), true, nil, nil
		}
	}
	ids := make([]string, 0, len(uniq))
	for id := range uniq {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		disp := uniq[id].display
		if disp == "" {
			disp = strings.TrimPrefix(id, "person:")
		}
		ambiguous = append(ambiguous, disp+" <"+strings.TrimPrefix(id, "person:")+">")
	}
	return "", nil, false, ambiguous, nil
}

// clampSinceDays normalizes a since-days flag value: a negative is treated as no
// filter (all-time), matching the established `connect --since-days` convention and
// preventing the future-cutoff "hide everything" footgun (P1-D). Applied at every
// CLI/MCP value-extraction seam before briefOpts is built.
func clampSinceDays(n int) int { return previewfilter.ClampSinceDays(n) }

// aliasIDSet builds the membership set for one resolved entity: the canonical
// person id plus a personID() for every ADDRESS/HANDLE alias (an alias is an
// address/handle if it contains '@' or starts with '+', mirroring the trusted-
// identity test in graph.go). Plain display-name aliases (e.g. "Alex Owner") are
// excluded because personID would yield "person:Alex Owner", a key personRefs
// never emits. The stored address aliases are already lowercased; personID
// lowercases again defensively, so the result matches personRefs' raw ids exactly.
func aliasIDSet(canonical string, aliases []string) map[string]bool {
	return previewfilter.AliasIDSet(canonical, aliases)
}
