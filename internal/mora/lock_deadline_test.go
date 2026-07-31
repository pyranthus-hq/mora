package mora

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lock_deadline_test.go pins issue #235: every lease acquire spin and the lease
// REMOVAL retry are bounded by a stated WALL-CLOCK budget, not by an attempt
// count whose real duration was a jittered-backoff draw. The four paths carry
// per-path constants (sourcesAcquireTimeout, governanceAcquireTimeout,
// producerAcquireTimeout, leaseRemovalTimeout) precisely so one path's envelope
// cannot silently redefine another's — the failure mode that sank the first
// attempt at this fix, which borrowed the share subsystem's 10 s lease timeout
// for all four and made shutdown-time lease removal block for ten seconds.

// deadlineSlack is the scheduling headroom a give-up assertion allows over its
// budget. Generous enough for a loaded CI host under -race, tight enough that
// adopting a multi-second foreign timeout (e.g. the share lease's 10 s) fails.
const deadlineSlack = 2 * time.Second

// acquireLockCase describes one lease-acquire path: how to build a Config for
// it, how to acquire, where its lock file lives, its budget, and the exact
// user-facing give-up message the path has always returned.
type acquireLockCase struct {
	name    string
	newCfg  func(t *testing.T) Config
	acquire func(cfg Config, now time.Time) (func(), error)
	path    func(cfg Config) string
	budget  time.Duration
	wantMsg string
}

func acquireLockCases() []acquireLockCase {
	return []acquireLockCase{
		{
			name:    "sources",
			newCfg:  func(t *testing.T) Config { return Config{ConfigDir: t.TempDir()} },
			acquire: acquireSourcesLock,
			path:    sourcesLockPath,
			budget:  sourcesAcquireTimeout,
			wantMsg: "sources.json is locked by another mora process",
		},
		{
			name:    "governance",
			newCfg:  func(t *testing.T) Config { return Config{VaultDir: t.TempDir()} },
			acquire: acquireGovernanceLock,
			path:    governanceLockPath,
			budget:  governanceAcquireTimeout,
			wantMsg: "governance ledger is locked by another mora process",
		},
		{
			name:    "producer",
			newCfg:  func(t *testing.T) Config { return Config{StateDir: t.TempDir()} },
			acquire: acquireProducerLock,
			path:    producerLockPath,
			budget:  producerAcquireTimeout,
			wantMsg: "producer ledger is locked by another mora process",
		},
	}
}

// TestLockDeadlineBudgetsAreDedicatedAndSized is the constants contract: each
// acquire path owns its budget, every budget stays inside the envelope the old
// attempt-count spin actually produced (~1.5 s typical, ~3 s worst), and lease
// REMOVAL — which runs at shutdown and inside the guard every waiting acquirer
// is blocked behind — is strictly shorter than any acquire budget.
func TestLockDeadlineBudgetsAreDedicatedAndSized(t *testing.T) {
	// The pre-existing envelope: the retired 100-attempt bound slept at most
	// 1+2+4+8+16 ms then 32 ms per further attempt, so ~3.04 s worst case. No
	// path's wait may grow past that.
	const oldWorstCase = 3040 * time.Millisecond
	for _, tc := range acquireLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.budget <= 0 {
				t.Fatalf("%s acquire budget must be positive, got %s", tc.name, tc.budget)
			}
			if tc.budget > oldWorstCase {
				t.Fatalf("%s acquire budget %s exceeds the pre-existing ~%s envelope", tc.name, tc.budget, oldWorstCase)
			}
			if leaseRemovalTimeout >= tc.budget {
				t.Fatalf("lease removal budget %s must be shorter than the %s acquire budget %s", leaseRemovalTimeout, tc.name, tc.budget)
			}
		})
	}
	if leaseRemovalTimeout <= 0 {
		t.Fatalf("lease removal budget must be positive, got %s", leaseRemovalTimeout)
	}
}

