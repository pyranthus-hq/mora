# Mora — Concurrency contract

Mora runs short processes on one host. Its processes and goroutines can run
**together**. An interactive `mora write` can overlap an MCP
`write_memory`, an `index-hourly` rebuild, and an `ingest-hourly` sync. Many
`search`/`read` calls can also run at once. All can touch one vault and one
`index.db`. This AS-BUILT contract states what stays correct when they overlap.
It also states which controls keep it correct.

The rules below apply to the full system. Focused unit tests pin each rule.
`internal/mora/concurrency_contract_test.go` tests them together. It runs an
N-writer, concurrent-reader, and full-rebuild storm under `-race`.

Concurrency follows one principle: **one host, one user, many short processes
and in-process goroutines.** Cross-host and multi-user work belongs to a durable
execution runtime, a formally rejected direction. See the
[overview](./00-overview.md) and do-not-build ledger. Mora uses atomic files and
a one-host file lease. These controls fit one machine's Mora processes.

## The four guarantees

| # | Guarantee | Mechanism | Anchor |
|---|---|---|---|
| G1 | **No lost writes** — a write reported as saved is on disk exactly once. A same-instant id collision never silently overwrites a rival's memory | Create-exclusive publish (`os.Link`, fails `EEXIST`) + bounded id re-mint | `createMemory`, `atomicCreate` |
| G2 | **No torn reads** — no reader ever parses half-written frontmatter | Every memory file is published fully-formed via an atomic link/rename of a staged temp | `atomicWrite`, `atomicCreate` |
| G3 | **No surfaced `database is locked`** — concurrent reader *processes* never block a writer, and a contended writer waits out its rival's commit instead of erroring | `journal_mode(WAL)` on the index (readers read a snapshot, never hold a lock a writer must wait for) + `busy_timeout(15000)` on every writer and read-only DSN + `_txlock=immediate` on writers | `rwIndexDSN`, `roIndexDSN`, `rebuildIndexWithPolicy`, `indexUpsert` |
| G4 | **Bounded eventual consistency** — the vault is the source of truth. The index converges | Tiny synchronous upsert on the write path. Serialized full rebuilds reconcile the rest | `indexUpsert`, `rebuildIndexWithPolicy` |

## 1. Per-memory atomic files

The vault is the source of truth (invariant I1). Every durable write goes
through one of two publish primitives in `internal/mora/mora.go`, and both are
**content-atomic** — the target name only ever appears with the full body behind
it, so a concurrent reader (`rebuildIndex`, `findMemory`, `listMemories`,
`digest`, `meetingprep`, share) never observes a partial file.

