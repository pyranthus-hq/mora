# 13 — Sharing (`mora share`)

`mora share` is the outbound face of memory governance (#51): publish **one scope of authored memories**, age-encrypted per recipient, to a **dedicated private git remote**; a subscriber decrypts it into a **read-only, separately-indexed corpus** that unions into `search`/`think` with owner attribution. This document describes what is built. Nothing here is roadmap; the deferred items are listed at the end.

## Files

| File | Lines | Responsibility |
|---|---|---|
| `internal/mora/share.go` | ~1500 | The whole subsystem: registry, keygen, export filter, staging/push, subscribe/pull/import, share index, query-time union, list/remove, path guard. |
| `internal/mora/share_test.go` | ~1300 | End-to-end coverage with real age crypto + the `fakeExec` git seam (no network, no git binary needed). |
| `internal/mora/gitsync.go` | 299 | Reused trust primitives: `execFunc`/`realExec` + `redactCredentials` (`gitsync.go:20-48`), `vaultRepoState` plain-`.git` guard (`gitsync.go:228`), `configureRemote` (`gitsync.go:265`), `commitIdentityArgs` (`gitsync.go:245`). |

## The two sides

**Publish** (`shareInit` `share.go:301`, `sharePush` `share.go:607`):

```mermaid
flowchart LR
    V[vault memories/ scope match] -->|collectShareMemories :201| X[export set]
    X -->|preview + confirm| E[age encrypt per recipient]
    E --> S[staging repo DataDir/share/publish/name]
    S -->|git add -A → ls-files hard-stop → commit → push origin HEAD| R[(private remote)]
```

**Subscribe** (`shareSubscribe` `share.go:987`, `sharePull` `share.go:1064`, `shareImport` `share.go:813`):

```mermaid
flowchart LR
    R[(private remote)] -->|clone / pull --ff-only| C[repo DataDir/share/subs/name/repo]
    C -->|decrypt + validate| P[corpus/*.md plaintext]
    P -->|rebuildShareIndex :921| I[(index.db per share)]
    I -->|unionSharedResults :1220| Q[search / think, owner-attributed]
```

State lives in three places: the grant registry `<ConfigDir>/shares.json` (`loadShares`/`saveShares`, 0600), the subscriber's age identity `<ConfigDir>/share/identity.txt` (`shareKeygen` `share.go:144`, 0600, never overwritten), and the publisher's local change-detection record `<StateDir>/share/publish/<name>.json` (`sharePushStatePath` `share.go:426`) — plaintext content hashes that deliberately never enter the repo (they would let a ciphertext holder confirm guessed plaintext).

## What may be exported

`collectShareMemories` (`share.go:201`) is the entire answer to "what can leave":

- Walks `VaultDir/memories` **only** — connector evidence under `sources/` is structurally out of reach (provider-derived IDs collide across vaults, `meta` carries participant PII, `att_` paths are machine-local).
- Frontmatter `scope` exact-match; the scope itself must match `^(personal|global|project:[A-Za-z0-9][A-Za-z0-9._-]*)$` **before** any filesystem access.
- Tombstones (`deleted_at`) and anything provider-stamped are skipped; a symlink anywhere in the tree aborts the export loudly; every selected file is re-verified (`resolveReal`) to resolve inside the memories root; ids must match `shareExportIDRE` (safe-filename charset) because they become filenames in every subscriber's corpus.

## Query-time union

`unionSharedResults` (`share.go:1220`) is called from exactly two seams: `defaultSearch` (`hybrid.go`) — covering `mora search` CLI and MCP `search_memory` — and `buildThink` (`think.go`). With zero subscriptions it returns the local slice **unchanged**, so the no-share path is byte-identical (the T0 MCP budget gate depends on this). With subscriptions it rank-fuses (RRF, `rrfWeighted`) the local list against each share's BM25 list: local arm weight 1.5 (the hybrid fusion's strongest-arm anchor), all shares together share 1.0 (`share.go` fusion constants) so multiple subscriptions cannot collectively out-vote the user's own vault. Results carry `Memory.Owner` (= subscription name, `omitempty`); `ThinkEvidence.Owner` and a `(shared:<owner>, …)` prompt line label think evidence.

