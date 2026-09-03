# The Mora companion contract

This is the published contract between Mora's kernel and a paired phone. It is the one page to read
before writing a client — a Swift app, a shortcut, a test harness, or the async worker that picks
work off the lane.

Every schema here lives in `internal/companion`, every claim on this page is enforced by a named test
in that package, and every payload has a frozen document under
`internal/companion/testdata/v1/`. Where a capability does not exist yet, this page names the node
that will deliver it rather than describing it as present.

Code is cited by file plus function or constant name, never by line number.

---

## 1. What this contract covers, and what it deliberately does not

Mora owns memory: the vault, the index, connectors, evidence, retrieval, freshness, and write
governance. This contract is that surface and nothing else.

A shell — the iOS app, or any other — owns presentation, model invocation, per-prompt consent,
conversation history, and its own features. **The kernel never returns a model-generated answer.** It
returns a context bundle: evidence, citations, gaps, freshness, and a synthesis prompt. The shell
runs the model. `TestContextBundleIsNotAnAnswer` asserts the bundle has no field an answer could hide
in.

Not covered, on purpose: attachments and images, push notifications, team scopes, a phone-side index,
and any phone-side vault replica. The Mac is the only writer.

## 2. The envelope

Every payload carries two keys, merged into the object rather than wrapping it, exactly as the
[machine contract](architecture/22-cli-contracts.md) defines them:

| Key | Type | Meaning |
|---|---|---|
| `schema` | string | The payload's published name, e.g. `mora.companion.today`. |
| `schema_version` | integer | A MAJOR-only version. It is `1`. |

**Adding a field is MINOR.** `schema_version` does not move; read the fields you know and ignore the
rest. **Removing a field, renaming one, or changing its type is BREAKING** and requires a
`schema_version` bump and a new `testdata/v<N>/` directory.

New *enum values* are also MINOR. A client must render an unknown enum value as its raw string rather
than failing. The kernel is the exception in the other direction: it rejects an enum value it does
not know on anything a device sent, because it is the authority on what it can execute.

## 3. Strict inbound, tolerant outbound

This is the asymmetry to internalise.

- **The kernel decodes strictly.** `Unmarshal` sets `DisallowUnknownFields` and rejects an unknown
  field, trailing data, an oversize body, a wrong schema name, an unpublished enum value, or a
  malformed timestamp. `TestUnknownFieldIsRejected` runs it over every golden.
- **A client decodes tolerantly.** Fields will appear that your version has never seen.
  `TestAddingAFieldIsSafeForAPinnedConsumer` injects a field nothing emits and proves a pinned
  consumer's view is byte-identical afterwards, so "adding is safe" is measured rather than assumed.

## 4. Timestamps

Every timestamp is **RFC3339, UTC, second precision, `Z` suffix, no fractional part**.

That is narrower than RFC3339, and the narrowness is the point: it is what Swift's
`ISO8601DateFormatter` parses with default options, so Go and Swift decode the same golden without
per-field date strategies. It also sorts lexicographically, which is how the kernel checks that an
operation was not updated before it was created. `TestTimestampFormatIsNarrow` rejects a fractional
part, a numeric offset, and a lowercase `z`.

## 5. Identifiers, keys, and fingerprints

| Thing | Shape | Rule |
|---|---|---|
| Identifier | `<prefix>_<opaque>` | Prefixes `dev_`, `req_`, `rcp_`, `mem_`, `exe_`. At most 64 bytes, characters `[A-Za-z0-9_.:-]`. **The opaque part is not parseable** — nothing may be read out of it. |
| Idempotency key | device-generated, stable across retries | At most 128 bytes, same character set. Spaces and prose are rejected: the key travels into the content-free operation envelope, so it may not become a second text field. |
| Fingerprint | `sha256:<64 lowercase hex>` | `Fingerprint(text)` is the published derivation: SHA-256 over the exact UTF-8 bytes. A client computes it, the kernel recomputes it. |
| Scope | `personal` or `project:<name>` | Nothing else. A device cannot name a source. |

