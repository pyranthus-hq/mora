# Index Health & the Pending-Ops Ledger

Mora must show when the **derived index** fails (Gate 2,
HEALTH-09/-10/-12). Gate 1 shows a dead *source*. This gate shows a stale,
half-built, degraded, or missing-last-write *index*. A fresh source time can
never hide an older committed index. The rule is simple: *never let a fresh
source timestamp mask an older index.*

The index is a **derived, eventually-consistent cache** over the Markdown vault
(invariant I1). The daily brief, `list`, `read`, and no-query `context` read the
vault without opening the index. Thus, the dirty gate cannot live only in the
index-open path. It is a **health rule that each surface checks**. Mora computes
it from durable state and carries it as data, like Gate 1's `sourceHealth`.

## Files

| File | Responsibility |
|---|---|
| `internal/mora/pending.go` | The **pending-ops ledger**: `pendingOp` (write \| delete \| rebuild), `markIndexDirty`/`unmarkIndexDirty`, `listPendingOps`, the A3 clearing rules (`clearCoveredPendingOps`/`shouldClearOp`), `pendingDeleteIDs`/`suppressPendingDeletes` (the B4 read-path suppression). `indexClock` is the injectable clock all of Gate 2's stamps resolve against. |
| `internal/atomicio/atomicio.go` | `atomicio.WriteDurable` — `atomicio.Write` plus two crash barriers (`f.Sync` before rename, `atomicio.SyncDir` after), behind the `atomicio.MarkerSyncFn`/`atomicio.SyncDirFn` seams so the durability call-trace is testable. |
| `internal/mora/pending.go` | `testHookPostMarkerWrite` fires inside `markIndexDirty` after a marker is durably on disk (the crash-window seam) — declared here (not in `internal/atomicio`) since it is set and read entirely from this file. |
| `internal/atomicio/sync_notwindows.go` / `sync_windows.go` | The `atomicio.SyncDir` build-tag pair — a real parent-dir fsync on POSIX/darwin (`F_FULLFSYNC`), a documented no-op on Windows (NTFS `MoveFileEx` is metadata-journaled). Mirrors `internal/atomicio/rename_*windows.go`. |
| `internal/mora/ingest_journal.go` | The **durable ingest journal** (`StateDir/ingest/<source>/journal.log`): a durable `run <op_id> <marked_at>` header written before the first connector publish, best-effort per-path lines, `ingestJournalStatus` (the B1-rule-4 read), and `recoverIngestJournals` (error-returning post-commit compaction + retired run ids). |
| `internal/mora/operation_activity.go` | The bounded, content-free operation receipt primitive under `StateDir/operations/`: owner-fenced begin/heartbeat/finish writes plus read-only running/stalled/failed/completed classification for ingest and index-rebuild work. |
| `internal/health` | The canonical typed health DTOs/state vocabulary, projection-lag relation, B1b fail-closed worst-of collapse, and bounded one-line aggregate banner across sources, personal/share indexes, producers, and operation activities. |
| `internal/mora/indexhealth.go` | I/O-backed health fact assembly: `indexHealthOf` (the seven-rule first-match-wins predicate), `healthOf` (the composition entry), index database/journal/block-record reads, and no-probe embedder-provenance comparison. |
| `internal/mora/health_banner.go` | Thin compatibility adapters from Mora surfaces to `internal/health.BannerAll` and its fixed byte cap. |
| `internal/mora/indexstamp.go` | The `index_meta` stamps written inside the rebuild commit tx + the content-manifest helpers (`manifestLine`/`manifestDigestOf`, `indexManifestAlgo`), and `stampIndexAttemptFailure`. |
| `internal/mora/doctor_index.go` | Doctor-side helpers: the `sources_config` predicate (`enabledSourceCount`/`vaultHasConnectorMemories`), `disabledCorpusTypes`, and `indexMatchesVault` (the B1a manifest recompute). |
| `internal/mora/index.go` | `rebuildIndexWithPolicy` marks itself, snapshots `listing_started_at`, computes the manifest for free from the parse bytes, stamps the projections + embedder + manifest inside the commit tx, and clears covered ops + journals after commit, fails visibly on partial cleanup, and only then completes operation receipts. |
| `internal/mora/index_upsert.go` + `index_reconcile.go` | Upsert makes FTS immediate; one coalesced elected rebuild then reconciles graph, vectors, commitments, and the manifest. |

