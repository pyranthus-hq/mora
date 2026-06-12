package mora

import (
	"sort"
	"strings"
)

// connectors.go owns the Phase-12 connector seams that the delta-watermark
// digest (Plans 03/04) consumes WITHOUT re-architecting connector storage:
//
//   - sourceInstanceKey  — the single watermark/grouping keying seam (M-1).
//   - ingestingConnectors — the enabled∩ingesting enumeration set (M-2).
//   - connectorDisplay    — the single owner of section Rank/Label DATA (M-6).
//
// These are pure additive helpers. The catalog descriptor (connectorInfo /
// connectorCatalog) lives in mora.go; this file only reads it and adds the new
// behavior, so Plan 04 consumes the descriptor here and never redefines it.

// sourceInstanceKey is THE single seam through which all watermark keying and
// three-state grouping routes (M-1). Today the key is exactly m.Provider — the
// single-account reality ("gmail" | "calendar" | "imessage"). This is the ONE
// place a future multi-account connector phase changes the key to
// "provider:account"; callers MUST NOT read m.Provider directly for keying.
//
// Provider elsewhere is demoted to a render-grouping LABEL only — never a key.
//
// The bool is the silent-data-loss guard (M-1): an empty-Provider memory (the
// filesystem connector sets no Provider — see ingestFilesystem in mora.go) is
// rejected with ("", false). Callers SKIP such memories rather than minting one
// shared empty-key watermark bucket that would collapse distinct sources (and
// trip the content-hash skip across them). It deliberately does NOT read the
// per-item m.Source field (== ProviderID), which would mint thousands of
// one-item keys.
func sourceInstanceKey(m Memory) (string, bool) {
	if m.Provider == "" {
		return "", false
	}
	// Normalize the memory-side Provider onto its catalog Type (applecal →
	// applecalendar) so this key reconciles with instanceKeyForSource by
	// construction. Alias at the lookup boundary ONLY — frontmatter on disk
	// keeps the provider the connector minted.
	provider := providerToType(m.Provider)
	// Multi-account (the seam this comment block promised): a labeled account
	// composes "provider:account" so each mailbox gets its own watermark bucket,
	// digest section, and three-state — never collapsed into the default's.
	if m.Account != "" {
		return provider + ":" + m.Account, true
	}
	return provider, true
}

// providerToType maps a memory-side Provider string onto its catalog connector
// Type. For most connectors the two are identical; a catalog entry with an
// explicit Provider (applecalendar mints "applecal") aliases here. Unknown
// providers pass through unchanged, so a future connector that keeps
// Provider == Type needs no entry. TestConnectorProviderKeysReconcile holds
// every ingesting connector to this round-trip.
func providerToType(provider string) string {
	for _, c := range connectorCatalog {
		if c.Provider != "" && c.Provider == provider {
			return c.Type
		}
	}
	return provider
}

// instanceKeyForSource is sourceInstanceKey's source-side twin: the instance
// key a SOURCE row produces (used by enumeration + sync-status resolution so
// they agree with the memory-side key by construction).
func instanceKeyForSource(s Source) string {
	if s.Account != "" {
		return s.Type + ":" + s.Account
	}
	return s.Type
}

// ingestingConnectors returns the SORTED set of connector TYPES that are BOTH
// enabled in sources.json (Source.IsEnabled()) AND tagged Ingesting in the
// catalog (M-2). This is the enumeration set the three-state classifier
// (Plan 04) drives from — explicitly NOT the sync/ dir and NOT
// providers-found-in-memories (which would silently hide a broken/all-deleted
// source, the SC#3 gap).
//
// Consequences this set buys by construction:
//   - A disabled connector is excluded (consent gate honored).
//   - An enabled ingesting connector with ZERO memories is still INCLUDED, so an
//     all-deleted / never-synced source can correctly surface "unavailable".
//   - A non-ingesting (live-passthrough PostHog/Linear, on-demand GitHub)
//     connector is excluded, so it can NEVER read "unavailable — sync error":
//     it persists no memory and no SyncStatus.
//
// Note for Plan 04: today Provider == connector-type for gmail/calendar/imessage,
// so the enumerated connector types reconcile directly against the
// memory-grouped sourceInstanceKey values. The connector-expansion phase is the
// one place that mapping becomes provider:account.
func ingestingConnectors(cfg Config) ([]string, error) {
	sources, err := loadSources(cfg)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		if !s.IsEnabled() {
			continue
		}
		ci, ok := lookupCatalog(s.Type)
		if !ok || !ci.Ingesting {
			continue
		}
		key := instanceKeyForSource(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out) // byte-stable determinism invariant
	return out, nil
}

