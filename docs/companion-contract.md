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

The published values are reachable through `VocabularyFamilies()` and `VocabularyFor(family)`, which
returns a **copy**. The vocabulary map itself is unexported: an exported map is writable by any
package in the process, and a vocabulary a caller can widen is not a stable enum
(`TestVocabularyIsNotWritableFromOutside`).

## 3. Strict inbound, tolerant outbound

This is the asymmetry to internalise.

- **The kernel decodes strictly.** `Unmarshal` sets `DisallowUnknownFields` and rejects an unknown
  field, trailing data, an oversize body, a wrong schema name, an unpublished enum value, or a
  malformed timestamp. `TestUnknownFieldIsRejected` runs it over every golden. "Trailing data" means
  a real end-of-input check: `json.Decoder.More` reports whether another element follows in the
  current stream and answers false on a stray `}` or `]`, so the decoder reads one more token and
  requires `io.EOF` (`TestTrailingTokensAreMalformed`).
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
| Fingerprint | `sha256:<64 lowercase hex>` | `Fingerprint(text)` is the published derivation: SHA-256 over the exact UTF-8 bytes. A client computes it and **the kernel recomputes it over the text it received**: a capture whose `payload_fingerprint` does not cover its own `text` is rejected, syntax notwithstanding. |
| Source key | at most 64 bytes, `[A-Za-z0-9_.:-]` | Applies to `freshness[].key` and `evidence[].source`. A freshness row rides inside the 4 KiB operation envelope, so an unbounded key is a way to push content through it. |
| Public key | `<alg>:<base64 of 32 bytes>`, `alg` ∈ `ed25519`, `x25519` | Both key types a paired device presents are 32 bytes, so a truncated or invented key fails at the boundary rather than at first use. |
| Scope | `personal` or `project:<name>` | Nothing else. A device cannot name a source. |

`payload_fingerprint` is what makes idempotency answerable: same key with the same fingerprint
returns the same receipt; **same key with a different fingerprint is `idempotency_conflict`**, never
a silent overwrite. Checking only the fingerprint's *syntax* would defeat that — two different
captures could then share one identity and the second would take the first's receipt — so
`TestCaptureFingerprintMustCoverItsText` states that case directly.

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
| Source key, public key | 64 B, 128 B |
| Retry cap (`attempt.max`, `operation.attempts`) | 10 |

Bounds are enforced **in both directions**: `Unmarshal` refuses an oversize body *before* decoding
it, and `Marshal` refuses to emit one. A decode-only check would leave a producer free to emit an
operation envelope past 4 KiB, and the peer that rejected it would be the only one to find out —
`TestMarshalEnforcesTheByteLimit` builds an envelope that is valid field by field and still too
large. `TestGoldensAreWithinTheirByteLimit` proves the published bounds are not aspirational.

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
- **A freshness row must agree with itself and with the payload carrying it**
  (`TestFreshnessRowsMustAgree`, `TestAgeIsCheckedAgainstTheCarryingPayload`):
  `never` implies `age_seconds` of `-1` and no `last_success_at`; `failed` must name an
  `error_code`; `fresh` must carry none; `fresh` and `stale` must carry a `last_success_at`, and
  their `age_seconds` must be **exactly** the distance from that timestamp to the payload's own
  `generated_at` — `created_at` for an operation, whose freshness is the grounding it was accepted
  against. No tolerance: both timestamps are already pinned to second precision, and the age is what
  a client renders as "15 minutes ago", so a number that drifts from the two timestamps beside it is
  the quietest way for a projection to lie.
- `freshness[].error_code` is a **frozen enum**, never the connector's prose: `auth_expired`,
  `permission_denied`, `network_unavailable`, `rate_limited`, `source_unavailable`,
  `not_configured`, `internal`. Connector error strings carry addresses, subject lines and
  filenames, and freshness rows travel inside the content-free operation envelope — so this being an
  enum is what keeps that envelope content-free by construction rather than by review.
- **An active device carries a `token_fingerprint`**; a pending one carries none, and a revoked one
  need not, because revocation deletes the credential.

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

### State is bound to policy

A receipt cannot describe an outcome its write policy could not have produced
(`TestReceiptStateIsBoundToPolicy`):

| Policy | Outcome |
|---|---|
| `readonly` | `rejected` with `reason: policy` |
| `propose` | `accepted` |
| `open` | `applied`, after vault publication |

