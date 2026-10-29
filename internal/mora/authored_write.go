package mora

import "context"

// An authored write whose FTS upsert committed answers with searchable=true,
// and its health reads index "refreshing": it is searchable now, and the
// coalesced background reconcile is rebuilding its graph, vector and commitment
// projections. The response names no command on
// purpose. Agents obeyed the old "mora index rebuild" hint and collided with
// that reconcile on the writer lock (SQLITE_BUSY).

// authoredWriteProjectionPending reports whether this write's durable marker
// survived its immediate FTS transaction. A full rebuild (including the
// cold-start delegate) clears a covered marker, so callers can distinguish a
// complete index from the ordinary FTS-only fast path without guessing from
// elapsed time or projection stamps.
//
// A ledger read failure is fail-closed: the caller must present the write as
// stale rather than claim that graph/vector/commitment projections are current.
func authoredWriteProjectionPending(cfg Config, op pendingOp) bool {
	if op.OpID == "" {
		return false
	}
	ops, err := listPendingOps(cfg)
	if err != nil {
		return true
	}
	for _, candidate := range ops {
		if candidate.OpID == op.OpID {
			return true
		}
	}
	return false
}

// reflectDeleteInIndex removes one just-deleted memory from the search index
// with a single targeted transaction (no whole-vault rebuild). The caller's
// pending-delete marker is left in place: it keeps suppressing the id on every
// read path and pins graph/vector/commitment lag until a covering rebuild
// retires it. Emptying the vault is the one case that rebuilds inline (instant,
// Allow mode): the deferred Enforce rebuild would otherwise refuse an empty
// vault and strand the marker.
func reflectDeleteInIndex(ctx context.Context, cfg Config, id string) error {
	remaining, err := indexDelete(ctx, cfg, id)
	if err != nil {
		return err
	}
	if remaining == 0 {
		_, err = rebuildIndexWithPolicy(ctx, cfg, policyAllow)
	}
	return err
}

// scheduleDeleteReconciliation schedules the deferred projection catch-up for a
// delete whose marker is still pending (an emptying delete's inline rebuild
// already retired it).
func scheduleDeleteReconciliation(cfg Config, op pendingOp) {
	if authoredWriteProjectionPending(cfg, op) {
		scheduleAuthoredReconciliation(cfg)
	}
}
