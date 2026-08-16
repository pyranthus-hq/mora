# Data Model & Storage

This document defines the Markdown memory format on disk. It also defines the
identity rules that make repeated syncs safe. Mora builds a SQLite index from
the Markdown. The vault is the source of truth. The database is a cache.

## Files

| File | Lines | Responsibility |
|---|---|---|
| `internal/mora/mora.go` | 3933 | `Memory`/`Source`/`Config` model; `createMemory`; `atomicCreate` |
| `internal/atomicio/atomicio.go` | 163 | Atomic file primitives: `atomicio.Write` (temp file + `os.Rename`); `atomicio.AppendFile` |
| `internal/mora/ingest.go` | 1104 | Connector ingest/sync wiring & the write boundary: `writeMappedMemory`; `cmdIngest`/`cmdConnect`/`cmdSync`/`cmdReingest`; `ingestGoogle`/`ingestIMessage`/`ingestAppleCal`/`ingestFilesystem`; `persistSyncStatus`; `sourceFreshness`; `curatedExtractExt`/`extractDocxText` |
| `internal/mora/index.go` | composition | `rebuildIndex`/`rebuildIndexWithPolicy`, SQLite DDL/deletes, transaction lifecycle, governance/compiler wiring, vectors and schema metadata. |
| `internal/graphstore/store.go` | SQL projection | Same-transaction graph entity/edge/merge writes and low-level graph reads; accepts caller-owned `*sql.Tx`/`*sql.DB` and never commits. |
| `internal/mora/config.go` | 496 | `Config` load/parse/write (`defaultConfig`/`loadConfig`/`parseConfigValue`/`cmdConfig`/`writeConfig`); `init` scaffolding (`cmdInit`/`scaffoldControlFiles`/`confirmVaultRepoint`). Retrieval-weight accessors (`Config.fusion`/`Config.mmr`) |
| `internal/mora/memfile.go` | — | Memory-file render/parse/path: `renderMemory`/`parseMemory`/`writeMemory`; `findMemory`/`allMemoryFiles`/`listMemories`. The `memoriesRoot`/`sourcesRoot`/`memoryPath`/`osSafeBase` path helpers; `newID`. The mora-local `ContentHash` (filesystem ids only) |
| `internal/memory/mapped.go` | 154 | `MappedMemory` hand-off struct; `MapItem` (Item→MappedMemory, byte budget, content-hash fold); `CanonicalMeta`. Kind→(type,provider) registry |
| `internal/memory/ids.go` | 25 | `StableID` (provider identity), `ContentHash` (provider change-detect, sha256/16), `SafeFilename` (`/`,`:`,` ` → `_`) |
| `internal/memory/types.go` | 70 | `Item`, `ItemKind`, `Attachment` (metadata + in-transit `Path`), `FetchWindow`, `Page`, `Fetcher` — the connector-agnostic fetch types feeding `MapItem` |
| `internal/mora/pdf.go` | 129 | `extractPDFText` (pinned `ledongthuc/pdf`, recover-wrapped, capped); `writeAttachmentMemories` — one derived `att_…` memory per readable iMessage PDF attachment |
| `internal/mora/render.go` | 122 | Human-facing TTY styling (`colorEnabled`, `styler`, `styleDigestTTY`). **Not** part of the persisted data path — gated so ANSI never reaches `--json`/MCP/files |

## The `Memory` model

A memory is one Markdown file. YAML-like frontmatter comes first, then a text
body. `Memory` is the in-memory form (`internal/mora/mora.go:50-71`):

```go
type Memory struct {
    ID, Scope, Type, Title string
    Tags                   []string
    Source, CreatedAt, Path, Text string
    Score                  float64        // populated by search only
    Provider, ProviderID, ContentHash, LastSynced string
    Truncated              bool
    DeletedAt              string
    Meta                   map[string]any // structured identity; feeds the entity graph
}
```

The connector layer produces `MappedMemory`
(`internal/memory/mapped.go:12-31`) as its **parallel hand-off struct**. Its
fields match the frontmatter. It lives in `internal/memory`, so connectors
never import `internal/mora`. AGENTS.md makes this a hard no-cycle rule.
`writeMappedMemory` (`ingest.go`) is the only wiring boundary. It copies a
`MappedMemory` to a `Memory` and saves it.

### On-disk Markdown format

`renderMemory` (`memfile.go`) emits the canonical bytes:

```
---
id: gmail_thread/199abc
scope: global
type: email
title: "Re: launch plan"
tags: [inbox, work]
source: 199abc
created_at: 2026-05-30T14:02:11Z
provider: gmail
provider_id: 199abc
content_hash: 9f1c2a3b4d5e6f70
last_synced: 2026-06-06T09:00:00Z
truncated: true
meta: {"from":"a@x.com","participants":["a@x.com","b@y.com"]}
---

<body text>
```

Render rules that the parser depends on:
- **Conditional fields**: `provider`/`provider_id`, `content_hash`, `last_synced`, `truncated`, `deleted_at`, and `meta` are only written when non-empty (`memfile.go`). A hand-written `mora write` memory has no provider block.
- **`quoteYAML`** (`memfile.go`) wraps `title`/`source`/`provider_id` in Go-quoted form only if they contain `:#[]`, so a colon in a subject line cannot break the line parser.
- **`meta` is one canonical JSON line.** `CanonicalMeta` (`mapped.go:48-57`) is `json.Marshal` of the map, which emits **sorted keys on a single line** — stable bytes independent of insertion order, and no raw newline to break the line-split parser.

`parseMemory` (`memfile.go`) is the inverse and is deliberately hand-rolled (no YAML lib):
- It requires a leading `---\n` and splits on the **first** `\n---\n` (`memfile.go`).
- Each frontmatter line is split on the **first** colon via `strings.Cut`. For the `meta` line it decodes the **raw substring after the first colon** with `json.NewDecoder` + `UseNumber()` (`memfile.go`) — `UseNumber` keeps a 19-digit thread id from decoding to a lossy `float64`.
- A corrupt `meta:` line is **not** silently dropped — it logs `warn: … meta frontmatter is corrupt` to stderr and continues (`memfile.go`), because losing a memory's structured identity silently would corrupt the entity graph.
- A missing `id` is a hard error (`memfile.go`).

