package mora

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// indexUpsert incrementally reflects a single authored memory into the search
// index — reprocessing ONLY that memory, instead of the DELETE-then-reinsert-
// everything (re-parse every vault file, rebuild the whole entity graph, re-embed
// every vector) that rebuildIndex performs. It is the hot-path replacement for
// rebuildIndex on `mora write` and MCP write_memory: N concurrent agent writers
// otherwise each serialize a full-vault rebuild (O(N × vault) work + writer-lock
// thrash that overruns busy_timeout and surfaces as degraded index_stale warnings).
//
// This is a large CONSTANT-FACTOR win, not an asymptotic one: ~59× faster than a
// full rebuild at ~1k memories (BenchmarkIndexUpsert1k vs BenchmarkRebuildIndex1k),
// dominated by the fsync/commit. It is NOT O(1) per write — `DELETE FROM
// memories_fts WHERE id=?` is a full FTS vtable SCAN and `SELECT COUNT(*) FROM
// memories` walks the PK index (both EXPLAIN QUERY PLAN-verified), so the per-write
// cost still grows linearly with vault size, just with a tiny constant. The win is
// that it skips the whole vault's re-parse / graph-rebuild / re-embed work.
//
// It touches ONLY the memories table and its FTS row — the single chokepoint every
// search arm JOINs through, so a written memory is immediately findable via FTS. It
// does NOT update the entity graph (entities/edges) or per-memory vectors
// (mem_vectors):
//
//   - The entity graph is a whole-corpus product: buildGraph derives structural
//     entities (scope / tag / [[wikilink]] / category, plus a per-memory hub) from
//     EVERY memory Meta-INDEPENDENTLY, and canonicalizePersons merges person
//     identities ACROSS memories while writeGraph's INSERT OR REPLACE recomputes an
//     entity's mention_count from the rows it sees. So an authored write genuinely
//     adds graph nodes (at minimum its scope: entity and hub) — but a CORRECT
//     single-memory graph delta is not a local operation, and rebuilding the graph
//     per write is the O(vault) cost this change removes.
//   - Vectors feed only the HYBRID retrieval path, which defaultSearch enables ONLY
//     when a semantic embedder (Ollama) is configured; under the default static-hash
//     embedder search is FTS-only, so a missing vector has no effect there (invariant
//     I9). Under a semantic embedder the new memory is a real but BOUNDED, self-
//     healing recall gap on the hybrid arm: fully searchable via FTS immediately, and
//     it gains its vector at the next full rebuild.
//
// Both reconcile on the next FULL rebuild, which still runs on the untouched paths:
// the scheduled index-hourly job, `mora index rebuild`, connector sync, and delete —
// so the graph/vector lag is bounded by the rebuild cadence, not indefinite. The
// vault (Markdown files) remains the source of truth; the index is a derived,
// eventually-consistent cache (invariant I1).
//
// Identity safety: indexUpsert honors the SAME vault-identity guard as rebuildIndex
// (vaultid.go). A write to a vault whose marker does not match the index the write
// would touch returns errRebuildBlocked WITHOUT modifying the index, so callers keep
// today's degraded-success handling (CLI: warn + exit 0; MCP: index_stale + warning,
// never isError). Cold-start and legacy/unbound states delegate to the full
// rebuildIndex, which creates the complete schema and binds identity.
func indexUpsert(ctx context.Context, cfg Config, m Memory) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}

	// Cold-start / legacy gate — decided on a READ connection, holding NO write
	// transaction, so the delegated rebuild's own immediate tx is never blocked by
	// ours. A missing index, a stale schema version, missing tables, or an index
	// that never recorded a vault_id (pre-identity / never-bound) all delegate to
	// the full rebuild: it builds the complete schema, binds identity, and is cheap
	// precisely in these one-time cold-start cases. Once bound, vault_id is only ever
	// rewritten to another non-empty id, so a probe that sees it non-empty guarantees
	// the incremental path below always operates on an identity-bound index.
	ready, indexID, err := indexReadyForUpsert(ctx, cfg)
	if err != nil {
		return err
	}
	if !ready || indexID == "" {
		_, rerr := rebuildIndex(ctx, cfg)
		return rerr
	}

	// Incremental fast path. Mirror rebuildIndex's DSN and one-tiny-immediate-tx
	// discipline: _txlock=immediate grabs the writer lock up front (no mid-tx
	// deferred-to-immediate upgrade that cannot retry), and the 15s busy_timeout
	// lets concurrent writers wait out each other's sub-millisecond commit window
	// instead of surfacing a raw "database is locked".
	db, err := sql.Open("sqlite", dbPath(cfg)+"?_txlock=immediate&_pragma=busy_timeout(15000)")
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds; the safety net on any early return

	// Capture prior state before staging the change, for the identity guard.
	oldCount := 0
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&oldCount); err != nil {
		return err
	}
	curIndexID := ""
	_ = tx.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='vault_id'`).Scan(&curIndexID)

	// Delete-then-insert this one id in memories AND its FTS row (the FTS5 table is
	// a plain, non-external-content table, so an ordinary DELETE ... WHERE id=? is
	// valid). Insert shapes mirror rebuildIndex exactly so an incrementally-added
	// row is byte-identical to a fully-rebuilt one. A tombstone (deleted_at set)
	// stays deleted from the searchable tables, mirroring rebuild's DeletedAt skip —
	// authored writes are never tombstones, so this is defensive.
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id=?`, m.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories_fts WHERE id=?`, m.ID); err != nil {
		return err
	}
	if m.DeletedAt == "" {
		path := memoryPath(cfg, m)
		tags := strings.Join(m.Tags, ",")
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memories VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.Scope, m.Type, m.Title, tags, m.Source, m.CreatedAt, path, m.Text); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memories_fts VALUES (?, ?, ?, ?, ?, ?)`,
			m.ID, m.Scope, m.Title, tags, m.Source, m.Text); err != nil {
			return err
		}
	}

	// Real post-count (reflects this tx's staged delete+insert). Feeding the TRUE
	// count into assessRebuild keeps decBlockEmpty reachable — a single-row upsert
	// can't empty a populated index, but computing it honestly means the guard can
	// never be fooled by a fabricated count.
	newCount := 0
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&newCount); err != nil {
		return err
	}

	// Validate-before-commit vault-identity guard (mirrors rebuildIndex): a write
	// against a vault whose marker does not match the index rolls back (via the
	// deferred Rollback) and returns errRebuildBlocked — the index is left untouched
	// and the caller degrades. On the incremental path curIndexID is non-empty
	// (guaranteed by the cold-start gate above), so the live decisions are
	// decProceed vs decBlockIdentity; decBlockEmpty is handled defensively.
	marker, markerPresent, err := readVaultMarker(cfg)
	if err != nil {
		return err
	}
	decision := assessRebuild(oldCount, newCount, marker.VaultID, markerPresent, curIndexID)
	if decision == decBlockEmpty || decision == decBlockIdentity {
		if werr := writeBlockRecord(cfg, decision, cfg.VaultDir, oldCount, newCount); werr != nil {
			_ = werr // best-effort; do not mask the block error
		}
		return fmt.Errorf("%w: %s", errRebuildBlocked, rebuildBlockMessage(decision, cfg.VaultDir, oldCount))
	}

	// Bind identity exactly as rebuildIndex does (adopt a fresh id if unbound,
	// self-heal a lost marker), then keep index_meta.memory_count consistent with
	// the row count. effID equals curIndexID on the ordinary same-vault write, so
	// this is a single tiny upsert in the common case.
	effID := curIndexID
	if effID == "" {
		seed := marker.VaultID
		if seed == "" {
			seed = "v_" + newID()
		}
		effID, err = createVaultMarkerIfAbsent(cfg, seed)
		if err != nil {
			return err
		}
	} else if !markerPresent {
		if _, err = createVaultMarkerIfAbsent(cfg, effID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO index_meta(key,value) VALUES('memory_count',?),('vault_id',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("%d", newCount), effID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	_ = clearBlockRecord(cfg) // best-effort: a stale block record must not fail a good write
	return nil
}

// indexReadyForUpsert reports whether the on-disk index is usable for an
// incremental single-row upsert — it exists, carries the schema version this binary
// writes, has the three tables the upsert touches, and returns the bound vault_id
// (empty if the index never recorded one). It opens a READ connection only, so it
// holds no write lock while the caller decides whether to delegate to a full
// rebuild. Any "not ready" condition (including a missing db file) returns
// ready=false with a nil error so the caller cleanly falls back to rebuildIndex.
func indexReadyForUpsert(ctx context.Context, cfg Config) (ready bool, indexID string, err error) {
	if _, statErr := os.Stat(dbPath(cfg)); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", statErr
	}
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return false, "", err
	}
	defer db.Close()

	var uv int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&uv); err != nil {
		return false, "", err
	}
	if uv != indexSchemaVersion {
		return false, "", nil // stale/unstamped schema -> full rebuild re-stamps it
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('memories','memories_fts','index_meta')`).Scan(&n); err != nil {
		return false, "", err
	}
	if n != 3 {
		return false, "", nil // partial schema -> full rebuild creates the rest
	}
	var vid string
	switch err := db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='vault_id'`).Scan(&vid); {
	case errors.Is(err, sql.ErrNoRows):
		return true, "", nil // ready but unbound -> caller delegates so identity is bound
	case err != nil:
		return false, "", err
	}
	return true, vid, nil
}
