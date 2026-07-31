//go:build !windows

package mora

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestRemoveLeaseFileGuardedSingleAttemptOffWindows pins the unchanged
// non-Windows behavior of the deadline rewrite (#235). The retry loop exists
// only for Windows sharing violations; off Windows sharingViolationRetryable is
// always false, so the guarded removal must collapse to EXACTLY one os.Remove
// and consume none of its budget. Only the remove itself is stubbed — the real
// classifier stays in place, because it is the thing under test.
func TestRemoveLeaseFileGuardedSingleAttemptOffWindows(t *testing.T) {
	calls := 0
	denied := errors.New("permission denied")
	orig := leaseRemoveFn
	t.Cleanup(func() { leaseRemoveFn = orig })
	leaseRemoveFn = func(string) error { calls++; return denied }

	start := time.Now()
	err := removeLeaseFileGuarded(filepath.Join(t.TempDir(), "lease.lock"))
	elapsed := time.Since(start)

	if !errors.Is(err, denied) {
		t.Fatalf("removeLeaseFileGuarded err = %v, want %v", err, denied)
	}
	if calls != 1 {
		t.Fatalf("off Windows removal must be exactly one os.Remove, got %d", calls)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("off Windows removal must not back off at all, took %s", elapsed)
	}
}
