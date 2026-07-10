# 18 — Merge confidence (`mora merge`)

Mora unifies the many source-native identities of one human — a Gmail address, a Calendar invitee, an iMessage phone handle — into a single canonical person in the [entity graph](./03-entity-graph.md). Because **a wrong-person merge is a severity-1 error** (it silently attributes one person's context to another), identity clustering runs in **three confidence tiers**, ordered by how *provable* the "same human" claim is. When the evidence is not provable, Mora **refuses to gap**: it leaves the identities unmerged rather than guessing.

## The three tiers

| Tier | Who decides | What qualifies | Signal |
|---|---|---|---|
| **AUTO** | the graph build, no user action | RULE 1 — addresses resolving to the same provider mailbox (`mailboxKey`; Gmail dot/`+tag` normalization). RULE 2 — PERSON-classified identities sharing a distinctive multi-token trusted name, **echo-corroborated** (a member address's local part spells the name), bridging ≤ `maxNameMergeClusters` clusters. | `same-mailbox`, `name-echo` |
| **CONFIRM** (one-tap) | the user, via the confirm-queue | cross-channel **email↔phone** candidates (see below). Proposed, **never** auto-applied. | `confirmed` |
| **REFUSE** (refuse-to-gap) | nobody — left unmerged | everything ambiguous: a too-common name, a name with no address signature, a single-token name, a service/org. | — |

RULE 1/2 live in `canonicalizePersons` (`graph.go`) and are **never loosened** — they are the precision floor. RULE 3 (CONFIRM) only ever *adds* a fusion a human explicitly authorized, so it can never introduce an *inferred* wrong-person merge.

## The email↔phone join

Across channels there is **no byte-provable shared token**: a phone handle carries no address for the graph to echo against, the way two email addresses corroborate each other in RULE 2. So Mora never auto-merges email↔phone. Instead it proposes candidates from the **default evidence path** (`emailPhoneCandidates`, `merge.go`), both signals required:

1. **address-book corroboration** — a phone handle carries a distinctive (multi-token) **trusted contact name**, resolved from the user's own macOS AddressBook, that an email PERSON also self-presents. The name bridges the two channels.
2. **shared-signature** — the email address's local part echoes ≥1 token of that name (`echoTokens`), so the address structurally corroborates the identity rather than relying on a spoofable display name.

Anything short of both is REFUSE, not a low-confidence merge: a name borne by more than `maxNameMergeClusters` identities is too common; a single-token name is not distinctive; an address with no echo corroborates nothing.

> The Phase-12 iMessage mention-edge is a **future enhancement** this consumes as an additional signal *when built* — it is **not** a prerequisite. The address-book + signature path stands alone.

A candidate is only ever a **proposal**. A wrong candidate is queue noise the user rejects; it is never a merge.

## The confirm-queue (keyed on source atoms)

Decisions ride the [governance ledger](./17-governance-ledger.md) as `merge_confirm` entries, keyed on the **source-native stable atoms** — `{imessage, handle, +1…}` and `{"", address, a@b.com}` — **never** the post-merge `person:` id, which moves as identities cluster (the #52 trap). This is what makes a confirmation survive the next connector re-sync.

| Command | Effect |
|---|---|
| `mora merge list [--json]` | the pending queue: proposed candidates minus every already-decided pair |
| `mora merge confirm --handle H --email A` | record a confirm, rebuild → the pair unifies |
| `mora merge reject --handle H --email A` | record a reject → the pair stays apart, never re-proposed |
| `mora merge undo <ledger-id>` | revoke a prior decision |

`governance.mergeDecisions()` resolves the ledger's `merge_confirm` entries into (a) the confirmed pairs the graph build applies and (b) the set of decided pairs the queue must not re-propose. Last-writer-wins per pair, so a reject after a confirm takes effect. The confirm application is **general** (it honors any explicit pair), while the auto-*proposer* is email↔phone-specific — so a manual `confirm` also serves as the escape hatch for a real pair the proposer refused.

## Provenance on every merge

Every applied fusion is recorded in the `person_merges` index table — `(member_a, member_b, signal, detail)` — so *"why is X the same as Y"* is durable and auditable (it feeds the trust model). Endpoints are the SOURCE person ids (pre-merge), never the canonical, so the record stays truthful about which atoms were joined. A dedicated table (not a graph edge) keeps merged-away member ids out of `get_entity`'s evidence and neighbor derivation.

## Invariants

- **Precision-first.** `canonicalizePersons` RULE 1/2 are byte-identical to before P13. Ambiguous → refuse-to-gap. The only cross-channel path is an explicit human confirm.
- **Zero-egress, deterministic.** All of the above is pure Go over local Meta + the vault ledger; no model, no network. The graph build (including confirmed merges and provenance) is byte-identical across rebuilds for a fixed vault + ledger — the ledger is part of vault state.
- **#52-safe.** Confirm/reject decisions and provenance are keyed on source atoms, so they survive re-sync.
