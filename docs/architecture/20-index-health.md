# Index Health & the Pending-Ops Ledger

How Mora proves the **derived index** cannot die silently (Gate 2, HEALTH-09/-10/-12). Gate 1 made a dead *source* impossible to miss; this gate makes a stale, half-built, degraded, or missing-the-last-write *index* impossible to miss — and guarantees a perfectly fresh source timestamp can never mask an older committed index. The whole job is one sentence: *never let a fresh source timestamp mask an older index.*

The index is a **derived, eventually-consistent cache** over the Markdown vault (invariant I1). Half the product never opens it — the daily brief, `list`, `read`, and the no-query `context` walk the vault directly — so the dirty gate is **not** a check inside the index-open path. It is a **health predicate every surface consults**, computed from durable state and carried as data, exactly like Gate 1's `sourceHealth`.

## Files

| File | Responsibility |
|---|---|
| `internal/mora/pending.go` | The **pending-ops ledger**: `pendingOp` (write \| delete \| rebuild), `markIndexDirty`/`unmarkIndexDirty`, `listPendingOps`, the A3 clearing rules (`clearCoveredPendingOps`/`shouldClearOp`), `pendingDeleteIDs`/`suppressPendingDeletes` (the B4 read-path suppression). `indexClock` is the injectable clock all of Gate 2's stamps resolve against. |
| `internal/mora/atomicio.go` | `atomicWriteDurable` — `atomicWrite` plus two crash barriers (`f.Sync` before rename, `syncDir` after), behind the `markerSyncFn`/`syncDirFn` seams so the durability call-trace is testable. `testHookPostMarkerWrite` fires after a marker is durably on disk (the crash-window seam). |
| `internal/mora/sync_notwindows.go` / `sync_windows.go` | The `syncDir` build-tag pair — a real parent-dir fsync on POSIX/darwin (`F_FULLFSYNC`), a documented no-op on Windows (NTFS `MoveFileEx` is metadata-journaled). Mirrors `rename_*windows.go`. |
| `internal/mora/ingest_journal.go` | The **durable ingest journal** (`StateDir/ingest/<source>/journal.log`): a durable `run <op_id> <marked_at>` header written before the first connector publish, best-effort per-path lines, `ingestJournalStatus` (the B1-rule-4 read), and `recoverIngestJournals` (post-rebuild compaction). |
| `internal/mora/indexhealth.go` | The **typed health kernel**: `Health`/`indexHealth`/`projectionHealth`/`embedderProvenance`/`producerHealth`, `indexHealthOf` (the seven-rule first-match-wins predicate), `healthOf` (the one public entry), `aggregateHealthState` (the B1b worst-of collapse), and the no-probe embedder-provenance comparison. |
| `internal/mora/health_banner.go` | `healthBannerFrom(Health)` — the **one-line aggregate banner** across sources/index/producers, `indexBannerLine`, `healthBannerLineCap`. |
| `internal/mora/indexstamp.go` | The `index_meta` stamps written inside the rebuild commit tx + the content-manifest helpers (`manifestLine`/`manifestDigestOf`, `indexManifestAlgo`), and `stampIndexAttemptFailure`. |
| `internal/mora/doctor_index.go` | Doctor-side helpers: the `sources_config` predicate (`enabledSourceCount`/`vaultHasConnectorMemories`), `disabledCorpusTypes`, and `indexMatchesVault` (the B1a manifest recompute). |
| `internal/mora/index.go` | `rebuildIndexWithPolicy` marks itself, snapshots `listing_started_at`, computes the manifest for free from the parse bytes, stamps the projections + embedder + manifest inside the commit tx, and clears covered ops + journals after commit. |
| `internal/mora/index_upsert.go` | The incremental path advances `indexed_at` + `fts_indexed_at` only (never graph/vectors) and invalidates the manifest. |

## The core invariant: mark before visible, clear only on commit

Every vault mutation writes a **crash-durable pending-op file** *before* the vault byte becomes visible, and only a successfully-committed rebuild/upsert transaction may retire it. A pending op — or a non-empty ingest journal — makes the index read **dirty** on every surface. So a memory that landed in the vault but not the index can never masquerade as indexed.

