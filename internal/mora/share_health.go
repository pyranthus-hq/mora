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

// shareRefreshing is a subscription whose committed generation verified intact
// while a live, owned pull replaces it. It serves normally and is not an alarm:
// a pull that dies leaves an abandoned attempt, which reads failed.
const shareRefreshing = "refreshing"

// shareStaleThreshold is the share head's staleness bound (the fifth threshold
// family, Landmine 13). A committed generation older than this ages to `stale`.
const shareStaleThreshold = 48 * time.Hour

// shareHealth is one subscription's freshness snapshot in the sourceHealth
// four-state vocabulary (never/failed/stale/fresh), plus shareRefreshing.
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

	// Head artifact integrity (hash BOTH digests — per-artifact serving, cross-
	// artifact visibility).
	genIndex := shareGenIndexPath(cfg, name, commit.Gen)
	if d, derr := fileDigestOf(genIndex); derr != nil || d != commit.IndexDigest {
		return shareHealth{Name: name, State: healthFailed, LastError: "published index failed its integrity digest", BuiltAt: commit.BuiltAt}
	}
	if d, derr := corpusDigestOf(shareGenCorpusDir(cfg, name, commit.Gen)); derr != nil || d != commit.CorpusDigest {
		return shareHealth{Name: name, State: healthFailed, LastError: "published corpus failed its integrity digest", BuiltAt: commit.BuiltAt}
	}

	// The durable attempt record: a terminal-failed or active-but-abandoned latest
	// attempt is failed; a currently-owned active attempt is refreshing (the
	// verified head keeps serving); `fresh` requires a matching durable succeeded
	// record.
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
	switch attempt.State {
	case "failed":
		return shareHealth{Name: name, State: healthFailed, LastError: attempt.LastError, BuiltAt: commit.BuiltAt}
	case "active":
		if liveImportOwner(cfg, name, now) == attempt.RunID {
			// A pull in progress does not excuse a head that is already stale:
			// until it commits, that old generation is what gets served.
			if headStale(commit.BuiltAt, now) {
				return shareHealth{Name: name, State: healthStale, LastError: "refresh in progress", BuiltAt: commit.BuiltAt}
			}
			return shareHealth{Name: name, State: shareRefreshing, BuiltAt: commit.BuiltAt}
		}
		// Active but abandoned/interrupted (its lease is gone/expired).
		return shareHealth{Name: name, State: healthFailed, LastError: "import interrupted — run `mora share pull " + name + "`", BuiltAt: commit.BuiltAt}
	case "succeeded":
		freshOK = attempt.Seq == commit.Seq && attempt.RunID == commit.RunID
	}

	// Staleness of the committed head's built_at.
	if headStale(commit.BuiltAt, now) {
		return shareHealth{Name: name, State: healthStale, BuiltAt: commit.BuiltAt}
	}
	if !freshOK {
		// A committed head with no matching succeeded attempt (e.g. a crash before
		// the terminal transition) must never read fresh.
		return shareHealth{Name: name, State: healthStale, LastError: "no matching completed import record", BuiltAt: commit.BuiltAt}
	}
	return h
}

// headStale reports whether a committed head's built_at is past the share
// staleness bound. An unparseable built_at is not judged here; the attempt
// record still gates freshness.
func headStale(builtAt string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, builtAt)
	return err == nil && now.Sub(t) > shareStaleThreshold
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
		if h.State == healthFresh || h.State == shareRefreshing {
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
		case shareRefreshing:
			ih.State = idxRefreshing
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