// connectorUnknownRank is the deterministic rank for any instance key whose
// provider is NOT in the catalog. It sorts AFTER every known connector (the
// catalog max is 3 = filesystem) so an unknown connector is grouped last
// together — but it is a single shared, stable rank rather than the OLD
// switch's default rank 3, which collided with filesystem and made the section
// "first to be budget-truncated" (behavioral data loss). Unknown connectors
// then tie-break by key, preserving determinism.
const connectorUnknownRank = 100

// connectorDisplay maps an instance key to its section Rank + Label (M-6
// descriptor half). It is the SINGLE owner of rank/label DATA: Plan 04 calls it
// from buildDigest's sort + section-label code, replacing the hardcoded
// sourceDigestRank / digestSourceLabel switches so no digest code reads provider
// strings directly anymore.
//
// Resolution order:
//  1. exact catalog match (today's single-account keys: "gmail", "calendar", …);
//  2. a future "provider:account" composite resolves via its provider prefix, so
//     a 2nd gmail account inherits gmail's rank/label (Rank 2 / "Emails") rather
//     than falling to the unknown bucket;
//  3. a genuinely unknown connector gets connectorUnknownRank (never the
//     last-truncated default) and a clean, deterministically-derived label
//     (first rune upper-cased, rest unchanged) — never a title-cased raw
//     provider string and never empty.
func connectorDisplay(instanceKey string) (rank int, label string) {
	// 1. exact catalog match.
	if ci, ok := lookupCatalog(instanceKey); ok {
		return ci.Rank, ci.Label
	}
	// 2. composite "provider:account" → resolve by provider prefix, with the
	//    account label appended so two mailboxes never render as one "Emails".
	if i := strings.IndexByte(instanceKey, ':'); i > 0 {
		if ci, ok := lookupCatalog(instanceKey[:i]); ok {
			return ci.Rank, ci.Label + " (" + instanceKey[i+1:] + ")"
		}
	}
	// 3. unknown: shared stable rank + clean derived label.
	return connectorUnknownRank, cleanLabel(instanceKey)
}

// connectorUpcoming reports whether an instance key belongs to a connector
// whose items are future-dated events, so cold-start windows look FORWARD
// (inColdStartWindow). It reads the catalog's Upcoming capability — never a
// provider-string heuristic (the old HasPrefix(key, "calendar") silently
// missed applecalendar). Resolution mirrors connectorDisplay: exact key, then
// the provider prefix of a composite "provider:account" key; unknown
// connectors default to the past window.
func connectorUpcoming(instanceKey string) bool {
	if ci, ok := lookupCatalog(instanceKey); ok {
		return ci.Upcoming
	}
	if i := strings.IndexByte(instanceKey, ':'); i > 0 {
		if ci, ok := lookupCatalog(instanceKey[:i]); ok {
			return ci.Upcoming
		}
	}
	return false
}

// cleanLabel derives a human label from an unknown connector key: upper-case the
// first rune, leave the rest unchanged (e.g. "notion" → "Notion"). It is
// deliberately NOT strings.Title (which mangles the whole string) and never
// returns empty — a degenerate empty key yields the "Other" sentinel rather than
// panicking or producing junk.
func cleanLabel(key string) string {
	if key == "" {
		return "Other"
	}
	r := []rune(key)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
