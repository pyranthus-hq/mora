# Sync & Freshness

Mora tracks and shows the age of each source under an **honest-snapshot** model.
Sync gets the full window again. It is not a live stream. Mora treats stale
data as a product invariant and always shows it.

## Files

| File | Lines | Responsibility |
|---|---|---|
| `internal/memory/status.go` | 62 | Canonical `SyncStatus` struct + `LoadStatus` / `SaveStatus` (atomic, per-source JSON). Cursor fields live here. The M-3 last-attempt health fields (`LastAttemptAt`/`LastSuccessAt`) were appended in Phase 12. |
| `internal/memory/ingest.go` | 94 | The shared resumable `Ingest` loop: paginates a `Fetcher` from the checkpoint cursor, counts items/errors, clears the checkpoint on clean completion, and (Phase 12 M-3) stamps `LastAttemptAt`/`LastSuccessAt` + resets the error tally on a clean recovery. |
| `internal/memory/types.go` | 64 | `Page.NextCursor`, `FetchWindow`, the `Fetcher` seam — the page-cursor shapes `Ingest` drives. |
| `internal/google/status.go` | 12 | Re-exports `SyncStatus`/`LoadStatus`/`SaveStatus` as `google.*` aliases (no cycle into `internal/mora`). |
| `internal/google/ingest.go` | 12 | Re-exports `Ingest`/`IngestParams`/`IngestResult` as `google.*` aliases. |
| `internal/mora/mora.go` | 3791 | The wiring: `cmdSync` (`sync status` / `sync google` / `sync filesystem` / `sync imessage` / `sync applecalendar`), connector ingest (load→ingest→save status), per-source status paths, `sourceFreshness`, connector windows, and the enabled-source backfill helpers (all now in `ingest.go`) plus the sync-first seams, `cmdPulse`'s `--sync` flag, and scheduled periodic re-pull. |
| `internal/mora/brief.go` | 261 | The Phase-12 **`brief/` watermark store** — a SEPARATE per-instance state from `sync/` (`briefSnapshot`, `loadBriefSnapshot`/`saveBriefSnapshot`, `acquireBriefLock`). Decoupled from `SyncStatus` *on purpose* (see below). Consumed by the digest, never by freshness. Detailed in [synthesis-think-digest](./07-synthesis-think-digest.md). |
| `internal/mora/connectors.go` | 132 | `ingestingConnectors` (enabled∩ingesting enumeration — the set the digest's three-state classifier drives from) + `sourceInstanceKey` (the watermark/grouping key seam) + `connectorDisplay`. |
| `internal/mora/sources_lock.go` | — | The **sources.json read-modify-write lease (P3)**: `mutateSources` (acquire → reload-inside-lock → mutate → `saveSources` → release) and `acquireSourcesLock` (`<ConfigDir>/sources.json.lock`, TTL-reaped). Reuses the crash-safe file-lock primitives from `loop.go` (`publishLockFile`/`reapStaleLockTTL`/`breakLock`/`loopLockReleaser`). The single boundary every registry mutation goes through — see the invariant below. |
| `internal/mora/gitsync.go` | 234 | `mora sync git` — opt-in **off-device backup** of the vault to a private git remote (issue #6). `syncGit` (one-way push-only orchestration), `configureRemote` (`--github`/`--remote`/existing-origin precedence), `commitIdentityArgs` (fresh-machine identity fallback), `redactCredentials` (strips PAT userinfo from fail-loud git output), `realExec` (the injectable git/gh exec seam). |
| `internal/mora/digest.go` | 758 | `buildDigest` embeds `sourceFreshness(cfg)` into the digest `Freshness` map and reads per-instance `SyncStatus` via `loadConnectorSyncStatus`/`syncStatusPathFor` for the three-state health labels; `renderDigest` prints the `Fresh as of:` line. |
| `internal/mora/health.go` | — | Gate 1 (HEALTH-01..05): `sourceHealth`/`sourceHealthAll` (the never/failed/stale/fresh classification, stricter thresholds than the digest three-state), `healthBanner`/`healthBannerFromSources` (the red one-line alarm), `stampSyncAttemptFailure` (closes the pre-Ingest stamping gap). |
| `internal/mora/doctor.go` | — | `cmdDoctor`'s `--pulse` flag (`cmdDoctorPulse`) — freshness-only check, exits 2 + posts a toast when unhealthy — plus the `source_fresh:<key>` checks appended to the normal `doctor`/`doctor --json`/`doctor --strict` report. |

> Canonical source only: `./internal/`, `./cmd/`, repo-root config. The `SyncStatus` type and the `Ingest` loop physically live in `internal/memory`; `internal/google` re-exports thin aliases so connector call-sites read unchanged (`internal/google/status.go:7`, `internal/google/ingest.go:8-12`).

## The honest-snapshot model

**Sync is NOT live and NOT incremental.** Every sync is a full re-pull of the configured window from page one. The product never claims "you are seeing everything as of right now". It claims "this is a snapshot, and here is exactly how old it is." Freshness *is* the value proposition, so the staleness is measured and surfaced rather than hidden.

Concretely, each `Ingest` run:

1. Seeds the page cursor from the stored `Checkpoint` (normally `""` after a clean prior run) — `internal/memory/ingest.go:40`.
2. Pages the `Fetcher` over the `FetchWindow` (Gmail default last 90 days, Calendar −6/+3 months, iMessage last 90 days — `internal/mora/ingest.go`).
3. **Clears** the `Checkpoint` on clean completion (`internal/memory/ingest.go:66`), so the next run restarts from page one over a window recomputed against `time.Now()`.

Two consequences worth internalizing before you touch this:

- **It is upsert-only, not a reconciliation.** `writeMappedMemory` skips unchanged content by content-hash and preserves `created_at` (`internal/mora/ingest.go`). It never deletes a local memory just because the remote object vanished from the new window. So "snapshot" means "everything currently in-window, merged onto what was there before," not an exact mirror of the provider.
- **Re-pulling is cheap by design.** The lean default windows exist so a periodic full re-pull stays affordable (`windowForSource` comment, `internal/mora/ingest.go`). A larger lookback is opt-in (`--since-days N`, persisted on the source. Or `mora reingest --full` which bumps `SinceDays` to `reingestFullDays = 36500` ≈ all-time *for that run only*, `internal/mora/ingest.go`).

Manual refresh routing is explicit: `mora sync` requires a named source and an
unknown name is an error. `mora sync filesystem` walks only enabled filesystem
sources, records each source's real status, and rebuilds the derived index once
after all walks. This prevents a missing argument or typo from accidentally
entering the networked Google backfill path.

"Periodic" freshness comes from the OS scheduler, not a daemon: `mora schedule install ingest-hourly` writes a launchd plist on macOS and bootstraps it into the `gui/<uid>` domain right away (writing a plist alone does NOT load it — the job would otherwise stay inert until the next login, which is exactly how the daily brief once died silently for a week. A failed bootstrap is a loud non-zero exit that prints the manual `launchctl bootstrap` command), creates a Task Scheduler entry through `schtasks` on Windows, and prints a cron line on Linux/WSL. When the installing executable lives inside signed `Mora.app`, the macOS plist runs `/usr/bin/open -n -W -a <Mora.app> --args ...`: LaunchServices applies the eye-icon app's TCC/FDA identity, while `-W` keeps launchd attached to the launched CLI lifetime. `open -W` waits but does **not** propagate the launched CLI's exit status, so launchd's process status is not the success receipt; the command-owned `SyncStatus` and producer ledgers record the genuine outcome and drive `doctor`/health. Directly execing `Contents/MacOS/mora` is forbidden for the app lane because macOS evaluates it as the generic executable and protected Messages/Calendar reads can fail even while Mora.app is enabled in Full Disk Access. Standalone installs retain direct execution. The shared job map still names `ingest-hourly` as `mora ingest run --all`; Windows task names use `Mora\<job>` so uninstallers can remove every Mora job under `\Mora\`. Either way, the freshness clock keeps ticking and `sync status` keeps telling the truth between runs. The Phase-12 watermark-commit job `pulse-daily` is the one exception that deliberately DROPS `RunAtLoad` (`scheduleRunAtLoad`, `schedule.go`), see the gotcha below.

Two scheduled-run hardenings (2026-06-10), both born from a live failure where the hourly job silently broke while terminal syncs worked:

- **The plist snapshots `MORA_GOOGLE_CREDENTIALS`.** launchd jobs do NOT inherit the user's shell profile, so a BYO-creds setup hit the embedded `DEV_PLACEHOLDER` OAuth client on every scheduled Google sync and the vault went stale with no visible error. `schedulePlistFor` now writes the var (a path, not a secret) into an `EnvironmentVariables` dict at install time, and omits the dict when the var is unset (`mora_schedule_env_test.go`).
- **Ingest activity is mark-before-visible and anonymous.** `ingestSource` durably creates a content-free operation receipt and run-id-bound journal header before provider dispatch. Long work, including the wait for sibling sources in a batch, heartbeats every five minutes. A clean fetch remains `awaiting_rebuild` until the covering rebuild commits and retires its journal; source/account identity never enters the health projection.
- **`ingest run --all` is warn-and-continue, not abort-on-first-error.** A single broken connector (e.g. iMessage without Full Disk Access under launchd — TCC grants are per-binary, so a terminal grant does not cover the launchd-spawned process) used to kill the whole run: later sources never synced and the final `rebuildIndex` never ran, leaving even successfully-ingested sources invisible to search. `cmdIngest` now mirrors `backfillEnabledGoogle`: per-source warn, keep going, always rebuild, aggregate error at the end (honest non-zero exit). The named `--source` path still aborts — there, the failure IS the result (`mora_ingest_all_test.go`).

Agent-facing freshness rides on the query surfaces themselves: `search_memory` and `context_memory` both return the `sourceFreshness` per-source `last_synced` map alongside results (see [mcp-server](./06-mcp-server.md)), so an agent can qualify every answer with its data age.

Two connect-path guards (2026-06-10, from live multi-account testing):

- **Same-account re-auth exits gracefully.** `connect google` fetches the signed-in address (`AuthedEmail`, Gmail profile), stamps it onto the account's source rows (`Source.Email`), and `googleAccountForEmail` refuses to connect a mailbox that is already registered under a different label — proceeding would double-ingest every thread under distinct `@account` StableIDs. The just-written duplicate token file is removed on exit.
- **Skip-if-fresh.** The connect backfill skips a source whose `LastSuccessAt` is inside `connectFreshWindow` (1h) — a re-auth minutes after a clean backfill must not re-pull the whole window. `mora sync google` remains the explicit force path and never skips. (The full-window re-pull itself is the honest-snapshot design; TRUE incremental — Gmail `history.list` / Calendar `syncToken`, the reserved `GmailHistory`/`CalSyncToken` cursor fields — remains the deferred next step.)

The digest gained a preview-only **source filter** (`digestSourceMatches`: exact instance key, or provider family — `"gmail"` spans `gmail` + `gmail:work`): MCP `digest {source, since_hours}` / `pulse --source`. It exists because section rank order let calendar sections eat the whole byte budget before an "iMessages this week" ask ever rendered. It is rejected in combination with `--advance` (a filtered advance would mark unseen sources' items read).

## The scheduled brief is SYNC-FIRST (Phase 13)

The honest-snapshot rule extends to the scheduled brief. Before Phase 13 the `pulse-daily` job built its digest off whatever the last `ingest-hourly` run happened to leave on disk — a brief could silently reflect hours-old data. Phase 13 makes the scheduled job **sync-first**: refresh the enabled sources, THEN build the digest, so the brief reflects current data.

The wiring is `cmdPulse`'s additive `--sync` flag (default OFF. The scheduled job is the sole caller that opts in). In **delta mode only** (`*sinceHours <= 0` — an explicit `--since-hours` ad-hoc window is intentionally NOT synced), when `--sync` is set, `cmdPulse` runs the SAME backfills `mora sync` runs, in `cmdSync`'s order — `backfillGoogleFn` then `backfillIMessageFn` (`internal/mora/mora.go:771-776`) — BEFORE `buildDigest` (`mora.go:779`). Those are the package-level seams `backfillGoogleFn`/`backfillIMessageFn` (`internal/mora/mora.go:1329-1332`), defaulting to `backfillEnabledGoogle`/`backfillEnabledIMessage`. Tests swap them (`t.Cleanup`-restore, never `t.Parallel`) to assert sync-first ordering and honest pass-through WITHOUT real network.

### A sync failure surfaces through the EXISTING three-state — it is NOT swallowed and NEVER aborts the brief

This is the load-bearing nuance that keeps sync-first honest. The two backfill errors are **captured + logged but NEVER returned** (`internal/mora/mora.go:771-776`): each prints a `warn: <source> sync incomplete; the brief reflects last good data (run mora sync status)` line, and `cmdPulse` continues to build and print the brief. This is NOT swallowing the error — the error is surfaced through the existing machinery, twice over:

- The backfill has already **written the failure into `SyncStatus`** (`LastError`/`ErrorCount`, and `LastAttemptAt` advances while `LastSuccessAt` does not — the M-3 model above). The digest's three-state classifier reads exactly those fields, so the failed source renders as **`unavailable (sync error)`** (or `stale`) right in the brief via `classifyState` ([synthesis-think-digest](./07-synthesis-think-digest.md)). The reader sees the brief AND sees that one source is behind — they are never told a stale source is fresh (T-13-09).
- The warn line names the source and points at `mora sync status` for the forensic detail.

Aborting the whole brief on a single source's sync error would defeat the point: **a partial honest brief beats no brief** (T-13-12). So a Gmail auth-expiry on the 7am cron still produces a brief — with iMessage current and Gmail honestly flagged `unavailable` — rather than a silent no-show. This is the same "never swallow a sync error — surface it" invariant the rest of this doc enforces, now extended to the scheduled job: the error changes how the source is *labelled*, not whether the brief *exists*.

### The durable `pulse-daily` wrapper

The OS scheduler does not invoke the non-idempotent pulse directly. Its stable command enters the durable loop wrapper (`scheduleCommands`, `internal/mora/mora.go`):

```
scheduleCommands["pulse-daily"] = "schedule run pulse-daily"
```

`runScheduledPulseDaily` opens `loop begin daily-brief`, treats an already-succeeded day as a successful no-op, and only then calls `cmdPulse` with `--write --digest --advance --sync --brief-file --notify --loop daily-brief --loop-run <run_id>`. The pulse heartbeats the active lease, then holds that loop's persistent OS guard across the complete `advanceBrief` build/persist/watermark transaction. A holder suspended after validation cannot be TTL-reaped midway through its effect, while a holder reaped before acquiring the guard fails validation and never enters it. The run's authorized period must equal the same injected UTC `now` used for the artifact and watermarks, so a midnight boundary cannot spend one run in two periods. Before entering the transaction Mora durably records `effect_started_at`. The artifact and every watermark then land through synced file+directory writes. Only after those barriers does Mora durably add `effect_committed_at`, and later heartbeat/done rewrites preserve that durability. A start without a commit is explicitly **uncertain**: the process may have died after writing the artifact or only some per-source watermarks, so Mora refuses an automatic same-period retry instead of overwriting the saved brief or consuming another delta. This pre-intent plus success checkpoint closes both sides of the process-kill window even while the human-facing run remains `running`/`failed`. The wrapper records exactly one `loop done` success/failure when it remains alive. `--sync` is the sync-first refresh above; `--brief-file` persists the dated vault artifact and `--notify` posts the macOS toast (both in [synthesis-think-digest](./07-synthesis-think-digest.md)). Critically, **`--advance` remains the SOLE watermark-commit surface** (D-02): sync-first refreshes the *snapshot* (`sync/`), `--advance` advances the *delta watermark* (`brief/`), and `--brief-file` writes the *artifact* (`briefs/`) — three independent stores, only `--advance` commits the delta. The exact command line written by pre-wrapper Mora versions (`pulse --write --digest --advance --sync --brief-file --notify`) is recognized by the upgraded binary and routed through this same durable wrapper, so an existing launchd/cron/Task Scheduler entry is protected before reinstall. The `RunAtLoad` drop still applies as defense in depth. Duplicate fires are now rejected by durable same-period state as well.

## `SyncStatus`: per-source persisted state

One JSON file per source under `<StateDir>/sync/`:

- Google sources: `google-<source>.json` (`googleStatusPath`, `internal/mora/ingest.go`).
- iMessage sources: `imessage-<source>.json` (`imessageStatusPath`, `internal/mora/ingest.go`).
- Filesystem sources: `filesystem-<source>.json`. The filesystem connector has no fetcher `Status` of its own, so `ingestFilesystem` (`internal/mora/ingest.go`) writes the `SyncStatus` itself after the walk — `LastSuccessAt`/`LastAttemptAt` on a clean walk, `ErrorCount`/`LastError` otherwise. A missing root or unreadable directory is a failed walk: it returns through the backfill aggregator, advances only `LastAttemptAt`, and preserves the prior `LastSynced`/`LastSuccessAt`. Later configured filesystem sources still run. Without this status path the digest's `classifyState` found no status (`syncStatusPathFor` returned `""`) and mislabelled an ingested filesystem source as **`unavailable (sync error)`**. The `filesystem` case in `syncStatusPathFor` + this write fix it so the Files section reads its real state.

The struct (`internal/memory/status.go:13-32`):

```go
type SyncStatus struct {
    Source       string `json:"source"`
    LastSynced   string `json:"last_synced"`          // RFC3339, the freshness clock
    ItemCount    int    `json:"item_count"`
    ErrorCount   int    `json:"error_count"`
    LastError    string `json:"last_error,omitempty"`
    Checkpoint   string `json:"checkpoint,omitempty"`    // in-progress page token (resume)
    GmailHistory string `json:"gmail_history,omitempty"` // future incremental — UNUSED in v1
    CalSyncToken string `json:"cal_sync_token,omitempty"`// future incremental — UNUSED in v1

    // Last-attempt health (M-3, Phase 12). Appended so prior on-disk JSON
    // round-trips with no migration (LoadStatus zero-values them).
    LastAttemptAt string `json:"last_attempt_at,omitempty"` // every attempt (success OR fail)
    LastSuccessAt string `json:"last_success_at,omitempty"` // clean finish only
}
```

`SaveStatus` is atomic (write `path.tmp`, then `os.Rename`) with `0600` perms and `0700` parent dir (`internal/memory/status.go:49-62`). `LoadStatus` treats a missing file as a fresh zero-value `SyncStatus`, **not** an error (`internal/memory/status.go:36-37`) — a never-synced source reads cleanly as empty rather than crashing the status command.

### The M-3 last-attempt health model (Phase 12)

Before Phase 12, `ErrorCount`/`LastError` were a **sticky lifetime tally**: a source that errored once read "broken" forever, even after a later clean sync. That was harmless while nothing *derived behavior* from it — but Phase 12's digest derives a per-source "unavailable — sync error" three-state from exactly these fields (`classifyState`, [synthesis-think-digest](./07-synthesis-think-digest.md)), so a stale lifetime tally would invert SC#3: a recovered source would read unavailable for the rest of time. M-3 fixes this by modeling health as the **last attempt's outcome**:

- **Two new timestamps.** `LastAttemptAt` is stamped on *every* attempt — both a clean finish (`internal/memory/ingest.go:80-82`) and a page-fetch failure (`internal/memory/ingest.go:54`). `LastSuccessAt` is stamped **only on a clean finish** (`internal/memory/ingest.go:83`). Keeping them distinct lets a consumer tell "never succeeded" (`LastSuccessAt==""`) from "succeeded but stale" — the exact distinction the digest's `unavailable` vs `stale` branch needs.
- **Clean recovery resets the error state.** On a clean finish the loop resets `ErrorCount = 0` / `LastError = ""` (`internal/memory/ingest.go:89-92`), so a source that errored on a *prior* run and then completes a clean sync stops reading "unavailable."
- **But the reset is CONDITIONAL — gated on an `errorsBefore` snapshot** taken at run start (`internal/memory/ingest.go:45`). A run that itself accumulates per-item *write* errors (which `continue` and then fall through to clean completion) is **not** a clean attempt and KEEPS its errors (`internal/memory/ingest.go:89`). This preserves the package's existing per-item-write-error contract: only a run that introduced no new errors clears prior error state. "Health is the last attempt's outcome, not a 'paging finished' signal."

So the precise reading of the fields after Phase 12: `LastSuccessAt==""` ⇒ never synced cleanly (→ unavailable); `ErrorCount>0`/`LastError!=""` ⇒ the *last attempt* failed (→ unavailable). Both clean but `LastSuccessAt` older than 48h ⇒ stale. Otherwise healthy.

### What `LastSynced` means precisely

`LastSynced` is set to `time.Now().UTC()` at exactly **two** moments, both inside the connector mapping/write path:

- Per item, on `MappedMemory.LastSynced` at write time (`internal/memory/ingest.go:61`) — this is what each memory file's frontmatter records.
- On `SyncStatus.LastSynced` only on **clean completion** of the whole run (`internal/memory/ingest.go:81`), right after the checkpoint is cleared. As of Phase 12 the same instant is also stamped on `LastAttemptAt`/`LastSuccessAt` (`internal/memory/ingest.go:80-83`) — see the M-3 health model above.

So a source's `LastSynced` advancing is itself a signal: it means the last run reached the end of pagination without a fatal fetch error. A run that aborts mid-pagination saves a status whose `LastSynced` is unchanged but whose `ErrorCount` ticked up, whose `LastAttemptAt` advanced (but NOT `LastSuccessAt`), and whose `Checkpoint` is non-empty (see below).

### The cursor fields: present, but unused in v1

Three cursor-shaped fields exist. Only one does anything today:

- **`Checkpoint`** — the *only* live cursor. It is the provider page token for resuming an interrupted backfill (next section).
- **`GmailHistory`** / **`CalSyncToken`** — declared for a *future* incremental-sync upgrade (Gmail `historyId`, Calendar `syncToken`). They are **read and written nowhere outside the struct definition and the serialization round-trip test** (verified: the only references are `internal/memory/status.go:20-21` and `internal/memory/status_test.go`). Do not assume they carry state. The struct comment says exactly this (`internal/memory/status.go:11-12`): *"Cursors are stored for a future incremental upgrade but are not the v1 refresh path."*

This is the deliberate v1 honesty: the schema reserves room for incremental sync, but v1 ships full re-pull only. If you wire incremental, those two fields are your slots — and you must add pruning logic, because the current upsert-only path has no delete-on-vanish.

## The checkpoint resume mechanism

`Ingest` is a single paginated loop (`internal/memory/ingest.go:32-69`):

```mermaid
flowchart TD
    A["ingestGoogle / ingestIMessage<br/>LoadStatus(statusPath)"] --> B["cursor = Status.Checkpoint<br/>(usually empty)"]
    B --> C{"FetchPage(kind, window, cursor)"}
    C -->|fetch error| E["ErrorCount++ · LastError = err<br/>LastAttemptAt = now (NOT LastSuccessAt)<br/>Checkpoint = cursor (PRESERVE)<br/>return err"]
    C -->|ok| F["for each Item:<br/>Map → set LastSynced → Write"]
    F --> G{"Write error?"}
    G -->|yes| H["ErrorCount++ · LastError<br/>continue (NON-FATAL)"]
    G -->|no| I["ItemCount++"]
    H --> J{"NextCursor == ''?"}
    I --> J
    J -->|no| K["cursor = NextCursor<br/>Checkpoint = cursor (advance)"]
    K --> C
    J -->|yes| L["Checkpoint = '' (CLEAR)<br/>LastSynced = LastAttemptAt = LastSuccessAt = now<br/>if no NEW errors this run: ErrorCount=0, LastError='' (M-3)"]
    E --> M["SaveStatus(statusPath, res.Status)"]
    L --> M
    M --> N{"ingErr != nil?"}
    N -->|yes| O["surface 'sync incomplete (resumable)'<br/>+ return error up the stack"]
    N -->|no| P["return ItemCount, nil"]
```

Key behaviors:

- **Fetch error ⇒ stop, preserve cursor.** A `FetchPage` error increments `ErrorCount`, records `LastError`, stamps `LastAttemptAt` (but not `LastSuccessAt`), and sets `Checkpoint = cursor` (the page that failed) before returning the error (`internal/memory/ingest.go:48-57`). The next run resumes that exact page instead of restarting.
- **Write error ⇒ count, continue.** A per-item `Write` failure is non-fatal: it bumps `ErrorCount`/`LastError` and moves on (`internal/memory/ingest.go:62-66`). One bad item never aborts a backfill — and it keeps the run from being a "clean" attempt, so the M-3 reset (below) leaves those errors in place.
- **Clean finish ⇒ clear cursor + stamp success + conditionally reset errors.** When `NextCursor == ""` the loop clears `Checkpoint`, stamps `LastSynced`/`LastAttemptAt`/`LastSuccessAt` to one shared instant, and — only if this run added no new errors (`ErrorCount == errorsBefore`) — resets `ErrorCount`/`LastError` (`internal/memory/ingest.go:75-93`). This is why a normal next run re-pulls from page one AND why a recovered source stops reading "unavailable" (M-3, above).
- **Status is persisted once, after `Ingest` returns** — `SaveStatus(statusPath, res.Status)` in `ingestGoogle`/`internal/mora/ingest.go` and `ingestIMessage`/`ingest.go`. The checkpoint advances *in memory* per page, but only the final value is written to disk. **Consequence:** the checkpoint resumes a *returned fetch error* (the function returned normally with an error and saved), not a hard process kill mid-loop. A SIGKILL between pages loses the in-memory checkpoint and the next run restarts the window — acceptable because upserts are idempotent (content-hash skip).

### Source sync lifecycle

```mermaid
stateDiagram-v2
    [*] --> NeverSynced: no status file<br/>(LastSuccessAt='')
    NeverSynced --> Backfilling: mora sync / connect / scheduled ingest
    Backfilling --> Resumable: FetchPage error<br/>(Checkpoint preserved, ErrorCount++,<br/>LastAttemptAt stamped, LastSuccessAt unchanged)
    Resumable --> Backfilling: next run resumes at Checkpoint
    Backfilling --> Fresh: pagination complete<br/>(Checkpoint='', LastSynced=LastSuccessAt=now,<br/>M-3: ErrorCount reset IFF no new errors this run)
    Resumable --> Fresh: clean recovery run<br/>(M-3 reset → digest reads healthy, not unavailable)
    Fresh --> Stale: >48h since LastSuccessAt<br/>(reported, not enforced)
    Stale --> Backfilling: re-pull (manual or launchd)
    Fresh --> Backfilling: re-pull (full window, page one)
    Resumable --> [*]
    Fresh --> [*]: surfaced by `mora sync status`<br/>+ the digest three-state (D-03)
```

The three named states map onto the digest's three-state per-source labels ([synthesis-think-digest](./07-synthesis-think-digest.md)): `NeverSynced`/`Resumable` (a recorded error) ⇒ **unavailable**, `Fresh` past 48h ⇒ **stale**, `Fresh` ⇒ **delta** or **no changes**. M-3 is what makes the `Resumable → Fresh` edge actually clear the unavailable reading.

## What `mora sync status` surfaces

`cmdSync` with `status` reads every file in `<StateDir>/sync/`, loads each as a `SyncStatus`, and prints one line per source (`internal/mora/ingest.go`):

```
<source>: <ItemCount> items, <ErrorCount> errors, last_synced <LastSynced> [(STALE)]
```

- **STALE threshold = 48h.** If `time.Since(LastSynced) > 48*time.Hour`, the line is tagged `(STALE)` in the "bad" style (`internal/mora/ingest.go`). This is a *report*, not an enforcement — nothing blocks or auto-syncs. The user (or the scheduler) decides. (Note: `sync status` keys staleness off `LastSynced`. The digest's three-state keys off `LastSuccessAt` — the same instant on a clean run, but `LastSuccessAt` is the field that survives an aborted attempt without advancing.)
- **Error count is highlighted** when `> 0` (`internal/mora/ingest.go`), but **`LastError` is not printed** by `sync status` — it's persisted in the JSON for forensics, but the human line shows only the count. The actual error text reaches the user through the *ingest* path's warnings instead (below).
- **`status` never fetches.** The help text is explicit: `status` shows freshness "(no fetch)" — `internal/mora/ingest.go`. It is a pure read of persisted state.
- An empty `sync/` dir prints `no sources synced yet` (`internal/mora/ingest.go`).

The styling (`newStyler`/`sty.accent`/`sty.bad`/`sty.dim`) is the human-facing lipgloss layer. On non-TTY/`--json` it is byte-clean (see [cli-and-ux](./08-cli-and-ux.md)).

## Freshness as a first-class output of MCP tools

Freshness is not confined to the `sync status` command — it rides along inside agent-facing tool results so an MCP agent always knows how old its context is:

- `context_memory` returns `{"context": ..., "freshness": sourceFreshness(cfg)}` (`internal/mora/mora.go:3006`).
- `digest` carries `d.Freshness` (populated by `buildDigest` → `sourceFreshness(cfg)`, `internal/mora/digest.go:253`) rendered as a `Fresh as of: <src> <ts> · …` header line (`internal/mora/digest.go:529-539`). The Phase-12 MCP `digest` payload also derives a richer `source_states` array (per-instance three-state + health) from the same data — see [synthesis-think-digest](./07-synthesis-think-digest.md).

`sourceFreshness` (`internal/mora/ingest.go`) is a parallel reader: it walks `<StateDir>/sync/`, loads each status, and builds a `map[source]LastSynced`.

### The Phase-12 `sourceFreshness` fix

`sourceFreshness` now **keys off the loaded `SyncStatus.Source` field**, not a filename-prefix strip (`internal/mora/ingest.go`). Two bugs this corrects, both load-bearing for the digest's three-state:

- **iMessage was mis-keyed.** The old reader stripped only the `google-` prefix from the filename, so `imessage-<name>.json` became the map key `imessage-<name>` instead of `imessage` — disagreeing with `cmdSync status`, which keys off `st.Source`. Keying off `st.Source` makes both readers agree (the filename stem is now only a fallback used when `st.Source` is empty, and even then strips both known prefixes — `internal/mora/ingest.go`).
- **Never-synced sources were silently dropped.** The old reader effectively only surfaced sources with a real timestamp. A present-but-never-cleanly-synced status (`LastSynced==""`) vanished from the map, hiding a broken source. It is now **INCLUDED with an empty value** (`internal/mora/ingest.go`) so a broken source can read "unavailable" downstream rather than disappearing (the SC#3 gap).

## Gate 1: the health alarm (HEALTH-01..05)

The digest's three-state (`classifyState`, above) computes the right freshness state, but pre-Gate-1 it was consumed **only by a digest section heading** — text nobody reads on a schedule. The incident that motivated this: a source can fail on every hourly attempt for days while `LastSuccessAt` just sits there, aging quietly, with no surface that actively alarms. Gate 1 (`internal/mora/health.go`) adds a SEPARATE, stricter freshness signal that feeds `mora doctor`, a red banner on every brief surface, and a schedulable pulse check — so a dead source can no longer go unnoticed for six days.

### `sourceHealth` — a second, stricter classification (not a replacement for the digest three-state)

`sourceHealthAll(cfg, now)` (`internal/mora/health.go`) walks the same enabled-source set `loadConnectorSyncStatus` does, and classifies each instance into one of four states — **`never` / `failed` / `stale` / `fresh`** — first-match-wins in that order:

1. `never` — no `LastSuccessAt` has ever been recorded.
2. `failed` — the last recorded attempt carries `LastError`/`ErrorCount>0`, even over an OLDER success (HEALTH-04: a fresh failure must read as a live problem, not merely "old").
3. `stale` — the last success is older than the type's threshold.
4. `fresh` — otherwise.

This deliberately does **not** unify with `classifyState`'s `unavailable`/`stale`/`no changes`/`baseline` states, and the thresholds deliberately DIFFER:

| | digest three-state (`classifyState`) | `sourceHealth` |
|---|---|---|
| Threshold | `digestStaleHours` = 48h for every type | 24h for `gmail`/`calendar`/`applecalendar`, 48h for `imessage`/`filesystem` |
| Consumed by | digest section headings (cosmetic) | `doctor`, `doctor --pulse`, the red banner |
| `never` vs `failed` | both collapse into `unavailable` | kept distinct — "no data point" reads differently from "actively erroring" |

**WHY two systems instead of one:** the digest heading is a background-informational label the reader skims. The health alarm is the PRODUCT-INVALID threshold — the point past which Mora is confidently answering from a corpus that stopped updating. Google connectors are polled hourly, so 24h already means several missed cycles. IMessage/filesystem are local, slower-moving stores where 48h is still honest. Forking the threshold silently (without a name for the new concept) would read as a bug; `sourceHealth` is the named, separate concept instead. Do not fork `digestStaleHours` itself — both call sites (`digest.go`, `health.go`) keep independent constants with a comment explaining why.

### Closing the pre-Ingest stamping gap

`LastAttemptAt`/`LastError` are stamped by `memory.Ingest` itself (`internal/memory/ingest.go:56-108`) — but only once it actually RUNS. OAuth config resolution, token load, and fetcher construction in `ingestGoogle`/`ingestIMessage`/`ingestAppleCal` (`internal/mora/ingest.go`) can all fail BEFORE `memory.Ingest` is ever called, leaving the on-disk `SyncStatus` completely untouched — `doctor` could see a source had gone old, but never learn WHY.

`ingestSource` (`internal/mora/ingest.go`) — the single dispatch chokepoint every caller (`backfillEnabledGoogle`, `cmdIngest`, `cmdReingest`, `applySetupSelection`) routes through — closes this: on any returned error it calls `stampSyncAttemptFailure` (`internal/mora/health.go`), which loads the current on-disk status and, **only if it doesn't already carry this exact error** (i.e. the inner path hasn't already stamped it), stamps `LastAttemptAt`/`LastError`/`ErrorCount++`. The compare-first guard matters: re-stamping unconditionally would risk clobbering a checkpoint/counter update that `persistSyncStatus` already wrote with a stale re-read for no benefit. A save failure here is warned, never returned — it must not mask the real ingest error the caller is already propagating.

### The red banner

`healthBannerFromSources([]sourceHealth) string` (`internal/mora/health.go`) is a PURE function: given the worst state present (`failed` > `never` > `stale`, ties broken by age descending), it renders ONE line —

```
🔴 MORA HEALTH: gmail — no successful sync for 52h (database or disk is full (13)). Run: mora doctor
```

— or `""` when every enabled source is fresh. `healthBanner(cfg, now)` is the cfg/now convenience wrapper. Render paths never call it directly. Instead, `sourceHealthAll(cfg, now)` is computed ONCE at build time and stored on `Digest.SourceHealth` (`digest.go`) and `MeetingBrief.SourceHealth` (`meetingbrief.go`), so every render function stays a pure function of the already-built struct — no `time.Now()` in a render path (the determinism invariant `TestMCPDigestEnvelopeOffByteIdentical` depends on).

Render points:

- **Daily brief** — `renderDigestHealthBanner(d)` runs right after `renderDigestHeader`, before `renderDigestFreshness` (`digest.go`). Its bytes are folded into `budgetDigestForMarkdown`'s reserved frame alongside header/freshness/tasks — a banner rendered outside that frame would let the final `truncateRunes` safety net silently clip an item the budgeter had already counted as a survivor (pinned by `TestDigestTightBudgetNeverEvictsASurvivor`).
- **Meeting brief** — `renderMeetingBrief` renders the banner FIRST, before the `Event == nil` early return — a broken source is worth surfacing even when there happens to be no upcoming meeting. MCP `meeting_prep` returns the `MeetingBrief` struct directly, so the health snapshot rides along with no extra wiring: a brief that renders confidently over a dead corpus is a WRONG brief, not an ops footnote.
- **MCP `digest`/`brief`** — `source_health` rides in `digestMCPPayload`'s always-included frame (alongside `source_states`) and in `DigestEnvelope`/`budgetEnvelopePayload`, so an agent reads the typed state without parsing Markdown.

### `mora doctor --pulse`

A new flag on the existing `doctor` command (`internal/mora/doctor.go`) that runs ONLY the freshness checks — none of `doctor`'s vault/index/token checks — and is meant to run on a schedule:

- All sources fresh → prints one OK line, exits 0.
- Any source unhealthy → prints the banner, posts a best-effort native macOS toast (`notifyHealthAlarm`, gated identically to the existing brief-toast: `shouldNotify(goos)`, best-effort, a failed/missing `osascript` never fails the check), and **exits 2** — a distinct code from `--strict`'s generic non-zero, so automation can tell "sick" from "broken."
- `--pulse --json` emits ONLY `{"sources": [...]}` — no banner text mixed into the JSON stream.
- `--pulse --strict` is a no-op combination — `--pulse` alone already exits 2.

Exit 2 requires the TYPED `exitCodeError{code: 2}` (`internal/mora/loop.go`, the same sentinel `mora loop begin` already uses) — an ordinary `error` maps to exit 1 in `cmd/mora/main.go`'s `ExitCodeFor` dispatch and could never produce 2.

Scheduling: `doctor-pulse` → `doctor --pulse` in `scheduleCommands` (`internal/mora/mora.go`), installed daily at 09:00 (after the 08:00 `pulse-daily` brief), snapshotting `MORA_GOOGLE_CREDENTIALS`/`MORA_CONFIG_DIR` exactly as `schedulePlistFor` already does for every other job (the same launchd-inherits-nothing lesson as `pulse-daily`, above).

### Fail-closed, not fail-open

Two properties worth calling out because they were explicitly tested (`internal/mora/incident_replay_test.go`), not just implied:

- **A corrupt `sources.json` alarms rather than reading as "no sources enabled."** `sourceHealthAll` returns a synthetic `{Key: "sources_config", State: "failed"}` entry when `loadSources` errors, rather than silently returning `[]sourceHealth{}` (which would read as healthy — the connectors just happen not to exist). Fail-closed: the config being unreadable is itself a health event.
- **An unwritable stamp still alarms.** `SaveStatus` writes `<path>.tmp` + rename (`internal/memory/status.go`), which bypasses the TARGET file's permissions — chmodding the status file itself does nothing. The alarm survives a genuinely unwritable STATUS DIRECTORY (disk full, permissions) because it only ever READS the last successfully recorded stamp and keys on its AGE — an unwritable stamp just keeps getting older, and the alarm fires anyway. `stampSyncAttemptFailure`'s own save failures are swallowed (warned, never returned) for the same reason: a write failure while trying to RECORD a failure must not additionally break the read-only alarm path.

## Off-device git backup (`mora sync git`) — opt-in egress

Issue #6. The vault is plaintext Markdown with no durable off-machine copy; `mora
sync git` adds a **one-way, push-only** backup to a **private git remote the user
controls** (`internal/mora/gitsync.go`). It is the *only* deliberate exception to
Mora's otherwise-zero-egress posture, so the design is opt-in and loud by construction:

- **Shells out to system `git` (and optional `gh`), not a vendored Go lib.** This
  honors the user's existing credential helper / SSH config / `gh` auth for free —
  a go-git impl would not. The single seam is `realExec(ctx, dir, name, args...)`
  (`execFunc`), faked in tests so the whole flow runs without subprocesses.
- **`--init` is idempotent and remote-agnostic.** Repo detection is `vaultRepoState`:
  `os.Lstat(vault/.git)` accepting ONLY a plain directory. A gitfile or symlink
  (worktree/submodule-style `gitdir:` indirection) is refused loudly — following it
  would stage the vault into a parent/unrelated repository and push to THAT repo's
  remote (`rev-parse` is avoided for the same reason: it walks up into a parent repo
  when the vault is nested). `--github` and `--remote` are mutually exclusive, a
  destination flag without `--init` is rejected (never silently ignored while pushing
  to whatever origin exists), and positional args are errors. In `configureRemote`:
  `--github` creates a PRIVATE repo via `gh repo create … --private --source --remote`
  but is skipped when origin already exists (re-init never orphans a duplicate repo);
  `--remote <URL>` adds/sets origin. An already-configured origin passes. Else **fail-loud**.
- **No `--force`, ever.** Push is plain `git push [-u] origin HEAD`. A non-fast-forward
  rejection means the remote diverged and is surfaced loudly — never silently
  overwritten. For a single-writer backup that should not happen. If it does, the
  user must know. (Same spirit as "never swallow a sync error" below.)
- **Credential redaction is a hard requirement.** `git` echoes the remote URL on a
  push failure. If the user embedded a PAT (`https://token@host`), that secret would
  land in the fail-loud error. `redactCredentials` strips HTTP(S) userinfo from both
  the args and git's output before it reaches the terminal/logs/returned error.
- **Tracked-sensitive hard stop + detached-HEAD refusal.** `.gitignore` shields only
  *untracked* files, so after `git add -A` a `git ls-files` guard hard-stops the sync
  if `index.db` / `*.db` / `tokens/` / `*.token` are TRACKED (a pre-existing vault
  repo, a user-edited ignore list) — remediation is printed (`git rm --cached`),
  nothing is pushed. A detached HEAD is refused *before any staging*: `git push
  origin HEAD` cannot update a branch from one, and the commit made first would be
  left dangling.
- **Defensive `.gitignore` + fresh-machine identity fallback.** `--init` writes a
  `.gitignore` (`index.db`, `*.db`, `.DS_Store`, `tokens/`) so the ~87MB rebuildable
  index and any stray secrets never leave — restore is `git clone` → `mora index
  rebuild`. `commitIdentityArgs` injects a fallback `-c user.name/email` ONLY for the
  field(s) the user has not configured, so the first commit succeeds on a clean
  machine without clobbering a real identity.
- **Scheduling reuses the launchd scaffolding.** `git-daily` → `sync git` in
  `scheduleCommands` (`internal/mora/mora.go:4073`), a 3am `StartCalendarInterval`
  in `launchdSchedule`. `mora doctor` discloses (a `warn` line) whenever `vault/.git`
  exists, qualifying the zero-egress claim honestly.

## Invariants & gotchas

- **Never swallow a sync error — surface it.** This is the hard rule and it has teeth in code. `Ingest` records `LastError` and returns the fetch error (`internal/memory/ingest.go:48-57`); `ingestGoogle`/`ingestIMessage` save status and **return** the error (`internal/mora/ingest.go`); `backfillEnabledGoogle` counts failures and returns `"%d source(s) failed to sync; data may be stale (run mora sync status)"`, and `cmdReingest` mirrors it. **WHY:** a silently-stale memory that the agent treats as current is worse than no memory. A swallowed error would let the snapshot lie about its own age.
- **Manual sync requires an explicit, known source.** Bare `mora sync` and unknown source names return an error before any backfill runs; `mora sync filesystem` selects enabled filesystem rows only and rebuilds the index after all walks. **WHY:** a typo must never silently trigger the Google network path, and newly refreshed local files must be searchable in the same successful command.
- **Sync-first `pulse-daily` surfaces a sync failure WITHOUT aborting the brief (Phase 13).** When `--sync` is set, `cmdPulse` runs `backfillGoogleFn`/`backfillIMessageFn` before `buildDigest` (`internal/mora/mora.go:771-776`) — but their errors are LOGGED as a warn line and NOT returned. The brief still builds and prints. This is not a contradiction of "never swallow": the backfill already wrote the failure into `SyncStatus`, where the digest's three-state renders the source `unavailable (sync error)`/`stale` (`classifyState`, [synthesis-think-digest](./07-synthesis-think-digest.md)). **WHY:** a partial honest brief beats no brief (T-13-09/T-13-12) — the error changes how the source is *labelled*, not whether the brief *exists*. Do NOT make a single source's sync error abort the whole scheduled brief, and do NOT add a parallel error channel — the existing `SyncStatus` three-state IS the surfacing path.
- **Health is the LAST attempt's outcome, not a lifetime tally (M-3).** A clean run resets `ErrorCount`/`LastError` IFF it added no new errors (`internal/memory/ingest.go:89-92`), and stamps `LastAttemptAt`/`LastSuccessAt`. **WHY:** the digest derives "unavailable — sync error" from these fields, so a sticky lifetime error tally would make a recovered source read broken forever (inverting SC#3). Conversely, do NOT reset unconditionally — a run with per-item write errors must keep them (the `errorsBefore` gate, `internal/memory/ingest.go:45`), or a write-failing backfill would falsely read healthy.
- **Auth-expiry gets a named cause, not a bare warning.** On `isGoogleAuthError`, `backfillEnabledGoogle` prints the specific fix (re-run `mora connect google`) and warns about the 7-day Testing-mode refresh-token trap. **WHY:** the most common real-world staleness cause is an expired token. A generic "resumable" warning would send the user chasing the wrong problem.
- **A clean run clears the checkpoint. Treat that as the resume contract.** `Checkpoint=""` on success (`internal/memory/ingest.go:75`) is what makes the next run a fresh full window. If you add incremental sync, do NOT repurpose `Checkpoint` for the incremental token — that is what `GmailHistory`/`CalSyncToken` are reserved for. **WHY:** conflating intra-run resume state with inter-run incremental state would make a mid-backfill crash silently skip data on the next run.
- **`GmailHistory` / `CalSyncToken` are dead-for-now. Don't read them as truth.** They round-trip through JSON but are never populated by v1 (`internal/memory/status.go:20-21`). **WHY:** an "obvious" optimization that reads them would read uninitialized empty strings and silently no-op or, worse, request an incremental delta that the rest of the pipeline can't apply (no prune path).
- **Sync is upsert-only. It does not prune vanished remote items.** `writeMappedMemory` only writes/updates by content-hash and preserves `created_at` (`internal/mora/ingest.go`). **WHY:** so a narrowed window or a deleted remote thread doesn't silently delete a local memory — but it also means "snapshot" ≠ "exact mirror." A future incremental must add explicit tombstoning. (Trashed/cancelled provider objects *do* arrive as tombstones via `Item.Deleted`. See [connectors-google](./04-connectors-google.md).) **Note (Phase 12):** the `created_at`-preserved-on-change behavior here is *exactly why* the digest delta keys off `content_hash`, not timestamps — a grown thread keeps its old `created_at` and only its hash moves (see [synthesis-think-digest](./07-synthesis-think-digest.md)).
- **`SinceDays` overrides for `reingest --full` are per-run copies, never persisted.** `cmdReingest` mutates a *copy* of the `Source` (`internal/mora/ingest.go`, comment "copy; not persisted"); `reingestFullDays = 36500` (`internal/mora/ingest.go`). **WHY:** a full all-time reingest is a one-shot. Persisting the 36500-day window would make every later scheduled re-pull enormous.
- **Status file naming is connector-prefixed. Both readers now key off `Source`.** `google-<name>.json` vs `imessage-<name>.json`. As of Phase 12 BOTH `cmdSync status` and `sourceFreshness` key off the in-file `SyncStatus.Source` (`internal/mora/ingest.go`), so they agree — the old `google-`-only filename-strip bug that mis-keyed iMessage is fixed. The digest resolves a connector's status via `loadConnectorSyncStatus`/`syncStatusPathFor` (`internal/mora/digest.go:447-487`), which reconstructs the `google-`/`imessage-` filename from `Source.Type`. **WHY:** any new connector adding a status path must set `SyncStatus.Source` so all three readers (status / freshness / digest health) resolve the same key.
- **The watermark store is SEPARATE from `sync/` — by design (Phase 12).** The per-instance delta watermark lives at `<StateDir>/brief/<key>.json` (`internal/mora/brief.go:80-82`), NOT in `sync/` and NOT as a `SyncStatus` field. **WHY:** `SyncStatus.LastSynced` advances on *every* sync regardless of whether content changed, so a watermark built on it would mark everything "seen" on the next re-pull and surface no delta. `sourceFreshness` must keep scanning only `sync/` and never read `brief/`. The two stores answer different questions: `sync/` = "how old is this snapshot," `brief/` = "what have I already shown you." (Store details: [synthesis-think-digest](./07-synthesis-think-digest.md).)
- **`LastSynced` advancing means "reached end of pagination."** It is stamped only at `internal/memory/ingest.go:81`, after the checkpoint clears (alongside `LastSuccessAt`). **WHY:** if you start writing `LastSynced` per-page, the 48h STALE check and the agent-facing freshness map would report a source as "fresh" even when its last run aborted halfway — defeating the honesty guarantee.
- **The health alarm is a SEPARATE, stricter system from the digest three-state — do not merge their thresholds.** `sourceHealth` (`internal/mora/health.go`) alarms at 24h (Google)/48h (local) and keeps `never` distinct from `failed`; `classifyState` (`digest.go`) still alarms its heading at a flat 48h and collapses both into `unavailable`. **WHY:** the digest heading is informational. The health alarm is the product-invalid threshold that drives `doctor --pulse`'s exit code and the red banner. Silently forking one from the other (without naming the new concept) would read as an inconsistency bug rather than a deliberate, tighter alarm.
- **`sourceHealthAll` fails CLOSED on a broken registry, never open.** A `loadSources` error returns a synthetic `failed` entry, not `[]sourceHealth{}` (`internal/mora/health.go`) — an empty slice would read as "no sources enabled," which is indistinguishable from healthy. **WHY:** this is the exact shape of silent failure Gate 1 exists to close. The one input that could make the alarm itself go quiet must not.
- **The sources.json registry serializes its read-modify-write behind a lease (P3).** Every `load → mutate → save` on the source registry goes through `mutateSources` / `acquireSourcesLock` (`internal/mora/sources_lock.go`), which holds a crash-safe file lease at `<ConfigDir>/sources.json.lock` around the WHOLE cycle and **reloads the registry inside the lease**. `saveSources` + `atomicWrite` already made each individual write collision-free (a unique temp per writer), but two processes — e.g. a manual `mora connect` racing the scheduled `ingest run --all` — each doing load→mutate→save could still lose an update: last `os.Rename` wins, silently dropping the other's enable bit / deny-list / persisted window. The lease closes that hole. **WHY reload-inside-lock:** loading before the lease reintroduces the race — the fix is that the second writer re-reads the first writer's *committed* state before it mutates, so no field is clobbered. The lease reuses the exact primitives proven for `mora loop` (`publishLockFile`'s `os.Link`-atomic publish + `reapStaleLockTTL`'s TTL reap in `loop.go`), so it is Windows-portable. A lease leaked by SIGKILL is force-reaped after `sourcesLockTTL` (30s). The brief commit lock uses the sibling crash-released kernel-guard model because its critical section may span the whole render transaction. The sources lease is scoped to load→mutate→save ONLY — `connectFilesystem` releases it *before* its ingest/rebuild, so a long backfill never holds it. **Adding a new registry writer:** route it through `mutateSources` (or `acquireSourcesLock` directly if you need custom load-error handling, as `connectFilesystem` does) — a bare `loadSources`→`saveSources` outside the lease reopens the lost-update hole.

## Related

- [data-model-and-storage](./01-data-model-and-storage.md) — `Memory.LastSynced` frontmatter, content-hash skip, `SafeFilename`, atomic writes.
- [connectors-google](./04-connectors-google.md) — `LiveFetcher.FetchPage`, the `FetchWindow`, tombstones, OAuth/refresh-token expiry.
- [connectors-imessage](./05-connectors-imessage.md) — the iMessage `Fetcher`, Full Disk Access gating, `imessage-` status path.
- [synthesis-think-digest](./07-synthesis-think-digest.md) — how `digest`/`context_memory` embed `sourceFreshness` into agent-facing results, the `brief/` watermark store, and the digest three-state that reads the M-3 health fields.
- [meeting-brief-assembly](./19-meeting-brief-assembly.md) — where `MeetingBrief.SourceHealth` is populated and the banner is rendered ahead of the cited sections.
- [cli-and-ux](./08-cli-and-ux.md) — `mora sync` subcommands, the lipgloss styler, byte-clean non-TTY output.
- [distribution-and-ops](./10-distribution-and-ops.md) — `mora schedule install` launchd jobs, the periodic `ingest run --all` re-pull, state-dir layout. Note the `pulse-daily` job enters through `schedule run pulse-daily` and drops `RunAtLoad` (`scheduleRunAtLoad`, `internal/mora/schedule.go`). The wrapper's daily loop gate makes duplicate same-day fires no-ops.

## Open questions / unverified

- **`sourceFreshness` iMessage key bug — FIXED in Phase 12.** `sourceFreshness` now keys off `SyncStatus.Source` (`internal/mora/ingest.go`), so iMessage is keyed `imessage`, consistent with `cmdSync status`. The filename strip is now only a fallback for a status with no `Source`, and strips both `google-` and `imessage-` prefixes (`ingest.go`). Verified by reading the new code + the Plan-05 regression tests (`mora_test.go`: keys-off-Source + includes-never-synced). Not re-run against a live multi-source vault here.
- **`loadConnectorSyncStatus` resolves the FIRST enabled source of a connector type** (`internal/mora/digest.go:447-474`). Today `Source.Name == connector type` for the ingesting connectors, so this is unique. A future multi-account phase would need to reconcile per-account `sourceInstanceKey` values to per-account status files. The comment flags this. Not exercised in v1.
- **Crash-resume durability:** the checkpoint survives a *returned* fetch error (status is saved on return, `internal/mora/ingest.go`), but a hard process kill between pages loses the in-memory checkpoint and forces a full-window restart next run. The idempotent content-hash upsert makes this safe, not lossy — but there is no test exercising a true mid-loop `SIGKILL`, so this is reasoned from the save-on-return structure rather than directly verified. The `brief/.lock` (`acquireBriefLock`) shares this stale-after-SIGKILL property: a hard kill before release leaks the lockfile until removed (documented, accepted — `internal/mora/brief.go:236-240`).
