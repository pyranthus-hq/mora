package mora

// share_health.go — Packet H4/H5: ONE health policy for subscribed shares.
// Serving and health-state are independent axes. A subscription serves the
// highest committed generation whenever one resolves; health STATE is worst-of,
// bounded to the committed HEAD plus the durable attempt record, and is `fresh`
// ONLY when the latest attempt has a matching durable succeeded record — so a
// SIGKILLed pull is surfaced, never read fresh.

import (
	"os"
	"time"
)

// shareStaleThreshold is the share head's staleness bound (the fifth threshold
// family, Landmine 13). A committed generation older than this ages to `stale`.
const shareStaleThreshold = 48 * time.Hour

// shareHealth is one subscription's freshness snapshot in the sourceHealth
// four-state vocabulary (never/failed/stale/fresh).
type shareHealth struct {
	Name      string
	State     string
	LastError string
	BuiltAt   string
}

// shareHealthOne computes the worst-of health of one subscription over the
// committed head + the durable attempt record.
func shareHealthOne(cfg Config, name string, now time.Time) shareHealth {
	h := shareHealth{Name: name, State: healthFresh}
	commit, ok, err := resolvePublishedCommit(cfg, name)
	if err != nil {
		// Unreadable commits/ dir or corrupt head record: fail closed.
		return shareHealth{Name: name, State: healthFailed, LastError: err.Error()}
	}
	if !ok {
		// No committed generation resolves.
		if _, lerr := os.Stat(shareMigratedLatchPath(cfg, name)); lerr == nil {
			return shareHealth{Name: name, State: healthFailed, LastError: "committed generation lost — run `mora share pull " + name + "`"}
		}
		if hasLegacyFlat(cfg, name) {
			return shareHealth{Name: name, State: healthFailed, LastError: "legacy share not yet migrated — run `mora share pull " + name + "`"}
		}
		return shareHealth{Name: name, State: healthNever}
	}
	h.BuiltAt = commit.BuiltAt

	if msg := shareHeadIntegrityError(cfg, name, commit); msg != "" {
		return shareHealth{Name: name, State: healthFailed, LastError: msg, BuiltAt: commit.BuiltAt}
	}

	// The durable attempt record: a terminal-failed or active-but-abandoned latest
	// attempt is failed; a currently-owned active attempt is stale ("refresh in
	// progress"); `fresh` requires a matching durable succeeded record.
	attempt, haveAttempt, attemptErr := loadShareAttempt(cfg, name)
	if attemptErr != nil {
		return shareHealth{Name: name, State: healthFailed, LastError: attemptErr.Error(), BuiltAt: commit.BuiltAt}
	}
	if !haveAttempt {
		// A generation-format commit proves this subscription has crossed the
		// migration boundary. From then on an absent attempt record is unknown
		// latest-attempt state, never a reason to infer stale/fresh from built_at.
		return shareHealth{Name: name, State: healthFailed, LastError: "attempt record missing after migration — run `mora share pull " + name + "`", BuiltAt: commit.BuiltAt}
	}
	freshOK := false
	// confirmedAt is when the served head was last confirmed current: its build
	// time, or a later verified no-op pull of that same head.
	confirmedAt := commit.BuiltAt
	switch attempt.State {
	case "failed":
		return shareHealth{Name: name, State: healthFailed, LastError: attempt.LastError, BuiltAt: commit.BuiltAt}
	case "active":
		if liveImportOwner(cfg, name, now) == attempt.RunID {
			return shareHealth{Name: name, State: healthStale, LastError: "refresh in progress", BuiltAt: commit.BuiltAt}
		}
		// Active but abandoned/interrupted (its lease is gone/expired).
		return shareHealth{Name: name, State: healthFailed, LastError: "import interrupted — run `mora share pull " + name + "`", BuiltAt: commit.BuiltAt}
	case "succeeded":
		switch {
		case attempt.Seq != commit.Seq:
		case attempt.RunID == commit.RunID:
			freshOK = true
		case attempt.VerifiedAt != "":
			// A verified no-op pull re-checked this exact head without building.
			freshOK = true
			confirmedAt = laterRFC3339(commit.BuiltAt, attempt.VerifiedAt)
		}
	}

	// Staleness of the last confirmation (built_at, or a later verified pull).
	if t, perr := time.Parse(time.RFC3339, confirmedAt); perr == nil {
		if now.Sub(t) > shareStaleThreshold {
			return shareHealth{Name: name, State: healthStale, BuiltAt: commit.BuiltAt}
		}
	}
	if !freshOK {
		// A committed head with no matching succeeded attempt (e.g. a crash before
		// the terminal transition) must never read fresh.
		return shareHealth{Name: name, State: healthStale, LastError: "no matching completed import record", BuiltAt: commit.BuiltAt}
	}
	return h
}