`payload_fingerprint` is what makes idempotency answerable: same key with the same fingerprint
returns the same receipt; **same key with a different fingerprint is `idempotency_conflict`**, never
a silent overwrite.

## 6. Bounds

Every bound is enforced by `Validate` or by `Unmarshal`, never left to the caller.

| Bound | Value |
|---|---|
| Capture body / capture text | 24 KiB / 16 KiB |
| Any other request body | 64 KiB |
| Any projection | 4 MiB |
| **Operation envelope** | **4 KiB** |
| Today items | 3 |
| Evidence per Today item / per context bundle | 5 / 32 |
| Gaps, freshness rows | 16, 32 |

`TestGoldensAreWithinTheirByteLimit` proves the published bounds are not aspirational, and
`TestOversizePayloadIsRejectedBeforeDecoding` proves an oversize body is refused *before* it is
decoded rather than after.

## 7. The schemas

Ten published names, sixteen frozen documents — the extra six are the honest cases (rejected,
accepted-under-`propose`, revoked, failed, done, research-lane), because a client that only ever
decoded the happy path has not been tested.

| Schema | Direction | What it is |
|---|---|---|
| `mora.companion.device` | out | A paired device. Carries a `token_fingerprint`, **never a token**. |
| `mora.companion.pairing` | out | The QR payload: endpoint, one-time `pairing_code`, expiry, host fingerprint. |
| `mora.companion.pairing.confirmation` | in | The phone's reply: label, platform, public key. |
| `mora.companion.today` | out | `generated_at`, health strip, at most three items each carrying evidence, freshness, `truncated`. |
| `mora.companion.context.request` | in | `mode` (`think`/`search`/`meeting_prep`), query, optional scope. |
| `mora.companion.context` | out | Evidence, **gaps**, freshness, synthesis prompt. Not an answer. |
| `mora.companion.capture` | in | Verbatim text, requested lane, intent, scope, fingerprint. |
| `mora.companion.receipt` | out | The terminal record of one capture, plus the job state when it went async. |
| `mora.companion.health` | out | Overall state, write policy, index health, per-source freshness. |
| `mora.companion.operation` | both | The async operation envelope. See §9. |

### Rules a client can rely on

- An array-valued field is never `null`. An empty collection is `[]`
  (`TestEmptyCollectionsAreNeverNull`).
- Every projection carries `generated_at` (`TestEveryProjectionCarriesGeneratedAt`). A projection is
  displayed with its age and its freshness, or it is not displayed.
- `truncated` on Today is part of the claim: three of three is not three of nine.
- Every Today item carries at least one evidence row (`TestTodayItemMustCarryEvidence`). A claim you
  cannot check is not shippable.
- A context bundle always carries `gaps`, even empty — what the vault does not know is a claim, not
  an omission.

## 8. Capture, receipts, and the word "Saved"

A device declares an `intent`; the kernel decides what runs. `KindFor` is the only supported routing
path, and `requested_lane` is a request, never an authorization.

| Intent | Kind | Lane |
|---|---|---|
| `remember` | `capture` | `memory` |
| `ask` | `ask` | `memory` |
| `investigate` | `research` | `research` |

Any other pairing is rejected at the boundary (`TestRoutingMatrixIsEnforced`).

A receipt is terminal and there is exactly one per capture. **Rejections are receipts, not drops.**

| State | Means | Enforced |
|---|---|---|
| `accepted` | Staged for local approval under the `propose` policy. **Nothing is in the vault.** | `memory_id` must be absent. |
| `applied` | The memory exists in the vault. | `memory_id` and `settled_at` are required. |
| `rejected` | It will never be applied. | `reason` is required, `memory_id` must be absent. |

A client may show **"Saved" only for `applied`.** The schema is what makes that checkable:
`TestAppliedReceiptMustNameItsMemory` and `TestAcceptedReceiptCannotClaimAMemory` make an applied
receipt that names no memory, and an accepted one that claims a memory, both impossible to construct.

Rejection reasons: `policy`, `unknown_device`, `revoked_device`, `too_large`, `malformed`,
`unsupported_lane`, `idempotency_conflict`, `unavailable`, `internal`.