// TestSleepWithinDeadlineRechecksAfterWake pins the scheduler-delay edge of
// the wall-clock contract. A backoff can fit when planned but wake late; that
// late wake must not authorize another publish, reap, or remove attempt.
func TestSleepWithinDeadlineRechecksAfterWake(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start
	var slept []time.Duration
	sleep := func(wait time.Duration) {
		slept = append(slept, wait)
		now = now.Add(wait)
	}

	if sleepWithinDeadlineWith(5*time.Millisecond, start.Add(5*time.Millisecond), func() time.Time { return now }, sleep) {
		t.Fatal("a wait ending at the deadline must be refused")
	}
	if len(slept) != 0 {
		t.Fatalf("refused wait slept %d time(s), want 0", len(slept))
	}

	now = start
	sleep = func(wait time.Duration) {
		slept = append(slept, wait)
		now = now.Add(wait + 10*time.Millisecond) // simulate a delayed wake
	}
	if sleepWithinDeadlineWith(5*time.Millisecond, start.Add(10*time.Millisecond), func() time.Time { return now }, sleep) {
		t.Fatal("a sleep that wakes after the deadline must refuse another attempt")
	}
	if len(slept) != 1 || slept[0] != 5*time.Millisecond {
		t.Fatalf("delayed wake slept %v, want [5ms]", slept)
	}

	now = start
	slept = nil
	sleep = func(wait time.Duration) {
		slept = append(slept, wait)
		now = now.Add(wait)
	}
	if !sleepWithinDeadlineWith(5*time.Millisecond, start.Add(10*time.Millisecond), func() time.Time { return now }, sleep) {
		t.Fatal("a sleep that wakes before the deadline must allow another attempt")
	}
}

// TestLockAcquireUncontendedTakesNoBudget pins the fast path: with no rival, an
// acquire publishes on its first attempt and returns immediately. A deadline
// rewrite that slept before (or after) a successful publish would show up here
// as seconds of latency on every uncontended mutation.
func TestLockAcquireUncontendedTakesNoBudget(t *testing.T) {
	for _, tc := range acquireLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.newCfg(t)
			start := time.Now()
			release, err := tc.acquire(cfg, time.Now())
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("uncontended %s acquire: %v", tc.name, err)
			}
			release()
			if elapsed > time.Second {
				t.Fatalf("uncontended %s acquire took %s; it must not consume its wait budget", tc.name, elapsed)
			}
			if _, err := os.Stat(tc.path(cfg)); !os.IsNotExist(err) {
				t.Fatalf("%s lease should be gone after release; stat err=%v", tc.name, err)
			}
		})
	}
}

// TestLockContentionOneWinnerThenWaiterProceeds is the contention proof for all
// three acquire paths: while writer A holds the lease, writer B must NOT get it
// (mutual exclusion), and once A releases, B must win inside its budget rather
// than starve. The deadline rewrite must not turn "wait for a microsecond
// holder" into either "steal it" or "give up early".
func TestLockContentionOneWinnerThenWaiterProceeds(t *testing.T) {
	for _, tc := range acquireLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.newCfg(t)
			relA, err := tc.acquire(cfg, time.Now())
			if err != nil {
				t.Fatalf("A acquire: %v", err)
			}

			bDone := make(chan error, 1)
			go func() {
				relB, berr := tc.acquire(cfg, time.Now())
				if berr == nil {
					relB()
				}
				bDone <- berr
			}()

			// B must still be spinning: two holders at once is the lost update the
			// lease exists to prevent.
			select {
			case berr := <-bDone:
				relA()
				t.Fatalf("B acquired the %s lease while A held it (mutual exclusion broken): err=%v", tc.name, berr)
			case <-time.After(60 * time.Millisecond):
			}

			relA()
			select {
			case berr := <-bDone:
				if berr != nil {
					t.Fatalf("B must win the %s lease once A releases, got: %v", tc.name, berr)
				}
			case <-time.After(tc.budget + deadlineSlack):
				t.Fatalf("B never acquired the %s lease within %s of A's release", tc.name, tc.budget+deadlineSlack)
			}
		})
	}
}

