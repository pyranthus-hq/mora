package mora

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	indexstore "github.com/pyranthus-hq/mora/internal/index"
)

var indexClock = time.Now

const (
	opKindWrite   = indexstore.KindWrite
	opKindDelete  = indexstore.KindDelete
	opKindRebuild = indexstore.KindRebuild
)

type pendingOp = indexstore.PendingOp

var removePendingOpFile = os.Remove
var testHookPostMarkerWrite func()

func pendingOpPath(cfg Config, opID string) string { return indexstore.PendingPath(cfg, opID) }
func cleanVaultPath(path string) string            { return indexstore.CleanVaultPath(path) }
func markIndexDirty(ctx context.Context, cfg Config, op pendingOp) (pendingOp, error) {
	return indexstore.MarkDirty(ctx, cfg, op, indexstore.MarkSeams{Ready: indexReadyForUpsert, NewID: newID, Clock: indexClock, PostWrite: testHookPostMarkerWrite})
}

// unmarkIndexDirty retires a marker in either form. It removes the dirty form
// first: a concurrent markFTSCommitted rename then either already happened (the
// second removal finds the FTS-committed form) or fails on the missing source,
// so no interleaving leaves a covered marker behind.
func unmarkIndexDirty(cfg Config, opID string) error {
	if opID == "" {
		return nil
	}
	if err := removePendingMarker(pendingOpPath(cfg, opID)); err != nil {
		return err
	}
	return removePendingMarker(indexstore.FTSCommittedPath(cfg, opID))
}

// markFTSCommitted records that an authored write's FTS upsert committed. The
// marker stays until a covering rebuild retires it (graph, vectors and
// commitments still lag), but health reads it as refreshing, not dirty.
func markFTSCommitted(cfg Config, op pendingOp) error {
	return indexstore.MarkFTSCommitted(cfg, op.OpID, renamePendingOpFile)
}

var renamePendingOpFile = atomicio.RenameReplaceWithRetry

func removePendingMarker(path string) error {
	var lastErr error
	deadline := time.Now().Add(leaseRemovalTimeout)
	for attempt := 0; ; attempt++ {
		err := removePendingOpFile(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !leaseRemovalRetryableFn(err) {
			return err
		}
		lastErr = err
		if !sleepWithinDeadline(sourcesAcquireBackoff(attempt), deadline) {
			return lastErr
		}
	}
}

func listPendingOps(cfg Config) ([]pendingOp, error) { return indexstore.ListPending(cfg) }
func clearCoveredPendingOps(cfg Config, started time.Time, files, parsed []string) error {
	return indexstore.ClearCovered(cfg, started, files, parsed, unmarkIndexDirty)
}
func suppressPendingDeletes(cfg Config, mems []Memory) []Memory {
	return indexstore.SuppressPendingDeletes(cfg, mems)
}
func cleanPathSet(paths []string) map[string]bool { return indexstore.CleanPathSet(paths) }
