# 21 — Teach and human correction (`mora teach`)

Teach is Mora's local human-review plane. It turns a correction into durable
governance instead of editing a derived index or silently rewriting evidence.
Every accepted decision is stored in the vault governance ledger, can be
audited, and can be undone. A rebuild projects the same current state from the
same vault and ledger.

Teach is deliberately a CLI-only mutation surface. MCP and other restricted
agent APIs can read the resulting current state, but they cannot confirm an
identity, change a commitment verdict, revise an authored memory, grant
evaluation consent, or undo a human decision.

## Identity proposals

`mora teach identity` delegates to the existing merge queue; it does not build
a second Contacts or identity system.

```bash
mora teach identity list [--json]
mora teach identity confirm --handle <phone> --email <address> --yes
mora teach identity reject --handle <phone> --email <address>
mora teach identity undo <ledger-id>
```

Each proposal contains typed corroborating evidence (`address_book_name`,
`email_name`, and `address_signature`) and `affected_items`, the stable ids of
memories whose graph identity would change. `confirm` prints that preview and
refuses to write without `--yes`. Ambiguous or merely similar identities stay
separate.

## Commitment verdicts

Teach applies a human verdict after commitment extraction, lifecycle
resolution, and automatic de-duplication. Targeting uses the opening memory's
stable id and, when that memory contains more than one commitment, the
commitment id.

```bash
mora teach commitment not-a-commitment --memory-id <id> --yes
mora teach commitment wrong-person --memory-id <id> --person sam@example.com --yes
mora teach commitment wrong-direction --memory-id <id> --direction owed_by_self --yes
mora teach commitment already-closed --memory-id <id> --yes
mora teach commitment duplicate --memory-id <id> --duplicate-of <commitment-id> --yes
mora teach commitment useful --memory-id <id> --yes
```

The six verdicts respectively remove a false positive from the current
projection, correct the counterparty, correct ownership direction, close the
item with a governance citation, identify the canonical duplicate, or record a
positive review. `mora teach undo <ledger-id>` revokes the verdict and rebuilds
the prior projection.

## Authored-memory history

Source evidence from Gmail, Calendar, iMessage, and other connectors remains
immutable. A user can correct, supersede, or retract only an authored memory:

```bash
mora teach memory correct --id <id> --title "..." --text "..." --yes
mora teach memory supersede --id <id> --title "..." --text "..." --yes
mora teach memory retract --id <id> --yes
mora teach history --memory-id <id> [--json]
mora teach undo <ledger-id>
```

Correction and supersession create a new immutable authored-memory file and
link it to the original in the governance ledger. Retraction records a ledger
decision without deleting the evidence. Default read, list, search, context,
graph, digest, meeting, commitment, index, and share projections show only the
current revision. Undo restores the prior revision; revision chains must be
undone newest first.

## Decision validity

Decision memories can carry a structured validity contract:

```bash
mora write --type decision --scope project:acme --title "Ship OAuth" --text "..." \
  --as-of 2026-07-25T12:00:00Z \
  --durability working \
  --flip-conditions "security review fails;provider terms change" \
  --review-by 2026-08-25T12:00:00Z
```

`as_of`, `durability` (`provisional`, `working`, or `standing`), and at least
one flip condition are required for a complete validity contract. `review_by`
is optional. Legacy or incomplete decisions and decisions past `review_by` are
marked `needs_review`; digest surfaces also label them `[NEEDS REVIEW]`.
They remain queryable for human review, but cannot open or close commitments
and cannot enter meeting briefs as current obligation evidence. This check runs
at the surface clock, so crossing `review_by` fails closed without waiting for
another index rebuild.

## Consent-gated evaluation examples

Human corrections do not become evaluation data by default:

```bash
mora teach consent status
mora teach consent enable --yes
mora teach examples --json
mora teach consent disable --yes
```

Without explicit consent, export refuses. With consent, export contains only
the decision kind, verdict, undo state, and a deterministic ordinal reference.
It never emits timestamps, memory text, titles, source ids, commitment ids,
addresses, handles, identity-derived hashes, or other raw identities.

## Persistence and rebuild contract

`internal/governance` owns the pure replay projections for authored-memory visibility, active commitment feedback, teaching-entry history, and evaluation consent. Mora owns CLI authority, publication, rebuild ordering, and applying those projected decisions to index/graph output.

Teach entries use the same vault-resident `.mora-governance.json` ledger as
forget and identity decisions. Appends and revocations are serialized by the
governance lease. Each projection-changing operation first marks the index
dirty, commits the ledger decision, and performs a full rebuild. If rebuild
fails, the durable decision and dirty marker remain for recovery; it is never
reported as a successful fresh snapshot.

For a fixed vault, governance ledger, and evaluation instant, index, graph,
current-memory visibility, and commitment projections are deterministic.
Original evidence and revoked history remain auditable while ordinary reads
expose only current state.