- **`atomicWrite(path, body, mode)`** — the general persistence primitive
  (updates, connector re-writes, status files, watermarks, `sources.json`,
  control files). It stages through a **unique** temp (`os.CreateTemp(dir,
  "."+base+"-*.tmp")`, never a fixed `<path>.tmp` — a fixed name once let two
  writers truncate each other's in-flight temp, incident #16), `chmod`s to the
  caller's mode, then `os.Rename`s into place. `os.Rename` **replaces** an
  existing target (last-writer-wins), which is the correct idempotent behavior
  when re-rendering an existing memory onto its own stable path.
  - Windows wrinkle: `os.Rename` → `MoveFileEx(MOVEFILE_REPLACE_EXISTING)`;
    concurrent writers racing onto the SAME target transiently fail with sharing
    violations, so the rename retries with **jittered**, capped backoff up to a
    5 s deadline (`renameReplaceWithRetry`/`renameReplaceRetryable`. Always a
    single attempt off Windows). Deterministic backoff made 16 goroutines retry
    in lockstep and keep colliding — the jitter is load-bearing (#73/#74).

- **`atomicCreate(path, body, mode)`** — the **create-exclusive** primitive for
  brand-new authored memories. Unlike `atomicWrite`, it MUST NOT clobber: it
  stages a unique temp and `os.Link`s it onto the target. `os.Link` is both
  create-exclusive (fails `os.ErrExist` on a present target, never replaces) and
  content-atomic, so a racing second writer gets `EEXIST` — exactly one wins.
  This mirrors `loop.go`'s proven `publishLockFile`. On Windows `os.Link` is
  `CreateHardLinkW`, which likewise fails on a present target (and needs no
  `MoveFileEx` retry because it never replaces).
  - Fallback: some filesystems (exFAT/FAT32, some SMB/NFS) refuse hard links
    (`EPERM`/`ENOTSUP`, never `EEXIST`). Since `vault_dir` is user-configurable,
    `atomicCreate` preserves the no-clobber guarantee there via
    `os.OpenFile(O_CREATE|O_EXCL)` to claim the path, then renames its temp onto
    its **own** claimed placeholder. Documented tradeoff: only on this branch can
    a concurrent reader briefly observe an EMPTY placeholder — which every
    parse-error caller already skips with `continue`, so it degrades to
    "ignored until the rename lands", never a crash. The pure-link path
    (POSIX/NTFS) has no such window.

## 2. Create-exclusive IDs (no lost writes)

Authored ids are minted by `newID`: `mem_<yyyymmdd_hhmmss>_<8 hex>` — a
**second-granularity** timestamp plus 4 `crypto/rand` bytes. Two authored writes
in the same second can therefore mint the same id and thus the same
`memoryPath`. Under the old `atomicWrite` publish, the second writer's
`os.Rename` would replace the first's file: both callers report success, but one
memory is silently lost.

`createMemory` closes this hole (the single path for `cmdWrite` and MCP
`write_memory`):

1. mint an id (`newIDFn`, a test seam over `newID`),
2. render, `atomicCreate` it,
3. on `os.ErrExist` (a real id collision), **re-mint and retry**, bounded by
   `maxCreateAttempts` (8) — a liveness backstop, since each retry draws fresh
   entropy so exhausting it is astronomically improbable.

Two correctness details reinforce it:

- `newID` handles a `crypto/rand` failure explicitly: it falls back to
  `math/rand/v2` entropy (ids are uniqueness tokens, not secrets) and warns —
  never an all-zero suffix, which would collide every time within a second and
  stall the re-mint loop.
- Only **new** authored memories use `createMemory`. Updates and connector
  re-writes deliberately keep `writeMemory`/`writeMappedMemory` →
  `atomicWrite`, because re-rendering an existing memory onto its own **stable**
  path is a correct idempotent overwrite, not a collision.

The re-mint-on-collision mechanism itself is pinned by `createexclusive_test.go`,
which forces a `newIDFn` collision deterministically (single-path single-winner,
bounded retry, CSPRNG fallback). The contract stress test's G1 assertion covers the
integration-level property — every reported-saved memory on disk exactly once under
real contention — but its writers mint natural 4-byte-random ids that effectively
never collide, so it does not itself provoke the create-exclusive re-mint path;
`createexclusive_test.go` is the authority for that mechanism.

## 3. Tiny upsert transactions (the write hot path)

Every authored write reflects itself into the index synchronously, but does NOT
rebuild the whole index. `indexUpsert` (`internal/mora/index_upsert.go`) writes
just three things inside one small `_txlock=immediate` transaction: the memory's
`memories` row, its `memories_fts` row — the single chokepoint every search arm
JOINs through — and the `index_meta` bookkeeping (`memory_count` + `vault_id`,
kept consistent for the identity guard). It uses insert shapes byte-identical to
`rebuildIndex` and deliberately does NOT touch the entity graph or vectors, so the
new memory is immediately findable via FTS while the rest reconciles later (§6).

Why this matters for concurrency: the write path used to call the full
`rebuildIndex` (DELETE-all + reinsert every vault file). N concurrent agent
writers therefore each serialized an O(N × vault) rebuild, thrashed the writer
lock, and overran `busy_timeout` — surfacing as degraded `index_stale` warnings.
The tiny upsert removes that whole-vault work from the write path (a large
constant-factor win — ~59× at ~1k memories — not asymptotic. The per-write
`DELETE FROM memories_fts WHERE id=?` is still a full FTS vtable scan).

- **Cold-start / legacy gate.** `indexReadyForUpsert` probes on a **read**
  connection (holding no write lock) whether the index exists, carries this
  binary's schema version, has the three tables, and is identity-bound. Any "not
  ready" state (missing db, stale schema, partial schema, never-bound
  `vault_id`) delegates to the full `rebuildIndex`, which builds the complete
  schema and binds identity — cheap precisely in these one-time cases. Deciding
  on a read connection means the delegated rebuild's own immediate tx is never
  blocked by ours.
- **Identity guard.** `indexUpsert` runs the SAME validate-before-commit
  vault-identity guard as `rebuildIndex` (`assessRebuild`, `vaultid.go`): a write
  against a vault whose `.mora-vault.json` marker does not match the index rolls
  back and returns `errRebuildBlocked` **without touching the index**, so callers
  keep degraded-success semantics (CLI: warn + exit 0; MCP: `index_stale`
  warning, never `isError`) — failing a write that already landed on disk would
  invite a retry that mints a duplicate.

Pinned by `index_upsert_test.go`.

## 4. Serialized full rebuilds

`rebuildIndexWithPolicy` (the scheduled `index-hourly` job, `mora index
rebuild`, connector sync, delete, and the cold-start delegate) does the whole
destructive DELETE-all + reinsert **inside one `_txlock=immediate`
transaction**:

- **`_txlock=immediate`** grabs the writer lock at `BeginTx` rather than lazily
  upgrading a deferred read lock mid-transaction — two concurrent rebuilds would
  otherwise both start, then one hits `SQLITE_BUSY` on its first write with no
  way to retry inside an open tx. Immediate + `busy_timeout(15000)` means the
  second rebuild simply **waits** for the first to commit.
- **List the vault INSIDE the tx** (`listRebuildFiles`, after the lock is held).
  Listing before the lock let two rebuilds interleave: rebuild A lists, rebuild B
  (fired by a newer write) lists + commits, then A commits LAST carrying its
  OLDER snapshot — silently dropping the just-written memory. Because the lock
  serializes rebuilds, whichever commits later necessarily listed later, so a
  committed index can no longer be clobbered by an older rebuild's stale
  snapshot. (A memory written AFTER the surviving rebuild's listing is ordinary
  until-next-reconcile staleness — see §6 — not this race.) This is the P1 fix.
- **One transaction for the whole rebuild** (schema, DELETEs, `memories`+FTS,
  `writeGraph`, `writeVectors`) so a mid-rebuild failure rolls back to the prior
  committed index rather than leaving a half-empty one.

Interaction with the write path: a full rebuild can never drop a committed
on-disk memory (it re-lists the vault from disk under the lock), and a write's
`indexUpsert` either lands before the rebuild's listing (included) or is blocked
on the writer lock and commits after the rebuild (re-added) — so every written
memory survives every interleaving. Pinned by `mora_rebuild_atomic_test.go` (the
lock is held at listing time) and the G4 assertion in the contract stress test.

## 5. The `sources.json` lease

`sources.json` (the consent/source registry in `ConfigDir`) is the one piece of
state mutated by a **read-modify-write**, not a single write. `atomicWrite`
makes each `saveSources` durable and temp-collision-free, but two callers each
doing `loadSources → mutate → saveSources` still race: the last rename wins and
silently drops the other's mutation (a lost enable bit, deny-list, or persisted
window). A manual `mora sources ...` racing the scheduled `ingest-hourly` sync is
exactly this shape.