// shareHeadIntegrityError hashes BOTH head artifacts against the committed
// digests (per-artifact serving, cross-artifact visibility) and returns the
// health error for the first mismatch, or "" when both verify. A no-op pull
// runs the same check before it re-verifies a head instead of rebuilding it.
func shareHeadIntegrityError(cfg Config, name string, commit shareCommit) string {
	if d, err := fileDigestOf(shareGenIndexPath(cfg, name, commit.Gen)); err != nil || d != commit.IndexDigest {
		return "published index failed its integrity digest"
	}
	if d, err := corpusDigestOf(shareGenCorpusDir(cfg, name, commit.Gen)); err != nil || d != commit.CorpusDigest {
		return "published corpus failed its integrity digest"
	}
	return ""
}

// laterRFC3339 returns whichever RFC 3339 instant is later; an unparseable b
// never displaces a.
func laterRFC3339(a, b string) string {
	tb, err := time.Parse(time.RFC3339, b)
	if err != nil {
		return a
	}
	if ta, err := time.Parse(time.RFC3339, a); err == nil && !tb.After(ta) {
		return a
	}
	return b
}

// hasLegacyFlat reports whether a subscription still carries the pre-upgrade flat
// layout (subs/<name>/index.db or subs/<name>/corpus) — the untrusted local
// state that is never served or re-cut.
func hasLegacyFlat(cfg Config, name string) bool {
	if _, err := os.Stat(shareIndexPath(cfg, name)); err == nil {
		return true
	}
	if _, err := os.Stat(shareCorpusDir(cfg, name)); err == nil {
		return true
	}
	return false
}

// shareHealthAll computes every subscription's health (deterministic order).
func shareHealthAll(cfg Config, now time.Time) []shareHealth {
	out := []shareHealth{}
	sf, err := loadShares(cfg)
	if err != nil {
		return out
	}
	for _, s := range sf.Subscriptions {
		out = append(out, shareHealthOne(cfg, s.Name, now))
	}
	return out
}

// shareUnhealth is the compact surfaced shape (shares_unhealthy).
type shareUnhealth struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	LastError string `json:"last_error,omitempty"`
}

// sharesUnhealthy returns the non-fresh subscriptions for the shares_unhealthy
// surface key (search_memory / think / read_memory).
func sharesUnhealthy(cfg Config, now time.Time) []shareUnhealth {
	out := []shareUnhealth{}
	for _, h := range shareHealthAll(cfg, now) {
		if h.State == healthFresh {
			continue
		}
		out = append(out, shareUnhealth{Name: h.Name, State: h.State, LastError: h.LastError})
	}
	return out
}

// shareIndexHealthAll converts each subscription's health into the indexHealth
// shape for the Health.Index.Shares aggregate arm (worst-of across the personal
// index and every subscription).
func shareIndexHealthAll(cfg Config, now time.Time) []indexHealth {
	out := []indexHealth{}
	for _, h := range shareHealthAll(cfg, now) {
		ih := indexHealth{SchemaVersion: indexSchemaVersion, IndexedAt: h.BuiltAt, LastError: h.LastError}
		switch h.State {
		case healthFresh:
			ih.State = idxFresh
		case healthStale:
			ih.State = idxDegraded
		case healthNever:
			ih.State = idxNever
		default:
			ih.State = idxFailed
		}
		out = append(out, ih)
	}
	return out
}
