package mora

import (
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
		return []sourceHealth{{Key: sourceHealthUnreadableKey, State: healthFailed, LastError: "sources.json: " + err.Error()}}
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
	return healthpkg.Classify(key, s.Type, fact, now)
}
func healthBannerFromSources(sources []sourceHealth) string { return healthpkg.Banner(sources) }
func worstSource(sources []sourceHealth) *sourceHealth      { return healthpkg.Worst(sources) }
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
		return // the inner path already stamped this attempt.
	}
	st.Source = s.Name
	st.LastAttemptAt = attemptStart.UTC().Format(time.RFC3339)
	st.LastError = ingestErr.Error()
	st.ErrorCount++
	if serr := saveSyncStatusFn(path, st); serr != nil && out != nil {
		warnf(out, "could not stamp sync failure (%s): %v", path, serr)
	}
}

var saveSyncStatusFn = memory.SaveStatus