`mutateSources`/`acquireSourcesLock` (`internal/mora/sources_lock.go`, the P3
fix) close it with a short-lived, crash-safe **cross-process file lease** held
around the WHOLE read-modify-write — and, crucially, it **reloads inside the
lease**, so a concurrent writer's committed change is always observed, never
clobbered. Every `load → mutate → save` on `sources.json` MUST go through it;
`saveSources` is called directly only while already holding the lease.

The lease reuses `loop.go`'s proven primitives — `publishLockFile`'s
`os.Link`-atomic publish, TTL/corrupt reap (`sourcesLockTTL` = 30 s, far longer
than any legitimate microsecond hold), guarded compare/remove `breakLock`, and a
jittered, capped acquire backoff (same #74 rationale as §1: fixed backoff makes
rivals retry in lockstep on Windows). It is a single-host, single-user lease —
which is exactly the concurrency model. Pinned by `sources_lock_test.go`.

Every lease transition is additionally serialized by a persistent OS-locked
guard keyed by the lease's physical filesystem identity. Mora resolves the
deepest existing ancestor, normalizes the missing tail, and folds identity case
on Darwin/Windows, so symlink and Unicode aliases converge without rewriting the
physical path spelling. Guards stay within the writable Mora root. For explicitly
removable import and loop roots, the guard anchors in their stable parent so
deletion cannot split one logical guard into two live inodes. Other guards stay
under the lease's containing root. Guard filenames end in `.lock`, so vault Git
ignores them. Failure to create the one deterministic guard fails closed.

