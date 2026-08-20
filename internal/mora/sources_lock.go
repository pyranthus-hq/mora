package mora

import (
	"time"

	registrypkg "github.com/pyranthus-hq/mora/internal/registry"
)

const (
	sourcesLockTTL        = registrypkg.SourceLockTTL
	sourcesAcquireTimeout = registrypkg.SourceAcquireTimeout
)

func sourcesAcquireBackoff(attempt int) time.Duration {
	return registrypkg.SourceAcquireBackoff(attempt)
}

func sleepWithinDeadline(wait time.Duration, deadline time.Time) bool {
	return sleepWithinDeadlineWith(wait, deadline, time.Now, time.Sleep)
}
func sleepWithinDeadlineWith(wait time.Duration, deadline time.Time, now func() time.Time, sleep func(time.Duration)) bool {
	if !now().Add(wait).Before(deadline) {
		return false
	}
	sleep(wait)
	return now().Before(deadline)
}
func sourceStore(cfg Config) registrypkg.SourceStore {
	return registrypkg.SourceStore{ConfigDir: cfg.ConfigDir}
}
func sourcesLockPath(cfg Config) string { return sourceStore(cfg).LockPath() }
func acquireSourcesLock(cfg Config, now time.Time) (func(), error) {
	return sourceStore(cfg).Acquire(now)
}
func mutateSources(cfg Config, mutate func([]Source) ([]Source, error)) error {
	return sourceStore(cfg).Mutate(mutate)
}
