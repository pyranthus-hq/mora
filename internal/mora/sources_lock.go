package mora

import (
	"encoding/json"
	"fmt"
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
// reap, breakLock's rename-claim, loopLockReleaser) — the same mechanism that
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
	// sourcesLockBackoff is the fixed pause between acquire attempts. Fixed (not
	// jittered) mirrors acquireLoopLock: correctness never depends on the timing
	// (os.Link is the atomic arbiter), only the number of wasted spins does.
	sourcesLockBackoff = 20 * time.Millisecond
	// maxSourcesAcquireAttempts * sourcesLockBackoff ~= 2s of contention budget.
	// Unlike the loop lease (which no-ops when a live run holds the lock), a
	// sources RMW must WAIT for the current holder — which releases within
	// microseconds — so the budget is generous, deterministic, and wall-clock-
	// independent.
	maxSourcesAcquireAttempts = 100
)

// sourcesLockPath is the lease file, co-located with sources.json so the
// os.Rename inside breakLock stays atomic (same filesystem) and the lock lives
// beside the file it guards.
func sourcesLockPath(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "sources.json.lock")
}

// acquireSourcesLock takes the sources.json lease for the duration of one
// read-modify-write. It mirrors acquireLoopLock's publish/reap loop but with the
// sources TTL and a longer, waiting spin, and it returns a real error (never a
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
	for attempt := 0; attempt < maxSourcesAcquireAttempts; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		if perr != nil {
			return nil, perr // real fs error: fail, never interleave a partial write
		}
		if published {
			return loopLockReleaser(lockPath), nil
		}
		reaped, rerr := reapStaleLockTTL(lockPath, now, sourcesLockTTL)
		if rerr != nil {
			return nil, rerr // transient read error: surface it, do not reap blindly
		}
		if reaped {
			continue // cleared an abandoned lease; retry publish (may lose a racer — fine)
		}
		if attempt < maxSourcesAcquireAttempts-1 {
			time.Sleep(sourcesLockBackoff)
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
