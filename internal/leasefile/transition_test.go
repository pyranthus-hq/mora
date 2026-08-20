package leasefile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var leaseNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func leasePath(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "lease.lock") }
func plant(t *testing.T, path, owner string, pid int, at time.Time) []byte {
	t.Helper()
	body, err := json.Marshal(Body{RunID: owner, PID: pid, AcquiredAt: at.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return body
}
func TestReapStaleLock_ByTTL(t *testing.T) {
	path := leasePath(t)
	plant(t, path, "run_x", os.Getpid(), leaseNow.Add(-16*time.Minute))
	reaped, err := Reap(path, leaseNow, 15*time.Minute, DefaultRemovalOptions())
	if err != nil || !reaped {
		t.Fatalf("reap=(%v,%v)", reaped, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat=%v", err)
	}
}
func TestReapStaleLock_FreshDeadPidNotReaped(t *testing.T) {
	path := leasePath(t)
	plant(t, path, "run_x", 999999, leaseNow)
	reaped, err := Reap(path, leaseNow, 15*time.Minute, DefaultRemovalOptions())
	if err != nil || reaped {
		t.Fatalf("reap=(%v,%v)", reaped, err)
	}
}
func TestReapStaleLock_FreshNotReaped(t *testing.T) {
	path := leasePath(t)
	plant(t, path, "run_x", os.Getpid(), leaseNow)
	reaped, err := Reap(path, leaseNow, 15*time.Minute, DefaultRemovalOptions())
	if err != nil || reaped {
		t.Fatalf("reap=(%v,%v)", reaped, err)
	}
}
func TestReapStaleLock_CorruptIsStale(t *testing.T) {
	for _, body := range [][]byte{{}, []byte("garbage"), []byte(`{"pid":0}`)} {
		path := leasePath(t)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		reaped, err := Reap(path, leaseNow, 15*time.Minute, DefaultRemovalOptions())
		if err != nil || !reaped {
			t.Fatalf("body=%q reap=(%v,%v)", body, reaped, err)
		}
	}
}
func TestBreakLock_ContentChangedPreserves(t *testing.T) {
	path := leasePath(t)
	fresh := []byte("AAA-fresh-holder")
	if err := os.WriteFile(path, fresh, 0o600); err != nil {
		t.Fatal(err)
	}
	reaped, err := Break(path, []byte("BBB-observed"), DefaultRemovalOptions())
	if err != nil || reaped {
		t.Fatalf("break=(%v,%v)", reaped, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, fresh) {
		t.Fatalf("body=%q err=%v", got, err)
	}
}
func TestBreakLockSerializesRemoveBeforeFreshPublish(t *testing.T) {
	path := leasePath(t)
	stale := []byte("stale")
	fresh := []byte("fresh")
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		published bool
		err       error
	}
	started := make(chan struct{})
	done := make(chan result, 1)
	o := DefaultRemovalOptions()
	o.BeforeRemove = func() {
		go func() { close(started); ok, err := Publish(path, fresh); done <- result{ok, err} }()
		<-started
		select {
		case r := <-done:
			t.Fatalf("publisher completed early: %+v", r)
		case <-time.After(20 * time.Millisecond):
		}
	}
	reaped, err := Break(path, stale, o)
	if err != nil || !reaped {
		t.Fatalf("break=(%v,%v)", reaped, err)
	}
	r := <-done
	if r.err != nil || !r.published {
		t.Fatalf("publish=%+v", r)
	}
}
func TestReleaseSerializesAgainstSameOwnerHeartbeat(t *testing.T) {
	path := leasePath(t)
	plant(t, path, "run_A", os.Getpid(), leaseNow)
	started := make(chan struct{})
	done := make(chan bool, 1)
	o := DefaultRemovalOptions()
	o.AfterRead = func() {
		go func() { close(started); done <- Heartbeat(path, "run_A", leaseNow.Add(time.Minute)) }()
		<-started
		select {
		case owned := <-done:
			t.Fatalf("heartbeat completed early: %v", owned)
		case <-time.After(20 * time.Millisecond):
		}
	}
	Release(path, "run_A", o)
	if <-done {
		t.Fatal("heartbeat resurrected lease")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat=%v", err)
	}
}
func TestOwnerCASAndRemovalRetry(t *testing.T) {
	path := leasePath(t)
	body := plant(t, path, "owner", 1, leaseNow)
	Release(path, "other", DefaultRemovalOptions())
	if _, err := os.Stat(path); err != nil {
		t.Fatal("other owner removed")
	}
	release := Releaser(path, body, DefaultRemovalOptions())
	release()
	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("releaser stat=%v", err)
	}
	path = leasePath(t)
	plant(t, path, "owner", 1, leaseNow)
	attempts := 0
	clock := leaseNow
	o := DefaultRemovalOptions()
	o.Remove = func(string) error {
		attempts++
		if attempts < 3 {
			return os.ErrPermission
		}
		return os.Remove(path)
	}
	o.Retryable = func(error) bool { return true }
	o.Backoff = func(int) time.Duration { return time.Millisecond }
	o.Now = func() time.Time { return clock }
	o.Sleep = func(d time.Duration) { clock = clock.Add(d) }
	Release(path, "owner", o)
	if attempts != 3 {
		t.Fatalf("attempts=%d", attempts)
	}
}