## The core invariant: mark before visible, clear only on commit

Every vault mutation writes a **crash-durable pending-op file** *before* the vault byte becomes visible, and only a successfully-committed covering rebuild may retire it for an authored write (an upsert makes its FTS row visible but does not cover every projection). A pending op — or a non-empty ingest journal — makes the index read **dirty** on every surface. So a memory that landed in the vault but not the index can never masquerade as indexed.

```
mutate(memory m):
  1. markIndexDirty(cfg, op)   — atomicio.WriteDurable StateDir/pending/<op_id>.json
        (f.Sync + atomicio.SyncDir; MUST fully return before step 2). On a real I/O fault:
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

**Durability is the hard requirement.** Plain `atomicio.Write` gives neither data nor directory-entry durability on POSIX (a bare `os.Rename`), so a power loss could persist the vault publish while losing the earlier marker — the forbidden **false-clean**. `atomicio.WriteDurable` fsyncs the temp file *before* the rename and fsyncs the parent directory *after*, both propagating their errors. Because `StateDir` and `VaultDir` are independently settable (and the vault is often an external/synced volume), ordering — the marker fully returning before the vault publish — is the invariant, not a shared journal.

## The clearing rules (A3)

A committed rebuild retires only the ops it **demonstrably** covered. It snapshots `listing_started_at` immediately before listing and collects `parsed` — the paths `parseMemory` actually succeeded on:

| Kind | Cleared when | Why |
|---|---|---|
| `rebuild` | `marked_at ≤ listing_started_at` | Any rebuild that listed after the op was marked covers it — **not** only its own `op_id`, or a SIGKILLed rebuild's op is unrecoverable forever. |
| `write` | `path ∈ parsed` | `parsed`, not the listing: a truncated/hand-mangled file is listed yet not indexed, and must stay dirty (never "fresh but missing"). |
| `delete` | `path ∉ files` **or** `path ∈ parsed` | The second clause covers legitimate re-ingest — a connector rewrites a memory onto its own stable path — so the op does not live forever and B4 does not hide a live memory. |

Ingest uses a **crash-recoverable journal** instead of one pathless op, because a killed backfill cannot write a terminal completion bit (`SyncStatus` only persists after `memory.Ingest` returns). The durable header keeps the index dirty across a SIGKILL. The next committed rebuild lists every published file — journaled or not — indexes it, and retires the journal (rule d: retire on *listed*, deliberately asymmetric with the write rule, so one persistently-malformed connector file cannot pin thousands of memories dirty forever — the manifest's `unparseable` count owns that signal instead).

## The typed health kernel

`indexHealthOf(cfg, now)` is first-match-wins, `now` injected (never `time.Now()` in a check path):

1. `index.db` absent → **never**
2. cannot open / schema mismatch / any query error → **failed**
2b. `index_meta` wiped of its committed provenance (no `vault_dir` row — a truncated db, a hand `DELETE FROM index_meta`, a torn restore) → **failed**. Every committed rebuild stamps `vault_dir` and never deletes it, so its absence is an uncomputable state, not a legacy index. (The absent-is-not-dirty tolerance is only for the *new* keys — embedder provenance and the content manifest — never the binding rows that prove a rebuild committed.)
3. a rebuild block record present → **failed** (`Blocked`)
4. any pending op **or** non-empty ingest journal → **dirty**
5. recorded embedder ≠ configured embedder → **degraded** (HEALTH-12)
6. projection lag `fts_indexed_at − graph_indexed_at > threshold` → **dirty**
7. else → **fresh**

The **fail-closed rule**, enforced everywhere: any state Mora cannot *compute* is `unhealthy`, never `healthy`. There is no "assume fine" branch.

**Three projections, three freshnesses (Finding 2).** A full rebuild advances `fts_indexed_at`, `graph_indexed_at`, and `vectors_indexed_at`; incremental `indexUpsert` advances FTS first, making an authored memory immediately searchable. MCP `write_memory` schedules a leased reconciler before returning from that small transaction but does not await it. The worker coalesces bursts for 75 ms before it invokes the ordinary atomic full rebuild, keeping whole-vault work off the request path. If the process exits first, the durable marker is instead retired by an explicit rebuild, the scheduled `index-hourly` run (normally within one hour), connector sync, or delete. One-shot `mora write` waits for the same reconciliation because it cannot guarantee a background worker survives process exit. Until a commit covers it health remains honestly dirty; no MCP response claims full-projection freshness. Projection lag is therefore a **relation** between two stamps, never wall-clock age.

**Minimal embedder provenance (HEALTH-12 mismatch arm).** The rebuild stamps `embedder_model`/`embedder_dim` (what *ran*) inside its commit tx. `indexHealthOf` compares that against what the config *asks for*, resolved **without probing Ollama** (so doctor stays fast/offline), and reports `degraded` on a mismatch — the recorded incident where the config said `ollama` but the index was silently rebuilt static. Absent provenance (a legacy index) is treated as a match, so an upgrade does not redden every existing user's first doctor.

**Fail-closed rebuild + semantic digest (HEALTH-12, Packet D/PR 3).** `Embedder.Embed` returns `([]float32, error)` and no path fabricates a zero vector; `embedderForPref` returns `errEmbedderUnavailable` for an unreachable/non-loopback `ollama` opt-in instead of silently substituting static. The ONE gate lives in `rebuildIndexWithPolicy` (resolved before `BeginTx`), so **all** rebuild triggers fail closed — a rebuild with the daemon down refuses (nonzero exit) and preserves the previous vectors byte-for-byte, and both read-path rebuild doors (rebuild-on-missing, schema-stale auto-heal) refuse to re-embed with the static fallback. The rebuild also stamps the semantic **`embedder_digest`** (the resolved Ollama model digest); `indexHealthOf` rule 5 additionally reports `degraded` on **mixed provenance** — more than one distinct `mem_vectors.model`, or a stored model ≠ the recorded one — catching a partially-completed re-embed the single meta row cannot. `mora config` reports the **resolved** embedder (e.g. `ollama (UNREACHABLE — index built with static-hash-v1)`), not the raw config value.

**The content manifest (B1a).** The ledger only sees mutations that go *through* Mora. A hand-edit, Obsidian edit, or backup-restore needs a filesystem check. A committed `sha256`-over-relpath manifest (stored in `index_meta`, computed for free from the same bytes the rebuild parses) is compared by `mora doctor` against a live recompute — never mtime, which a backdated restore or a coarse clock defeats. Absent ⇒ *unverified* (non-critical, so a legacy index is not red-on-upgrade). Present + mismatch ⇒ critical. The recompute runs only on the `doctor` path (where a vault walk is already paid), never `--pulse`, never the MCP hot path.

## Fail-closed doctor, the aggregate banner, and B4

`mora doctor` gains critical `index_fresh` / `index_embedder` checks, a typed `index` object and non-null `producers` array in `--json`, a `sources_config` predicate that is now **critical** and counts only *enabled* sources (so `mora sources disable gmail` can no longer silently switch off the alarm while a corpus exists), and `source_disabled_with_corpus:<type>` for the disable-one-connector case. `--strict`/`--pulse` are nonzero for a dirty, failed, degraded, blocked, or never index.

The **banner** (`healthBannerFrom`) is now the single worst arm across sources, index, and producers — still exactly **one line** and the **first content line** (four tests and the digest budget frame pin that), capped by `healthBannerLineCap`. It is a pure function of the snapshotted `Health`, so no render path calls `time.Now()`.

**Pending deletes suppress reads (B4 — a data-safety P0, not cosmetics).** A delete `os.Remove`s the file then rebuilds. A rebuild failure would otherwise leave the index serving content the user deleted. While a `kind=delete` op is pending, its `memory_id` is filtered out of **both** index read chokepoints — the search `memories` JOIN and `loadMemoriesByID` (the graph arm reached by `get_entity`, `list_entities`, and the meeting brief). This turns fail-closed from a banner into an actual guarantee for the one case where serving stale content is harmful.

Finally, `vault/index.md` (injected verbatim into every `context_memory` payload) is refreshed from the *same* stamp the rebuild wrote into `index_meta`, so the page an agent trusts can never claim a freshness the index does not have (B5).

## Subordinate operation activity

`Health.Activities` explains *why* a dirty index may currently be changing without
weakening the freshness floor. Each sanitized record has only the operation kind
(`ingest` or `index_rebuild`), lifecycle state, run id, timestamps, phase, and
bounded counts. It contains no provider/account label, memory path/id, query, or
source content. Doctor JSON returns a non-null `activities` array and human doctor
renders the same fields. A live activity does not make health green: `index.state`
remains `dirty`, aggregate health remains unhealthy, and strict doctor still fails.
The banner may instead explain that refresh is in progress and the last committed
snapshot is being served.

The durable record is `StateDir/operations/<kind>/<run_id>.json` (0600,
crash-durable atomic replacement). A heartbeat update is authorized by the tuple
(kind, run id, owner pid) under one bounded persistent OS lease guard per kind. PID existence is
only corroboration: a record is `running` only while its heartbeat is within the
15-minute TTL and its owner is live. Writers refuse a heartbeat or terminal stamp
that moves backward. A dead owner or expired heartbeat is classified `stalled`;
path/record identity mismatch, unknown JSON fields or trailing values, a future
schema, an invalid phase/timestamp, or incoherent lifecycle fields classify
`failed`. Classification is strictly read-only — plain doctor and health surfaces
never repair, reap, or delete markers. Health exposes only the newest valid terminal receipt per kind (plus every
active/stalled/corrupt receipt), so an old failure cannot keep health red after a
newer success. On-disk terminal retention is bounded to 16 per kind and pruned only
by a terminal writer.

Every `ingestSource` begins its receipt and a run-id-bound journal header before
provider dispatch can publish a vault byte. A bounded heartbeat keeps long fetches and batch-wait time
live; clean ingest stops at `awaiting_rebuild`. It becomes `completed` only when a
committed rebuild actually retires that run's journal. Failed ingest is terminal
`failed`, and concurrent sources retain separate anonymous receipts (the source key
exists only in the pre-existing journal layout, never the activity projection).

`rebuildIndexWithPolicy` similarly begins before its pending dirty marker and
advances through choosing-embedder, open, list, parse, graph, vectors, commitments,
commit, marker retirement, and finalization phases. Its success transition happens
only after the SQLite commit, covered pending-op removal, relevant journal removal,
and covered-ingest completion. A cleanup failure after commit is returned as a
visible partial failure (`post_commit_cleanup_failed`): the committed database is
preserved, the uncleared marker keeps the index dirty, and no completed rebuild
receipt is written. Setup/connect flows already call this rebuild synchronously, so
they naturally wait for this terminal result; there is no separate setup-status
surface.

## Producer liveness (HEALTH-11) — the watchman

A healthy source and a clean index prove data *arrived* and is *indexed* — not that any **product surface** was ever produced from it. The 7-day dead-automation SEV was exactly this: every arm green while the brief had not run in a month. The producer arm convicts that state. `internal/health.ProducerStore` owns its two **disjoint** files under `StateDir/producers/`; Mora owns the command chokepoints, expectation/adoption authorization, and orchestration:

- **`status.json` — evidence.** One row per producer, stamped at each producer command's **own chokepoint** (`withProducerStamp`), so launchd, cron, an external orchestrator, and a human all record identically — never the scheduler (a broken scheduler must not look like a healthy producer). Wrapped sites: `pulse --advance` (pulse-daily), `index rebuild`, `ingest run --all`, `backup`, `lint`, `sync git`, and `doctor --pulse`. Each keeps a bounded ring of raw success timestamps.
- **`expected.json` — expectation.** What *should* run. It is a **separate file on purpose**: the deleted-worktree incident is modeled by deleting the stamp, and an expectation inferred from the stamp would be erased by the very event it must detect (the alarm would delete itself). A producer is expected when **adopted** — ≥3 non-interactive successes whose consecutive gaps are each ≥1h and which span ≥3 distinct UTC days. The interval is the **median inter-run gap**, clamped to [1h, 7d]. Adoption is gated on non-interactive runs so a human running `mora index rebuild` three times while debugging cannot pin a ~2-minute cadence and redden the product forever. `mora doctor --forget-producer <name>` retires one (removes both files' rows) — an adoption you regret is never a permanent red banner.

**Health:** `producerHealthAll` reports one record per *expected* producer — `never` (no success), `stale` (newest success older than **2× interval**), `failed` (latest attempt errored), else `fresh`. A never-expected producer is simply **absent**, so a user who scheduled nothing is never nagged. `producer_live:<name>` is a **critical** doctor check (OK only when `fresh`). Known producer liveness issues (`prodStale`, `prodFailed`, `prodNever`) yield a `degraded` (yellow) state in `internal/health.AggregateState` and render as yellow banners (`🟡 MORA HEALTH: <producer> has not been produced...`) from the same pure package when sources and index are sound, distinguishing ops attention for background jobs from red data staleness (`🔴 MORA HEALTH:`). An unreadable or corrupt producer ledger emits an explicit typed `subject:"ledger"` record and the critical `producer_ledger_readable` doctor check; it remains fail-closed `unhealthy` (red). This metadata, not a reserved producer name, distinguishes ledger failure, so a legitimate producer named `producers` still receives ordinary yellow liveness treatment. Source, personal-index, and subscription-index (`h.Index.Shares`) data-integrity alarms outrank producer warnings in `healthBannerFrom` so yellow never hides red. A consumer-side detector, `brief_artifact_fresh`, needs no registration at all: if a dated `briefs/*-brief.md` artifact exists but the newest is older than 2× the daily cadence, the surface is stale even if nothing was ever registered.

**Filesystem health identity:** each configured filesystem folder owns its own status file, so health uses the local key `filesystem:<source-name>` (for example, `filesystem:docs` and `filesystem:notes`). This prevents a fresh folder from deduplicating a failed one. The helper is intentionally health-only: filesystem memories still have no `Provider`, digest/watermark identity remains unchanged, and `briefHashSchemaVersion` remains 2.

**The watchman does not deadlock on its own stamp.** `doctor --pulse` stamps producer `doctor-pulse`, so it monitors *itself*. The stamp rule is **read-then-write**: it classifies the *prior* stamp (Phase 1) **before** writing, then stamps `LastSuccessAt = now` **unconditionally on the completion path — including the exit-2 path** (Phase 2). "The pulse succeeded" means *it ran to completion*, **not** "everything is healthy" — so a legitimately-failing gmail can never rot the watchman arm to stale and make doctor scream "the watchman is dead" while it runs every hour. This self-recovers in exactly one cadence: run N (stale stamp) reports the missed cadence, exits 2, **stamps**. Run N+1 sees a fresh stamp and exits 0. A plain `mora doctor` (not `--pulse`) never stamps, so a developer running it once cannot silence the watchman for a cadence.

**The ledger RMW is cross-process safe.** `status.json` is a single shared file every producer appends to, so a manual `mora index rebuild` racing the scheduled `index-hourly` would lose an update under a plain `atomicio.Write` (last rename wins). `internal/health.ProducerStore` holds the crash-safe, Windows-CI-green `internal/leasefile` publish/reap/CAS lease around the whole read-modify-write and **reloads inside the lease** so a concurrent writer's committed change is always observed. The lease is held only for the microsecond stamp — never across a producer's actual work.

Identity is **argv-derived** at each chokepoint (`pulse --advance` ⇒ pulse-daily, `index rebuild` ⇒ index-hourly, …), which keeps a legacy pre-flag plist's alarm alive. A `--producer=<job>` token overrides it when present.

## Related

- [sync-and-freshness](./11-sync-and-freshness.md) — Gate 1's per-source `sourceHealth`, the vocabulary this kernel extends.
- [retrieval-search](./02-retrieval-search.md) — `rebuildIndex`/`indexUpsert`, the `memories` JOIN chokepoint, the WAL multi-process contract.
- [concurrency-contract](./15-concurrency-contract.md) — `_txlock=immediate`, the writer-lock discipline the file-based ledger avoids contending on.
- [meeting-brief-assembly](./19-meeting-brief-assembly.md) — the most index-dependent surface. Where its banner now carries the index arm.

## Upgrade + permission loss (HEALTH-08)

A binary swap must leave every durable artifact parseable — `config.toml`, the vault,
`index.db` (+ WAL sidecars), `sources.json`, per-source status files, the governance
ledger, the usage db, the vault marker, brief snapshot, loops state, OAuth tokens,
installed plists. `indexSchemaVersion` is a package **var** (same seam pattern as
`indexAutoHeal`) so a schema bump can be simulated in-process: a stale index either
auto-heals via rebuild or `openIndexRO` refuses loudly — in both branches
`indexHealthOf` is non-`fresh` until a rebuild commits. Guarded by
`TestUpgradePreservesState`.

Full Disk Access loss on iMessage must fail **loud** and never advance
`LastSuccessAt`. `ingestIMessage` is gated through the injectable `runtimeGOOS`
seam and `newIMessageFetcher` (so Linux/Windows CI can drive the denial path —
macOS is not in CI). A denied open stamps failure via `stampSyncAttemptFailure`. A
legitimate zero-row sync still stamps success. Guarded by
`TestFDALossNeverStampsSuccess`.

**Signing clause closure has two evidence gates.** The standalone macOS bridge
is signed with Developer ID identifier `com.pyranthus.mora`, Team Identifier
`VS8M5VJBZ5`, hardened runtime, and a secure timestamp. The release workflow
submits both Darwin binaries to Apple and refuses to publish unless notarization,
strict signature verification, the designated requirement, Apple's notarized
code requirement, and a quarantined native launch pass. `install.sh` verifies
the same identity and notarized code requirement before and after its
copy. It does not clear quarantine or ad-hoc re-sign. A raw executable cannot
carry a stapled ticket, so its first notarization-ticket check can require a network
connection.

That closes the **distribution-mechanism** blocker, not the macOS permission
claim by itself. HEALTH-08 remains a dated HOLE until a real host records all of
these results: grant FDA to signed version N; atomically upgrade to signed
version N+1; read iMessage without a re-grant; then replace N+1 with a binary
from an unrelated signer and prove the read fails loudly without advancing
`LastSuccessAt`. CI cannot manufacture TCC evidence.

The identity migrations are also not silent. An FDA grant made to an older
ad-hoc executable can require one grant to the first Developer ID-signed bridge.
The later branded `Mora.app` changes the TCC target again, so plan for one final
grant to the app. Routine app upgrades can be called permission-preserving only
after a real version N to N+1 **whole-bundle** replacement passes the same read
test without a re-grant; replacing only `Contents/MacOS/mora` invalidates the
bundle seal and is not an accepted test.

## Open questions / unverified

- **Projection-lag threshold** (`indexProjectionLagThreshold`, 6h) is a judgment call: long enough that a fresh write does not immediately redden the product, short enough that a genuinely-owed rebuild surfaces within a few missed hourly cycles. Not tuned against live telemetry.
- **Ingest journal path lines are best-effort** (only the header is fsync-durable). This is sound because the recovery rebuild lists every file on disk regardless of journaling — the header's durability is the only load-bearing barrier. A concurrent committed rebuild must NOT retire an active run's header, though: a file that lands *after* that rebuild listed (then a crash before its path line appends) would otherwise be a false-clean. So an in-flight run holds a **live lease** (a pid-keyed marker beside its journal, A3 rule d) and a rebuild only fully retires a header when no live lease is held. A SIGKILLed run leaves a stale lease naming a dead pid, which the next rebuild reclaims — so a crash never pins the index dirty forever, and killed-ingest recovery still fires.
- **A present-but-empty journal is dirty, not absent.** `appendJournalDurable` creates/opens `journal.log` before writing and syncing the header, so a crash in that window leaves a zero-byte file. A committed rebuild *removes* a fully-covered journal, so any lingering `journal.log` — zero-byte, header-less, or truncated — means an uncovered run and reads **dirty** (fail-closed). Only an actually-missing file is treated as "no run."
- **`indexMatchesVault` recompute cost** is O(vault) and runs on every `mora doctor` (not `--pulse`, not MCP). At ~10⁴ memories that is one full hash walk. Acceptable where a walk is already paid, but a very large vault pays it on each manual doctor run.