The binding covers the **policy-determined** outcomes — those with no reason, or with
`reason: policy`. Every other reason (`unknown_device`, `too_large`, `idempotency_conflict` and the
rest) can occur under any policy, and those receipts are required to be `rejected` but are not bound
further. Binding them too would make an oversize capture unrepresentable under `open`, which is a
real case the capture path has to return.

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
required, enforced, and **itself capped at `MaxAttempts` (10)**: a retry cap the caller picks freely
is not a cap — a queue could set it to a million and the budget enforcement the async lane depends on
would be advisory. The receipt's `operation.attempts` carries the same ceiling, so it holds on both
paths (`TestRetryCapHasACeiling`).

A receipt carries this state machine as its `operation` block, which is a projection of the envelope
(`Operation.Status`), not a second source of truth (`TestStatusProjectsTheEnvelope`).

## 10. What this node does not decide

Stated plainly, so nobody reads a shape here as an approved mechanism.

- **The pairing code and bearer token formats are a human decision gate.** This schema fixes the
  envelope around them; `pairing_code` is an opaque string here. Never log a pairing payload — call
  `Redacted()`, which masks the code (`TestPairingRedactionMasksTheOneTimeCode`). The pairing
  `endpoint` *is* checked: it is parsed with `net/url`, must be `https`, or `http` only to a
  loopback host (`127.0.0.1`, `::1`, `localhost`), and must carry no userinfo. A prefix match is not
  sufficient and the difference is exploitable — `http://127.0.0.1:@evil.example/` begins with
  `http://127.0.0.1:` and resolves to `evil.example`, because everything before the `@` is userinfo
  (`TestLoopbackEndpointCannotBeSpoofed`).
- **Transport, listener, and device registry are not here.** The narrow loopback listener is graph
  node N12, the registry and `mora companion pair/list/revoke/status` are N11, governed capture and
  durable idempotency are N21 (§14), and the Tailscale Serve boundary is N22. This package is types,
  bounds, and validation only.
- **The async lane's transport is undecided** — encrypted relay against replica-read is a Wave 3
  decision. The envelope in §9 is what both options carry, which is why it is content-free.
- **This package imports only the standard library** and must never import `internal/mora`
  (`TestPackageIsALeaf`).

## 11. The listener (N12)

`mora companion serve` is the only server that speaks this contract. It is a **separate** loopback
HTTP listener from `mora serve http`, and the separation is the security boundary rather than a
packaging choice.

| | `mora serve http` | `mora companion serve` |
|---|---|---|
| Credential | one shared token in `~/.config/mora/http.json`, embedded in a web page | a per-device token from `mora companion pair`, revocable one device at a time |
| Surface | a `/call` escape hatch plus eight convenience routes, including `write` | three read routes |
| Audience | a sandboxed AI browser on the same Mac | a paired phone |

A token from either side is refused by the other. `TestCompanionListenerRefusesTheGenericLoopbackToken`
and `TestGenericLoopbackAPIRefusesADeviceToken` prove both directions.

### The routes

| Method | Path | Payload |
|---|---|---|
| `GET` | `/v1/companion/today` | `mora.companion.today` |
| `POST` | `/v1/companion/context` | `mora.companion.context.request` in, `mora.companion.context` out |
| `GET` | `/v1/companion/health` | `mora.companion.health` |
| `POST` | `/v1/companion/captures` | `mora.companion.capture` in, `mora.companion.receipt` out |

That is the whole allowlist, and it is a table in one file rather than a series of registrations, so
a test can walk it (`TestServerServesExactlyFourRoutes`). There is no `/call`, no delete, no sync, no
connector command, no configuration write and no read-a-memory-by-id route: a device that can name a
memory id can enumerate the vault, and a device that can name a tool has the generic API back under a
different name.

The capture route is the ONE write, added by N21 and covered in §14. There is deliberately no
`GET /v1/companion/receipts`: this contract publishes a receipt schema and no list-response schema
for one, so a listing route would have to invent an envelope. It is a later node's to add.

### What the listener refuses

- **Any address but `127.0.0.1`.** Not `localhost`, not `::1`, not `0.0.0.0` — one literal string,
  checked in `NewServer` and again in `Serve`. Publishing the listener beyond the loopback interface
  is N22's job. A `Host` header that is not the literal loopback address is a 403, which is the
  DNS-rebinding defense.