Each share index is a schema-compatible subset (`memories` + `memories_fts`, `user_version` stamped) built by `rebuildShareIndex` (`share.go:921`) — FTS-only, no vectors/graph/entities — and opened only via `openShareIndexRO` (`share.go:961`): direct DSN with `query_only(1)` pragma, never `openIndexRO`, whose auto-heal would rebuild the file from the wrong (personal) vault.

## Invariants & gotchas

- **The subscriber's vault and identity graph are never mutated by a share.** Everything share-related lives under `<DataDir>/share/`; the personal index walk covers only `memories/`+`sources/`, `mora backup` tars only the vault, vault git-sync stages only the vault, and `delete_memory` can only reach the two vault roots. *Why:* #51's core guarantee — reading someone must never rewrite you.
- **`shareGuardPaths` (`share.go:1479`) refuses every share verb when the share root or identity dir resolves inside the vault.** `data_dir` is user-configurable and may be co-located with the vault; without the guard a subscriber's DECRYPTED corpus would ride the vault backup/git-sync. Resolution walks symlinks through the deepest existing ancestor (`resolveRealDeep`). *Why:* placement is the security boundary, so it is enforced at runtime, not assumed.
- **Encryption is mandatory and structural.** `push` refuses without a parseable recipient key (even a hand-edited registry), only `*.md.age` is ever written into staging, the staging `.gitignore` excludes `*.md`, and the post-`git add` `ls-files` hard-stop (mirroring `sync git`) refuses to commit tracked plaintext/db/token/identity files. `doctor` runs `share_staging_clean` and discloses every publish. *Why:* "unencrypted share" must be unrepresentable, not discouraged.
- **Preview before anything mutates.** `sharePush` prints the exact add/update/remove list (and `share preview` the full content) *before* the confirm gate; non-TTY without `--yes` is refused (`confirmSharePushFn` seam, mirroring `confirmVaultRepointFn`). *Why:* #51 P0 — the user sees exactly what leaves, every time.
- **Push state is recorded only after a successful `git push`.** A failed push re-publishes next run instead of silently leaving the remote stale; `push` always pushes even with no content changes for the same reason. Never `--force`; origin must match the registry remote (`sharePush` origin check) so a swapped origin cannot exfiltrate to an unapproved destination. *Why:* honest-snapshot rule applied to egress.
- **Imported repo content is untrusted input.** Non-regular files, unsafe names, id-spoofs (frontmatter id ≠ filename stem), scope mismatches vs the manifest, and >4 MiB memories all abort the import; the attribution label is the **subscriber-chosen** subscription name, never publisher-controlled manifest metadata. `pull` is `--ff-only`: a publisher history rewrite fails loudly with a re-subscribe pointer. *Why:* a chosen counterparty is still not a trusted code path.
- **Gap analysis stays local.** `buildThink` computes gaps from the pre-union local results only — "what the vault does not know" means the *user's* vault, and shared ids must never be compared against the personal retrieval trace or entity graph. *Why:* honest gaps are the trust feature; polluting them with foreign corpora would fabricate coverage.
- **Revocation is honest.** `share remove` stops future pushes and deletes local state; the message states plainly that git history is durable and subscribers keep what they pulled — rotation (new repo + new keys) is the way to cut future access. *Why:* docs-honesty rule; implying recall would be a lie.

## Related

- [01 — Data Model & Storage](./01-data-model-and-storage.md) — the frontmatter format shared files carry verbatim.
- [02 — Retrieval & Search](./02-retrieval-search.md) — `defaultSearch`, RRF fusion, the FTS arm the share index reuses.
- [07 — Synthesis](./07-synthesis-think-digest.md) — `think`'s evidence/gap envelope that now carries `owner`.
- [11 — Sync & Freshness](./11-sync-and-freshness.md) — `mora sync git`, whose trust model (redaction, hard-stop, never-force) this subsystem reuses on a dedicated repo.

## Open questions / unverified

- Fusion weights (1.5 local / 1.0 shared total, k=10) are principled but untuned — no T2-style eval discriminates local-vs-shared ranking yet.
- v1.1 per-share graph view (`mora graph --share <name>`) and the deferred items from #51 (connector-evidence snapshots, owner-namespaced merged graph, multi-writer, field-level redaction) are not built.
- The grant ledger is `shares.json`; #52 (forget) and #53 (prune) are expected to grow the same registry pattern into suppressions consulted at `writeMappedMemory` — not designed here.
