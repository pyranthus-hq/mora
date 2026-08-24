package mora

import (
	"errors"
	healthpkg "github.com/pyranthus-hq/mora/internal/health"
	"github.com/pyranthus-hq/mora/internal/memory"
	"io"
	"sort"
	"time"
)

const (
	healthNever               = healthpkg.Never
	healthFailed              = healthpkg.Failed
	healthStale               = healthpkg.Stale
	healthFresh               = healthpkg.Fresh
	sourceHealthUnreadableKey = healthpkg.UnreadableKey
)

type sourceHealth = healthpkg.Source

// syncErrorCodeForState resolves the published error code one persisted sync
// record carries, given the state it classified to. A typed code always wins.
// A failure with no typed code reads as connector.unclassified — the backfill
// rule for records written before this taxonomy shipped. A stale record with no
// recorded failure carries connector.stale, because staleness IS the CON-07
// discrimination that state expresses. A fresh source carries nothing.
func syncErrorCodeForState(state, code, lastError string) string {
	if backfilled := syncErrorCodeOrUnclassified(code, lastError); backfilled != "" {
		return backfilled
	}
	// No typed code and no recorded prose: the state is the only signal left.
	switch state {
	case healthFailed:
		return errCodeConnectorUnclassified
	case healthStale:
		return errCodeConnectorStale
	}
	return ""
}

// connectorErrorCodeFor reports the published code a connector failure already
// carries. An error nothing typed reads as connector.unclassified, which is the
// same value a pre-taxonomy record backfills to — "we failed and nothing named
// the cause" has exactly one representation, never two.
func connectorErrorCodeFor(err error) string {
	var typed moraError
	if errors.As(err, &typed) && classForErrorCode(typed.Code) == errClassConnector {
		return typed.Code
	}
	return errCodeConnectorUnclassified
}

func healthInstanceKeyForSource(s Source) string {
	if s.Type == "filesystem" && s.Account == "" && s.Name != "" {
		return s.Type + ":" + s.Name
	}
	return instanceKeyForSource(s)
}
func sourceHealthAll(cfg Config, now time.Time) []sourceHealth {
	out := []sourceHealth{}
	sources, err := loadSources(cfg)
	if err != nil {
		// Fail closed (not open): a corrupt sources.json must alarm, not vanish.
		// This one is NOT a connector failure — the config Mora reads to find its
		// connectors is the thing that could not be decoded, so it carries the
		// data class rather than a connector.* code.
		return []sourceHealth{{
			Key:       sourceHealthUnreadableKey,
			State:     healthFailed,
			LastError: "sources.json: " + err.Error(),
			ErrorCode: errCodeDataCorrupt,
		}}
	}
	seen := map[string]bool{}
	for _, s := range sources {
		if !s.IsEnabled() {
			continue
		}
		key := healthInstanceKeyForSource(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sourceHealthFor(cfg, s, key, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
func sourceHealthFor(cfg Config, s Source, key string, now time.Time) sourceHealth {
	path := syncStatusPathFor(cfg, s)
	var st *memory.SyncStatus
	if path != "" {
		st, _ = memory.LoadStatus(path)
	}
	var fact *healthpkg.Status
	if st != nil {
		fact = &healthpkg.Status{LastSuccessAt: st.LastSuccessAt, LastError: st.LastError, ErrorCount: st.ErrorCount}
	}
	h := healthpkg.Classify(key, s.Type, fact, now)
	if st != nil {
		// ErrorCode is the typed companion to LastError (CON-07), resolved from the
		// state Classify assigned plus the persisted record. A nil record carries
		// nothing — never-synced with no status file is not an error.
		h.ErrorCode = syncErrorCodeForState(h.State, st.ErrorCode, st.LastError)
	}
	return h
}
func healthBannerFromSources(sources []sourceHealth) string { return healthpkg.Banner(sources) }
func sanitizeHealthError(s string) string                   { return healthpkg.SanitizeError(s) }
func healthBanner(cfg Config, now time.Time) string {
	return healthBannerFrom(healthOf(cfg, now))
}
func stampSyncAttemptFailure(cfg Config, s Source, ingestErr error, attemptStart time.Time, out io.Writer) {
	attemptStart = attemptStart.Truncate(time.Second)
	path := syncStatusPathFor(cfg, s)
	if path == "" {
		return
	}
	st, err := memory.LoadStatus(path)
	if err != nil {
		return
	}
	if lastAttempt, perr := time.Parse(time.RFC3339, st.LastAttemptAt); perr == nil && !lastAttempt.Before(attemptStart) {
		// The inner path already stamped this attempt — including its ErrorCode,
		// which persistSyncStatus (ingest.go) set at the same moment it wrote the
		// prose. Returning without a re-load-and-save is the point of this guard.
		return
	}
	st.Source = s.Name
	st.LastAttemptAt = attemptStart.UTC().Format(time.RFC3339)
	st.LastError = ingestErr.Error()
	// The typed companion lands in the SAME stamp as the prose (CON-07).
	//
	// This branch covers failures raised BEFORE or AROUND memory.Ingest — an
	// unopenable database, a missing credential, an unknown source type. It is
	// NOT the only place a connector failure is persisted: a failure raised
	// INSIDE memory.Ingest stamps its own LastAttemptAt, trips the guard above,
	// and is typed by persistSyncStatus instead. Both boundaries are needed and
	// neither covers the other's family.
	st.ErrorCode = connectorErrorCodeFor(ingestErr)
	st.ErrorCount++
	if serr := saveSyncStatusFn(path, st); serr != nil && out != nil {
		warnf(out, "could not stamp sync failure (%s): %v", path, serr)
	}
}

var saveSyncStatusFn = memory.SaveStatus