`writeMemory` (`memfile.go`, function `writeMemory`) renders then `atomicio.Write`s (temp file + `os.Rename`, function `atomicio.Write`) so a partial write never leaves a torn frontmatter file. Directories are created `0o700`, files `0o644`.

**Brand-new user memories publish create-exclusively.** `mora write` and MCP `write_memory` go through `createMemory` (function `createMemory`), not `writeMemory`: it mints an id, renders, and `atomicCreate`s (function `atomicCreate`) — a temp file published with `os.Link`, which fails `EEXIST` instead of replacing an existing file. `atomicio.Write`'s final `os.Rename` REPLACES the target (last-writer-wins), so two concurrent writers that mint the *same* `newID()` (same-second timestamp + identical random bytes) would silently clobber each other; `atomicCreate` makes that impossible — exactly one writer wins the link and the loser gets `os.ErrExist`, on which `createMemory` re-mints a fresh id and retries (bounded by `maxCreateAttempts`). This mirrors the loop lock's proven `publishLockFile` (`loop.go`). On Windows `os.Link` is `CreateHardLinkW`, which likewise fails on a present target. **Connector re-writes go through `writeMappedMemory` → `atomicio.Write`** — re-rendering an existing provider memory onto its own stable path is an idempotent overwrite, not a collision, so the replacing `os.Rename` is correct there. (`writeMemory` itself — the plain render-then-`atomicio.Write` helper — is no longer on the new-user-memory path. It writes a memory at a known caller-supplied id, e.g. test seeding.)

*No-hardlink fallback.* `vault_dir` is user-configurable, and some filesystems (exFAT/FAT32 USB sticks, some SMB/NFS mounts) do not support hard links, so `os.Link` returns `EPERM`/`ENOTSUP`/`EOPNOTSUPP` (POSIX) or `ERROR_NOT_SUPPORTED` (Windows) — never `os.ErrExist`. A hard failure there would regress `mora write`/`write_memory` below where the old plain-`atomicio.Write` worked, so `atomicCreate` (function `atomicCreate`) classifies that error class (`linkUnsupported`, a build-tagged `link_windows.go`/`link_notwindows.go` split like `renameReplaceRetryable`) and falls back WITHOUT losing the no-clobber guarantee: it claims the path with `os.OpenFile(O_CREATE|O_EXCL)` (still fails `EEXIST` on a racer/collision → re-mint), then renames its own staged temp onto its own empty placeholder. A link error that is neither `os.ErrExist` nor link-unsupported surfaces as-is (never masked). The one honest trade-off: on those filesystems a concurrent reader can briefly observe an empty placeholder between the claim and the rename; `parseMemory` returns `"missing frontmatter"` on it and every index/list/find caller skips a parse error (`continue`), so it is ignored (no crash) and picked up once the rename lands. POSIX/NTFS never reach the fallback.

## Identity: `StableID` vs `SafeFilename` (the critical distinction)

This is the single most footgun-prone area of the data model.

```mermaid
flowchart LR
    K["ItemKind<br/>gmail_thread"] --> SID
    P["ProviderID<br/>199abc"] --> SID
    SID["StableID()<br/>gmail_thread/199abc"] -->|"stored verbatim as<br/>frontmatter id"| FM["memory.ID"]
    SID -->|"SafeFilename()<br/>/ : space → _"| FN["gmail_thread_199abc.md"]
    FN -->|"on disk"| FILE["sources/gmail/gmail_thread_199abc.md"]
    FM -.->|"lookup by id<br/>must match BOTH forms"| FIND["findMemory()"]
    FILE -.-> FIND
```

- **`StableID`** (`ids.go:11-13`) = `kind + "/" + providerID`. Derived from **immutable provider identity only, never content** — re-syncing an edited thread overwrites the same logical memory instead of forking a new one. It is stored verbatim as the frontmatter `id`.
- **`SafeFilename`** (`ids.go:22-25`) replaces `/`, `:`, and space with `_`, so `gmail_thread/199abc` files as `gmail_thread_199abc.md`. Provider memories live at `sources/<provider>/<SafeFilename>.md` (`writeMappedMemory`, `ingest.go`).
- **Therefore the on-disk basename is NOT the id.** `findMemory` (`memfile.go`) must check **both** shapes: it builds `base := id + ".md"` and `safeBase := SafeFilename(id) + ".md"`, matches either (plus a `strings.Contains` fallback), then confirms by re-parsing and comparing `m.ID == id`. If you write any new id-based lookup, you must match the `SafeFilename` form too or Gmail/Calendar/iMessage ids silently won't resolve.

Two other id shapes exist on disk:
- **Manual memories** (`mora write`, MCP `write_memory`) get `newID()` (`memfile.go`, function `newID`): `mem_<local-timestamp>_<8 hex>` — the timestamp is `time.Now().Format("20060102_150405")`, **local time, not UTC** (only the provider `created_at` is UTC) — filed under `memories/<scope-as-path>/<id>.md` via `memoryPath` (function `memoryPath`, which turns scope `a:b` and `/` into path separators). The 8 hex are 4 `crypto/rand` bytes. Because the timestamp is only second-granular, an id collision between two concurrent writers is possible, which is why manual memories publish create-exclusively (see `createMemory` above) instead of trusting the id to be unique. `newID` also handles a `crypto/rand` failure explicitly — it falls back to `math/rand/v2` entropy (emitting one non-fatal `warn:` line to stderr, never failing the write) rather than an all-zero suffix (which would collide every time within a second and stall the re-mint retry). The id is a uniqueness token, not a secret.
- **Filesystem source files** get `src_<ContentHash(name:relpath)>` (`ingest.go`) using the **mora-local** `ContentHash` (`memfile.go`, a small FNV — distinct from the provider `ContentHash` in `ids.go`). This is the only place the FNV hash is used for an id.

## Content-hash idempotency & `created_at` preservation

`MapItem` (`mapped.go:93-154`) computes `ContentHash` over `(it.Title, it.Body, canonicalMeta)` — using the **original, untruncated** `it.Body` (`mapped.go:143`), not the byte-budgeted body it persists — but folds Meta in **only when non-empty** (`contentHashWithMeta`, `mapped.go:36-41`), so pre-Meta legacy files keep their exact two-part hash and aren't spuriously rewritten on the next sync. A new participant or recovered address therefore *does* change the hash and trigger a rewrite. Cosmetic Meta-absence does not.