```
mutate(memory m):
  1. markIndexDirty(cfg, op)   — atomicWriteDurable StateDir/pending/<op_id>.json
        (f.Sync + syncDir; MUST fully return before step 2). On a real I/O fault:
        ABORT before a single vault byte changes (errIndexUnmarkable) — a retry
        then cannot mint a duplicate memory, preserving the MCP isError asymmetry.
  2. write the vault file                          — the mutation becomes VISIBLE
  3. indexUpsert(m): one tx that stamps indexed_at + fts_indexed_at, then COMMITS;
        only after Commit returns nil is the op removed.
        - tx fails  → vault has it, index does not, op REMAINS ⇒ dirty ⇒ every surface red
        - crash between Commit and unlink → a FALSE-DIRTY op, cleared by the next rebuild
  X. compensating retirement: if step 2 fails, the mutation removes its own op before
        returning — else a failed vault write pins the index dirty forever.
```

**Why files, not an index table.** The first design put pending ops in an `index.db` table. Adversarial review killed it: it would brick every existing v2 install (the upsert fast-path's readiness check would fail on the new table) and **deadlock against the rebuild's own `_txlock=immediate` write transaction** — a rebuild of thousands of memories against Ollama holds that lock for minutes, and a second immediate transaction (the mark) would block and die on `busy_timeout`. Files never contend on the writer lock, never fail on a locked/corrupt/missing index, and need **no schema bump** (`indexSchemaVersion` stays 2, shared with every subscriber's share index).

**Durability is the hard requirement.** Plain `atomicWrite` gives neither data nor directory-entry durability on POSIX (a bare `os.Rename`), so a power loss could persist the vault publish while losing the earlier marker — the forbidden **false-clean**. `atomicWriteDurable` fsyncs the temp file *before* the rename and fsyncs the parent directory *after*, both propagating their errors. Because `StateDir` and `VaultDir` are independently settable (and the vault is often an external/synced volume), ordering — the marker fully returning before the vault publish — is the invariant, not a shared journal.

## The clearing rules (A3)

A committed rebuild retires only the ops it **demonstrably** covered. It snapshots `listing_started_at` immediately before listing and collects `parsed` — the paths `parseMemory` actually succeeded on:

| Kind | Cleared when | Why |
|---|---|---|
| `rebuild` | `marked_at ≤ listing_started_at` | Any rebuild that listed after the op was marked covers it — **not** only its own `op_id`, or a SIGKILLed rebuild's op is unrecoverable forever. |
| `write` | `path ∈ parsed` | `parsed`, not the listing: a truncated/hand-mangled file is listed yet not indexed, and must stay dirty (never "fresh but missing"). |
| `delete` | `path ∉ files` **or** `path ∈ parsed` | The second clause covers legitimate re-ingest — a connector rewrites a memory onto its own stable path — so the op does not live forever and B4 does not hide a live memory. |

Ingest uses a **crash-recoverable journal** instead of one pathless op, because a killed backfill cannot write a terminal completion bit (`SyncStatus` only persists after `memory.Ingest` returns). The durable header keeps the index dirty across a SIGKILL; the next committed rebuild lists every published file — journaled or not — indexes it, and retires the journal (rule d: retire on *listed*, deliberately asymmetric with the write rule, so one persistently-malformed connector file cannot pin thousands of memories dirty forever — the manifest's `unparseable` count owns that signal instead).

## The typed health kernel

`indexHealthOf(cfg, now)` is first-match-wins, `now` injected (never `time.Now()` in a check path):

1. `index.db` absent → **never**
2. cannot open / schema mismatch / any query error → **failed**
3. a rebuild block record present → **failed** (`Blocked`)
4. any pending op **or** non-empty ingest journal → **dirty**
5. recorded embedder ≠ configured embedder → **degraded** (HEALTH-12)
6. projection lag `fts_indexed_at − graph_indexed_at > threshold` → **dirty**
7. else → **fresh**

The **fail-closed rule**, enforced everywhere: any state Mora cannot *compute* is `unhealthy`, never `healthy`. There is no "assume fine" branch.

**Three projections, three freshnesses (Finding 2).** A full rebuild advances `fts_indexed_at`, `graph_indexed_at`, and `vectors_indexed_at`; an incremental `indexUpsert` advances only `fts_indexed_at`. So an authored write is findable by FTS immediately but its graph/vector projections lag until the next rebuild. Projection lag is therefore a **relation** between two stamps, never wall-clock age: an idle vault has `fts == graph` and never reddens by aging; the alarm fires only when an authored write has genuinely advanced FTS past the graph and a rebuild is owed.

**Minimal embedder provenance (HEALTH-12 mismatch arm).** The rebuild stamps `embedder_model`/`embedder_dim` (what *ran*) inside its commit tx. `indexHealthOf` compares that against what the config *asks for*, resolved **without probing Ollama** (so doctor stays fast/offline), and reports `degraded` on a mismatch — the recorded incident where the config said `ollama` but the index was silently rebuilt static. Absent provenance (a legacy index) is treated as a match, so an upgrade does not redden every existing user's first doctor. Packet D/PR 3 adds the fail-closed rebuild and the semantic digest.

**The content manifest (B1a).** The ledger only sees mutations that go *through* Mora; a hand-edit, Obsidian edit, or backup-restore needs a filesystem check. A committed `sha256`-over-relpath manifest (stored in `index_meta`, computed for free from the same bytes the rebuild parses) is compared by `mora doctor` against a live recompute — never mtime, which a backdated restore or a coarse clock defeats. Absent ⇒ *unverified* (non-critical, so a legacy index is not red-on-upgrade); present + mismatch ⇒ critical. The recompute runs only on the `doctor` path (where a vault walk is already paid), never `--pulse`, never the MCP hot path.

## Fail-closed doctor, the aggregate banner, and B4

`mora doctor` gains critical `index_fresh` / `index_embedder` checks, a typed `index` object and non-null `producers` array in `--json`, a `sources_config` predicate that is now **critical** and counts only *enabled* sources (so `mora sources disable gmail` can no longer silently switch off the alarm while a corpus exists), and `source_disabled_with_corpus:<type>` for the disable-one-connector case. `--strict`/`--pulse` are nonzero for a dirty, failed, degraded, blocked, or never index.

The **banner** (`healthBannerFrom`) is now the single worst arm across sources, index, and producers — still exactly **one line** and the **first content line** (four tests and the digest budget frame pin that), capped by `healthBannerLineCap`. It is a pure function of the snapshotted `Health`, so no render path calls `time.Now()`.

**Pending deletes suppress reads (B4 — a data-safety P0, not cosmetics).** A delete `os.Remove`s the file then rebuilds; a rebuild failure would otherwise leave the index serving content the user deleted. While a `kind=delete` op is pending, its `memory_id` is filtered out of **both** index read chokepoints — the search `memories` JOIN and `loadMemoriesByID` (the graph arm reached by `get_entity`, `list_entities`, and the meeting brief). This turns fail-closed from a banner into an actual guarantee for the one case where serving stale content is harmful.

Finally, `vault/index.md` (injected verbatim into every `context_memory` payload) is refreshed from the *same* stamp the rebuild wrote into `index_meta`, so the page an agent trusts can never claim a freshness the index does not have (B5).

## The `producer_live:*` fail-open contract

The producer arm (HEALTH-11) is Packet E / PR 4. Until that ledger exists, `healthOf` reports an **empty** `Producers` slice and doctor emits **no** `producer_live:*` checks — the check **fails OPEN on ledger absence** so an unbuilt ledger cannot redden PR 1's tree. When PR 4 lands the ledger, the same predicate flips critical automatically because the ledger is now present-and-stale-able.

## Related

- [sync-and-freshness](./11-sync-and-freshness.md) — Gate 1's per-source `sourceHealth`, the vocabulary this kernel extends.
- [retrieval-search](./02-retrieval-search.md) — `rebuildIndex`/`indexUpsert`, the `memories` JOIN chokepoint, the WAL multi-process contract.
- [concurrency-contract](./15-concurrency-contract.md) — `_txlock=immediate`, the writer-lock discipline the file-based ledger avoids contending on.
- [meeting-brief-assembly](./19-meeting-brief-assembly.md) — the most index-dependent surface; where its banner now carries the index arm.

## Open questions / unverified

- **Projection-lag threshold** (`indexProjectionLagThreshold`, 6h) is a judgment call: long enough that a fresh write does not immediately redden the product, short enough that a genuinely-owed rebuild surfaces within a few missed hourly cycles. Not tuned against live telemetry.
- **Ingest journal path lines are best-effort** (only the header is fsync-durable). This is sound because the recovery rebuild lists every file on disk regardless of journaling — the header's durability is the only load-bearing barrier — but a concurrent unrelated rebuild can briefly delete an active run's journal between page appends. That window can only ever produce a false-*dirty* on the next appended line, never a false-clean of an unindexed memory.
- **`indexMatchesVault` recompute cost** is O(vault) and runs on every `mora doctor` (not `--pulse`, not MCP). At ~10⁴ memories that is one full hash walk; acceptable where a walk is already paid, but a very large vault pays it on each manual doctor run.