Write policy maps as you would expect and as the kernel already behaves: `readonly` → `rejected:
policy`, `propose` → `accepted`, `open` → `applied` after vault publication.

## 9. The async operation envelope

This is the one seam between the companion program and the agent spine. Two properties make it a
seam rather than a leak, and both are tests rather than intentions.

**It is content-free.** The envelope carries a *reference* to the payload — an opaque ref, a
fingerprint, a byte count, a media type — and never the user's words. A queue, a lease table, a
sweeper log, and a phone's Activity list can all hold envelopes without holding anything the user
wrote. `TestOperationCarriesNoUserText` marshals the envelope and fails on any word from the capture
it refers to; the 4 KiB bound leaves no room for a smuggled paragraph.

**It names no executor.** There is no worker, host, model, provider, or lease-owner field. The phone
learns that a job is running and nothing about what is running it, so the runtime can change without
a client release and without a privacy story to rewrite. `TestOperationNamesNoExecutorAndNoContent`
walks the struct, so a field added later fails even if no fixture populates it.

### The job state machine

```
captured -> triaged -> leased -> running -> done | needs_input | failed
```

- `leased -> triaged` and `running -> triaged` are lease expiry: a lapsed claim returns the job to
  the pool. **No operation is ever silently claimed forever.**
- `done` and `failed` are terminal. `needs_input` is **not** — it is waiting for a human and it
  resumes. A shell must render it differently from `running`: a spinner over a question nobody was
  asked is the dishonest case.
- A same-state transition is legal, so a repeated update or a re-delivered heartbeat is idempotent.
- A terminal operation carries a `result`; a live one does not. A `done` result names its receipt and
  carries no error code; a `failed` result names an error code and no memory.

`ValidateTransition` is the whole machine, and `TestStateMachineIsFrozen` pins it. `attempt.max` is
required and enforced: an operation without a cap could retry forever.

A receipt carries this state machine as its `operation` block, which is a projection of the envelope
(`Operation.Status`), not a second source of truth (`TestStatusProjectsTheEnvelope`).

## 10. What this node does not decide

Stated plainly, so nobody reads a shape here as an approved mechanism.

- **The pairing code and bearer token formats are a human decision gate.** This schema fixes the
  envelope around them; `pairing_code` is an opaque string here. Never log a pairing payload — call
  `Redacted()`, which masks the code (`TestPairingRedactionMasksTheOneTimeCode`).
- **Transport, listener, and device registry are not here.** The narrow loopback listener is graph
  node N12, the registry and `mora companion pair/list/revoke/status` are N11, governed capture and
  durable idempotency are N21, and the Tailscale Serve boundary is N22. This package is types,
  bounds, and validation only.
- **The async lane's transport is undecided** — encrypted relay against replica-read is a Wave 3
  decision. The envelope in §9 is what both options carry, which is why it is content-free.
- **This package imports only the standard library** and must never import `internal/mora`
  (`TestPackageIsALeaf`).

## 11. How the contract is enforced

`internal/companion/testdata/v1/<schema>.json` holds one frozen document per fixture, and:

| Test | What it proves |
|---|---|
| `TestGoldenCorpusIsFrozen` | The committed bytes are still what the builders emit. |
| `TestGoldenCorpusIsComplete` | A published schema with no golden fails, so nothing escapes the gate. |
| `TestGoldenFieldSetIsFrozen` | The removal and retype gate: a dropped, renamed, or retyped field fails against a literal key list held in the test file. |
| `TestGoldensDecodeStrictAndValidate` | Every golden survives the exact path the kernel uses on inbound bytes, and round-trips byte-identically. |
| `TestVocabularyIsFrozen` | Every published enum value is pinned. |
| `TestPackageIsALeaf` | The package imports only the standard library. |

**Regeneration cannot be used to drop a field.** `MORA_UPDATE_COMPANION_GOLDEN=1` rewrites the
documents under `testdata/v1/`, but the frozen key list lives in the test file, so a removal still
fails and can only be made by editing a test — which is the reviewable act it should be.

Swift decodes the same documents, byte for byte, under graph node N14. A fixture only Go can read
proves nothing.