> Scope note: `shares.json` (the share grant registry) has the same RMW shape
> and is not yet routed through a lease. It is out of scope here and gated by the
> share subsystem's separate security review.

## 6. The eventual-consistency window of the index

`index.db` is a **derived, eventually-consistent cache**. The vault Markdown is
the source of truth (invariant I1). The write path keeps this window as tight as
correctness allows and bounds the rest:

- **Immediately consistent on the write path:** the `memories` table and its FTS
  row. A just-written authored memory is findable via `search_memory`/`search`
  the instant `indexUpsert` commits.
- **Eventually consistent (the window):** the **entity graph** (`entities`,
  `edges`) and **per-row vectors** (`mem_vectors`). `indexUpsert` deliberately
  does not touch them, because a correct single-memory graph delta is not a
  local operation (the graph is a whole-corpus product — `buildGraph` +
  `canonicalizePersons` recompute mention counts and merge identities across
  memories) and per-memory re-embedding is the O(vault) cost the upsert removes.
  - The graph gap is bounded by the full-rebuild cadence (the `index-hourly`
    job, `mora index rebuild`, connector sync, delete) — it is never indefinite.
  - The vector gap has **no effect under the default static-hash embedder**,
    where `defaultSearch` is FTS-only (embedder-gated routing: `defaultSearch`
    enables hybrid only under a semantic embedder, because hybrid regresses recall
    under static-hash — see [retrieval & search](./02-retrieval-search.md)). Under a semantic (Ollama)
    embedder it is a real but bounded, self-healing recall gap on the hybrid arm
    only — the memory is fully searchable via FTS immediately and gains its
    vector at the next full rebuild.

So the honest statement of the contract is: **after a write, the memory is on
disk (durable) and FTS-searchable (indexed) immediately. Its graph edges and
vector reconcile at the next full rebuild.** A reconciling full rebuild after any
storm restores exact vault↔index correspondence — which is what the contract
stress test asserts as G4.

### Multi-process reader/writer coexistence — WAL (+ `busy_timeout`)

The index is opened in **WAL** (`journal_mode(WAL)` on both `roIndexDSN` and the
writer `rwIndexDSN`). This is what makes the contract hold across *processes*, not
just goroutines: in production ~18 long-lived `mora mcp serve` processes (one per
agent session) read the index concurrently. In the default rollback journal a
writer needs an EXCLUSIVE lock incompatible with every reader's SHARED lock, so a
rebuild/write must wait for **all** readers and — under that load — blows past
`busy_timeout` and surfaces `database is locked`. In WAL, readers read the last
committed snapshot and never block the writer (and the writer never blocks them);
a full rebuild builds the whole new index in the `-wal` file while readers keep
serving the old snapshot, then commits atomically (a `wal_checkpoint(TRUNCATE)`
folds it back and resets the `-wal`). WAL persists in the db header, so the first
open of either DSN converts a legacy `delete`-mode index in place.

