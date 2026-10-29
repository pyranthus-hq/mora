package mora

import (
	"context"
	"time"
)

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

// reflectDeleteInIndex removes one just-deleted memory from the search index and
// its derived projections with a single targeted transaction (no whole-vault
// rebuild). It reports whether the caller must run the deferred reconcile. The
// caller's pending-delete marker is left in place: it keeps suppressing the id
// on every read path until a covering rebuild retires it.
//
// A rebuild that listed the file before it was removed can commit the row back
// and retire the delete marker (it parsed the path). So a rebuild-kind marker is
// written AFTER the file is gone; ShouldClear retires it only for a rebuild that
// listed after it, which therefore cannot have seen the file. It is stamped one
// second ahead because markers carry second resolution. The reconcile keeps
// passing while such a marker is pending.
//
// Emptying the vault rebuilds inline (instant, Allow mode) and needs no
// reconcile: an Enforce rebuild would refuse an empty vault and strand markers.
func reflectDeleteInIndex(ctx context.Context, cfg Config, id string) (reconcile bool, err error) {
	remaining, err := indexDelete(ctx, cfg, id)
	if err != nil {
		return false, err
	}
	if remaining == 0 {
		_, err = rebuildIndexWithPolicy(ctx, cfg, policyAllow)
		return false, err
	}
	_, err = markIndexDirty(ctx, cfg, pendingOp{Kind: opKindRebuild, MemoryID: id, MarkedAt: indexClock().Add(time.Second).UTC().Format(time.RFC3339)})
	return true, err
}
