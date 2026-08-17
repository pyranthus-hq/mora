package health

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const (
	ProducerLockTTL        = 30 * time.Second
	ProducerAcquireTimeout = 2 * time.Second
)

// ProducerStore owns the crash-safe producer evidence and expectation files.
type ProducerStore struct {
	StateDir string
	WallNow  func() time.Time
	Sleep    func(time.Duration)
	Backoff  func(int) time.Duration
	PID      func() int
	Publish  func(string, []byte) (bool, error)
	Reap     func(string, time.Time, time.Duration, leasefile.RemovalOptions) (bool, error)
}

// Dir returns the producer ledger directory.
func (s ProducerStore) Dir() string { return filepath.Join(s.StateDir, "producers") }

// StatusPath returns the producer evidence ledger path.
func (s ProducerStore) StatusPath() string { return filepath.Join(s.Dir(), "status.json") }

// ExpectedPath returns the producer expectation ledger path.
func (s ProducerStore) ExpectedPath() string { return filepath.Join(s.Dir(), "expected.json") }

// LockPath returns the evidence RMW lease path.
func (s ProducerStore) LockPath() string { return s.StatusPath() + ".lock" }
func (s ProducerStore) wallNow() time.Time {
	if s.WallNow != nil {
		return s.WallNow()
	}
	return time.Now()
}
func (s ProducerStore) sleep(d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(d)
		return
	}
	time.Sleep(d)
}
func (s ProducerStore) backoff(attempt int) time.Duration {
	if s.Backoff != nil {
		return s.Backoff(attempt)
	}
	capMs := 1 << min(attempt, 5)
	return time.Duration(1+rand.IntN(capMs)) * time.Millisecond
}
func (s ProducerStore) pid() int {
	if s.PID != nil {
		return s.PID()
	}
	return os.Getpid()
}
func (s ProducerStore) removalOptions() leasefile.RemovalOptions {
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
func (s ProducerStore) publish(path string, body []byte) (bool, error) {
	if s.Publish != nil {
		return s.Publish(path, body)
	}
	return leasefile.Publish(path, body)
}
func (s ProducerStore) reap(path string, now time.Time, ttl time.Duration, o leasefile.RemovalOptions) (bool, error) {
	if s.Reap != nil {
		return s.Reap(path, now, ttl, o)
	}
	return leasefile.Reap(path, now, ttl, o)
}

// Acquire takes the bounded producer evidence lease using logical time only for TTL.
func (s ProducerStore) Acquire(now time.Time) (func(), error) {
	if err := os.MkdirAll(s.Dir(), 0700); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(leasefile.Body{PID: s.pid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	deadline := s.wallNow().Add(ProducerAcquireTimeout)
	path := s.LockPath()
	for attempt := 0; ; attempt++ {
		published, err := s.publish(path, body)
		wait := s.backoff(attempt)
		switch {
		case err == nil && published:
			return leasefile.Releaser(path, body, s.removalOptions()), nil
		case err != nil && !atomicio.SharingViolationRetryable(err):
			return nil, err
		case err == nil:
			reaped, rerr := s.reap(path, now, ProducerLockTTL, s.removalOptions())
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
	return nil, fmt.Errorf("producer ledger is locked by another mora process (%s); retry in a moment", path)
}

// LoadStatus returns an empty map on absence and fails loudly on corruption.
func (s ProducerStore) LoadStatus() (map[string]ProducerStatus, error) {
	data, err := os.ReadFile(s.StatusPath())
	if os.IsNotExist(err) {
		return map[string]ProducerStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]ProducerStatus{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("producer ledger %s is corrupt: %w", s.StatusPath(), err)
	}
	return m, nil
}

// SaveStatus atomically persists indented evidence JSON with mode 0600.
func (s ProducerStore) SaveStatus(m map[string]ProducerStatus) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return atomicio.Write(s.StatusPath(), b, 0600)
}

// MutateStatus reloads evidence under the real-wall-clock lease before saving.
func (s ProducerStore) MutateStatus(mutate func(map[string]ProducerStatus) error) error {
	release, err := s.Acquire(time.Now())
	if err != nil {
		return err
	}
	defer release()
	m, err := s.LoadStatus()
	if err != nil {
		return err
	}
	if err := mutate(m); err != nil {
		return err
	}
	return s.SaveStatus(m)
}

// LoadExpected returns an empty map on absence and fails loudly on corruption.
func (s ProducerStore) LoadExpected() (map[string]ExpectedProducer, error) {
	data, err := os.ReadFile(s.ExpectedPath())
	if os.IsNotExist(err) {
		return map[string]ExpectedProducer{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]ExpectedProducer{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("producer expectation %s is corrupt: %w", s.ExpectedPath(), err)
	}
	return m, nil
}

// SaveExpected atomically persists indented expectation JSON with mode 0600.
func (s ProducerStore) SaveExpected(m map[string]ExpectedProducer) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return atomicio.Write(s.ExpectedPath(), b, 0600)
}