`busy_timeout(15000)` still matters — WAL still serializes concurrent *writers*
(one writer at a time), and a reader can briefly contend during a checkpoint — so a
contended writer/reader waits out the short window instead of erroring, and
`openIndexRO` won't misread a transient `SQLITE_BUSY` as a stale schema and fire a
spurious rebuild. `humanizeIndexBusy` gives an actionable message only if a caller
genuinely outlasts 15 s. Guarded by `TestIndexIsWAL` / `TestIndexUpsertKeepsWAL`.

**Multi-process proof (Gate 2 / HEALTH-06).** The goroutine-only storms in
`concurrency_contract_test.go` / `index_busy_test.go` / `index_wal_test.go` are
necessary but not sufficient — #108's own lesson was *"test separate PROCESSES."*
`TestNoUserVisibleSQLITEBUSY` (`concurrency_multiproc_test.go`) re-execs the test
binary as 4 writers + 4 readers (CLI `search` + MCP `search_memory`) + 1 rebuild +
1 filesystem sync against one shared HOME and asserts: zero raw or humanized
`SQLITE_BUSY` in any process output. The index is never observed empty while clean;
and for every parseable write, at every observation the memory is in the index **or**
the index is dirty — never "clean and missing." Personal `index.db` only. Share
DSNs are Packet H / PR 5. Cost of mark-dirty: `BenchmarkIndexUpsertWithMarking1k`
beside `BenchmarkIndexUpsert1k` (flip-condition: >2× regression).

> modernc caveat (now load-bearing, not just intent): with `modernc.org/sqlite`,
> `mode=ro` on a non-`file:` DSN is parsed but **not enforced** — connections open
> read-write. That is *why* a "read-only" open can still create the `-wal`/`-shm`
> sidecars WAL needs, so there is no read-only-WAL breakage. The read-path contract
> remains WAL + `busy_timeout` + the convention that read paths never write. Do not
> rely on `mode=ro` for mutual exclusion.

## 7. What is deliberately NOT provided

- **No cross-host / multi-writer-machine coordination.** The lease and file
  atomicity are single-host. Two machines pointed at one shared vault over a
  network filesystem are out of contract.
- **No long-held locks.** There is no resident daemon and no global mutex. The
  sources lease is held for microseconds and never across an ingest/rebuild. The
  design is short-lived processes + files-are-truth (the daemon/watchdog
  direction is formally rejected — see the do-not-build ledger).
- **No transactional coupling between the vault and the index.** They are
  separate stores by design (I1). A crash between the vault write and the index
  upsert leaves the memory durable on disk and reconciled at the next rebuild,
  never lost.

## Verification

- `internal/mora/concurrency_multiproc_test.go` — **HEALTH-06** multi-*process*
  storm (`TestNoUserVisibleSQLITEBUSY`): re-exec'd writers/readers/rebuild/sync
  against one HOME. Zeros raw and humanized `SQLITE_BUSY`. Never "clean and
  missing."
- `internal/mora/concurrency_contract_test.go` — the in-process integration storm:
  N writers through the real `cmdWrite` and MCP `write_memory` paths, concurrent
  `search`/`read` readers, and concurrent full `rebuildIndex`, asserting G1–G4
  (every memory in vault AND index after reconciliation, zero surfaced
  `database is locked`, zero torn files via `parseMemory`). The heavy variant is
  `-short`-gated. A smaller always-on variant keeps the signal under `-short`.
  Run it under `-race`.
- Unit pins: `createexclusive_test.go` (G1/G2), `index_upsert_test.go` (G3/G4
  write path), `index_busy_test.go` (G3 read path), `mora_rebuild_atomic_test.go`
  (G4 serialized rebuild), `sources_lock_test.go` (§5 lease) +
  `TestSourcesRMWNoLostUpdateAcrossProcesses` (cross-process sources.json RMW).

See also: [data model & storage](./01-data-model-and-storage.md) (memory file
anatomy, `atomicWrite`, the vault-identity guard), [MCP server](./06-mcp-server.md)
(the write_memory/read/search tool surface), [index health](./20-index-health.md)
(Gate 2 pending-ops ledger), and [sync &
freshness](./11-sync-and-freshness.md) (honest-snapshot sync, the scheduled
jobs).
