package mora

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
