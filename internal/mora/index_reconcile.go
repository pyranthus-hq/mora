package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// authoredReconcileLockPath is a short-lived, cross-process election for the
// expensive whole-projection catch-up following an authored write. The vault and
// pending-op ledger remain authoritative: losing the election is deliberately a
// success-with-pending, never a claim that the index is fresh.
func authoredReconcileLockPath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "index", "authored-reconcile.lock")
}

const authoredReconcileTTL = 2 * time.Minute

// defaultAuthoredReconcileScheduler resolves the reconciler from the config:
// configs resolved under a context that pinned an override (tests keeping the
// async worker inert) use it; everything else uses the production launcher.
// The runner rides the Config instead of a package global so concurrent tests
// never share — or race each other's cleanup of — this seam.
func defaultAuthoredReconcileScheduler(cfg Config) {
	// Resolve before the goroutine boundary: a short-lived caller or a hermetic
	// test may release its seam immediately after scheduling, but that must
	// never change the work this invocation already elected to perform.
	runner := cfg.AuthoredReconciler()
	if runner == nil {
		runner = reconcileAuthoredWrites
	}
	go func(run func(context.Context, Config) error) {
		if err := run(context.Background(), cfg); err != nil {
			// stderr is outside the MCP stdio transport. Do not clear or alter a
			// marker here: a logged failure must remain visible to health and the
			// next explicit/scheduled recovery path.
			fmt.Fprintf(os.Stderr, "warn: authored projection reconciliation pending: %v\n", err)
		}
	}(runner)
}

// scheduleAuthoredReconciliation schedules (but does not await) the coalescing
// worker before mcpWriteMemory returns. The worker itself waits through its 75 ms
// burst window before any rebuild work, keeping the whole-vault rebuild off the
// request path. Failures leave the pending ledger and health dirty.
func scheduleAuthoredReconciliation(cfg Config) {
	defaultAuthoredReconcileScheduler(cfg)
}

// reconcileAuthoredWrites elects at most one concurrent background worker to
// rebuild every derived projection after a small coalescing window. Other writers
// keep their durable pending markers; the elected rebuild covers and retires them.
// This avoids N full rebuilds during a multi-agent write storm while retaining the
// ordinary rebuild's WAL transaction, snapshot, activity, and crash recovery
// semantics.
//
// A held live lease is not an error. Its owner is already responsible for the
// catch-up, so callers must leave their pending op in place and health remains
// honestly dirty until that committed rebuild covers it.
func reconcileAuthoredWrites(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(authoredReconcileLockPath(cfg)), 0o700); err != nil {
		return err
	}
	path := authoredReconcileLockPath(cfg)
	now := time.Now()
	body, err := json.Marshal(loopLockBody{PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	published, err := publishLockFile(path, body)
	if err != nil {
		return err
	}
	if !published {
		reaped, rerr := reapStaleLockTTL(path, now, authoredReconcileTTL)
		if rerr != nil {
			return rerr
		}
		if reaped {
			published, err = publishLockFile(path, body)
			if err != nil {
				return err
			}
		}
	}
	if !published {
		return nil
	}
	defer loopLockReleaser(path, body)()

	// Let simultaneously-started writers finish their tiny FTS transactions so a
	// single rebuild usually covers the whole burst. This is intentionally far
	// shorter than the writer busy timeout and never runs while holding index.db.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(75 * time.Millisecond):
	}

	// A write can land while a rebuild is listing. Repeat a bounded number of
	// times while there are still authored-write markers. Each pass is a normal
	// atomic rebuild; on any failure markers remain and health stays dirty.
	for pass := 0; pass < 3; pass++ {
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			return fmt.Errorf("reconciling authored write projections: %w", err)
		}
		ops, err := listPendingOps(cfg)
		if err != nil {
			return err
		}
		hasWrite := false
		for _, op := range ops {
			if op.Kind == opKindWrite {
				hasWrite = true
				break
			}
		}
		if !hasWrite {
			return nil
		}
	}
	// A writer raced the final snapshot. Its durable marker remains and a later
	// invocation will elect another pass; this is an honest self-clearing pending
	// state, not a failed save and not an error a caller should retry.
	return nil
}
