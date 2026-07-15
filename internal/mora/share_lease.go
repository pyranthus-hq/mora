package mora

// share_lease.go — Packet H3: the import lease and the shareBuildAndPublish
// chokepoint. The lease is the ownership token the commit fence checks (H2c), so
// its release and heartbeat are run_id compare-and-claim, never blind. One outer
// chokepoint (used by subscribe, pull, and heal) acquires the global storage
// lease then the per-subscription import lease and releases both on every
// return; the inner build never reacquires either.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// shareImportAcquireAttempts bounds the contention spin for the import lease.
const shareImportAcquireAttempts = 100

// shareStorageLockPath is the ONE global admission lease serializing aggregate
// byte accounting across every subscription.
func shareStorageLockPath(cfg Config) string {
	return filepath.Join(cfg.DataDir, "share", "storage.lock")
}

// acquireImportLease takes subs/<name>/import.lock for run runID, stamping a
// {run_id,pid,acquired_at} body with the generous shareImportTTL abandonment
// bound. It reuses the crash-safe publish/reap primitives; a live holder is
// waited out within the budget, an abandoned lease (over TTL) is reaped, and a
// real fs error fails loudly.
func acquireImportLease(cfg Config, name, runID string, now time.Time) (release func(), err error) {
	if err := os.MkdirAll(shareSubRoot(cfg, name), 0o700); err != nil {
		return nil, err
	}
	lockPath := shareImportLockPath(cfg, name)
	body, _ := json.Marshal(loopLockBody{RunID: runID, PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	for attempt := 0; attempt < shareImportAcquireAttempts; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		switch {
		case perr == nil && published:
			return func() { releaseLockFileFor(lockPath, runID) }, nil
		case perr != nil && !sharingViolationRetryable(perr):
			return nil, perr
		case perr == nil:
			reaped, rerr := reapStaleLockTTL(lockPath, now, shareImportTTL)
			if rerr != nil && !sharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				continue
			}
		}
		if attempt < shareImportAcquireAttempts-1 {
			time.Sleep(sourcesAcquireBackoff(attempt))
		}
	}
	return nil, fmt.Errorf("share %q is being imported by another mora process (%s); retry in a moment", name, lockPath)
}

// acquireStorageLease takes the global share storage lease (same run-id owner-CAS
// primitive), serializing aggregate byte admission so two subscriptions cannot
// each pass the whole-product check concurrently.
func acquireStorageLease(cfg Config, runID string, now time.Time) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(shareStorageLockPath(cfg)), 0o700); err != nil {
		return nil, err
	}
	lockPath := shareStorageLockPath(cfg)
	body, _ := json.Marshal(loopLockBody{RunID: runID, PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	for attempt := 0; attempt < shareImportAcquireAttempts; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		switch {
		case perr == nil && published:
			return func() { releaseLockFileFor(lockPath, runID) }, nil
		case perr != nil && !sharingViolationRetryable(perr):
			return nil, perr
		case perr == nil:
			reaped, rerr := reapStaleLockTTL(lockPath, now, shareImportTTL)
			if rerr != nil && !sharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				continue
			}
		}
		if attempt < shareImportAcquireAttempts-1 {
			time.Sleep(sourcesAcquireBackoff(attempt))
		}
	}
	return nil, fmt.Errorf("share storage is locked by another mora process (%s); retry in a moment", lockPath)
}

// verifyImportLeaseOwner is the ownership re-verify the commit fence runs: the
// on-disk import.lock must exist, still carry run_id==runID, and be within TTL.
// A reaped holder (lease is a successor's or absent) fails here and ABORTS before
// linking a commit.
func verifyImportLeaseOwner(cfg Config, name, runID string, now time.Time) error {
	data, err := os.ReadFile(shareImportLockPath(cfg, name))
	if err != nil {
		return fmt.Errorf("share %q: import lease is gone — this run was reaped; aborting commit", name)
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil {
		return fmt.Errorf("share %q: import lease unreadable; aborting commit", name)
	}
	if body.RunID != runID {
		return fmt.Errorf("share %q: import lease now belongs to %q, not this run %q — reaped; aborting commit", name, body.RunID, runID)
	}
	if t, perr := time.Parse(time.RFC3339, body.AcquiredAt); perr != nil || now.UTC().Sub(t.UTC()) >= shareImportTTL {
		return fmt.Errorf("share %q: import lease is past its TTL; aborting commit", name)
	}
	return nil
}

// shareHeartbeatDivisor sets the heartbeat cadence (shareImportTTL/3) so a
// live-but-slow import is never spuriously reaped.
const shareHeartbeatDivisor = 3

// startImportHeartbeat runs a background CAS re-stamp every shareImportTTL/3 for
// the whole hold. If the re-stamp finds the lease is no longer ours (reaped), it
// stops; the commit fence's own re-verify then aborts the run. The returned stop
// func halts the ticker.
func startImportHeartbeat(cfg Config, name, runID string) (stop func()) {
	interval := shareImportTTL / shareHeartbeatDivisor
	if interval <= 0 {
		interval = time.Millisecond
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if !heartbeatLockFileFor(shareImportLockPath(cfg, name), runID, time.Now()) {
					return // reaped: stop; the fence re-verify will abort the commit
				}
			}
		}
	}()
	return func() { close(done) }
}

// shareBuildMode selects the chokepoint's attempt-record behavior.
type shareBuildMode int

const (
	buildModeImport shareBuildMode = iota // subscribe/pull: durable attempt lifecycle
	buildModeHeal                         // re-cut from frozen corpus: no new attempt record
)

// shareBuildAndPublish is the single chokepoint. It acquires the storage lease
// then the per-subscription import lease (the ONE legal order), runs a GC
// preflight sweep, starts the CAS heartbeat, and — in import mode — publishes the
// durable active attempt BEFORE fn does any transport write, transitioning it to
// its terminal state via the owner-CAS after fn returns. fn receives the fresh
// run_id and returns the committed seq (0 if it committed nothing).
func shareBuildAndPublish(ctx context.Context, cfg Config, name string, mode shareBuildMode, fn func(runID string) (int, error)) error {
	runID := newRunID(time.Now())
	storeRel, err := acquireStorageLease(cfg, runID, time.Now())
	if err != nil {
		return err
	}
	defer storeRel()
	leaseRel, err := acquireImportLease(cfg, name, runID, time.Now())
	if err != nil {
		return err
	}
	defer leaseRel()
	stop := startImportHeartbeat(cfg, name, runID)
	defer stop()

	// GC preflight: reclaim losers/orphans/stale staging before we account bytes.
	if serr := shareGCSweep(cfg, name, time.Now()); serr != nil {
		return serr
	}

	if mode == buildModeImport {
		if serr := startShareAttempt(cfg, name, runID, time.Now()); serr != nil {
			return serr
		}
	}
	seq, ferr := fn(runID)
	if mode == buildModeImport {
		if ferr != nil {
			_ = finishShareAttempt(cfg, name, runID, shareAttempt{
				RunID: runID, State: "failed", LastError: sanitizeHealthError(ferr.Error()),
			})
		} else {
			_ = finishShareAttempt(cfg, name, runID, shareAttempt{
				RunID: runID, State: "succeeded", Seq: seq,
			})
		}
	}
	return ferr
}
