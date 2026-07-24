# 17 — Governance ledger (`mora forget`)

The governance ledger is Mora's durable local decision record. It states *who
decided what about which memory, and when*. Selective forgetting (#52) and
bounded retention (#53) both use this source record.

The ledger is the durable half of `mora forget`. It makes a deletion
**survive the hourly, agent-less sync**. Sync cannot silently create the item
again. This document describes the built system. The final section records
later work.

## The problem it solves

`mora delete` does not last for connector memories. Sync gets all data in its
window with no agent in the loop. The single write boundary
(`writeMappedMemory`) creates any file missing from its stable path. Thus, "I
deleted that chat" becomes "that chat is back within the hour." See
[11 — Sync & Freshness](./11-sync-and-freshness.md): sync is upsert-only. A
durable fix needs a saved, **identity-keyed** block. The write path checks it
before it creates data.

## Files

| File | Responsibility |
|---|---|
| `internal/mora/governance.go` | The whole primitive: the ledger types, load/save/append/revoke (serialized under `acquireGovernanceLock`), the stable-atom key derivation from `Meta`, the suppression decision, and the shared lease-held write primitive `governanceWriteLease` (behind both `writeUnlessForgotten` and `writeMappedMemory`) plus the one-shot guard entry point `shouldSuppressWrite`. |
| `internal/mora/governance_cmd.go` | The CLI: `cmdForget` / `cmdUnforget` / `forget list`, the vault scan that computes the removal set, and the confirm/dry-run gating. |
| `internal/mora/ingest.go` | Two lease-held guards: `writeMappedMemory` (the connector write chokepoint) holds the governance lease across its suppression check **and** its `atomicWrite`, and `ingestFilesystem` — which renders directly and bypasses `writeMappedMemory` — does the same per file via `writeUnlessForgotten` (see gotcha below). |
| `internal/mora/pdf.go` | One added guard at the top of `writeAttachmentMemories` (the derived-attachment write path — a **one-shot** parent-suppression check, not yet lease-held. See gotcha below). |

## The stable-atom key (why the key shape matters)

A suppression is keyed by the **source-native atom** `{provider, kind, value}`, where `kind ∈ {stable_id, handle, address, host}` — **never** the post-merge `person:` graph id, which *moves* as identities merge and split (the #52 trap). `provider == ""` is a cross-provider wildcard (e.g. one email address forgotten across Gmail **and** Calendar).

Atoms are derived from a memory the same way the [entity graph](./03-entity-graph.md) reads identity (`metaPairs`/`metaStrings`), so the ledger's identity view matches the graph's:

| Atom | Derived from | Example |
|---|---|---|
| `stable_id` | `mm.StableID` (verbatim — never `@account`-stripped) | `imessage_chat/<guid>`, `gmail_thread/<id>` |
| `handle` | iMessage `Meta["participants"][].handle` | `+14155550123` |
| `address` | Gmail/Calendar `Meta["from"\|"to"\|"cc"\|"attendees"\|"organizer"]` (lowercased) | `sam@example.com` |
| `host` | *reserved* — no connector populates a host field yet | — |

Normalization is deliberately minimal (lowercase addresses/email-shaped handles, trim. Phone handles trimmed only). It does **not** do phone/email canonicalization — that lives in `canonicalizePersons`, and coupling to it would risk a false merge. Precision-first: under-match rather than over-match.

## The write-chokepoint guard

`writeMappedMemory` (the one boundary every connector routes through) consults the ledger before persisting anything — and does so while **holding the governance lease across the check and the write** (`governanceWriteLease`), so a concurrent `mora forget` cannot commit its suppression between the check and the `atomicWrite` (the connector-path half of the #113 TOCTOU fix — see gotcha below):

```mermaid
flowchart LR
    MM[MappedMemory in] --> L[acquire governance lease<br/>+ load ledger]
    L --> G{ledger suppresses<br/>an atom of it?}
    G -->|corrupt ledger| ERR[return error<br/>fail-closed]
    G -->|yes| SKIP[return nil<br/>never persist]
    G -->|no| W[content-hash skip<br/>→ renderMemory → atomicWrite<br/>all under the held lease]
```

The decision (`governance.decideSuppress`, pure over `(ledger, provider, stableID, meta)`):

- **Item (`stable_id`) match → always suppress** the whole memory (forget a chat/thread/event). Exact stable-id only: forgetting `gmail_thread/x` does **not** touch `gmail_thread/x@work` (stripping `@account` would over-match across accounts).
- **Identity (`handle`/`address`) match → suppress only a SOLE-COUNTERPARTY (1:1) memory** — the forgotten identity is the memory's *only* external counterparty. A group thread where the identity is one of many is **kept** (the data-loss / "layoff email" guard). Its per-participant redaction is deferred to the P16 graph-compile-time filter. For Gmail/Calendar the address set includes the user's own address, so identity suppression there only fires on a genuinely single-address thread — person-level email suppression needs self-identity (P13).

## Invariants & gotchas

- **The primary guard cannot be bypassed by a new sync, and its check-and-write is atomic against `forget`.** It lives inside `writeMappedMemory`, so all connector call sites (gmail/imessage/applecal) are covered without per-site edits, and it holds the governance lease (`governanceWriteLease`) across the suppression check *and* the `atomicWrite`. *Why:* a suppression the sync path doesn't read is silently reverted every hour — and a suppression that commits between an *unlocked* check and the write is missed and resurrects the atom (the connector-path TOCTOU, sibling of the filesystem #113 bug). A once-per-item fresh load alone closes only the stale-snapshot variant. Holding the lease across check→write closes the check-to-write window too.
- **Two write paths route around the primary chokepoint, so each carries its own guard — and both the connector and filesystem writes are lease-serialized against `forget`.** (1) `ingestFilesystem` renders memories **directly** (never through `writeMappedMemory`), so it re-reads the ledger **per file, under the governance lease** (`writeUnlessForgotten`) right at the write — otherwise a `forget --chat <src-id>` is undone by the very next filesystem walk. The lease is what serializes it against a *concurrent* forget: because the walker's check-and-write and `mora forget`'s suppression-append take the same `acquireGovernanceLock`, once a mid-walk forget's suppression is **committed**, every later walker reload sees it and skips — so no walk re-materializes the atom after the forget commits. A once-per-walk **snapshot** left a TOCTOU window here (a forget committing after the snapshot but before the write silently resurrected the atom — #113). Re-reading per file *under the lease* is the deliberate fix (an `O_EXCL` create/remove per written file). `writeMappedMemory` and `writeUnlessForgotten` now share the same `governanceWriteLease` primitive, so the **connector paths get the identical guarantee** — the check and the `atomicWrite` are one lease-held critical section (the "concurrent-safe governance ledger" claim holds for *all* connector + filesystem writes, not just the filesystem walk). Only `stable_id` (item) forgets reach filesystem memories: they carry no participant identity, so `--handle`/`--email` never target them. (2) `writeAttachmentMemories` (iMessage PDFs) mints `att_<hash>` ids that carry **neither** the parent's `StableID` **nor** its participant `Meta`, so it checks the *parent's* suppression before extracting. **Known limitations, tracked in [#115](https://github.com/pyranthus-hq/mora/issues/115), NOT closed here:** that parent check is a **one-shot** `shouldSuppressWrite` (it does not hold the lease across the per-attachment writes, so a parent `forget` racing the extraction can be missed), and a parent `forget` does **not cascade** to already-written `att_` memories (`memoriesMatching` keys on the attachment's own `att_` id/nil-meta, never the parent's atom), so they orphan on disk. *Why:* the "single chokepoint" is really three — the primary plus these two derived/direct siblings.
- **A narrower, pre-existing forget-side window is out of scope here and tracked in [#115](https://github.com/pyranthus-hq/mora/issues/115).** `cmdForget` computes its removal set (`memoriesMatching`) *before* it appends the suppression, and neither the scan nor the scan→append window holds the governance lease. A connector write landing in that window materializes a file that the scan missed; #113 guarantees it is never *re-created* after the commit, but this `forget` will not *remove* it (it lingers until the next `forget`/cleanup). Closing it needs `forget` to compute its removal set under the lease (scan+append atomic) — a forget-side change, not a write-path change.
- **Ledger writes are serialized across processes.** `appendGovernanceEntry` / `revokeGovernanceEntry` take a crash-safe file lease (`acquireGovernanceLock`, the same primitive as the `sources.json` lease — see [15 — Concurrency contract](./15-concurrency-contract.md)) and **reload inside the lease**, so two racing `mora forget`s (or a future scheduled `prune`) can never clobber each other's entry. The `.mora-governance.json.lock` lease is a `*.lock` file, excluded by the vault `.gitignore`, so a leftover lock never rides `mora sync git`. *Why:* a dropped suppression whose files were already removed is a silent resurrection — the exact lost-update the concurrency campaign closed for `sources.json`.
- **A corrupt ledger fails closed.** `loadGovernance` returns an error on unparseable JSON (never treats it as empty). The write path surfaces it — an item write fails, and the [honest-snapshot rule](./11-sync-and-freshness.md) means the sync does not stamp success. *Why:* silently ignoring the ledger would resurrect forgotten (possibly abusive) content — a privacy violation worse than a loud sync failure.
- **The ledger is vault-resident and rides `mora sync git`.** `<VaultDir>/.mora-governance.json` sits beside the `.mora-vault.json` identity marker. Being a **dotfile, not `*.md`**, `rebuildIndexWithPolicy` never parses it as a memory. Being outside the vault `.gitignore` (index.db/tokens/identity*/share/), it syncs to a user's other devices, so a forget on the laptop is not undone by the desktop (#52). Written 0600 via `atomicWrite`.
- **Local-only. Never touches the source.** `forget` removes the local copy and records a suppression. It issues no Gmail/Apple/Calendar write — the read-only-at-source guarantee ([AGENTS.md R3](./00-overview.md)) is intact. The CLI says so plainly.
- **Byte-identical rebuild is preserved.** The ledger writes nothing to the index. The removal set is a deterministic (sorted) scan. The suppression decision is an order-independent OR over active entries. A rebuild with the ledger present is identical to one without it (the dotfile is skipped).

## The CLI

```bash
mora forget --chat <stable-id>   # forget one conversation/thread/event
mora forget --handle <handle>    # forget a 1:1 iMessage counterpart
mora forget --email <address>    # forget a 1:1 email counterpart
mora forget list                 # show active suppressions
mora unforget <entry-id> --yes   # revoke a suppression
#   --dry-run  preview exactly which memories would be removed
#   --yes      required to actually remove (destructive)
```

A `forget` **records the suppression first**, then removes the matching files, then rebuilds the index — so a crash after removal can never leave files gone but re-ingestable. The removal set is computed with the *same* `decideSuppress` the write path uses, so what is removed now is exactly what stays suppressed on re-sync (never more). Authored notes carry no connector identity and are never matched (a plain `mora delete` already forgets them permanently — nothing re-ingests them). `unforget` revokes the entry so future syncs may re-ingest the content again, subject to the connector's lookback window (not a guaranteed restore of already-removed older content).

## Reconciliation with the iMessage deny-list

The pre-existing iMessage deny-list (`Source.DenyContacts`/`DenyConversations`, applied inside the fetcher) filters at **fetch** time — a denied conversation never reaches the write path. The governance ledger is the **write**-time generalization #52 asked for: cross-connector, mutable after setup, and keyed on the same source-native identities. The two are complementary layers (fetch-time is an optimization that avoids decoding. Write-time is the durable enforcement) and do not conflict.

## Related

- [01 — Data Model & Storage](./01-data-model-and-storage.md) — `writeMappedMemory`, the content-hash skip, tombstones vs `mora delete` resurrection.
- [03 — Person Entity Graph](./03-entity-graph.md) — `metaPairs`/`metaStrings` (the same `Meta` coercion), `canonicalizePersons`, precision-first non-merging.
- [11 — Sync & Freshness](./11-sync-and-freshness.md) — the upsert-only sync and honest-snapshot rule the guard rides on.
- [13 — Sharing](./13-sharing.md) — the sibling governance verb. Its footer anticipated this ledger.

## Deferred (not built here)

- **Person-level fan-out.** Resolving "a person" to all their aliases/handles/addresses needs the identity graph (P13). v1 is atom-level: `--handle`/`--email` act on the exact identity, sole-counterparty only.
- **Group-thread redaction.** Filtering a forgotten participant out of a *kept* group thread is a graph-compile-time step (P16). The `redact` entry kind is reserved for it.
- **`mora prune` (#53).** Size-driven, tombstone-based bounded retention shares this ledger (a `prune`/`source_scope` kind, `suppress` action) but is a separate verb; `compact` (true disk reclaim) is out of scope.
- **`merge_confirm` application.** The ledger persists a two-atom correction ("these identities are / are not the same person") keyed by stable-atoms, and it survives re-sync; *applying* it to the graph is the P13 confirm-queue.
- **MCP `forget_memory` verb.** The CLI is the surface here. The MCP sibling is governed by the [MCP contract](./06-mcp-server.md) budget gate and is deferred.
- **Residual forget-side TOCTOU + attachment cascade ([#115](https://github.com/pyranthus-hq/mora/issues/115)).** #113 closed the check-to-write window on the two primary write paths (`writeMappedMemory`, `writeUnlessForgotten`). Two narrower gaps remain, tracked in #115: (a) `cmdForget` computes its removal set before appending the suppression, so a file written in that (unleased) window is suppressed-from-recreation but not removed by that forget; (b) derived `att_` attachment memories carry no parent atom, so a parent `forget` neither cascades to them nor lease-guards their extraction. Both are pre-existing and out of scope for #113.
