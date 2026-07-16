# Gate 2 mutation matrix — r7 closeout

**Status: CLOSED — 93/93 rows, 2026-07-16.**

The definition authority is the approved r7 Gate 2 packet at
`gsd-planning@f362323` (EXECUTION.md blob
`fc72be7432006d732fffcaafed96d1616e9ad192`). The executable registry is
[`scripts/eval/gate2-witnesses.tsv`](scripts/eval/gate2-witnesses.tsv), and
[`scripts/eval/gate2-mutation-matrix.sh`](scripts/eval/gate2-mutation-matrix.sh)
requires exactly 93 CLOSED matrix rows and 93 passing exact witnesses.

A green witness is necessary, not sufficient. CLOSED means an explicit
production-site edit made the named test fail and the edit was restored. The
closeout audit rejected green-only claims, compound edits, and mutations that
differed from final r7.

## Evidence record

Historical replay records accepted after the closeout audit:

- [PR #159](https://github.com/pyranthus-hq/mora/pull/159): durable index,
  journal, health-kernel, suppression, and manifest rows. Its initially weak
  rows 1, 2, 26a, and 34a were repaired and replayed in the PR review fixes.
- [PR #160](https://github.com/pyranthus-hq/mora/pull/160): fail-closed
  embedder rows.
- [PR #161](https://github.com/pyranthus-hq/mora/pull/161): concurrency,
  FDA, and upgrade rows.
- [PR #162](https://github.com/pyranthus-hq/mora/pull/162): the subset of
  generation-publish rows whose recorded edit exactly matched final r7.
- [PR #163](https://github.com/pyranthus-hq/mora/pull/163): surface and
  cached-brief rows. Its compound row-32 claim was rejected.
- [PR #165](https://github.com/pyranthus-hq/mora/pull/165): producer
  chokepoint and cross-process ledger rows.

Fresh independent closeout replays:

- Rows 23, 24, 25, and 37: producer criticality, artifact detector,
  interactive-adoption guard, and exit-2 pulse stamp each failed its exact
  named test under one production edit.
- Row 32: the old fixture was upstream-capped and false-green. It now uses a
  durable, user-defined producer identity with no upstream display cap.
  Removing only `capBannerLine` makes
  `TestUnhealthyBannerFitsTightestMCPBudget` fail at 3,464 tokens versus the
  1,500-token write ceiling.
- Rows 42 and 46a: healing from live corpus failed to recover the genuine
  frozen memory; removing only `--refmap=` mutated
  `refs/remotes/origin/*`.
- Rows 41a/b, 46c/d, 47b, 49, 50a-c, and 55a: 10/10 exact mutations RED.
- Rows 45a-d and 51a-d: 8/8 exact mutations RED.
- Rows 44a-f, 52a/b, 53a-f, and 54a/c: 16/16 exact mutations RED.

## Authoritative rows

| # | Production mutation | Named RED witness | Status / evidence |
|---|---|---|---|
| 1 | Drop `markIndexDirty` from one mutation site | `TestEveryVaultMutationMarksDirty` (+ that site's row) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 2 | Remove the op file **before** `Commit` returns nil (clear-then-commit) | `TestFailedUpsertLeavesIndexDirty` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 3 | Rebuild clears ops on **listing** membership instead of **parsed** membership | `TestUnparseableMemoryKeepsIndexDirty` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 4 | Rebuild clears every op regardless of coverage (drop A3 entirely) | `TestRebuildDoesNotClearRacedMutation` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 5 | Rebuild does not mark itself (drop A4) | `TestFailedRebuildIsVisibleOnACleanIndex` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 6 | Rebuild clears only its **own** `op_id` (drop rule (a)'s `marked_at ≤ listing_started_at`) | `TestKilledRebuildOpIsRecoverable` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 7 | Drop the compensating retirement (A2 step X) | `TestAbandonedMutationLeavesNoPendingOp` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 8 | Drop rule (c)'s reappearance clause | `TestReingestRetiresDeleteOp` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 9 | `indexHealthOf` returns `fresh` when a pending op exists | `TestDirtyIndexIsUnhealthy` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 10 | `indexHealthOf` returns `fresh` on an unopenable / schema-stale / **blocked** index | `TestIndexHealthFailsClosed` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 11 | Aggregate `Health.State` takes **best-of** instead of worst-of across the arms (B1b) | `TestFreshSourceCannotMaskDirtyIndex` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 12 | `index_fresh` made non-critical | `TestDoctorStrictNonzeroOnDirtyIndex` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 13 | Banner drops the index arm | `TestBriefRendersIndexBanner` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 14 | Any ONE surface drops `health` / the banner | `TestEverySurfaceCarriesHealth` | CLOSED — [PR #163](https://github.com/pyranthus-hq/mora/pull/163) |
| 15 | `resolveBrief` serves the cached body without re-checking health | `TestCachedBriefCarriesCurrentBanner` | CLOSED — [PR #163](https://github.com/pyranthus-hq/mora/pull/163) |
| 16a | Suppression dropped at the **`memories` JOIN** (search chokepoint) | `TestPendingDeleteIsNeverServed/search` (a pending-delete id is returned by search ⇒ RED) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 16b | Suppression dropped at **`loadMemoriesByID`** (meeting-prep chokepoint) | `TestPendingDeleteIsNeverServed/meeting_prep` (a pending-delete id surfaces as attendee evidence ⇒ RED) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 17 | `embedderForPref` substitutes the static embedder instead of returning `errEmbedderUnavailable` | `TestOllamaDownRebuildFailsClosed` | CLOSED — [PR #160](https://github.com/pyranthus-hq/mora/pull/160) |
| 18 | `writeVectors` swallows an `Embed` error (restore the zero vector) | `TestOllamaDiesMidRebuildFailsClosed` | CLOSED — [PR #160](https://github.com/pyranthus-hq/mora/pull/160) |
| 19a | The **unconditional rebuild-on-missing** door (`search.go:69`/`hybrid.go:140`/`graph_read.go:63`) is allowed to rebuild with an unresolvable embedder | `TestSearchCannotTriggerDegradedReEmbed/rebuild_on_missing` (a search on a missing index re-embeds with the static fallback and exits 0 ⇒ RED) | CLOSED — [PR #160](https://github.com/pyranthus-hq/mora/pull/160) |
| 19b | The **schema-stale auto-heal** door in `openIndexRO`/`indexAutoHeal` (`index.go:101-106`, `mora.go:826`) is allowed to rebuild with an unresolvable embedder | `TestSearchCannotTriggerDegradedReEmbed/schema_stale_autoheal` (a read against a schema-stale index auto-heals via a degraded re-embed instead of refusing ⇒ RED) | CLOSED — [PR #160](https://github.com/pyranthus-hq/mora/pull/160) |
| 20 | Provenance mismatch tolerated (drop `indexHealthOf` rule 5) | `TestEmbedderMismatchIsDegraded` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 21 | `mora config` prints `cfg.Embedder` verbatim again | `TestConfigReportsResolvedEmbedder` | CLOSED — [PR #160](https://github.com/pyranthus-hq/mora/pull/160) |
| 22 | Remove `withProducerStamp` at one producer | `TestProducerStampsAtRealChokepoint/<name>` | CLOSED — [PR #165](https://github.com/pyranthus-hq/mora/pull/165) |
| 23 | `producer_live:*` made non-critical | `TestDeadProducerFailsDoctor` | CLOSED — closeout replay |
| 24 | Remove the consumer-side artifact check (E3) | `TestDeadProducerSurfacesWithin24h` (artifact arm) | CLOSED — closeout replay |
| 25 | Adoption fires on interactive runs (drop the non-TTY / 3-day rule) | `TestInteractiveRunsNeverAdoptAProducer` | CLOSED — closeout replay |
| 26a | ▸R2 Doctor **skips the content-manifest recompute** entirely (B1a) | `TestOutOfBandVaultEditIsDirty/no_recompute` (an out-of-band edit is never detected ⇒ RED) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 26b | ▸R2 Doctor **compares `mtime`** instead of the committed content digest (B1a) | `TestOutOfBandVaultEditIsDirty/mtime_not_digest` (backdated-copy + equal-mtime + clock-rollback all read clean under mtime ⇒ RED) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 27 | `writeWikiIndex` left at its single CLI call site (`index.go:38`) | `TestWikiIndexTimestampMatchesIndexMeta` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 28 | Remove `busy_timeout` from `rwIndexDSN` | `TestNoUserVisibleSQLITEBUSY` | CLOSED — [PR #161](https://github.com/pyranthus-hq/mora/pull/161) |
| 29 | Swallow the FDA open error in `ingestIMessage` | `TestFDALossNeverStampsSuccess` | CLOSED — [PR #161](https://github.com/pyranthus-hq/mora/pull/161) |
| 30 | Serve a schema-stale index instead of refusing | `TestUpgradePreservesState` (branch 2) | CLOSED — [PR #161](https://github.com/pyranthus-hq/mora/pull/161) |
| 31 | `sources_config` keeps `OK: len(sources) > 0` (counts disabled rows) | `TestDisabledSourceWithCorpusIsNotHealthy` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 32 | Uncap the banner line (drop `healthBannerLineCap`) at the production render site | `TestUnhealthyBannerFitsTightestMCPBudget` (a MAX-length unhealthy banner still fits the `write_memory` 1500 **and** `delete_memory` 500 ceilings, double-counted) | CLOSED — closeout replay |
| 33 | **Delete the `markerSyncFn(f)` call** in `atomicWriteDurable`'s production body (the data `f.Sync`), keeping the rename — a production call-site edit, **not** a seam flip; `syncDir` retained | `TestDurableMarkerFsyncsBeforeRename` (the test installs a **recording wrapper** into the `markerSyncFn` seam and asserts the call trace is `[fsync, rename]`; deleting the production call ⇒ no `fsync` event before the rename ⇒ RED. fsync is unobservable via a process-crash sim, so observation is through the seam wrapper while the *mutation* is the removed production call) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 33b | **Delete the `syncDirFn(dir)` call** in `atomicWriteDurable`'s production body (the parent-dir sync), keeping the rename; `f.Sync` retained | `TestDurableMarkerSyncsDirBeforeReturn` (recording wrapper in the `syncDirFn` seam; asserts the dir sync fired before `atomicWriteDurable` returns, i.e. before the vault publish may begin; deleting the production call ⇒ no dir-sync event ⇒ RED) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 34a | ▸R2 Ingest writes its journal **AFTER** the terminal rebuild instead of before the first file (ordering) | `TestKilledIngestRecovers/journal_before_first_file` (SIGKILL a subprocess ingest after N publishes but before rebuild → no journal exists → next rebuild is clean-and-missing ⇒ RED) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 34b | ▸R2 Ingest **never `Sync`s the journal header line** (durability) | `TestKilledIngestRecovers/journal_header_synced` (power-cut sim / no-fsync leaves the header unwritten → recovery misses the published paths ⇒ RED) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 35 | `indexUpsert` advances `graph_indexed_at`/`vectors_indexed_at` (should advance FTS only) | `TestUpsertAdvancesFTSNotGraph` | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 36 | Projection lag computed as `now − graph_indexed_at` (wall-clock) instead of `fts_indexed_at − graph_indexed_at` (relation) | `TestProjectionLagUsesStampRelation` (one named table test: `idle_does_not_age` + `authored_write_advances_fts_only`; the wall-clock mutation fails both contract arms) | CLOSED — [PR #159](https://github.com/pyranthus-hq/mora/pull/159) |
| 37 | `cmdDoctorPulse` stamps only on the exit-0 path (gate the stamp on `banner==""`) | `TestPulseSelfRecoversInOneCadence` (stamp must advance on the exit-2 path) | CLOSED — closeout replay |
| 38 | Producer ledger RMW uses `atomicWrite` without the lease (drop `producer_lock.go`) | `TestProducerLedgerNoLostUpdateAcrossProcesses` (subprocess, not goroutines) | CLOSED — [PR #165](https://github.com/pyranthus-hq/mora/pull/165) |
| 39 | `resolvePublishedGen` returns a **non-maximal committed** generation (drop the max-committed-seq rule; the uncommitted-selection case is row 41b) | `TestOnlyHighestCommittedGenerationIsServed` (two committed gens seq5<seq6, the newer revoking a memory → a non-max pick returns the revoked memory ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 40a | Publish via **overwriting a mutable `current` pointer** instead of the atomic `os.Link(commits/<S+1>)` seq-claim | `TestConcurrentCommitClaimsAreAtomic` (two live builders each "win" the mutable pointer → the loser's gen clobbers the winner's ⇒ a lost/duplicated publish ⇒ RED; `os.Link` EEXIST admits exactly one winner) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 40b | Drop the **ownership re-verify** at the top of the H2(c) claim loop (a reaped holder may link/retry) | `TestZombieImportCannotPublishOverSuccessor` (reaped A resumes, links `gen-A` over successor B ⇒ the revoked memory served on search **and** read ⇒ RED — the by-construction zombie certificate) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 40c | Make the lease **release blind** (drop the generic `releaseLockFileFor` CAS so a reaped holder's late release removes a live successor's lease) | `TestBlindReleaseCannotStealLiveLease` (**three actors:** A reaped, B holds the lease, A's late blind release deletes B's lease, C acquires concurrently → two live imports on one sub ⇒ RED; the CAS release makes A's release a no-op) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 40d | Make the lease **heartbeat blind** (drop the run_id-CAS re-stamp so a reaped holder's ticker resurrects its lease) | `TestReapedLeaseCannotBeResurrectedByHeartbeat` (**three actors:** B reaps A; A's background heartbeat blindly re-stamps `acquired_at` → A's lease looks live again → A's commit fence passes falsely while B also holds it ⇒ RED; the CAS heartbeat sees `run_id≠A` and stops) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 41a | Link the commit record **before** `index.db` is built+fsynced (premature link — a half-built gen becomes resolvable) | `TestUncommittedOrHalfBuiltGenerationNeverServed/premature_link` (the commit is linked before the gen is durable → a crash leaves a resolvable half-built gen served ⇒ RED) | CLOSED — closeout replay |
| 41b | Resolver **enumerates `gens/`** instead of reading `commits/` (an uncommitted gen dir is resolvable) | `TestUncommittedGenerationDirIsNeverResolved` (a `gens/gen-X` with no `commits/` entry is served ⇒ RED) | CLOSED — closeout replay |
| 42 | `healShareIndex` rebuilds from **live/stray corpus files** instead of the published generation's own frozen corpus | `TestHealRebuildsOnlyFromFrozenPublishedCorpus` (a stray `corpus/<id>.md` outside the published gen is indexed and served ⇒ RED) | CLOSED — closeout replay |
| 43a | GC deletes the **published** (max-seq) generation or its record (drop the `seq < published_seq` guard) | `TestGCNeverRetiresPublishedGen` (the served gen is retired ⇒ the sub goes dark ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 44a | GC never reclaims **committed losers** (`seq < published` gens beyond K) | `TestGCReclaimsCommittedLosers` (superseded committed gens accumulate unbounded ⇒ RED) | CLOSED — closeout replay |
| 44b | GC drops only the **uncommitted `gen-*` directory** orphan reclaimer | `TestGCReclaimsCrashOrphans` (an ownerless gen older than TTL survives ⇒ RED) | CLOSED — closeout replay |
| 44c | GC drops only the stale **`fetch-*` bucket staging directory** reclaimer | `TestGCReclaimsStaleBucketFetchDirs` (ownerless bucket staging older than TTL survives ⇒ RED) | CLOSED — closeout replay |
| 44d | GC drops only the orphaned **Git private-ref** reclaimer (`git update-ref -d <ref> <observed-sha>`) | `TestGCReclaimsOrphanedGitImportRefs` (ownerless pin survives, or cleanup writes any other ref/control file ⇒ RED) | CLOSED — closeout replay |
| 44e | GC deletes stale commit records without requiring `published.BucketFloor >= max(all records)` | `TestGCReclaimsStaleCommitRecordsWithoutLoweringBucketFloor` (a zero-floor repair head would let GC erase the only V floor ⇒ RED) | CLOSED — closeout replay |
| 44f | GC **force-deletes or aborts** on a Windows sharing violation instead of deferring to the next sweep | `TestGCDefersWindowsOpenFileDeletion` (a reader holds a gen's `index.db` open → GC aborts/loops or errors instead of logging+deferring ⇒ RED) | CLOSED — closeout replay |
| 45a | Serve or re-cut a generation from the **untrusted local legacy** `index.db`/`corpus` instead of failing closed until a pinned-repo pull publishes gen-1 | `TestLegacyFlatShareIsFailClosedUntilPull` (a stale legacy index's revoked body OR a torn-corpus composite is served/blessed ⇒ RED) | CLOSED — closeout replay |
| 45b | Drop the one-way `migrated` latch write entirely | `TestMigratedLatchExistsBeforeLegacyRetirement` (commit loss after legacy retirement can resurrect legacy without the latch ⇒ RED) | CLOSED — closeout replay |
| 45c | Keep the latch but write it with `atomicWrite` instead of `atomicWriteDurable` | `TestMigratedLatchWriteIsCrashDurable` (file/dir sync trace missing; power-cut sim loses latch ⇒ RED) | CLOSED — closeout replay |
| 45d | Let empty-`commits/`-**with**-`migrated` fall back to legacy | `TestEmptyCommitsWithLatchFailsClosed` (fault-injected empty commits resurrects legacy ⇒ RED) | CLOSED — closeout replay |
| 46a | Remove `--refmap=` from the exact direct-ref fetch, allowing configured `remote.origin.fetch` to update shared tracking refs | `TestGitFetchWritesOnlyRunPrivatePin` (`pinRef` must advance while stale `FETCH_HEAD`, `HEAD`, and `refs/remotes/origin/*` remain unchanged ⇒ RED) | CLOSED — closeout replay |
| 46b | Drop `git merge-base --is-ancestor <published SourceRev> <pinRef>` before blob reads | `TestGitNonFastForwardPinIsRefused` (forced remote rewrite imports instead of returning the existing rotation/re-subscribe refusal ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 46c | Decrypt from shared worktree/`HEAD` instead of `git ls-tree`/`git cat-file` against `<pinRef>` | `TestGitGenerationReadsOnlyPinnedObjects` (reaped A resets branch/worktree after B pins → B imports A bytes ⇒ RED) | CLOSED — closeout replay |
| 46d | Fetch bucket input into fixed `subs/<name>/fetch` instead of `fetch-<run_id>` | `TestConcurrentBucketPullsDoNotShareStagingDir` (A/B staging mixture commits ⇒ RED) | CLOSED — closeout replay |
| 47a | `findSharedMemory` drops the committed **`corpus_digest`** verification entirely | `TestCorruptedPublishedCorpusFailsClosedOnRead/no_check` (corrupt `corpus/<id>.md` → read serves altered bytes while search keeps serving the intact index — a silent revoked-body read ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 47b | `findSharedMemory` verifies the digest then **re-reads** the file to serve (TOCTOU) instead of serving the **same bytes it hashed** (read-once) | `TestReadServesTheBytesItHashed` (corrupt `corpus/<id>.md` **between** the hash and the re-read → verify-then-reread serves the corrupted body ⇒ RED; serving the hashed bytes stays closed) | CLOSED — closeout replay |
| 48 | `openShareIndexRO` serves **without verifying the resolved generation's committed `index_digest`** (leave only the `user_version` + corpus-digest checks) | `TestSubstitutedShareIndexFailsClosedOnSearch` (replace the published gen's `index.db` with a *different* generation's structurally-valid v2 database — corpus intact → search/think serve the other gen's rows (revoked content) while read (corpus) 404s ⇒ RED; with the `index_digest` check search fails closed + heals from the frozen corpus) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 49 | H4's `shareHealthAll` checks only `index_digest`, **not `corpus_digest`**, of the published head | `TestCorruptedPublishedCorpusIsSurfacedByDoctor` (corrupt the head's corpus → doctor reads `fresh` while every direct read of the sub fails integrity ⇒ RED — the finding-3 cross-artifact-visibility gate) | CLOSED — closeout replay |
| 50a | Drop the `max(committed BucketFloor)` comparison and trust only `sub.LastVersion` | `TestReplayedOlderBucketEnvelopeRejectedAfterCommitCrash` (commit V, crash with old registry, replay V-1 ⇒ revoked content republishes ⇒ RED) | CLOSED — closeout replay |
| 50b | Store the accepted bucket version only in post-link `updateSubscriptionState`, not in the fsynced commit record linked for that generation | `TestBucketFloorIsPublishedInCommitRecord` (reboot after link resolves V but loses its floor ⇒ RED) | CLOSED — closeout replay |
| 50c | Heal writes `BucketFloor:0` instead of inheriting the source head floor | `TestBucketFloorSurvivesHealGCAndReplay` (crash→heal→GC deletes original V record→replay V-1 succeeds ⇒ RED) | CLOSED — closeout replay |
| 51a | Drop the active `attempt.json` publish before fetch | `TestShareAttemptStartPrecedesFetch` (killed pull leaves old success and doctor reads fresh ⇒ RED) | CLOSED — closeout replay |
| 51b | Delete only the attempt start's file-sync call before rename | `TestShareAttemptStartFsyncsBeforeRename` (recording trace lacks fsync ⇒ RED) | CLOSED — closeout replay |
| 51c | Delete only the attempt start's parent-dir sync before fetch | `TestShareAttemptStartSyncsDirBeforeFetch` (recording trace reaches fetch before dir-sync ⇒ RED) | CLOSED — closeout replay |
| 51d | Publish `state:succeeded` before the commit link's `syncDir(commits)` returns | `TestShareAttemptSuccessFollowsDurableCommit` (reboot observes success without matching durable commit ⇒ RED) | CLOSED — closeout replay |
| 51e | Replace owner-CAS terminal transition with blind clear/overwrite | `TestStaleCompleterCannotMaskSuccessorAttempt` (A completes over B active; killed B is masked as fresh ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 52a | Run sweep only after successful pull, not before admission | `TestGCPreflightUnblocksAfterReadersClose` (blocked pull can never trigger reclaim ⇒ RED) | CLOSED — closeout replay |
| 52b | Remove only the `mora share gc [<name>]` dispatcher/entrypoint | `TestManualShareGCDoesNotRequireSuccessfulPull` (out-of-band reclaim unavailable while pulls are blocked ⇒ RED) | CLOSED — closeout replay |
| 53a | Reset accounting per subscription instead of summing all subscriptions | `TestShareStorageLimitIsWholeProduct` (two individually-under-limit subs cross the 15 GiB default ⇒ RED) | CLOSED — closeout replay |
| 53b | Replace `productStorageBytes`'s root list `{VaultDir,ConfigDir,DataDir,StateDir}` with `{DataDir}` | `TestShareStorageLimitIncludesAllProductRoots` (one named table test puts bytes in each omitted root; the share admits past 15 GiB ⇒ RED) | CLOSED — closeout replay |
| 53c | Skip `subs/<name>/repo/` entries in `productStorageBytes` | `TestShareStorageLimitIncludesRepo` (the normal-clone pack makes actual total cross the limit but admission omits it ⇒ RED) | CLOSED — closeout replay |
| 53d | Skip `subs/<name>/fetch-*` entries in `productStorageBytes` | `TestShareStorageLimitIncludesFetchStaging` (bucket staging makes actual total cross the limit but admission omits it ⇒ RED) | CLOSED — closeout replay |
| 53e | Admit on current bytes without reserving/capping the in-flight corpus + SQLite build | `TestShareStorageLimitReservesInflightBuild` (commit makes actual total exceed its admitted limit ⇒ RED) | CLOSED — closeout replay |
| 53f | Drop the global `DataDir/share/storage.lock` around aggregate admission/build | `TestConcurrentSubscriptionsShareOneStorageReservation` (A and B both pass on the same free bytes and exceed the limit ⇒ RED) | CLOSED — closeout replay |
| 53g | Leave doctor on vault-only `vaultStorageBytes` while admission uses whole-product accounting | `TestDoctorStorageUsesWholeProductAccountant` (share-only bytes cross 15 GiB but doctor still reports under ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 53h | Preserve `dirBytes`'s best-effort “walk/stat error contributes zero” behavior in admission | `TestShareStorageAccountingFailsClosedOnUnreadablePath` (an injected accounting error undercounts and admits, or doctor reports under instead of critical unknown ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 54a | Hard-code the r6 4 GiB ceiling for every legal input | `TestLegalLargeShareIsNotHardCappedAt4GiB` (protocol-legal 50k×4 MiB share remains inadmissible even with a higher configured whole-product limit ⇒ RED) | CLOSED — closeout replay |
| 54b | Remove the durable `mora share storage-limit <bytes>` user-decision path from an oversubscription refusal | `TestLegalLargeShareHasExplicitOversubscriptionPath` (same legal input is refused forever with no actionable opt-in ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |
| 54c | Swallow/continue after `ENOSPC` during corpus or capped SQLite growth | `TestDiskFullLeavesGenerationUncommitted` (partial generation gains a commit or cannot be GC-reaped ⇒ RED) | CLOSED — closeout replay |
| 55a | Point the generation **writer** at the resolved published `index.db` instead of private `gen-<run_id>/index.db` | `TestShareGenerationBuilderNeverWritesPublishedIndex` (reader-held published file contends or its digest changes ⇒ RED) | CLOSED — closeout replay |
| 55b | Drop `mode=ro` from the resolved generation **reader** DSN | `TestPublishedShareIndexReaderIsReadOnly` (DDL through returned handle succeeds and mutates published bytes ⇒ RED) | CLOSED — [PR #162](https://github.com/pyranthus-hq/mora/pull/162) |

## HEALTH-08 signed-upgrade quarantine

Mora still ships unsigned/unnotarized; `install.sh` ad-hoc-signs at install
time. The “preserve macOS permission identity across a signed binary swap”
sub-clause is therefore not claimable yet. It is explicitly quarantined in
[#167](https://github.com/pyranthus-hq/mora/issues/167), with review/expiry
**2026-10-15**. FDA loud-failure and upgrade-preserves-state are closed; signing
identity is not falsely certified.

## Closure boundary

This closes Gate 2's technical 93-row mutation matrix. The ordered roadmap gate
[#140](https://github.com/pyranthus-hq/mora/issues/140) still depends on the
human-audited Gate 1 result in
[#139](https://github.com/pyranthus-hq/mora/issues/139); this artifact does not
bypass that dependency.