`writeMappedMemory` (`ingest.go`) is the idempotent write:

```mermaid
flowchart TD
    A["MappedMemory in"] --> B["out = sources/&lt;provider&gt;/SafeFilename(StableID).md"]
    B --> C{"parseMemory(out)<br/>exists?"}
    C -->|"no"| W["renderMemory + atomicio.Write"]
    C -->|"yes"| D{"existing.ContentHash == new<br/>AND DeletedAt == ''"}
    D -->|"unchanged"| SKIP["return nil — no write,<br/>created_at untouched"]
    D -->|"changed or tombstone"| E["m.CreatedAt = existing.CreatedAt<br/>(preserve original)"]
    E --> W
```

Two invariants live here: (1) an unchanged, non-deleted item is a **no-op** (the hash skip at `ingest.go`), so re-running a backfill is free; (2) when content *did* change, the **original `created_at` is preserved** (`ingest.go`) — the new fetch's recomputed timestamp never overwrites the first-seen time. A tombstone (`DeletedAt != ""`) always forces the rewrite even if the body hash matches.

## The three clocks on a memory (#218)

`created_at` is **not** one thing. On a connector memory it is the provider's *occurrence* time (`MapItem` copies `Item.OccurredAt` — a calendar event's start, a thread's newest message — and the rewrite above then pins it forever); on a locally minted memory (`createMemory` via `mora write` / MCP `write_memory`, a `mora teach` replacement, a filesystem source file from `ingestFilesystem`) it is the instant of the vault write. Ranking "recent memories" by it therefore put next January's calendar fixture above everything ingested today, and an agent quoting it as "when did I learn this" was quoting an event date.

`recency.go` separates the instants Mora can actually evidence, and refuses the ones it cannot:

| Field | Function | Source of truth | Omitted when |
|---|---|---|---|
| `event_start` | `eventStartOf` | `Meta["occurred_at"]` on a `type: event` memory — the start written by the google (`calendar.go`) and applecal (`applecal.go`) connectors | not an event memory. On gmail/imessage the same key holds the thread's *newest message* time, so the type gate is what stops a message time being relabelled a scheduled start |
| `source_created_at` | `sourceCreatedAtOf` | `Meta["source_created_at"]`, which the google calendar connector writes from `calendar.Event.Created` (normalized to UTC RFC3339 in `calEventToItem`), else — for gmail — the `at` of the FIRST entry of `Meta["messages"]`, the thread's opening message from a full `Threads.Get(format=full)` | any other source, **and** any calendar memory ingested before that key existed. The gate on the direct key is its *presence*: a key persisted as `""`, `null`, or a non-string means the connector recorded something unreadable, so the field is dropped rather than answered from the gmail opening message — only an absent key means "this source records no creation time". The applecal connector writes no creation clock (the store's `creation_date` is the local replica's row, not the event's origin), and an iMessage conversation persists only its newest message time |
| `indexed_at` | `indexedAtOf` | `last_synced` — stamped per item by `memory.Ingest` right before the write and reaching disk **only** when `writeMappedMemory` actually rewrites the file — else `created_at` for a memory with no provider, where that value *is* the write clock | a provider memory with no `last_synced`: a PDF attachment memory from `writeAttachmentMemories` (it inherits the parent's occurrence-time `created_at`) or a file left by an older binary |

Read `indexed_at` as *when Mora last wrote this memory into the vault*. It is deliberately **not** a sync-attempt time (that is `memory.SyncStatus.LastAttemptAt`, in the state dir, and never touches a memory file), not the global index-generation stamp (`index_meta.indexed_at`, one row for the whole index), and not a filesystem mtime. `indexedAtOf` is stricter than `observedAtOf` (`graph.go`), which falls back to `created_at` unconditionally: a best-effort observation time is fine for ranking a graph edge, but a timestamp an agent will quote back has to be right or absent.

**Right or absent, always parseable.** A published `event_start` / `source_created_at` / `indexed_at` is a valid RFC3339 instant or the field is not there: frontmatter is plain text on disk, so a hand edit, a truncated write, or an older binary can leave a stamp that does not parse, and every derivation validates through the one seam (`rfc3339Instant`) before publishing. That seam does not delegate the question to `time.Parse(time.RFC3339, …)`, because Go's RFC3339 layout is looser than the RFC 3339 grammar and these fields are published *verbatim*, so any form Go tolerates would reach a consumer whose parser does not. On the pinned toolchain `time.Parse` accepts at least a one-digit hour (`…T1:12:34Z`, because the layout's hour verb `15` is Go's non-padded form), an offset minute of 60 (`…+00:60`, silently folded into an ordinary `+01:00`, so a range check on the *parsed result* cannot see it), an offset hour of 24 (`…+24:00`), and a comma fractional separator (`…:00,5Z`). So `rfc3339Instant` checks the ABNF directly first — fixed digit counts, fixed literals, whole string consumed, and every component range-checked — and only then calls `time.Parse`, for the two jobs a syntax pass cannot do: rejecting a well-formed date that does not exist (`2026-02-30`) and producing the instant. Valid offsets and fractional seconds are preserved exactly; the gate rejects, it never rewrites. A stamp that fails validation is reported as **unknown**, never republished raw and never replaced — a provider memory with an unparseable `last_synced` omits `indexed_at` and sorts last even though its `created_at` sits right there and parses, because on a connector memory that value is the *event* time and borrowing it is the exact bug this section exists to remove. The same seam feeds `ingestRecencyOf`, so a memory can never rank by a stamp its row declines to show.

`source_created_at` on calendar is **forward-only**: `calEventToItem` records `Event.Created` from now on, and nothing back-fills the memories written before it. Existing Google Calendar rows keep omitting the field until a re-ingest rewrites them (the new Meta key changes the content hash, so the next sync of a still-live event rewrites it once); a past event outside the sync window simply never gains one. Apple Calendar rows omit it permanently.

All three derived fields are **read-time only and never persisted** — `renderMemory` writes no such frontmatter, so no schema changes and no vault migration. They are populated only on the MCP `list_memory` rows (`decorateBrowseRecency`), and `omitempty` keeps every other payload byte-identical. `created_at` itself is untouched, so existing consumers keep the value they have always received.

## Document extraction & attachment-derived memories (`.docx` / `.pdf`)

### Filesystem sources: extract-don't-read formats

`curatedExtractExt` (`ingest.go`) names the non-plain-text formats a filesystem source ingests by **extracting** text rather than indexing raw bytes — today `.docx` and `.pdf`. `ingestFilesystem` branches on it (`ingest.go`): `.pdf` goes through `extractPDFText`, everything else in the set through `extractDocxText`. An extraction error or empty result skips the file — unreadable/empty/oversized is never indexed as garbage. The extracted text then flows through the normal filesystem path: ids stay `src_<FNV>` (`ingest.go`), nothing else about filesystem identity changes.

`extractPDFText` (`pdf.go:32-77`) uses the pinned, audited `ledongthuc/pdf` (pure Go — the no-CGO constraint holds). The library panics on malformed input by design, so the entire parse is **recover-wrapped**: any panic becomes an error and the caller skips the file — a bad PDF must never crash a sync (`pdf.go:33-37`). Caps (`pdf.go:20-23`, package vars so tests can lower them):

| Cap | Value | Where enforced |
|---|---|---|
| File size | 20 MiB (`pdfMaxFileSize`) | rejected pre-parse, `pdf.go:42-44` |
| Pages | 500 (`pdfMaxPages`) | extraction truncates, `pdf.go:52-55` |
| Extracted text | 512 KiB | the existing index bound at every call site — over ⇒ the whole file is skipped (`pdf.go:72-74`, `pdf.go:102`) |

A scanned/image-only PDF extracts to `""` with a nil error and is skipped — there is no OCR (it would break the single-binary/no-CGO constraint), and Mora never fabricates text it can't read. A single garbled page is skipped without losing the rest of the document (`pdf.go:66-69`).

### `Attachment.Path` and the derived-memory shape

`Attachment` (`types.go:16-27`) is **metadata-plus-location**: `Filename`/`MimeType`/`Size`, plus — when the body already exists on local disk (iMessage) — the absolute `Path` to it. Connectors never open the file. Bytes are never carried on the struct. Neither `Path` nor bytes ever appear in rendered vault output (the IMSG-07 amendment — see [iMessage connector](./05-connectors-imessage.md)). For Gmail, attachments remain metadata-only and `Path` stays empty — the field is the future seam, not a fetch.

`writeAttachmentMemories` (`pdf.go:88-129`) consumes `Path` at the wiring boundary, immediately after the parent's `writeMappedMemory` in the iMessage write closure (`ingest.go`). For each attachment that has a `Path` and is a PDF by MIME **or** extension (`isPDFAttachment`, `pdf.go:79-86` — chat.db rows sometimes carry one without the other), it derives one `MappedMemory` (`pdf.go:109-122`):

- **`StableID`** = `"att_" + ContentHash(parent.StableID + ":" + a.Path)` (`pdf.go:110`) — the **provider** sha256 `ContentHash` (`ids.go:16`), not the mora-local FNV. Hashing parent id + path keeps the id stable across re-syncs, so an unchanged PDF is a no-op via the `writeMappedMemory` hash skip.
- **`Type`** = `"source"`; **`Tags`** = the parent's tags plus `"attachment"`; **`Source`** = the attachment's on-disk path; **`Title`** = the attachment filename (basename fallback).
- **Parent provenance**: `Provider`/`ProviderID`/`Account`/`Scope` and the parent's **`CreatedAt`** are copied verbatim — the derived memory files under the same `sources/<provider>/` and inherits the conversation's timestamp.
- **`ContentHash`** = `ContentHash(title, text)`, so an edited-in-place PDF rewrites and an untouched one skips.

Every extraction failure — missing file, malformed, encrypted, empty/scanned, past the caps — skips that attachment and keeps the metadata marker on the parent transcript. A body Mora can't read is not a sync error. Only a vault **write** failure propagates (`pdf.go:88-104, 123-125`).

## SQLite index (rebuilt, never authoritative)

The database at `<DataDir>/index.db` (`dbPath`, `index.go`) is a derived cache. Every table is `CREATE … IF NOT EXISTS` and fully `DELETE`d + rebuilt on each `rebuildIndex` (`index.go`), so deleting `index.db` loses nothing.

```mermaid
erDiagram
    memories {
        TEXT id PK
        TEXT scope
        TEXT type
        TEXT title
        TEXT tags "CSV"
        TEXT source
        TEXT created_at
        TEXT path "vault file path"
        TEXT text "full body"
        TEXT provider "canonical connector family"
        TEXT account "connector account label"
        INT created_at_unix "RFC3339 instant or MinInt64"
    }
    memories_fts {
        TEXT id "fts5 virtual"
        TEXT scope
        TEXT title
        TEXT tags
        TEXT source
        TEXT text
    }
    mem_vectors {
        TEXT memory_id PK
        INT dim
        TEXT model
        BLOB vec "LE float32"
    }
    commitments {
        TEXT generation
        TEXT row_key PK
        TEXT commitment_id UK
        TEXT memory_id
        TEXT payload "typed JSON"
    }
    entities {
        TEXT id PK
        TEXT kind "person|service|topic"
        TEXT display_name
        TEXT aliases "JSON array"
        INT mention_count
        TEXT first_seen
        TEXT last_seen
    }
    edges {
        TEXT src PK
        TEXT rel PK
        TEXT dst PK
        TEXT evidence_id PK
        TEXT valid_from
        TEXT valid_to
        TEXT observed_at
        TEXT invalidated_at
    }
    memories ||--|| memories_fts : "joined on id"
    memories ||--|| mem_vectors : "memory_id"
    memories ||--o{ commitments : "memory_id"
    memories ||--o{ edges : "evidence_id"
    entities ||--o{ edges : "src / dst"
```

DDL is at `mora.go:2036-2051`:
- **`memories`** — the row-store keyed by frontmatter `id`. `tags` stored CSV (`strings.Join(m.Tags, ",")`, `mora.go:2076`). Holds the full `text` and the vault `path` so search can return bodies and read can locate the file. Canonical schema v5 also carries `provider`, `account`, and `created_at_unix`, the indexed fields used for source/time predicates before ranking.
- **`memories_fts`** — an FTS5 virtual table over `id, scope, title, tags, source, text`. Note **`type`, `created_at`, and `path` are deliberately NOT in FTS** (they're metadata, not searchable prose). Search joins back to `memories` on `id` to recover them (`searchMemories`, `search.go`).
- **`mem_vectors`** — one static-hash (or Ollama) embedding per memory, written by `writeVectors` (`index.go`) over `m.Title + "\n" + m.Text`; `vec` is little-endian float32 bytes (`encodeVec`, `embed.go:96-102`). `model` is stored per-row (`emb.ModelID()` at `mora.go:2156`. Static floor is `static-hash-v1`, `embed.go:31`) so the embedder behind each vector is attributable. Every rebuild re-embeds all memories unconditionally (`INSERT OR REPLACE`, `mora.go:2149`). The retrieval path is what consults the stored `model`. See [retrieval](./02-retrieval-search.md).
- **`commitments`** — the typed obligation inventory derived by `writeCommitments` from the whole honest vault snapshot. `generation` binds the vault-manifest digest, the injected rebuild instant, and the sorted source-health snapshot. It is also stamped as `index_meta.commitments_generation`, and readers select only that generation. Health is included because `state_uncertain` is a material input: two different freshness snapshots must not share one generation id. `row_key` equals the evidence-derived `CommitmentID` when immutable message/block refs exist. A legacy pre-PR1 memory may be classified for owner/direction/lifecycle but receives an internal `legacy:` row key and a NULL `commitment_id` rather than a fabricated identity. The JSON payload carries owner, counterparty, the named shared `Direction` type, opening span, typed due value, lifecycle/closure state, evidence-preserving typed citations, dedup provenance, and a fail-closed `state_uncertain` signal. The same `Direction` vocabulary is used by task-ledger and evidence open-loop lanes. It is not independently redefined at each surface. Due classification reads only the authored opening span: a parsed month-name or ISO calendar date is `explicit_date` stored as `YYYY-MM-DD`, a relative anchor such as `tomorrow`, `next week`, a weekday, or `before …` is `relative`, and everything else is the explicit `none` sentinel. It never infers a clock — even when a clock appears near a dated phrase — because the opener does not provide the event-linked semantics needed to attach that clock to the obligation. Urgency and meeting placement likewise never create a deadline. No commitment field is added to Markdown.

`internal/commitment` owns the I/O-free lifecycle and duplicate decision kernels: it orders candidate evidence, refuses weak or ambiguous closure, distinguishes supersession from closure, and resolves canonical duplicates only with strong provenance. `internal/mora` adapts provider evidence into those DTOs, constructs typed citations, applies source-health uncertainty, and owns transaction/generation persistence.

Meeting briefs and daily digests both read this table through `readCommitmentSnapshot`. Neither independently reclassifies its clipped presentation text. The daily join copies `Owner`, `Direction`, `DueAt`, `Lifecycle`, and `ClosureRef` onto the surfaced `DigestItem` only when its memory has exactly one materialized commitment. Multiple independently anchored commitments remain untyped at artifact grain instead of being collapsed by guesswork.

Lifecycle is a guarded transition over authored evidence strictly later than the opener. Delivery by the owner or acknowledgement by the asker must share action/object evidence. Questions, future modality, negation, staged work, quoted/forwarded text, service mail, and timestamp ties cannot close an item. A changed action/deadline becomes `superseded` with `superseded_by`, never `closed`. When one closure fits multiple open candidates equally, every candidate stays open with an ambiguity gap. Closing evidence appends a `closure` citation after the retained `opener` citation.

Dedup uses normalized text only to generate candidates. It marks a copy automatically only with immutable same-message/block provenance or explicit quoted ancestry, plus matching owner, counterparty, due, direction, and lifecycle instance. Text-equal obligations without that provenance stay distinct. The earliest immutable opening is canonical. Supporting-copy citations are retained with a typed `supporting` role. If any enabled source is stale, failed, or never synced at rebuild time, the materialization keeps its evidence-derived state but marks it `state_uncertain` instead of presenting a partial snapshot as complete.
- **`entities` / `edges`** — the deterministically-derived person graph. `edges` PK is the composite `(src, rel, dst, evidence_id)` so duplicate edges are idempotent. Empty bi-temporal timestamps persist as SQL NULL via `nullStr` (`graph.go:50-57`). Inserted `OR IGNORE`. See [entity-graph](./03-entity-graph.md).
- **`gmail_segments` / `gmail_segments_fts` / `gmail_segment_diagnostics`** (issue #243) — the derived, disposable Gmail evidence-segment projection. `gmail_segments` (`evidence_ref TEXT PRIMARY KEY, memory_id, sender, recipients, at, block_refs, text`) holds one row per Gmail *message* inside a thread memory, keyed by its `MessageRef` verbatim (`gmailMessageRef`, `internal/google/gmail.go` — `"gmail_thread/"+threadID+"#"+messageID`), so `evidence_ref` is a stable, citable sub-reference into the thread. `gmail_segments_fts` is an FTS5 index over exactly `(evidence_ref UNINDEXED, text)`, so a `MATCH` hit yields the `evidence_ref` directly. `gmail_segment_diagnostics` (`memory_id TEXT PRIMARY KEY, reason, meta_count, body_count`) records — counts/ids **only, never memory text** — why a Gmail memory produced **zero** segments, checked in this priority order (first match wins): `truncated` (`Memory.Truncated`, checked independently of counts), `count_mismatch` (`len(meta.messages)` differs from the one canonical raw `gmailBodySeparator` block split used for count, sender validation, and text; splitting after `stripFromLine` is forbidden because an empty first message can erase a real boundary), `ordering_mismatch` (counts agree, but a declared `meta.messages[i].Sender` does not match the sender address parsed from that same raw rendered block's own `"From:"` header at the same position — a direct, positional, content-grounded signal, never a timestamp heuristic), `malformed_ref` (a `MessageRef` that does not carry the exact `memory.ID+"#"` prefix — i.e. names a different thread), and `duplicate_ref` (every ref is individually well-formed, but two or more messages declare the IDENTICAL `MessageRef` — checked LAST, since "duplicate" is a property of the whole set of refs, not any one ref; without this check the second insert would collide on `gmail_segments`' `evidence_ref` PRIMARY KEY, a real SQL error that would otherwise abort the whole rebuild transaction). Only an **absent** `meta.messages` key is the legacy pre-segment shape; an explicitly present empty or malformed value is evaluated as an alignment failure (after the higher-priority truncation check) rather than disappearing from diagnostics. Any refusal drops the **whole** memory's segments (never a partial/misattributed set); the parent memory itself stays indexed normally in `memories`/`memories_fts`. `internal/segments.Derive` is the single fail-closed derivation both `rebuildIndex` and `indexUpsert` call. The same package owns the exact DDL and accepts caller-owned `*sql.Tx` handles for prepare/write/clear; Mora owns the surrounding rebuild/upsert transaction and ordering — see below. The package also queries one strongest segment per parent before limiting, completes receipts only for the bounded final parent set, and admits SQL candidates through an injected vault-hydration callback; visibility and search failure policy remain above it.

### `rebuildIndex` pipeline

```mermaid
flowchart TD
    START["rebuildIndex(ctx, cfg)"] --> OPEN["sql.Open(index.db?_txlock=immediate<br/>&_pragma=busy_timeout(15000))"]
    OPEN --> TX["BeginTx — BEGIN IMMEDIATE<br/>ONE transaction, takes the write lock"]
    TX --> WALK["allMemoryFiles: WalkDir<br/>memories/ + sources/<br/>collect *.md, sort<br/>(inside the write lock)"]
    WALK --> DDL["CREATE IF NOT EXISTS (all tables)<br/>then DELETE FROM derived projections"]
    DDL --> LOOP["for each file: parseMemory"]
    LOOP -->|"parse err"| SKIPF["skip file (continue)"]
    LOOP --> INS["INSERT OR REPLACE memories<br/>+ INSERT memories_fts<br/>append to parsed[]"]
    INS --> GRAPH["Mora compile + graphstore.Write(tx, result)<br/>entities + edges + merge provenance"]
    GRAPH --> VEC["writeVectors(tx, chooseEmbedder, parsed)<br/>embed title+text → mem_vectors"]
    VEC --> OBL["writeCommitments(tx, manifest generation, parsed)<br/>speech/citation adapters → internal/commitment lifecycle + provenance dedup"]
    OBL --> COMMIT["tx.Commit"]
    COMMIT --> N["return count"]
```

The whole rebuild — schema, the destructive `DELETE`s, every memory/FTS/graph/vector/commitment write, and the commitment-generation stamp — runs inside **one transaction** (`rebuildIndexWithPolicy`, `index.go`). A mid-rebuild failure rolls back to the prior committed index rather than leaving a half-empty one. This is why a graph, embedder, or commitment-materialization error returns before `Commit`. The DSN sets `_txlock=immediate`, so `BeginTx` issues `BEGIN IMMEDIATE` and takes the SQLite writer lock up front (the `busy_timeout` lets a contending rebuild wait rather than fail fast). The vault directory listing (`allMemoryFiles`) therefore runs **after** `BeginTx`, inside the write lock — never before it. Snapshotting the directory before the lock allowed two concurrent rebuilds to interleave: rebuild A lists, rebuild B (fired by a newer write) lists + commits, then A commits *last* carrying its *older* list, silently omitting the just-written memory until a later rebuild. Because the immediate lock serializes rebuilds, whichever commits later necessarily listed later, so a committed index can no longer be clobbered by an older rebuild's stale snapshot (a memory written *after* the surviving rebuild's listing is ordinary until-next-rebuild staleness, not this race). The vault-identity guard (`assessRebuild`) still runs after the listing, so its block-on-empty/foreign semantics are unchanged. `allMemoryFiles` is a pure filesystem walk (it takes no DB lock), so holding the write lock across it cannot deadlock. A file that fails to `parseMemory` is skipped, not fatal. `searchMemories` lazily triggers a rebuild if `index.db` is missing. The same transaction stamps `PRAGMA user_version = indexSchemaVersion`. Every read-open goes through `openIndexRO` (`index.go`), which **refuses a mismatched index** with an actionable "run `mora index rebuild`" error rather than serving a stale schema silently (the pre-stamp failure: a swapped binary read zeroed salience off a pre-column index). On the static-hash floor a stale index **self-heals inline** (`indexAutoHeal` — a rebuild is seconds, same philosophy as rebuild-on-missing, and it covers every user's first upgrade across the stamp's introduction). Under a semantic embedder the read errors instead — a re-embed takes minutes and must not stall an MCP call; `mora upgrade` runs that rebuild at the consented moment.

**Combined schema v5.** Issues #241 and #243 were developed independently and each used predecessor version 4 for a different physical change: D-v4 added `memories.provider/account/created_at_unix`; E-v4 added the three Gmail segment tables. The combined binary therefore uses version 5. A v3 index or either v4 shape is rebuilt atomically from the Markdown vault into the complete v5 union. At the incremental-write boundary, `indexReadyForUpsert` also verifies the three D columns plus all three E tables before it permits a 12-column/segment upsert, so a same-stamp partial v5 rebuilds instead of reaching a mismatched insert.

### Incremental upsert on the user-write path (`indexUpsert`)

An authored write (`mora write` / `cmdWrite`, MCP `write_memory`) does **not** rebuild the whole vault. It calls `indexUpsert` (`internal/mora/index_upsert.go`), which reflects **only that one memory** into the index inside one tiny immediate transaction: `DELETE`-then-`INSERT` the single id's row in `memories` and its `memories_fts` row, using the exact insert shapes `rebuildIndex` uses (so an incrementally-added row is byte-identical to a fully-rebuilt one). It then keeps `index_meta.memory_count` consistent (`SELECT COUNT(*)`), and reuses the same identity DSN (`_txlock=immediate` + `busy_timeout(15000)`) as the full rebuild.

WHY: every user write previously triggered a full `DELETE`-and-reinsert of the entire vault. N concurrent agent writers therefore serialized `O(N × vault)` work, thrashed the writer lock, and overran `busy_timeout` — surfacing as degraded-success `index_stale` warnings. `indexUpsert` reprocesses only the one written memory (no whole-vault re-parse, graph rebuild, or re-embed), a large **constant-factor** win — ≈ **1.7 ms vs ≈ 98 ms** for a full rebuild at ~1k memories on an Apple M1 Pro (~59×. See `BenchmarkIndexUpsert1k` / `BenchmarkRebuildIndex1k`), dominated by the fsync/commit. It is **not** asymptotically `O(1)`: the per-write `DELETE FROM memories_fts WHERE id=?` is a full FTS vtable SCAN and the `COUNT(*)` walks the PK index (both EXPLAIN QUERY PLAN-verified), so the cost still grows linearly with vault size — just with a tiny constant, so the win over a full rebuild *widens* as the vault grows.

Deliberate scope — `indexUpsert` touches memories + FTS (and, since issue #243, that one memory's `gmail_segments`/`gmail_segments_fts`/`gmail_segment_diagnostics` rows, re-derived with the exact same fail-closed rules as a full rebuild) but **not** the entity graph (`entities`/`edges`), `mem_vectors`, or `commitments`:

- The entity graph is a whole-corpus product: `buildGraph` derives structural entities (scope / tag / `[[wikilink]]` / category, plus a per-memory hub) from **every** memory *Meta-independently*, and `canonicalizePersons` merges person identities *across* memories while `writeGraph`'s `INSERT OR REPLACE` recomputes an entity's `mention_count` from the rows it sees. So an authored write genuinely adds graph nodes (at minimum its `scope:` entity and hub) — but a *correct* single-memory graph delta is not a local operation, and rebuilding the graph per write is the `O(vault)` cost this change avoids.
- Vectors feed only the **hybrid** retrieval path, which `defaultSearch` enables **only** when a semantic embedder (Ollama) is configured. Under the default static-hash embedder, `defaultSearch` uses the static keyword surface (parent FTS plus bounded Gmail/iMessage segment FTS), so a missing vector has no effect there. Under a semantic embedder the new memory is a **real but bounded, self-healing recall gap** on the vector arm — fully searchable via FTS immediately, and it gains its vector at the next full rebuild.

Consequence: after an authored write, FTS **search is immediately fresh**, but the entity graph (`list_entities`, `get_entity`, `mora graph`), `mem_vectors` (hybrid recall under a semantic embedder), and typed commitment inventory reflect the new memory only after the **next full rebuild** — the scheduled `index-hourly` job, `mora index rebuild`, a connector sync, or a delete. Meeting and future daily obligation surfaces therefore read one common last-good commitment generation rather than recomputing different windowed views. The lag is **bounded by the rebuild cadence** (hourly by default), not indefinite. The vault stays the source of truth, and the index is a derived, eventually-consistent cache.

Identity safety is preserved end-to-end. `indexUpsert` applies the **same** vault-identity guard as `rebuildIndex` (`assessRebuild` in `vaultid.go`): a write against a vault whose `.mora-vault.json` marker does not match the index rolls back and returns `errRebuildBlocked` **without touching the index**, so `cmdWrite`/`write_memory` keep today's degraded-success handling (CLI: warn + exit 0; MCP: `index_stale:true` + warning, never `isError`). Cold-start and legacy/unbound states (no index yet, stale schema version, or an index that never recorded a `vault_id`) **delegate to the full `rebuildIndex`**, which creates the complete schema and binds identity — the readiness check runs on a read connection so the delegated rebuild's own immediate transaction is never blocked. The full `rebuildIndex` remains the path for `mora index rebuild`, connector sync, and delete.

## Config & paths

`defaultConfig` (`config.go`) seeds XDG-style defaults under `$HOME`; `loadConfig` (`config.go`) overlays a tiny hand-parsed `config.toml` (only `vault_dir`, `data_dir`, `state_dir` keys; `~` expanded via `genericutil.ExpandHome`). `ConfigDir` is **not** overridable — it's always where `config.toml` lives.

| Var / path | Default | Purpose |
|---|---|---|
| `VaultDir` | `~/vault/mora` | Markdown memories: `memories/` (manual), `sources/<provider>/…`, control files (`index.md`, `priority-map.md`, `live-tasks.md`, …) |
| `ConfigDir` | `~/.config/mora` | `config.toml`, `sources.json`, `tokens/google.json` (0600) — fixed, never repointed |
| `DataDir` | `~/.local/share/mora` | `index.db` (the rebuildable SQLite cache) |
| `StateDir` | `~/.local/state/mora` | `sync/google-<source>.json`, `usage/events.jsonl`, `usage/OFF` sentinel |
| `config.toml` keys | — | `vault_dir`, `data_dir`, `state_dir` (quote-stripped, `~`-expanded) |

`mora init` (`mora.go:346-382`) deliberately **loads existing config first** so a re-run never repoints a custom vault and orphans it (`mora.go:353-360`). It then creates the dir tree, writes config, scaffolds control files (skipping any that exist, `mora.go:396-398`), and rebuilds the index.

### Vault identity and the rebuild guard

`vault_dir` is the only precious directory: it holds the Markdown source files every other directory is derived from. The SQLite index, sync watermarks, and config are all rebuildable or re-creatable. The vault is not (without a `mora sync git` backup).

On `mora init`, Mora writes a `.mora-vault.json` marker inside `vault_dir`. This marker lets Mora recognize the vault on later runs. Before rebuilding the index, Mora checks the vault against the marker: if the vault looks empty or the marker is missing or foreign (a common sign that `vault_dir` has moved or been remounted), the rebuild refuses with an actionable error rather than silently stamping an empty or mismatched index. Run `mora doctor` to diagnose, or pass `--force` to `mora index rebuild` to override the guard.

## Invariants & gotchas

- **`index.db` carries a schema stamp (`PRAGMA user_version`), and incremental writes also check the physical v5 shape.** Bump `indexSchemaVersion` whenever the rebuilt shape changes meaning. Readers refuse a version mismatch; `indexReadyForUpsert` refuses an incomplete same-stamp D+E shape; and `mora upgrade` re-stamps by rebuilding with the new binary.
- **The vault is the source of truth; `index.db` is a disposable cache.** Every SQLite table is rebuilt from scratch on `rebuildIndex` (`index.go`). Never store state that lives *only* in SQLite — it will not survive a rebuild. WHY: a corrupt or deleted DB must be recoverable from the Markdown alone.
- **`StableID` is provider identity, never content** (`ids.go:9-13`). Re-syncing an edited item must overwrite the same file, not duplicate it. WHY: idempotent backfills.
- **Files are named by `SafeFilename`, not by id.** Any id→file lookup MUST match both `id+".md"` and `SafeFilename(id)+".md"` (`findMemory`, `memfile.go`). WHY: `gmail_thread/x` files as `gmail_thread_x.md`. A naive `id+".md"` match silently misses every provider memory.
- **Content-hash skip + `created_at` preservation are paired** (`writeMappedMemory`, `ingest.go`). Unchanged → no write. Changed → keep the original `created_at`. WHY: re-backfill must be free and must not rewrite first-seen timestamps.
- **Meta folds into the content hash only when non-empty** (`contentHashWithMeta`, `mapped.go:36-41`). WHY: a legacy pre-Meta file must keep its exact two-part hash, or every old source file gets spuriously rewritten on the next sync.
- **`meta:` is exactly one canonical JSON line.** `CanonicalMeta` relies on `json.Marshal` emitting sorted keys with no embedded newline (`mapped.go:48-57`). The parser splits frontmatter by lines and `meta` by the first colon. WHY: a multi-line or unsorted meta breaks the hand-rolled parser and makes the content hash non-deterministic.
- **A corrupt `meta:` line is surfaced (stderr warn), never swallowed** (`memfile.go`). WHY: silently dropping it erases the memory's entire entity-graph contribution.
- **`rebuildIndex` is one all-or-nothing transaction** (`index.go`). WHY: the `DELETE`s are destructive. A partial rebuild would otherwise leave a half-empty index live.
- **Authored writes upsert incrementally, not by full rebuild** (`indexUpsert`, `internal/mora/index_upsert.go`). `mora write` / MCP `write_memory` reflect only the one written memory into `memories` + `memories_fts` — a large constant-factor win (~59× at 1k memories), **not** asymptotically `O(1)` (the per-write FTS delete-scan and `COUNT(*)` still grow linearly with vault size). The entity graph and `mem_vectors` are **not** updated on write and reconcile on the next full rebuild (hourly job, `mora index rebuild`, sync, delete), so the graph/hybrid-recall lag is bounded by the rebuild cadence. WHY: a full rebuild per write serialized `O(N × vault)` work across concurrent agent writers and overran `busy_timeout`. FTS search is fresh immediately; `list_entities`/`get_entity` and semantic-embedder vector recall lag until the next rebuild. The same `assessRebuild` identity guard applies — a blocked vault returns `errRebuildBlocked` and leaves the index untouched (degraded-success, not a failed write).
- **Writes are atomic** (`atomicio.Write` = temp + rename, function `atomicio.Write`). WHY: a crash mid-write must not leave torn frontmatter that fails `parseMemory`.
- **New user memories are create-exclusive, not last-writer-wins** (`createMemory`/`atomicCreate`, functions `createMemory`/`atomicCreate`). `mora write` and MCP `write_memory` publish via `os.Link` (fails `EEXIST`) rather than `atomicio.Write`'s replacing `os.Rename`. On a colliding `newID()` the loser re-mints and retries (bounded, `maxCreateAttempts`). WHY: two concurrent writers can mint the same second-granular id, and `os.Rename` would silently clobber one — real data loss. Connector re-writes stay on `writeMappedMemory` → `atomicio.Write` (idempotent overwrite of a stable provider path is correct there). Mirrors `loop.go`'s `publishLockFile`. Portable to Windows (`os.Link` = `CreateHardLinkW`, fails on a present target). On filesystems without hard links (exFAT/FAT32, some SMB/NFS) `os.Link` reports unsupported (never `EEXIST`); `atomicCreate` falls back to an `O_CREATE|O_EXCL` claim + rename-onto-own-placeholder, which keeps no-clobber at the cost of a brief empty-placeholder reader window that `parseMemory`/index callers skip gracefully.
- **`type`/`created_at`/`path` are not in FTS** (`mora.go:2037` vs `2036`). WHY: they are metadata. Search joins back to `memories` to recover them. Adding a column to one table without the other will desync the search projection.
- **ANSI styling never reaches the data path.** `colorEnabled` (`render.go:21-32`) returns false on `--json`, `NO_COLOR`/`MORA_NO_COLOR`, empty-or-`dumb` `TERM`, or a non-TTY writer. The TTY test `isTTYWriter` (`render.go:38-44`) uses go-isatty (not `os.ModeCharDevice`, which is true for `/dev/null`). WHY: stray escape codes corrupt MCP stdio JSON and `--json` output. `render.go` styles human display only. It is *not* part of persisted bytes.
- **Document extraction never indexes garbage** (`pdf.go:88-104`, `ingest.go`). An unreadable, empty (scanned, no OCR), or over-cap extraction skips the file or attachment entirely. WHY: a scanned PDF extracting to `""` must not create an empty searchable memory, and a hostile PDF must not crash a sync (the parse is recover-wrapped, size/page/text-capped).
- **Two different `ContentHash` functions exist.** `internal/memory/ids.go:16` = sha256, first 16 hex chars, for provider change-detection. `internal/mora/memfile.go` = small FNV, used only to mint filesystem-source `src_…` ids (`ingest.go`). Don't confuse them.

## Related

- [overview](./00-overview.md)
- [retrieval & search](./02-retrieval-search.md) — FTS5 + `mem_vectors` hybrid scoring
- [entity graph](./03-entity-graph.md) — how `entities`/`edges` are derived from `Meta`
- [Google connector](./04-connectors-google.md) — produces `MappedMemory` via `MapItem`
- [iMessage connector](./05-connectors-imessage.md) — custom mapper, `KindIMessageChat`
- [MCP server](./06-mcp-server.md) — `read_memory`/`write_memory`/`search_memory` over this model
- [sync & freshness](./11-sync-and-freshness.md) — `SyncStatus`, `last_synced`, resumable ingest

## Open questions / unverified

- `Memory.LastSynced` is rendered/parsed and carried from `MappedMemory`, but `MapItem` (`mapped.go`) never sets `LastSynced` on the struct it returns — it appears populated downstream in the ingest write path (`ingestGoogle`/`Ingest`), which lives outside the files I own. Confirm in [Google connector](./04-connectors-google.md) where `LastSynced` is actually stamped.
- The mora-local FNV `ContentHash` (`memfile.go`) is used for filesystem-source ids. Whether filesystem ingest is reachable in shipped v1 (vs. the deferred `gdrive` stub) is a connector-layer question, not a storage one.
