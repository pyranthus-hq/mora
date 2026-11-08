package mora

import (
	"context"
	"errors"
	"fmt"
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

// testHookRebuildCommitted runs between a rebuild's commit and its marker cleanup.
var testHookRebuildCommitted func(Config)

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

// markDeleteUnlinked records that a delete removed its file, which lets a rebuild
// retire the marker. On failure the marker waits for its owner to die: that is
// the safe direction, and a delete that stuck must not report an error for it.
func markDeleteUnlinked(cfg Config, op pendingOp) {
	if err := markFTSCommitted(cfg, op); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not record the removal of %s: %v\n", op.MemoryID, err)
	}
}

// warnAbandonedDelete says out loud that a memory came back: its deleter died
// before removing the file, so the delete never happened.
func warnAbandonedDelete(op pendingOp) {
	fmt.Fprintf(os.Stderr, "warn: delete of memory %s was abandoned by dead process %d and its file is unchanged; the memory is back in search. Delete it again if you still want it gone (%s)\n", op.MemoryID, op.OwnerPID, op.Path)
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
func clearCoveredPendingOps(cfg Config, ops []pendingOp, c indexstore.Coverage) error {
	return indexstore.ClearCovered(cfg, ops, c, unmarkIndexDirty)
}

// deleteOp builds the marker for removing m. An unreadable file gets no hash, so
// the marker falls back to the hashless rule.
func deleteOp(m Memory) pendingOp {
	op := pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID}
	if b, err := os.ReadFile(m.Path); err == nil {
		op.ContentSHA256 = indexstore.ContentSHA256(b)
	}
	return op
}
func suppressPendingDeletes(cfg Config, mems []Memory) []Memory {
	return indexstore.SuppressPendingDeletes(cfg, mems)
}
func cleanPathSet(paths []string) map[string]bool { return indexstore.CleanPathSet(paths) }
