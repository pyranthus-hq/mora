package registry

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	SourceLockTTL        = 30 * time.Second
	SourceAcquireTimeout = 2 * time.Second
)

// SourceStore owns the crash-safe sources.json read-modify-write lease.
type SourceStore struct {
	ConfigDir string
	WallNow   func() time.Time
	Sleep     func(time.Duration)
	Backoff   func(int) time.Duration
	PID       func() int
	Publish   func(string, []byte) (bool, error)
	Reap      func(string, time.Time, time.Duration, leasefile.RemovalOptions) (bool, error)
}

func (s SourceStore) cfg() config.Config { return config.Config{ConfigDir: s.ConfigDir} }

// LockPath returns the sources.json lease path.
func (s SourceStore) LockPath() string { return filepath.Join(s.ConfigDir, "sources.json.lock") }

// SourceAcquireBackoff returns the jittered, capped contention pause.
func SourceAcquireBackoff(attempt int) time.Duration {
	capMs := 1 << min(attempt, 5)
	return time.Duration(1+rand.IntN(capMs)) * time.Millisecond
}
func (s SourceStore) wallNow() time.Time {
	if s.WallNow != nil {
		return s.WallNow()
	}
	return time.Now()
}
func (s SourceStore) sleep(d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(d)
		return
	}
	time.Sleep(d)
}
func (s SourceStore) backoff(a int) time.Duration {
	if s.Backoff != nil {
		return s.Backoff(a)
	}
	return SourceAcquireBackoff(a)
}
func (s SourceStore) pid() int {
	if s.PID != nil {
		return s.PID()
	}
	return os.Getpid()
}
func (s SourceStore) options() leasefile.RemovalOptions {
	o := leasefile.DefaultRemovalOptions()
	if s.WallNow != nil {
		o.Now = s.WallNow
	}
	if s.Sleep != nil {
		o.Sleep = s.Sleep
	}
	if s.Backoff != nil {
		o.Backoff = s.Backoff
	}
	return o
}
func (s SourceStore) publish(p string, b []byte) (bool, error) {
	if s.Publish != nil {
		return s.Publish(p, b)
	}
	return leasefile.Publish(p, b)
}
func (s SourceStore) reap(p string, n time.Time, d time.Duration, o leasefile.RemovalOptions) (bool, error) {
	if s.Reap != nil {
		return s.Reap(p, n, d, o)
	}
	return leasefile.Reap(p, n, d, o)
}

// Acquire takes the bounded source-registry lease using logical time only for TTL.
func (s SourceStore) Acquire(now time.Time) (func(), error) {
	if err := os.MkdirAll(s.ConfigDir, 0700); err != nil {
		return nil, err
	}
	path := s.LockPath()
	body, _ := json.Marshal(leasefile.Body{PID: s.pid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	deadline := s.wallNow().Add(SourceAcquireTimeout)
	for attempt := 0; ; attempt++ {
		published, err := s.publish(path, body)
		wait := s.backoff(attempt)
		switch {
		case err == nil && published:
			return leasefile.Releaser(path, body, s.options()), nil
		case err != nil && !atomicio.SharingViolationRetryable(err):
			return nil, err
		case err == nil:
			reaped, rerr := s.reap(path, now, SourceLockTTL, s.options())
			if rerr != nil && !atomicio.SharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				wait = 0
			}
		}
		if !s.wallNow().Add(wait).Before(deadline) {
			break
		}
		s.sleep(wait)
		if !s.wallNow().Before(deadline) {
			break
		}
	}
	return nil, fmt.Errorf("sources.json is locked by another mora process (%s); retry in a moment", path)
}

// Mutate reloads sources.json under the real-wall-clock lease before saving.
func (s SourceStore) Mutate(mutate func([]memory.Source) ([]memory.Source, error)) error {
	release, err := s.Acquire(time.Now())
	if err != nil {
		return err
	}
	defer release()
	sources, err := LoadSources(s.cfg())
	if err != nil {
		return err
	}
	next, err := mutate(sources)
	if err != nil {
		return err
	}
	return SaveSources(s.cfg(), next)
}
