package mora

// authoredWriteReconcileHint is the explicit recovery/catch-up surface for an
// authored write whose immediate FTS upsert has committed but whose whole-vault
// projections have not yet been rebuilt. The installed index-hourly job provides
// the ordinary eventual path; this command is the deliberate immediate one.
const authoredWriteReconcileHint = "mora index rebuild"

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