- **Every credential failure, identically.** No token, a malformed token, an unknown device and a
  revoked device all get the same 401 with the same body and no `WWW-Authenticate` header, so a
  caller holding a stolen token cannot classify it by probing. An operator learns which from
  `mora companion list`.
- **Oversize headers and bodies**, before the kernel is reached.

Authorization is checked twice: once in the guard chain, and again inside each handler. A middleware
chain is a claim about how a handler is mounted, and a mounting is one refactor away from being wrong.

### 200 never means fresh

Every projection carries the kernel's own freshness rows and its health summary, and a degraded index
or a dead connector still answers 200. The status code reports whether the request was served; the
body reports whether the answer can be trusted. A phone that renders only the status code would show a
confident empty screen during an outage.

Identifiers are derived, not passed through. A Mora stable id is `<kind>/<provider id>` — it carries
the provider, usually the account, and often the message id — so an evidence row ships
`mem_<32 hex>`, a deterministic one-way digest of it. The same memory is the same id across requests,
and nothing downstream can parse a provider out of it.

The listener logs nothing per request: not the token, not the query, not the body, not the answer.

### Mapping the kernel's index state onto `HealthState`

`index.state` and the top-level `state` are this contract's three-value vocabulary
(`healthy`/`degraded`/`unhealthy`). The kernel's own index health is a wider vocabulary, and the
listener must collapse it exactly this way rather than inventing a mapping:

| Kernel index state | `HealthState` | Why |
|---|---|---|
| `fresh` | `healthy` | The index matches the vault. |
| `dirty` | `degraded` | The index is behind the vault. Results are usable and incomplete, which is precisely `degraded`. |
| `degraded` | `degraded` | Straight through. |
| `failed` | `unhealthy` | The index could not be opened or does not match this build. |
| `never` | `unhealthy` | There is no index, so nothing can be retrieved. `never` must not collapse to `degraded`: an absent index is not a partial one. |

A projection whose `index.state` is `unhealthy` is still served — the contract's honesty rule is that
the client is told, not that the response is withheld.

Note that this is **not** the kernel's own aggregate collapse, which folds `dirty` into `unhealthy`.
The two answer different questions. The aggregate is `mora doctor`'s fail-closed verdict on whether
the vault is in a state anyone should trust; `index.state` is one arm of a projection that already
carries the aggregate beside it in the top-level `state`, and flattening a behind-but-usable index
into the same value as a missing one would cost the phone the distinction. `TestCompanionIndexStateFollowsTheContractTable`
pins the table.

## 12. Notes for Wave 1

### N11: canonicalizing and cryptographically validating public keys

`validatePublicKey` is a **format** check: an algorithm label, standard base64, and a 32-byte length.
That is the right depth for a schema package with no crypto dependencies, and it is not enough to
pin a device key. Before a key is pinned, the registry must:

- **Canonicalize.** Re-encode the decoded bytes and require the result to equal the input, so a
  non-canonical base64 encoding of the same key cannot be pinned as a second, different device.
  Compare pinned keys by their decoded bytes, never by their encoded string.
- **Validate cryptographically.** For `ed25519`, reject a point that is not a valid encoding and
  reject the low-order points; for `x25519`, reject the all-zero key and the known small-subgroup
  points. Thirty-two bytes of anything decodes; only some of it is a key.
- **Bind the algorithm.** A key pinned as `ed25519` is never usable as `x25519`, and the label is
  part of the pinned identity rather than a hint.

## 13. How the contract is enforced

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

## 14. Governed capture (N21)

`POST /v1/companion/captures` is the only route on this listener that changes the vault. Everything
below is enforced by a named test in `internal/companion/capture_test.go`,
`internal/companion/idempotency_test.go` or `internal/mora/companion_capture_test.go`.

### The policy is the gate, and the phone never holds the lever

The write policy is read from the vault's own configuration **on every request** — not captured when
the listener started, so `mora config mcp-write-policy readonly` takes effect without a restart
(`TestCompanionCapturePolicyIsReadPerRequest`). It is not a field on the capture, not a header, and
not a query parameter. The outcome is §8's table, and `Receipt.Validate` refuses any receipt that
describes an outcome its policy could not have produced.

