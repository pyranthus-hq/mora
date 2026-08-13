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
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"time"
)

// shareLeaseAcquireTimeout bounds contention waits for the import and global
// storage leases. The lease can cover a local clone plus index build, which is
// routinely several seconds on Windows CI; a sub-second attempt budget made a
// correctly serialized duplicate subscriber fail before the winner registered.
const shareLeaseAcquireTimeout = 10 * time.Second

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
	body, _ := json.Marshal(loopLockBody{RunID: runID, PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339Nano)})
	deadline := time.Now().Add(shareLeaseAcquireTimeout)
	for attempt := 0; ; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		switch {
		case perr == nil && published:
			return func() { releaseLockFileFor(lockPath, runID) }, nil
		case perr != nil && !atomicio.SharingViolationRetryable(perr):
			return nil, perr
		case perr == nil:
			reaped, rerr := reapStaleLockTTL(lockPath, now, shareImportTTL)
			if rerr != nil && !atomicio.SharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				continue
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(sourcesAcquireBackoff(attempt))
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
	body, _ := json.Marshal(loopLockBody{RunID: runID, PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339Nano)})
	deadline := time.Now().Add(shareLeaseAcquireTimeout)
	for attempt := 0; ; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		switch {
		case perr == nil && published:
			return func() { releaseLockFileFor(lockPath, runID) }, nil
		case perr != nil && !atomicio.SharingViolationRetryable(perr):
			return nil, perr
		case perr == nil:
			reaped, rerr := reapStaleLockTTL(lockPath, now, shareImportTTL)
			if rerr != nil && !atomicio.SharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				continue
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(sourcesAcquireBackoff(attempt))
	}
	return nil, fmt.Errorf("share storage is locked by another mora process (%s); retry in a moment", lockPath)
}

// verifyImportLeaseOwner is the ownership re-verify the commit fence runs: the
// on-disk import.lock must exist, still carry run_id==runID, and be within TTL.
// A reaped holder (lease is a successor's or absent) fails here and ABORTS before
// linking a commit.
func verifyImportLeaseOwner(cfg Config, name, runID string, now time.Time) error {
	return verifyShareLeaseOwner(shareImportLockPath(cfg, name), fmt.Sprintf("share %q: import", name), runID, now)
}

// verifyStorageLeaseOwner is the aggregate-admission fence. A build may run
// longer than shareImportTTL; if its storage lease was reaped, it must not
// publish after another subscription has admitted against the same headroom.
func verifyStorageLeaseOwner(cfg Config, runID string, now time.Time) error {
	return verifyShareLeaseOwner(shareStorageLockPath(cfg), "share storage", runID, now)
}

func verifyShareLeaseOwner(lockPath, label, runID string, now time.Time) error {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("%s lease is gone — this run was reaped; aborting commit", label)
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil {
		return fmt.Errorf("%s lease unreadable; aborting commit", label)
	}
	if body.RunID != runID {
		return fmt.Errorf("%s lease now belongs to %q, not this run %q — reaped; aborting commit", label, body.RunID, runID)
	}
	if t, perr := time.Parse(time.RFC3339, body.AcquiredAt); perr != nil || now.UTC().Sub(t.UTC()) >= shareImportTTL {
		return fmt.Errorf("%s lease is past its TTL; aborting commit", label)
	}
	return nil
}

// shareHeartbeatDivisor sets the heartbeat cadence (shareImportTTL/3) so a
// live-but-slow import is never spuriously reaped.
const shareHeartbeatDivisor = 3

// startImportHeartbeat runs a background CAS re-stamp every shareImportTTL/3 for
// the whole hold. If the re-stamp finds the lease is no longer ours (reaped), it
// stops; the commit fence's own re-verify then aborts the run. The returned stop
// func halts the ticker and waits for an in-flight heartbeat to leave the lease
// guard before the caller performs its final release.
func startImportHeartbeat(cfg Config, name, runID string) (stop func()) {
	return startShareLeaseHeartbeat(shareImportLockPath(cfg, name), runID)
}

func startStorageHeartbeat(cfg Config, runID string) (stop func()) {
	return startShareLeaseHeartbeat(shareStorageLockPath(cfg), runID)
}

func startShareLeaseHeartbeat(lockPath, runID string) (stop func()) {
	interval := shareImportTTL / shareHeartbeatDivisor
	if interval <= 0 {
		interval = time.Millisecond
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if !heartbeatLockFileFor(lockPath, runID, time.Now()) {
					return // reaped: stop; the fence re-verify will abort the commit
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
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
	return shareBuildAndPublishPrepared(ctx, cfg, name, mode, nil, fn, nil)
}

// shareBuildAndPublishPrepared is the subscribe form of the chokepoint. prepare
// runs after both leases and recovery but before an attempt is started; a
// waiting duplicate therefore cannot overwrite the winner's succeeded attempt
// with a synthetic failed one. finalize runs only after the attempt reaches its
// successful terminal state, while both leases are still held.
func shareBuildAndPublishPrepared(ctx context.Context, cfg Config, name string, mode shareBuildMode, prepare func() error, fn func(runID string) (int, error), finalize func() error) error {
	runID := newRunID(time.Now())
	storeRel, err := acquireStorageLease(cfg, runID, time.Now())
	if err != nil {
		return err
	}
	defer storeRel()
	stopStorage := startStorageHeartbeat(cfg, runID)
	defer stopStorage()
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
	if rerr := recoverShareAttemptClaims(cfg, name); rerr != nil {
		return rerr
	}
	if prepare != nil {
		if perr := prepare(); perr != nil {
			return perr
		}
	}

	if mode == buildModeImport {
		if serr := startShareAttempt(cfg, name, runID, time.Now()); serr != nil {
			return serr
		}
	}
	seq, ferr := fn(runID)
	if mode == buildModeImport {
		var terminalErr error
		if ferr != nil {
			terminalErr = finishShareAttempt(cfg, name, runID, shareAttempt{
				RunID: runID, State: "failed", LastError: sanitizeHealthError(ferr.Error()),
			})
		} else {
			terminalErr = finishShareAttempt(cfg, name, runID, shareAttempt{
				RunID: runID, State: "succeeded", Seq: seq,
			})
		}
		if terminalErr != nil {
			return errors.Join(ferr, fmt.Errorf("share %q: durable attempt transition failed: %w", name, terminalErr))
		}
	}
	if ferr == nil && finalize != nil {
		return finalize()
	}
	return ferr
}
