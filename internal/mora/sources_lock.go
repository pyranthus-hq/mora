package mora

import (
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// sources.json read-modify-write serialization (P3).
//
// atomicWrite makes each saveSources durable and collision-free, but two
// processes each doing loadSources -> mutate -> saveSources still race: the last
// rename wins and silently drops the other's mutation (a lost update — an enable
// bit, a deny-list, a persisted window). mutateSources closes that hole by
// holding a short-lived cross-process lease around the WHOLE read-modify-write
// and, crucially, RELOADING inside the lease so a concurrent writer's committed
// change is always observed, never clobbered.
//
// The lease reuses the crash-safe file-lock primitives proven in loop.go
// (publishLockFile's os.Link-atomic publish, reapStaleLockTTL's TTL/corrupt
// reap, breakLock's guarded compare/remove, loopLockReleaser) — the same mechanism that
// ships green on the Windows CI suite. It is a SINGLE-HOST, SINGLE-USER lease,
// which is exactly the concurrency model here: one machine's mora processes,
// e.g. a manual command racing a scheduled sync/ingest.

const (
	// sourcesLockTTL bounds a leaked lease: a crashed holder's .lock is force-
	// reaped once its acquired_at is older than this. A real read-modify-write
	// holds the lease for microseconds (read a small file, mutate in memory,
	// atomicWrite) and it is NEVER held across ingest/rebuild, so 30s is far
	// longer than any legitimate hold — it only ever reaps an abandoned lease.
	sourcesLockTTL = 30 * time.Second
	// sourcesAcquireTimeout is the WALL-CLOCK budget for the contention spin.
	// Unlike the loop lease (which no-ops when a live run holds the lock), a
	// sources RMW must WAIT for the current holder — which releases within
	// microseconds — so the budget is generous. It states in seconds what a
	// 100-attempt bound only implied: because each retry backs off with the
	// JITTERED sourcesAcquireBackoff, that bound's real wait was a random draw
	// (~1.5 s typical, ~3 s worst case), not a stated budget. 2 s keeps the same
	// envelope — far longer than any live-holder hold, with ample margin for
	// Windows sharing-violation retries — while making the wait a promise the
	// caller can rely on. Deliberately NOT the share subsystem's 10 s lease
	// timeout: that lease covers a clone + index build, this one covers a
	// microsecond file rewrite. Each lock path owns its own constant so one
	// path's envelope can never silently redefine another's.
	sourcesAcquireTimeout = 2 * time.Second
)

// sourcesAcquireBackoff returns the pause before acquire retry `attempt`. It is
// JITTERED and capped, mirroring atomicWrite's #74 Windows fix: a FIXED backoff
// makes rival writers retry in lockstep and keep colliding on the same `.lock`
// (repeated ERROR_SHARING_VIOLATION), whereas jitter de-correlates them so one
// wins each round. The cap keeps the total spin inside the acquire budget.
func sourcesAcquireBackoff(attempt int) time.Duration {
	capMs := 1 << min(attempt, 5) // backoff ceiling grows 1,2,4,8,16,32,32… ms
	return time.Duration(1+mrand.IntN(capMs)) * time.Millisecond
}

// sleepWithinDeadline pauses for wait and reports whether the caller may retry.
// It refuses — and does NOT sleep — when that pause would run past deadline, so a
// loop built on it never overshoots its stated budget by a backoff draw. This is
// what makes the lock loops bounded by WALL-CLOCK time instead of by an attempt
// count whose duration depends on how the jittered backoff happened to draw.
// A zero wait is the "retry immediately" case (an abandoned lease was just
// reaped) and is still deadline-checked, so even a rival that keeps planting
// reapable leases cannot spin a loop past its budget.
//
// The deadline is always real wall-clock (time.Now()-based), never a caller's
// injected logical `now`: the injected clock governs TTL/stamp decisions, while
// this budget bounds how long a physically-running process blocks. Feeding a
// logical clock in would make a test's skewed `now` either hang or fail
// instantly (mutateProducers draws the same line for the TTL).
func sleepWithinDeadline(wait time.Duration, deadline time.Time) bool {
	return sleepWithinDeadlineWith(wait, deadline, time.Now, time.Sleep)
}

// sleepWithinDeadlineWith keeps the deadline decision testable without a
// mutable package-level clock seam. The second check matters under scheduler
// delay: a sleep that was safe when planned can still wake after the deadline,
// and must not authorize one more lock operation.
func sleepWithinDeadlineWith(
	wait time.Duration,
	deadline time.Time,
	now func() time.Time,
	sleep func(time.Duration),
) bool {
	if !now().Add(wait).Before(deadline) {
		return false
	}
	sleep(wait)
	return now().Before(deadline)
}

// sourcesLockPath is the lease file co-located with sources.json. Its persistent
// OS guard is selected deterministically by leaseGuardPath under ConfigDir.
func sourcesLockPath(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "sources.json.lock")
}

