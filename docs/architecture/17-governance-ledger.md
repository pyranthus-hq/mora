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
| `internal/governance/store.go` | Durable primitive: canonical atom/entry/ledger DTOs, exact vault JSON load/save, fail-loud corruption, cross-process lease, reload-under-lock append/revoke, and identity normalization. It never renders or deletes Markdown and never imports Mora. |
| `internal/mora/governance.go` | Composition and policy adapters: active suppression/brief/merge projections, stable-atom derivation from memory metadata, attachment inheritance, and the lease-held Markdown write primitive used by connectors. |
| `internal/mora/governance_cmd.go` | The CLI: `cmdForget` / `cmdUnforget` / `forget list`, the lease-held vault scan and suppression append, and the confirm/dry-run gating. |
| `internal/mora/ingest.go` | Two lease-held guards: `writeMappedMemory` (the connector write chokepoint) holds the governance lease across its suppression check **and** its `atomicio.Write`, and `ingestFilesystem` — which renders directly and bypasses `writeMappedMemory` — does the same per file via `writeUnlessForgotten` (see gotcha below). |
| `internal/mora/pdf.go` | The derived-attachment path: parent provenance is stamped onto each attachment memory, and every derived write rechecks parent suppression under the governance lease. |

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

`writeMappedMemory` (the one boundary every connector routes through) consults the ledger before persisting anything — and does so while **holding the governance lease across the check and the write** (`governanceWriteLease`), so a concurrent `mora forget` cannot commit its suppression between the check and the `atomicio.Write` (the connector-path half of the #113 TOCTOU fix — see gotcha below):

```mermaid
flowchart LR
    MM[MappedMemory in] --> L[acquire governance lease<br/>+ load ledger]
    L --> G{ledger suppresses<br/>an atom of it?}
    G -->|corrupt ledger| ERR[return error<br/>fail-closed]
    G -->|yes| SKIP[return nil<br/>never persist]
    G -->|no| W[content-hash skip<br/>→ renderMemory → atomicio.Write<br/>all under the held lease]
```

The decision (`governance.decideSuppress`, pure over `(ledger, provider, stableID, meta)`):

- **Item (`stable_id`) match → always suppress** the whole memory (forget a chat/thread/event). Exact stable-id only: forgetting `gmail_thread/x` does **not** touch `gmail_thread/x@work` (stripping `@account` would over-match across accounts).
- **Identity (`handle`/`address`) match → suppress only a SOLE-COUNTERPARTY (1:1) memory** — the forgotten identity is the memory's *only* external counterparty. A group thread where the identity is one of many is **kept** (the data-loss / "layoff email" guard). Its per-participant redaction is deferred to the P16 graph-compile-time filter. For Gmail/Calendar the address set includes the user's own address, so identity suppression there only fires on a genuinely single-address thread — person-level email suppression needs self-identity (P13).

## Invariants & gotchas

- **The primary guard cannot be bypassed by a new sync, and its check-and-write is atomic against `forget`.** It lives inside `writeMappedMemory`, so all connector call sites (gmail/imessage/applecal) are covered without per-site edits, and it holds the governance lease (`governanceWriteLease`) across the suppression check *and* the `atomicio.Write`. *Why:* a suppression the sync path doesn't read is silently reverted every hour — and a suppression that commits between an *unlocked* check and the write is missed and resurrects the atom (the connector-path TOCTOU, sibling of the filesystem #113 bug). A once-per-item fresh load alone closes only the stale-snapshot variant. Holding the lease across check→write closes the check-to-write window too.
- **Direct and derived write paths carry the same guard.** `ingestFilesystem` bypasses `writeMappedMemory`, so it uses `writeUnlessForgotten` per file. PDF attachment extraction stamps the parent stable id, provider, and source atoms into each derived memory, then uses the same lease-held check-and-write primitive. A parent forget therefore removes already-written children and prevents a child whose extraction raced the forget from being published. For attachment files written by a pre-#115 binary, the removal scan reconstructs the legacy `att_<hash(parent-id:path)>` relation from the child's retained source path, so an upgrade followed immediately by forget also cascades without first requiring re-ingest. The early parent check remains only an extraction-cost optimization; correctness is decided at each derived write.
- **Forget's scan and suppression append are atomic against writers.** For an actual `--yes` operation, `cmdForget` acquires the governance lease, reloads the ledger, computes the exact removal set, and appends the suppression before releasing the lease. Connector, filesystem, or attachment writes can land before the scan and be removed, or after the suppression and be refused; none can land in between and linger.
- **Ledger writes are serialized across processes.** `appendGovernanceEntry` / `revokeGovernanceEntry` take a crash-safe file lease (`acquireGovernanceLock`, the same primitive as the `sources.json` lease — see [15 — Concurrency contract](./15-concurrency-contract.md)) and **reload inside the lease**, so two racing `mora forget`s (or a future scheduled `prune`) can never clobber each other's entry. The `.mora-governance.json.lock` lease is a `*.lock` file, excluded by the vault `.gitignore`, so a leftover lock never rides `mora sync git`. *Why:* a dropped suppression whose files were already removed is a silent resurrection — the exact lost-update the concurrency campaign closed for `sources.json`.
- **A corrupt ledger fails closed.** `loadGovernance` returns an error on unparseable JSON (never treats it as empty). The write path surfaces it — an item write fails, and the [honest-snapshot rule](./11-sync-and-freshness.md) means the sync does not stamp success. *Why:* silently ignoring the ledger would resurrect forgotten (possibly abusive) content — a privacy violation worse than a loud sync failure.
- **The ledger is vault-resident and rides `mora sync git`.** `<VaultDir>/.mora-governance.json` sits beside the `.mora-vault.json` identity marker. Being a **dotfile, not `*.md`**, `rebuildIndexWithPolicy` never parses it as a memory. Being outside the vault `.gitignore` (index.db/tokens/identity*/share/), it syncs to a user's other devices, so a forget on the laptop is not undone by the desktop (#52). Written 0600 via `atomicio.Write`.
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

A `forget` computes its removal set and records the suppression in one
lease-held critical section, then removes the matching files and rebuilds the
index. A crash after removal can never leave files gone but re-ingestable. The
removal set is computed with the *same* `decideSuppress` the write path uses, so
what is removed now is exactly what stays suppressed on re-sync (never more).
Authored notes carry no connector identity and are never matched (a plain
`mora delete` already forgets them permanently — nothing re-ingests them).
`unforget` revokes the entry so future syncs may re-ingest the content again,
subject to the connector's lookback window (not a guaranteed restore of
already-removed older content).

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