// TestLockAcquireGivesUpAtItsDeadline is the wall-clock bound itself: against a
// FRESH (non-reapable, live-holder) lease the acquire spends roughly its budget
// and then returns the path's unchanged "…is locked by another mora process
// (<path>); retry in a moment" error — the string callers match on
// (isTransientContention in governance_test.go) and must keep matching.
func TestLockAcquireGivesUpAtItsDeadline(t *testing.T) {
	for _, tc := range acquireLockCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.newCfg(t)
			now := time.Now()
			lockPath := tc.path(cfg)
			if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			// acquired_at = now, so the lease is well inside its TTL and is never
			// reaped: the acquirer can only wait it out and give up.
			fresh, _ := json.Marshal(loopLockBody{PID: 999999, AcquiredAt: now.UTC().Format(time.RFC3339)})
			if err := os.WriteFile(lockPath, fresh, 0o600); err != nil {
				t.Fatalf("plant live lease: %v", err)
			}

			start := time.Now()
			release, err := tc.acquire(cfg, now)
			elapsed := time.Since(start)
			if err == nil {
				release()
				t.Fatalf("%s acquire over a live lease must fail, never steal it", tc.name)
			}
			for _, want := range []string{tc.wantMsg, lockPath, "retry in a moment"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s give-up error %q must contain %q", tc.name, err, want)
				}
			}
			if elapsed < tc.budget/2 {
				t.Fatalf("%s acquire gave up after %s, far short of its %s budget", tc.name, elapsed, tc.budget)
			}
			if elapsed > tc.budget+deadlineSlack {
				t.Fatalf("%s acquire waited %s, past its %s budget (+%s slack)", tc.name, elapsed, tc.budget, deadlineSlack)
			}
		})
	}
}

// TestRemoveLeaseFileGuardedBudget covers the release path's three outcomes
// through the OS seams, so the Windows-only sharing-violation branch is
// deterministic on every host. Removal is retried only for a transient sharing
// violation, gives up inside leaseRemovalTimeout (never an acquire-sized wait at
// shutdown), and surfaces a genuine error on the first attempt.
func TestRemoveLeaseFileGuardedBudget(t *testing.T) {
	sharingErr := errors.New("stub ERROR_SHARING_VIOLATION")
	fatalErr := errors.New("stub permission denied")

	cases := []struct {
		name         string
		remove       func(calls *int) func(string) error
		retryable    func(error) bool
		wantErr      error
		wantMinCalls int
		wantMaxCalls int
	}{
		{
			name:         "transient sharing violation retries then gives up within budget",
			remove:       func(calls *int) func(string) error { return func(string) error { *calls++; return sharingErr } },
			retryable:    func(err error) bool { return errors.Is(err, sharingErr) },
			wantErr:      sharingErr,
			wantMinCalls: 2,
			wantMaxCalls: 0, // unbounded: the budget, not a count, is the contract
		},
		{
			name:         "genuine error is terminal on the first attempt",
			remove:       func(calls *int) func(string) error { return func(string) error { *calls++; return fatalErr } },
			retryable:    func(err error) bool { return errors.Is(err, sharingErr) },
			wantErr:      fatalErr,
			wantMinCalls: 1,
			wantMaxCalls: 1,
		},
		{
			name:         "already gone is success",
			remove:       func(calls *int) func(string) error { return func(string) error { *calls++; return os.ErrNotExist } },
			retryable:    func(err error) bool { return errors.Is(err, sharingErr) },
			wantErr:      nil,
			wantMinCalls: 1,
			wantMaxCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			origRemove, origRetryable := leaseRemoveFn, leaseRemovalRetryableFn
			t.Cleanup(func() { leaseRemoveFn, leaseRemovalRetryableFn = origRemove, origRetryable })
			leaseRemoveFn = tc.remove(&calls)
			leaseRemovalRetryableFn = tc.retryable

			start := time.Now()
			err := removeLeaseFileGuarded(filepath.Join(t.TempDir(), "lease.lock"))
			elapsed := time.Since(start)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("removeLeaseFileGuarded err = %v, want %v", err, tc.wantErr)
			}
			if calls < tc.wantMinCalls {
				t.Fatalf("os.Remove called %d time(s), want at least %d", calls, tc.wantMinCalls)
			}
			if tc.wantMaxCalls > 0 && calls > tc.wantMaxCalls {
				t.Fatalf("os.Remove called %d time(s), want at most %d", calls, tc.wantMaxCalls)
			}
			if elapsed > leaseRemovalTimeout+deadlineSlack {
				t.Fatalf("removal took %s, past its %s budget (+%s slack)", elapsed, leaseRemovalTimeout, deadlineSlack)
			}
		})
	}
}