// acquireSourcesLock takes the sources.json lease for the duration of one
// read-modify-write. It mirrors acquireLoopLock's publish/reap loop but with the
// sources TTL and a longer, waiting spin bounded by the sourcesAcquireTimeout
// wall-clock deadline, and it returns a real error (never a
// silent no-op) if the lease cannot be taken — a sources mutation that cannot
// serialize must fail loudly, not drop the write. The returned release removes
// the lease and is idempotent, so a deferred release is safe.
func acquireSourcesLock(cfg Config, now time.Time) (release func(), err error) {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return nil, err
	}
	lockPath := sourcesLockPath(cfg)
	// run_id is unused for the sources lease (acquire and release live in the
	// same scope, unlike loop's begin/done split); acquired_at drives the TTL.
	body, _ := json.Marshal(loopLockBody{PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	deadline := time.Now().Add(sourcesAcquireTimeout)
	for attempt := 0; ; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		wait := sourcesAcquireBackoff(attempt)
		switch {
		case perr == nil && published:
			return loopLockReleaser(lockPath, body), nil
		case perr != nil && !sharingViolationRetryable(perr):
			// A real, non-contention fs error: fail, never interleave a partial write.
			return nil, perr
		case perr == nil:
			// The lock exists; try to reap it if the holder abandoned it (over TTL).
			reaped, rerr := reapStaleLockTTL(lockPath, now, sourcesLockTTL)
			if rerr != nil && !sharingViolationRetryable(rerr) {
				return nil, rerr // a real, non-contention read/rename error
			}
			if rerr == nil && reaped {
				wait = 0 // cleared an abandoned lease; retry publish immediately
			}
			// else: a live holder, OR Windows sharing contention on the read/reap —
			// fall through to the backoff and retry within budget.
		}
		// A live holder, or transient Windows sharing contention on publish/reap
		// (ERROR_SHARING_VIOLATION / ERROR_ACCESS_DENIED — a rival writer racing an
		// os.Remove/os.Link on the same `.lock`). Both are "contended, retry", NOT
		// fatal: back off and try again until the budget is spent. On non-Windows,
		// sharingViolationRetryable is always false, so only the live-holder path
		// reaches here — behavior is unchanged off Windows.
		if !sleepWithinDeadline(wait, deadline) {
			break
		}
	}
	return nil, fmt.Errorf("sources.json is locked by another mora process (%s); retry in a moment", lockPath)
}

// mutateSources serializes a read-modify-write on sources.json across processes.
// It acquires the lease, RELOADS the registry inside the lease (the crux of the
// lost-update fix — loading before the lease would reintroduce the race), applies
// mutate to the freshly loaded slice, and saves the result before releasing. A
// mutate that returns an error aborts WITHOUT writing, exactly like the
// pre-serialization early returns did. This is the single boundary every
// sources.json mutation goes through; a new load->mutate->save added outside it
// reopens the race — route it through here (or acquireSourcesLock directly when a
// caller needs its own load-error handling, as connectFilesystem does).
func mutateSources(cfg Config, mutate func(sources []Source) ([]Source, error)) error {
	release, err := acquireSourcesLock(cfg, time.Now())
	if err != nil {
		return err
	}
	defer release()
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	next, err := mutate(sources)
	if err != nil {
		return err
	}
	return saveSources(cfg, next)
}