| Policy | Receipt | What is on disk afterwards |
|---|---|---|
| `readonly` | `rejected`, `reason: policy`, `settled_at` set | nothing: no memory and no staged proposal |
| `propose` | `accepted`, no `memory_id`, **no `settled_at`** | one entry in the same pending queue `mora mcp proposals` lists |
| `open` | `applied`, `memory_id` and `settled_at` set | one memory in the vault |

`accepted` carries no `settled_at` on purpose: the capture is waiting for a human at the Mac, and
stamping it settled would say the question is closed.

**`applied` is a statement about the vault, not about the request.** The state flips only after the
kernel's governed write path returns — the same path `mora write` and MCP `write_memory` use, through
`createMemory`'s create-exclusive publish. The listener opens no second door into the vault, which is
why the pending-op marker, the index upsert and the authored-write reconciliation all still happen
for a phone capture.

The kernel stamps the provenance: `source` is `companion`, the type is an insight, and the title is
derived from the text. `scope` is the device's, because the schema already restricts it to `personal`
or `project:<name>`.

### Idempotency

Every capture carries a device-generated `idempotency_key`. The reservation for that key is written,
fsynced and renamed into place **before** the kernel is asked to write anything:

```
reserve (durable)  ->  vault write  ->  settle the receipt (durable)
```

That ordering is the whole design. Reserving after the write would mean a crash in between leaves a
memory nothing points at, so the phone's retry writes a second one.

| Case | Answer |
|---|---|
| same key, same `payload_fingerprint` | the **same receipt bytes** — returned from storage, same `receipt_id` |
| same key, different `payload_fingerprint` | `rejected`, `reason: idempotency_conflict`; the first payload keeps the key |
| same key, concurrently | one caller wins the claim; the others wait and read the settled receipt, or get `503 in_flight` |
| same key, after a crash before the write | the retry completes the reservation — one applied receipt, one memory |
| same key, after a process restart | the same answers: the reservation is a file, not memory |

Reservations live at `<state>/companion/captures/<device_id>/<digest>.json`, 0600 inside 0700, with
the same atomic-write-and-fsync discipline as the device registry. **The key space is per device**, so
two devices choosing one key never collide and no device can read another's receipt by guessing its
key. The filename is a digest of the key rather than the key itself.

The store is bounded three ways — `MaxReservations` (512), `ReservationTTL` (7 days), and a
per-record read bound — so a device that keeps talking cannot grow it without limit. Pruning drops
expired entries first, then the oldest **settled** ones: a settled entry is pure idempotency memory,
while a pending one is what stops a duplicate right now.

**What this does not promise.** There is a window between the vault publication and the settle write.
A process killed inside it leaves a `pending` reservation over a memory that does exist, and a retry
past the takeover window writes a second one. Closing it would mean the vault write itself carrying
the idempotency key, which is a change to the kernel's write path rather than to this listener. The
window is one fsync wide and it is stated rather than papered over.

### Status codes

Every **decodable** capture answers `200` with a receipt, rejections included. That is §11's rule
applied to a write: the status code says whether the request was served, the body says what happened
to the vault. `4xx` and `5xx` are reserved for the cases where there is no receipt to give — no
credential (`401`, opaque), an oversize body (`413`), a body that does not decode (`400`, carrying the
schema code and never the value), a busy or unreachable kernel (`503`, with `Retry-After`).

### What a capture may not do

- **Claim another device.** `device_id` is compared against the authenticated device and the receipt
  is stamped with the authenticated one. A mismatch is `rejected: unknown_device` and never reaches
  the vault.
- **Reach a lane this build cannot execute.** v1 serves `intent: remember` only; `ask` is the context
  route and `investigate` is the async lane, which has no worker yet. Both are
  `rejected: unsupported_lane`.
- **Echo its payload back.** A receipt is identifiers, a state, a policy, a fingerprint and two
  timestamps. `TestCaptureReceiptNeverEchoesThePayload` drives the real handler and fails on any word
  of the capture appearing in the response.
- **Outlive its revocation.** A revoked device gets the same opaque `401` as every other credential
  failure, so a reservation it left pending is never completed by anyone.
- **Escape the tighter bound.** Capture caps its body at `MaxCaptureBytes` (24 KiB), not at the
  guard chain's `MaxRequestBytes` (64 KiB).

Capture goes through the same one-at-a-time work budget every read does, and stamps `last_seen_at`
only on a `2xx` — a last-seen stamp records that a device was served.
