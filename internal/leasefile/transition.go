package leasefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

const RemovalTimeout = 500 * time.Millisecond

type Body struct {
	RunID      string `json:"run_id"`
	PID        int    `json:"pid"`
	AcquiredAt string `json:"acquired_at"`
}
type RemovalOptions struct {
	Remove       func(string) error
	Retryable    func(error) bool
	Backoff      func(int) time.Duration
	Now          func() time.Time
	Sleep        func(time.Duration)
	Timeout      time.Duration
	BeforeRemove func()
	AfterRead    func()
}

func DefaultRemovalOptions() RemovalOptions {
	return RemovalOptions{Remove: os.Remove, Retryable: atomicio.SharingViolationRetryable, Backoff: defaultBackoff, Now: time.Now, Sleep: time.Sleep, Timeout: RemovalTimeout}
}
func defaultBackoff(attempt int) time.Duration {
	capMs := 1 << min(attempt, 5)
	return time.Duration(1+rand.IntN(capMs)) * time.Millisecond
}
func normalizeRemoval(o RemovalOptions) RemovalOptions {
	d := DefaultRemovalOptions()
	if o.Remove == nil {
		o.Remove = d.Remove
	}
	if o.Retryable == nil {
		o.Retryable = d.Retryable
	}
	if o.Backoff == nil {
		o.Backoff = d.Backoff
	}
	if o.Now == nil {
		o.Now = d.Now
	}
	if o.Sleep == nil {
		o.Sleep = d.Sleep
	}
	if o.Timeout <= 0 {
		o.Timeout = d.Timeout
	}
	return o
}
func removeGuarded(path string, o RemovalOptions) error {
	o = normalizeRemoval(o)
	var lastErr error
	deadline := o.Now().Add(o.Timeout)
	for attempt := 0; ; attempt++ {
		err := o.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !o.Retryable(err) {
			return err
		}
		lastErr = err
		wait := o.Backoff(attempt)
		if !o.Now().Add(wait).Before(deadline) {
			break
		}
		o.Sleep(wait)
		if !o.Now().Before(deadline) {
			break
		}
	}
	return lastErr
}
func Publish(path string, body []byte) (bool, error) {
	var published bool
	err := WithGuard(path, func() error { var err error; published, err = PublishGuarded(path, body); return err })
	return published, err
}
func PublishGuarded(path string, body []byte) (bool, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".lock-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
func Reap(path string, now time.Time, ttl time.Duration, o RemovalOptions) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var body Body
	stale := false
	if err := json.Unmarshal(data, &body); err != nil || body.AcquiredAt == "" {
		stale = true
	} else if t, err := time.Parse(time.RFC3339, body.AcquiredAt); err != nil {
		stale = true
	} else if now.UTC().Sub(t.UTC()) >= ttl {
		stale = true
	}
	if !stale {
		return false, nil
	}
	return Break(path, data, o)
}
func Break(path string, observed []byte, o RemovalOptions) (bool, error) {
	reaped := false
	err := WithGuard(path, func() error {
		cur, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			reaped = true
			return nil
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(cur, observed) {
			return nil
		}
		if o.BeforeRemove != nil {
			o.BeforeRemove()
		}
		if err := removeGuarded(path, o); err != nil {
			return err
		}
		reaped = true
		return nil
	})
	return reaped, err
}
func Releaser(path string, observed []byte, o RemovalOptions) func() {
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_, _ = Break(path, observed, o)
	}
}
func Release(path, owner string, o RemovalOptions) {
	_ = WithGuard(path, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if o.AfterRead != nil {
			o.AfterRead()
		}
		var body Body
		if json.Unmarshal(data, &body) == nil && body.RunID != "" && body.RunID != owner {
			return nil
		}
		return removeGuarded(path, o)
	})
}
func Heartbeat(path, owner string, now time.Time) bool {
	owned := false
	err := WithGuard(path, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var body Body
		if json.Unmarshal(data, &body) != nil || body.RunID != owner {
			return nil
		}
		body.AcquiredAt = now.UTC().Format(time.RFC3339Nano)
		next, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if err := atomicio.Write(path, next, 0o600); err != nil {
			return err
		}
		owned = true
		return nil
	})
	return err == nil && owned
}

func Remove(path string, o RemovalOptions) error { return removeGuarded(path, o) }
