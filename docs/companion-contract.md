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

Eleven published names, seventeen frozen documents — the extra six are the honest cases (rejected,
accepted-under-`propose`, revoked, failed, done, research-lane), because a client that only ever
decoded the happy path has not been tested.

| Schema | Direction | What it is |
|---|---|---|
| `mora.companion.device` | out | A paired device. Carries a `token_fingerprint`, **never a token**. |
| `mora.companion.pairing` | out | The QR payload: endpoint, one-time `pairing_code`, expiry, host fingerprint. |
| `mora.companion.pairing.confirmation` | in | The phone's reply: label, platform, public key. |
| `mora.companion.pairing.grant` | out | The answer to a confirmation. The **only** document that carries a bearer token. See §15. |
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
| `POST` | `/v1/companion/pairing/confirm` | `mora.companion.pairing.confirmation` in, `mora.companion.pairing.grant` out — **no bearer token**, see §15 |

That is the whole allowlist, and it is a table in one file rather than a series of registrations, so
a test can walk it (`TestServerServesExactlyFiveRoutes`). There is no `/call`, no delete, no sync, no
connector command, no configuration write and no read-a-memory-by-id route: a device that can name a
memory id can enumerate the vault, and a device that can name a tool has the generic API back under a
different name.

The pairing route is the ONE route reachable without a device token, added by N12b and covered in
§15. It cannot present a credential, because it is the request that asks for one. `Public` is a
column of the same route table rather than a second list, so a route cannot become unauthenticated
without the literal expectation in `TestServerServesExactlyFiveRoutes` changing, and
`TestExactlyOneRouteIsUnauthenticated` walks the table and proves every other route still answers
401 with no header at all.

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
  `mora companion list`. The pairing route joins the same answer: a wrong code, an expired code, a
  replayed confirmation, an unknown device id and a listener with no pairing window open at all are
  one refusal, byte for byte
  (`TestPairingConfirmAnswersEveryCredentialFailureIdentically`).
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

### Publishing it to a tailnet (N22)

`mora companion expose` prints the exact commands that publish the loopback listener over the
tailnet. What it prints is: the resolved listener URL, the public URL, the `--allow-host` value, the
count of active devices, `funnel: off`, the two commands to run in order, the targeted command that
stops publishing, a warning about `tailscale serve reset`, and a short explanation of why
`--allow-host` is needed. What it does **not** print is any secret: it never runs `tailscale`, never
reads the tailnet, and never prints a token, a pairing code or a fingerprint — it reads the
registry's COUNTS and nothing more.

Every argument in every printed command is wrapped in POSIX single quotes, including arguments that
need no quoting. Inside single quotes a POSIX shell interprets nothing, so a printed line can only
ever be one command with literal arguments. That is a safety property rather than a style: it holds
even if a future change lets an unvalidated value reach the renderer.

```
mora companion expose --hostname <this Mac's tailnet name>
```

It refuses in two cases. With no ACTIVE device it exits non-zero with `data.not_found` and names
`mora companion pair`: publishing a listener that nothing can authenticate to puts a port on your
tailnet for no one. A device that has been paired but not yet confirmed does not count, because the
listener answers 401 to everything it sends. With a listener port of zero it exits non-zero rather
than printing a command containing `:0`.

The output is two commands, in order, plus the targeted command that undoes them:

```
'mora' 'companion' 'serve' '--port' '7778' '--allow-host' '<NAME>'
'tailscale' 'serve' '--bg' '--https=443' 'http://127.0.0.1:7778'

'tailscale' 'serve' '--https=443' 'off'
```

`tailscale serve reset` is printed as a **warning**, not as a step. It removes every serve mapping on
the node, including one another tool created, so it is the wrong instrument for undoing this
publication and the targeted `off` above is the right one.

The same distinction is structural in `--json`. `off_command` is a top-level key; there is **no**
top-level `reset_command`. The reset lives under a `destructive` object whose only other key is a
`warning` sentence, so a machine caller scanning the top level for a command to run cannot reach the
global reset by mistake:

```json
{
  "off_command": ["tailscale", "serve", "--https=443", "off"],
  "destructive": {
    "reset_command": ["tailscale", "serve", "reset"],
    "warning": "removes EVERY serve mapping on this node, including ones another tool created; use the targeted off command instead"
  }
}
```

No Funnel command is ever printed. Funnel publishes to the public internet, and nothing in this
contract is meant to leave the tailnet. `expose` states `funnel: off` rather than omitting it, so a
reader looking for the Funnel command finds the answer instead of assuming it was forgotten.

#### Why `--allow-host` exists

Serve terminates TLS in `tailscaled` and reverse-proxies to the loopback backend, forwarding the
client's `Host` header **verbatim**. Measured against Tailscale 1.102.3 on macOS, a request to the
published node name arrives at the backend as:

| What the backend sees | Value |
|---|---|
| `Host` | the published node name, with the port unless it is the scheme's default |
| `X-Forwarded-Host` | identical to `Host` |
| `X-Forwarded-For` | the tailnet address of the real client |
| `RemoteAddr` | `127.0.0.1` |
| extra headers | `Tailscale-User-Login`, `Tailscale-User-Name`, `Tailscale-User-Profile-Pic` |

The listener's DNS-rebinding guard requires the literal loopback address in `Host`, so before N22 a
paired phone behind Serve got `403 forbidden_host` on **every** route. This is the first of N04's
three product-breaking findings, and it is confirmed rather than theoretical.

`--allow-host` is the whole fix, and it is deliberately the smallest one:

- **Opt-in.** Empty is the default and means the loopback-only behaviour is unchanged, byte for byte.
- **One value, and it must be a DNS name.** The grammar is an ALLOWLIST, not a list of forbidden
  characters: `<RFC 1123 hostname>[:port]` or `[<IPv6 literal>][:port]`, where a hostname is at most
  253 characters of dot-separated labels, a label is 1 to 63 ASCII letters, digits and hyphens and
  may not begin or end with one, and a port is 1 to 65535. A scheme, a path, a space, userinfo, a
  wildcard, a comma, a trailing dot or a shell metacharacter is refused **before** any value reaches
  a command line or a `Host` comparison — a blocklist was the first shape and was wrong, because it
  admitted `node.example;id`. The value is compared case-insensitively against `Host` as a whole
  string: no suffix rule, no list. A value that could never match is a failure to START rather than a
  403 an operator debugs against a phone.
- **Still loopback-only at the socket.** The bind is unchanged and still refuses any address but
  `127.0.0.1`.
- **Still loopback-only at the peer.** A request carrying the published name is admitted only when
  the connection's peer is a loopback address. Serve dials the backend from `127.0.0.1`. Be precise
  about what this buys: a loopback peer does **not** prove a request came through Serve — any process
  on the Mac can open the port and type the published name in, exactly as it could already type
  `127.0.0.1` in. It is worth having for one narrower reason: if the bind is ever widened, by this
  code or by a port forwarder someone runs, the published name stops being sufficient on its own.
  Proving *who* is asking is the device bearer token's job, and it is the only thing here that does
  it.
- **Compared as ASCII.** The fold is A-Z with a-z and nothing else. `strings.EqualFold` applies
  Unicode simple folding, under which U+212A KELVIN SIGN folds to `k` and U+017F LATIN SMALL LETTER
  LONG S folds to `s`, so a `Host` that is not this name byte-wise would compare equal to it — and
  an absolute-form request URI lets a client put that authority into `Host` directly. A MagicDNS name
  is ASCII, an internationalized name reaches the wire as punycode which is also ASCII, and a
  non-ASCII `--allow-host` is refused at startup.
- **Still one credential.** The device bearer token. The `Tailscale-User-*` headers are a real signed
  assertion about a tailnet USER and are worth nothing here, because this listener answers to a
  paired DEVICE. Neither they nor `X-Forwarded-For` are read for authentication, for throttling, or
  for logging.

The value belongs to the operator's machine, not to this repository: it is supplied on the command
line (or from the invocation the operator keeps in their own configuration), and no real tailnet
name appears in the code, the tests, the goldens or these docs. When `expose` is run without
`--hostname` it prints a placeholder that the listener **refuses at startup**, so a command pasted
without editing fails closed.

#### The throttle finding, which is recorded and not solved

Behind Serve the client IP does not survive the proxy: every device looks like `127.0.0.1` to the
listener. A per-IP throttle would therefore collapse into a single bucket shared by every phone.
That is N04's second finding and it stands. The listener's work budget is per-process
(`maxInFlightKernelCalls`), which is honest about what it bounds; nothing here pretends to
rate-limit per device from a network address. The real client address exists only in
`X-Forwarded-For`, which is attacker-settable on any path that is not the proxy, so it is not read.

N04's third finding — revoke must fail closed — is unchanged by publication: revocation is a
registry state change, and an unknown, revoked or malformed token gets the same 401 whether the
request arrived over loopback or through Serve.

#### Proving it on a live machine

`scripts/companion-network-audit.sh` runs against a live `mora companion serve` plus a live
`tailscale serve` and proves the boundary rather than asserting it:

| Probe | What it proves | Runs today |
|---|---|---|
| A | the listening socket is `127.0.0.1:<port>` and nothing else | yes |
| B | exactly one Serve handler, it proxies to this listener, no raw TCP forward, Funnel off — read from `tailscale serve status --json` and parsed structurally | yes |
| C1 | a tailnet request reaches the listener and gets the opaque 401 | yes |
| C2 | **every** (interface, address) pair on this Mac EXPLICITLY refuses the connection | partly — see below |
| D1 | the log holds no bearer token, no pairing code and no device id, all of which the session really sent | yes |
| D2 | the log does not name the published host | yes |
| D3 | after a served, decoded request the log still holds no prompt, answer or vault text | **BLOCKED** |
| D4 | an authenticated route call is served (200) and its projection decodes | **BLOCKED** |
| E | the peer the listener sees is loopback (the throttle finding above), with a no-`--allow-host` control that returns `403 forbidden_host` | yes |

A probe is PASS, FAIL or **BLOCKED**, and BLOCKED is the honest answer to a claim nothing exercised.
D3 and D4 are blocked today: `Registry.Confirm` has no production caller, so no device can reach the
ACTIVE state, every request the script can make is refused at the auth guard, and nothing decodes a
body, runs retrieval or produces an answer. Their absence from the log would therefore prove nothing.
Any blocked probe makes the final verdict **PARTIAL**, which is not PASS and exits 3.

There is deliberately no Mac-side shortcut that confirms a pairing to unblock them. A pairing is
proven by the phone with its one-time code; the kernel-side confirm route is a separate node. A local
backdoor would fake the exact evidence this audit exists to collect.

C2 deserves a note, because it was the probe most easily faked. It enumerates **(interface, address)
pairs**, not addresses: the same address on two interfaces is two endpoints, and a link-local IPv6
keeps its scope id all the way into the URL, where the `%` is percent-encoded as `%25` per RFC 6874
and the interface is handed to curl. The earlier version dropped the scope id, deduplicated addresses
independently of interfaces, and flattened every curl error to the same value as a refusal, so an
endpoint it could not route to was counted as an endpoint it had proved closed.

It now has three outcomes, and only one of them is proof:

| Outcome | What it means | C2 |
|---|---|---|
| **refused** | curl exit 7 *and* a refusal in curl's own message | the port is closed there |
| **responded** | the address answered HTTP at **any** status — 200 and 401 alike | **FAIL**: the port is reachable off loopback |
| **unreachable** | a timeout, no route, an unreachable network, or an exit 7 whose wording matches neither vocabulary | **BLOCKED**: no answer either way, so nothing is proved |

The refusal test is **anchored to curl's own message line** — it must begin `curl: (7) ` — and it is
two-directional. Substring-matching the whole of stderr was wrong: any text that happened to contain
"refused", from any component, about any host, passed as a refusal of *this* address. It accepts exactly **two** wordings, both verified against the
libcurl that produces them: `Connection refused`, the operating system's `strerror` for
`ECONNREFUSED` and what Linux prints, and `Couldn't connect to server`, libcurl's own text for
`CURLE_COULDNT_CONNECT` from `curl_easy_strerror` in `lib/strerror.c` and what macOS prints. A third
spelling, `Could not connect to server`, was accepted and has been **removed**: it is not a string
libcurl emits. Checked against the exact library the live audit measures (curl 8.2.1,
libcurl/8.2.1) — `strings libcurl.4.dylib | grep -c "Couldn't connect to server"` is 1 and the same
grep for `Could not connect to server` is 0. An exit 7 whose message matches *neither* the refusal
nor the unreachable vocabulary is recorded as unreachable rather than guessed at, so a future change
to curl's wording degrades to BLOCKED and can never become a silent pass.

**A proxy is the sharpest way to fake this probe, so it is locked out three times.** A configured
proxy that refuses connections answers exit 7 with the refusal wording verbatim for *every* URL, and
C2 would report that every address on the machine refused without a single packet having reached any
of them. The script clears `http_proxy`/`https_proxy`/`ALL_PROXY`/`NO_PROXY` and their variants
before anything runs; every probe curl is given `-q` (ignore `~/.curlrc`) and `--noproxy '*'`; and a
message naming a proxy is classified as unreachable even if it carries the refusal wording. A
self-test fixture asserts the argv *contract* rather than trusting this paragraph: the stub refuses
only when `-q` is curl's **first** argument and `--noproxy` is **immediately followed by** `*`, and
answers 200 otherwise. Both positions carry weight — curl applies `~/.curlrc` at the point `-q` would
have appeared, so a `-q` that is not first leaves a window in which the operator's own defaults are
already in effect, and `--noproxy` takes a value, so a `--noproxy` followed by another flag disables
proxying for the wrong host list and swallows that flag as its value.

A reply with **no status line but a non-empty body** — HTTP/0.9, or anything that is not HTTP at all
— counts as *responded*, not as unproven. Something is listening; it simply did not speak HTTP/1.

**The unreachable set is not empty on a normal Mac, and that is expected.** The `fe80::` link-locals
on `awdl0` (AirDrop) and on the `utun` interfaces never answer at all, and a tailnet IPv6 is
blackholed by `tailscaled` rather than refused; a connect to any of them simply hangs. Those
endpoints are named individually with their interface, address, zone and curl exit status, and they
make the run PARTIAL. Calling them a failure would keep the audit permanently red for a reason that
has nothing to do with the listener; calling them a refusal was the original bug.

Every probe fails closed: a missing tool, an empty command output or an unparseable line is a FAIL,
never a skip. `--self-test` drives the same probe functions against fixtures that must fail —
including a real listener bound to `0.0.0.0`, a log line containing the session token, a registry
holding only a pending device, real `ifconfig` text carrying a duplicated address and three zoned
link-local addresses, curl stubs returning a timeout, a 200 and a 401, a stub that refuses only the
unzoned URLs (the exact shape the old code passed on), and the counter set `(8 passed, 0 failed,
2 blocked)` which must render PARTIAL and never PASS.


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

**A policy that cannot be read is `readonly`.** A malformed or unreadable `config.toml` produces
`rejected: policy` — a terminal receipt at 200 — and the reservation settles
(`TestCompanionCaptureUnreadableConfigFailsClosed`, `TestCapturePolicyReadFailureFailsClosed`).
Returning an error instead was two defects in one: the phone was told the Mac was busy when it was
misconfigured, and the key stayed claimed, so an unreadable vault turned every capture into a
reservation nothing could ever settle.

**`applied` is a statement about the vault, not about the request.** The state flips only after the
kernel's governed write path returns — the same path `mora write` and MCP `write_memory` use, through
`createMemory`'s create-exclusive publish. The listener opens no second door into the vault, which is
why the pending-op marker, the index upsert and the authored-write reconciliation all still happen
for a phone capture. The write is also recorded in the same usage ledger every MCP tool call is
(`TestCompanionCaptureRecordsTheUsageLedgerRow`,
`TestCompanionCaptureLedgerRowMatchesTheMCPToolRow`) — a ledger that held some writes and not others
would be a record of nothing in particular.

The kernel stamps the provenance: `source` is `companion`, the type is an insight, and the title is
derived from the text. `scope` is the device's, because the schema already restricts it to `personal`
or `project:<name>`.

### Durable before it is claimed

A vault write is a rename, and a rename is atomic without being durable: the bytes and the directory
entry can both still be in cache when the write path returns. A receipt that says `applied` is a
promise about stable storage, so the memory file **and its parent directory are fsynced** before the
outcome reaches the listener, which is strictly before the receipt settles
(`TestCompanionCaptureSyncsThePublicationBeforeItReturns`,
`TestCompanionCaptureSyncsBeforeTheReservationSettles`). A sync that fails is not an `applied`
receipt: the capture is left unsettled so a retry re-runs the check
(`TestCompanionCaptureSyncFailureIsNotAnAppliedReceipt`).

### Exactly once

The vault id a capture publishes under is **derived**, from the device, the idempotency key and the
payload, in Mora's own `mem_YYYYMMDD_HHMMSS_<8 hex>` shape — the time half is the capture's own
`captured_at`, which is a real timestamp and stable across retries
(`TestCaptureMemoryIDIsDerivedAndStable`). The id is written into the reservation **before** the
kernel is asked to write anything, and the kernel is asked to create exactly that id.

That is what makes the publish exactly-once rather than merely reserved:

```
reserve (durable, carries the pinned id)  ->  vault write AT that id  ->  settle (durable)
```

`createMemory`'s create-exclusive publish decides every race the reservation cannot. The first
attempt links the file; every later one is told the memory already exists and answers `applied`
without writing again (`TestCompanionCaptureSecondPublishAtTheSameIDWritesOneFile`). A process killed
between the publication and the receipt therefore costs a retry, never a duplicate
(`TestCaptureCrashAfterPublicationAppliesExactlyOnce`,
`TestCompanionCaptureEndToEndCrashAfterPublicationLeavesOneMemoryFile`). When a retry reclaims a
crashed reservation it asks the kernel whether the pinned id is already published before it writes,
so recovery finishes the crashed attempt's receipt instead of repeating its work.

**A key that has published is remembered after its reservation is gone.** The ownership record is
written under two names — by memory id, and by (device, idempotency key) — and a **fresh** reservation
consults the second one first. That matters because a capture killed after its publication leaves a
pending reservation the sweep collects: after that the key looks unused, so without this lookup a
re-stamped retry would be a new identity, a new derived id and a second memory
(`TestCaptureRestampedRetryAfterSweepIsAConflict`). A key that published a *different* capture is
`idempotency_conflict`, refused before any reservation exists
(`TestCaptureConsultsThePublishedIndexBeforeReserving`); a key that published *this* capture falls
through and settles `applied` over the memory already there
(`TestCaptureIdenticalRetryAfterSweepReplaysTheApplication`).

The (device, key) record is the **canonical** one: it is staged, fsynced, and then **linked** into
place with `os.Link` — the primitive that makes the claim exclusive, because the filesystem refuses
the second link with `EEXIST` so exactly one caller ever learns it created the record and the other
learns it created nothing and must roll back nothing. A stat followed by a rename is not exclusive,
and the loser of that race would delete the winner's publication
(`TestCompanionCaptureCanonicalClaimIsExclusive`, `TestCompanionCaptureLoserRollsBackNothing`).

On a filesystem with no links the fallback is an **ownership token**: an `O_EXCL` create of
`<record>.claim`, held while the already-fsynced staging file is renamed onto the final name, so no
reader ever sees a half-written record. The token records **who holds it and when they took it**, and
a claimant that finds a corpse — a pid that is gone, or a token older than the same takeover window a
crashed reservation is reclaimed in — reclaims it. Without that a process killed between the token
and the rename wedged that key's publication for the life of the state directory
(`TestCompanionCaptureOrphanClaimTokenIsReclaimed`). A token whose owner is alive and inside the
window is **never** removed: the claimant reports a retryable busy error, not an integrity failure,
because a contended token says nothing about who owns the id, and the phone sees `503 unavailable`
rather than a receipt of any state (`TestCompanionCaptureBusyTokenAnswersUnavailableNotAReceipt`).
The whole publish — take the token, verify, rename, release — runs under a kernel-held lock
(`internal/leasefile`), because a staleness rule enforced with a second `O_EXCL` sentinel is a
check-then-use race — the lesson the device registry's lock already records.

The lock is not the only defence, because a lock is only as good as the thing enforcing it: an flock
is advisory, it follows the inode, and there are filesystems where it does not exclude at all. So the
token also carries a **per-claim nonce**, and every step that acts on it — the rename of the staged
file into place, and the removal of the token — re-reads the token and compares the nonce
**immediately before acting**. Holding the lock across only the token replacement left an **ABA**: an
owner whose token had aged past the window but whose process was merely slow could resume, pass the
pre-rename stat, rename after it had already lost ownership, and then unconditionally delete its
successor's token. A claimant that has lost ownership now renames nothing, deletes nothing, and
reports the busy error (`TestCompanionCaptureStaleTokenReclaimIsNotABA`,
`TestCompanionCaptureTokenReleaseOnlyRemovesItsOwn`). The nonce comes from the CSPRNG and the publish
**fails** rather than degrading to a PRNG if that is unavailable: elsewhere a random suffix is a
uniqueness token, but here it is the whole ownership proof.

The record carries the memory id and the capture identity. The **exact response bytes live in a
receipt sibling beside it**, `<record>.receipt`, claimed with the same exclusive primitive: the record
itself is immutable once claimed, and rewriting it in place let two racing callers mint two different
answers for one publication. A replay is answered from the sibling directly — so replay does not
depend on a reservation, whose retention moves independently
(`TestCaptureReplayDoesNotDependOnTheReservation`,
`TestCaptureReplayComesFromThePublishedRecordNotTheStore`). A sibling that is not a whole, valid
receipt is **repaired rather than answered with**, and the repair takes the same exclusion the first
publication does, so exactly one repairer acts and the other reads the winner's bytes
(`TestCompanionCaptureTornSiblingRepairIsExclusive`). The repair happens **once**, with one attempt
after it and then a typed error — never by recursion, which an unusable name turned into an unbounded
stack (`TestCompanionCaptureReceiptRepairIsBoundedNotRecursive`). The by-memory-id name is a **secondary
pointer** back to it, created after it; every lookup consults the canonical record first and repairs a
missing pointer when it finds one, so a crash between the two writes is repaired rather than fatal
(`TestCompanionCaptureCrashBetweenCanonicalAndIndexRepairsTheIndex`,
`TestCompanionCaptureCrashBetweenIndexAndMemoryWritesOneMemory`).

The receipt bytes reach the canonical record **before** the reservation settles, and a failure there
is fatal to the attempt rather than swallowed: the worst a crash can do is leave the reservation
pending, which a retry already knows how to finish
(`TestCaptureReceiptIsRecordedBeforeTheReservationSettles`). A replay that finds the canonical record
without its bytes — the state the other order left — backfills them before it answers, so the
reservation is never the only copy (`TestCaptureBackfillsCanonicalBytesOnReplay`), and a replay
repairs a missing pointer on its way past.

Ownership records are the durable audit trail, bounded by the same total cap as reservations (512) and
trimmed oldest-first. The trim is **settled-aware** — a record with no response bytes is a publication
still in flight and is never evicted — and it runs only after a capture has recorded its receipt, so a
**rejected request never trims** (`TestCompanionCapturePublishedStoreIsBounded`,
`TestCompanionCaptureAtCapRejectionLeavesTheTreeIdentical`). Like the reservation store, it counts
rather than walks: a claim and a by-key lookup walk nothing, and the census seeds itself on the first
trim (`TestCompanionCaptureClaimWalksNothing`). **A key is replayable only while its record survives
that retention.** Past it, the key is free again.

**"Already there" is verified, never assumed.** EEXIST says a file is at that path; it says nothing
about whose. So the kernel records who owns a pinned id when it claims one — a small ownership record
under the state directory naming the device, the key and the capture identity, written and fsynced
*before* the memory — and on EEXIST it reads both that record and the memory itself, comparing the
memory's text, scope, type and `source` against what this capture would have written. A file that is
not this capture's is **never** `applied`: it is `rejected` with reason `internal`, nothing is
written, and the specific cause (the pinned id and the device) goes to the operator's log rather than
onto the wire (`TestCompanionCaptureForeignFileAtThePinnedIDIsRejected`,
`TestCompanionCapturePublishedVerifiesRatherThanTrusting`). A missing ownership record is not a
failure — a state directory can be rebuilt independently of the vault — so the file comparison
narrows rather than skips (`TestCompanionCaptureOwnFileAtThePinnedIDIsAppliedWithoutASecondWrite`).

**A refusal writes nothing, including bookkeeping.** A claim reports the exact files it created, and a
failure rolls back only those: an id already owned by another capture is refused without creating or
removing anything, and a claim that merely repaired a pointer takes back the pointer and leaves the
record. A record this request *did* create is taken back when the memory turns out foreign, so the published tree is
byte-identical before and after (`TestCompanionCaptureForeignRejectionLeavesNoResidue`,
`TestCompanionCaptureForeignOwnerRecordIsNotTouched`). A record alone proves nothing about the vault:
the memory comparison still runs, so a record planted for an id nobody published cannot be used to
adopt whatever turns up there later (`TestCompanionCapturePrePlantedOwnerCannotClaimALaterMemory`).
The one crash the ordering exists for — the record durable, the memory not — is completed by the
retry rather than refused (`TestCompanionCaptureOwnerFsyncedButMemoryAbsentIsCompleted`).

### The one thing the listener logs

§11 says the listener logs nothing per request, and that stands. The single documented exception is a
**vault-integrity event**: a memory at a pinned id that is not the capture claiming it. It is an
incident rather than a served request, it goes to the listener's own log sink — the one the startup
banner uses, `io.Discard` when a caller supplies none — and it carries two kernel-derived
identifiers, the memory id and the device id, and nothing else. Not the token, not the text, not the
receipt. `TestCaptureLogsIntegrityEventsAndNothingElse` drives a healthy capture and a broken vault
through one listener and asserts silence for the first and exactly that line for the second;
`TestServerLogsNothingPerRequest` still holds for every ordinary exchange.

`internal` is the frozen vocabulary's word for this. Nothing the phone sent was wrong, and §8's
reasons describe client conditions; a vault holding a file where a capture belongs is a vault
integrity failure, and inventing an enum value for it would move a published vocabulary for a case
an operator reads out of a log.

### Idempotency

| Case | Answer |
|---|---|
| same key, same capture | the **same response bytes** — stored, not re-marshalled, same `receipt_id` |
| same key, different capture | `rejected`, `reason: idempotency_conflict`; the first capture keeps the key |
| same key, concurrently | one caller wins the claim; the others wait and read the settled bytes, or get `503 in_flight` |
| same key, after a crash | the retry completes it — one applied receipt, one memory file |
| same key, after a process restart | the same answers: the reservation is a file, not memory |

**Replay is byte-identical on the wire.** The settled reservation stores the exact bytes that
answered the first attempt and returns those, so a client that hashes or caches a response body gets
an identical one on the retry (`TestCaptureRetryIsByteIdenticalOnTheWire`,
`TestReservationReplayReturnsTheStoredBytes`, both asserting with `bytes.Equal` on raw bodies).

**"The same capture" covers every field that changes what is written** — device, `captured_at`, lane,
intent, scope and text, as canonical JSON — and deliberately excludes the idempotency key, which is
the lookup (`TestCaptureIdentityCoversEveryWriteAffectingField`). The wire `payload_fingerprint`
cannot do this job: §5 defines it as SHA-256 over the **text** alone, so the same key and text under a
different scope hashed identically and the second capture silently inherited the first one's
placement. `TestCaptureSameKeyDifferentScopeIsAConflict` is that case, and it is a conflict.

> **`captured_at` is immutable for a given idempotency key.** A retry must resend the capture it
> already sent, byte for byte. A phone queue that preserves the stamp across a relaunch satisfies this
> by construction; a client that re-stamps its clock on retry gets `idempotency_conflict`, not a
> second memory (`TestCaptureRestampedRetryIsAConflictNotASecondMemory`).
>
> This is not an arbitrary rule. The vault id takes its timestamp from `captured_at`, so a stamp that
> moved would derive a different id, aim at a path nothing holds, and give the create-exclusive
> publish nothing to refuse. Putting `captured_at` inside the identity is what makes the id stable:
> the two move together or not at all.

### Revocation

A capture is authenticated when it arrives, and it can then sit in the work-budget queue while an
operator runs `mora companion revoke`. The credential is **re-checked immediately before the write**;
a revoked device gets `rejected: unknown_device`, nothing is written, and the reservation **settles**
rather than staying pending, so a revoked device's claim is closed rather than left for a later
takeover (`TestCaptureRevokedBetweenReserveAndWriteWritesNothing`). A device revoked before the
request arrives never reaches the capture path at all and gets the same opaque 401 as every other
credential failure.

### The reservation store, and its hard bound

Reservations live at `<state>/companion/captures/<device_id>/<digest>.json`, 0600 inside 0700, with
the same atomic-write-and-fsync discipline as the device registry. **The key space is per device**, so
two devices choosing one key never collide and no device can read another's receipt by guessing its
key. The filename is a digest of the key rather than the key itself.

The bound is hard, in four directions:

| Bound | Value | What happens at it |
|---|---|---|
| In-flight captures | `MaxPendingReservations` (64) | `503` with code `too_many_pending`, and **no file is created** |
| Crashed pending records | swept after `PendingSweepAfter` | collected on open and on insert |
| Total records | `MaxReservations` (512) | the oldest **settled** records are trimmed |
| One record on read | 64 KiB | refused as corrupt or hostile |

**The bound is arithmetic, not a walk.** The store takes one census when it opens and moves it on
insert, settle and sweep, so a fresh reservation reads exactly one file — its own — and walks no
directory (`TestReservationFreshPathWalksNoDirectory`, `TestReservationOpeningWalksOnce`). Sweeping
walks the in-memory claim set rather than the store
(`TestReservationSweepWalksThePendingSetNotTheStore`), and expiry is checked where it costs nothing:
when a record is touched, and at the opening census. The one operation that still walks is the
total-cap trim, which runs only when the census says the store is over 512. The census is per
process, which is the honest bound: a second listener opens its own store and takes its own, so two
of them admit up to 2N in flight rather than infinity.

`PendingSweepAfter` is deliberately later than `ReservationTakeover`. Between the two, a retry
*reclaims* a crashed record, reads the id it pinned, and asks whether that memory is already
published; sweeping at the takeover line would delete the record before any retry could read it and
make the recovery path unreachable. Past the sweep line the record goes, and correctness does not
depend on it — the id is derived, so a later retry re-derives it and the create-exclusive publish
still refuses the second write. `TestReservationRefusesPastThePendingBound` and
`TestReservationSweepsCrashedPendingRecords` pin both.

### What a successful capture writes, in order

| # | File | Exclusive primitive | Rolled back by |
|---|---|---|---|
| 1 | `<state>/companion/captures/<device>/<sha256(key)>.json` — reservation, pending | staged temp + fsync + rename | nobody; the sweep collects it past `PendingSweepAfter` |
| 2 | `<state>/companion/published/keys/<device>/<sha256(key)>.json` — **canonical**, immutable | **`os.Link`** from a fsynced same-directory staging file; `EEXIST` means somebody else owns it | `publicationClaim.rollback`, and only if the link succeeded |
| 3 | `<state>/companion/published/<memory id>.json` — pointer back to the canonical record | `O_EXCL` create | same, and only if this call created it |
| 4 | `<vault>/mora/memories/<scope>/<memory id>.md` — the memory at the pinned id | `atomicCreate` (create-exclusive), then fsynced | nobody; a published memory is never unpublished, and a retry verifies it |
| 5 | `…/<sha256(key)>.json.receipt` — the receipt **sibling** | **`os.Link`**, same primitive; `EEXIST` means somebody recorded it first, and their bytes are the answer | nobody; a failure here fails the attempt |
| 6 | reservation rewritten as settled, with the **authoritative** bytes | staged temp + fsync + rename | nobody |

The trim runs inside step 5, immediately after the receipt is recorded and before
the reservation settles. It is placed there rather than after step 6 because that
is the last point a *successful* capture is still inside the kernel: the listener
has no seam after the settle, and adding one would buy nothing the settled-aware
rule does not already give. The two guarantees the position has to preserve both
hold: a **rejected** request never reaches step 5, so it can never evict
anything; and the trim skips any record without a receipt sibling, so a
publication still in flight is never taken.

The canonical record is **immutable** after step 2: the receipt is a sibling rather than a rewrite, because a rewrite is last-writer-wins and two callers reaching the receipt could mint different bodies for one publication. A canonical record with no sibling is "published, receipt not yet recorded", and a retry records it exclusively.



**The fallback is disciplined.** `os.Link` falls back to an `O_EXCL` create of the final path **only** when the error means the filesystem cannot do links at all (`EPERM`, `ENOTSUP`, `EOPNOTSUPP`, `errors.ErrUnsupported`). Every other link error — an I/O fault, a full disk — is a hard error that leaves nothing behind, rather than a direct write that would mask the fault and risk a half-written record. `EXDEV` is deliberately not in that set: the staging file is created in the destination's own directory, so a cross-device link is a bug, not a limitation. Any failure after the fallback's create removes the file
(`TestCompanionCaptureLinkFallbackDiscipline`).

**A sibling is never visible incomplete, and never trusted blindly.** The link path publishes bytes
that are already whole and already fsynced. The no-links fallback stages and fsyncs a temporary file,
takes an `O_EXCL` ownership token at `<name>.claim`, renames the staged file into place and then drops
the token — so no reader ever sees a partial record. A reader validates what it finds anyway: a
sibling that is empty, does not decode, or describes another capture reads as "receipt not yet
recorded", and the next retry replaces it under the token
(`TestCompanionCaptureLinkFallbackDiscipline`).

**Whoever wins the receipt owns the answer.** `RecordReceipt` returns the authoritative bytes — the
sibling's, whether this caller wrote them or lost the race for them — and the capture settles its
reservation with those bytes and returns exactly those bytes. A caller that answered with the receipt
it had built locally would give two racing claimants two different receipt ids for one publication
(`TestCaptureRacingClaimantsAnswerTheSameBytes`, `TestCaptureLoserAnswersTheWinnersBytes`).

**Contradictory bookkeeping is an integrity failure.** A by-memory-id pointer whose record names a
different memory is not shrugged at: it settles as `rejected` with reason `internal` and the same
integrity event a foreign memory produces, because answering "absent" would send a retry into a write
at an id another publication already owns
(`TestCompanionCaptureCrashBetweenCanonicalAndIndexRepairsTheIndex`,
`TestCapturePointerMismatchIsAnIntegrityFailure`).

That holds for a pointer that names a **different capture**, too, and it holds in production rather
than only in the store's own unit tests. The kernel reports that condition with its own sentinel; the
companion side keys on the contract's integrity error, so the two are mapped at the seam. An unmapped
mismatch fell through as a plain error — the request became a `503`, the phone was told to retry a
vault fault that will never resolve itself, and no integrity event reached the operator's log
(`TestCompanionCapturePointerMismatchSettlesIntegrityEndToEnd`, which drives a real capture, corrupts
the pointer it wrote, and retries).

**A replay finishes what it finds.** A publication whose receipt landed and whose reservation never settled leaves a pending row that would occupy the in-flight bound until the sweep; the replay settles it, under the reservation store's own lock, before it answers (`TestCaptureReplaySettlesAPendingReservation`).

### Status codes

Every **decodable** capture answers `200` with a receipt, rejections included. That is §11's rule
applied to a write: the status code says whether the request was served, the body says what happened
to the vault. `4xx` and `5xx` are reserved for the cases where there is no receipt to give — no
credential (`401`, opaque), an oversize body (`413`), a body that does not decode (`400`, carrying the
schema code and never the value), a busy or unreachable kernel (`503`, with `Retry-After`), a key
already in flight (`503 in_flight`), and the store's bound (`503 too_many_pending`).

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
- **Escape the tighter bound.** Capture caps its body at `MaxCaptureBytes` (24 KiB), not at the
  guard chain's `MaxRequestBytes` (64 KiB).

Capture goes through the same one-at-a-time work budget every read does, and stamps `last_seen_at`
only on a `2xx` — a last-seen stamp records that a device was served.

## 15. Pairing confirmation (N12b)

N11 shipped `mora companion pair` and `Registry.Confirm`. Nothing called `Confirm`. A phone could
scan a QR payload and then had nowhere to hand the code back, so no device ever reached `active` and
every request the listener served was a `401`. This node is the missing call, and it is the one route
on the listener that takes no credential — the request that asks for a bearer token cannot carry one.

```
POST /v1/companion/pairing/confirm
in   mora.companion.pairing.confirmation   device_id, pairing_code, label, platform, public_key, confirmed_at
out  mora.companion.pairing.grant          device_id, token, token_fingerprint, issued_at
```

`mora companion pair` prints the exact URL (`confirm_url` in the JSON form, `confirm` in the human
one), so a client never has to guess the path. `mora companion status` names the devices with a live
code under `awaiting`, because the confirmation identifies the device by id.

### The grant

`mora.companion.pairing.grant` is the only document in this contract that carries a bearer token. It
is returned exactly once per pairing, it is never written to disk and never logged, and the response
carries `Cache-Control: no-store`.

`token_fingerprint` travels beside the token deliberately. It is the same string
`mora companion list` prints, so a person can hold the phone next to the Mac and check that the
credential the phone stored is the credential the Mac issued — the confirmation half of the host
fingerprint the QR payload already carries.

Adding a name is additive: nothing that already decodes v1 has to change, which is the same rule N02
fixed for adding an enum value. No existing schema could carry the token —
`mora.companion.device` exists precisely to identify a credential *without* carrying one, and
`mora.companion.pairing.confirmation` is the phone's request, not the kernel's answer.

### What stands in for the missing credential

- **One slot.** At most one confirmation is in flight listener-wide, and the next is refused
  immediately with `503` and a `Retry-After` rather than queued. It is a *separate* slot from the
  kernel's read budget: a confirmation takes the registry's cross-process write lock, and neither
  side should be able to shut the other out.
- **One refusal.** A wrong code, an expired code, a replayed confirmation, an unknown device id and
  a listener with no pairing window open are the same opaque `401`, byte for byte, with no
  `WWW-Authenticate` header. In particular, "no window is open" is **not** a `404`: a `404` is an
  oracle for exactly the fact a prober wants.
- **One timing bucket.** Every answer — refusal and success alike — is padded up to a whole
  `PairingFloor` (250 ms) boundary: the elapsed time is rounded **up** to the next bucket, not merely
  raised to a minimum. The paths are not equally expensive. An unknown id returns before any
  comparison; a wrong code takes the registry's write lock and publishes the failure counter; a
  matching-but-expired code burns the code and writes a record and a receipt; a latched budget
  retries an owed write. A plain minimum hid that only while every path finished inside it, and the
  moment one overran, the overrun was readable with a stopwatch.

  Quantizing makes the answer a **step**: a path that does its work inside one bucket leaves at one
  bucket, and one that overruns leaves at two.
  `TestPairingRefusalsLandInTheSameTimingBucket` drives all four refusal paths and asserts bucket
  *equality*, not a minimum; `TestPairingFloorPadsToBucketBoundaries` pins the arithmetic at the
  boundaries a wall-clock measurement cannot separate from jitter.

  Be precise about what that buys: an observer can read which bucket a request fell in, not how much
  work it did, and every path this route has lands in the first one. It is **not** a cryptographic
  constant-time guarantee at the level of branches or cache lines, and nothing here claims one — the
  comparison that actually decides the answer is constant-time in `Registry.Confirm`. One slot times
  one bucket also caps the whole route at four attempts a second.
- **One attempt budget, and it is DURABLE.** `MaxPairingAttempts` (5) wrong codes against a live
  pairing and the pairing is **revoked**, with a `revoked` receipt written record-first. The right
  code buys nothing afterwards. The count lives in the pending device's own record
  (`pairing_attempts`), not in the listener's memory: an in-memory counter hands an attacker a fresh
  five guesses every time the process restarts, and an attacker who can make it exit — or who simply
  waits for the operator to restart it — would have no budget at all
  (`TestPairingBudgetSurvivesARestart`).
- **The revocation the budget ends in is checked, not fired and forgotten.** A revoke that fails
  leaves the pairing live while the listener believes it dealt with it, and a durable count would
  make that permanent rather than self-healing. So the failure fails **closed** — the attempt is
  still refused, even with the right code — and the revocation is retried on every subsequent
  attempt until it lands (`TestPairingRevokeFailureStillRefusesAndIsRetried`). A budget that cannot
  be *read* fails closed the same way (`TestPairingConfirmFailsClosedWhenTheBudgetCannotBeRead`).
- **A count that cannot be WRITTEN fails closed too.** A durable budget is only durable while the
  counter can be published. Discarding a failed write left the count where it was and let the next
  guess reach `Confirm` again, so anyone who could make that write fail — or who was simply unlucky
  with the registry lock — had no budget at all. An unwritable failure now marks the pairing
  exhausted for that process: the write is retried on the next attempt for that device, and **that
  attempt is refused too**, so the guess it carries is never tried. The retries drain, so the count
  the record ends up holding is the number of guesses actually made
  (`TestPairingCounterWriteFailureExhaustsThePairing`,
  `TestPairingCounterWriteFailureStillEndsAtTheBudget`). The latch is in memory and only ever makes
  the route *more* closed than the record does — a restart drops it and falls back to whatever the
  record says, which is exactly where the durable budget would have been.

### The grant's fingerprint must cover its token

`Validate` requires `token_fingerprint` to equal the fingerprint of the token it travels with,
through the same derivation N11 stores (`TestPairingGrantFingerprintMustCoverItsToken`). A
well-formed digest of some *other* string passes every format check and would send the person
comparing it against `mora companion list` to compare a value describing nothing they hold.
Validation runs on the way out as well as on the way in, so a producer bug becomes a `500` rather
than a wire-contract violation.

### A receipt warning is tolerated only on the success path

`Registry.Confirm` reports `ErrReceiptNotWritten` when a change committed and only its audit row did
not. On a **successful** confirmation that warning must not throw the credential away: the device is
active and the returned token is the only copy of it.

It is not tolerated on a refusal, and the expired code makes that concrete. Confirm *burns* a
matching-but-late code — so a clock rolled back cannot revive yesterday's photographed QR code — and
that burn is a durable write with a receipt. When the receipt fails, Confirm returns
`errors.Join(ErrPairingExpired, ErrReceiptNotWritten)`: a refusal with a warning attached. Treating
any receipt warning as success there built a grant out of an empty token and answered `500`, which
is both a refusal an attacker can tell apart by status code alone and a lie about what went wrong.
The handler now tolerates the warning only alongside a credential that was actually minted
(`TestPairingConfirmRefusesAnExpiredCodeWhoseReceiptAlsoFailed`,
`TestPairingConfirmStillIssuesWhenOnlyTheSuccessReceiptFailed`).

### Bootstrap: `expose` publishes for an open pairing window

`mora companion expose` used to refuse whenever no device was **active**, and that was circular: the
first device cannot become active until the phone reaches this route, and it cannot reach it until
the listener is published — which is what `expose` exists to explain. An operator following the
documented path got `data.not_found` and no way forward.

The refusal now asks the question it meant to ask. Publishing is refused only when there is neither
an active device **nor** a pending one with a live code. When only a pending device exists, the
serve commands print alongside the window's deadline, the pending device ids, and the confirm URL
(`TestCompanionExposeRefusesOnlyWhenNothingCouldEverAuthenticate`).

### The confirm URL is published, not guessed

`mora companion pair` emits `confirm_url`, and `mora companion expose` emits one for the published
origin. Both are derived from the endpoint's **origin** — scheme, host and port — with the route
mounted once. Appending the route to the endpoint was wrong for the endpoint shape the canonical
pairing golden carries:

```
endpoint   https://host/v1/companion
appended   https://host/v1/companion/v1/companion/pairing/confirm   404, mounted nowhere
derived    https://host/v1/companion/pairing/confirm                served
```

Every mount point on this listener is an absolute path from the origin, so the origin is the only
part of the endpoint that can contribute (`TestCompanionConfirmURLMountsTheRouteExactlyOnce`,
`TestCompanionPairEmitsAUsableConfirmURL`). The network audit posts its confirmation to the emitted
URL **verbatim**, origin and path — not to an origin it rebuilt — so a client that trusted the field
is exercised rather than assumed.

### Why a lockout can never revoke an active device

A device id is not a secret: `mora companion pair` prints it, the QR payload carries it, and it is
short enough to guess. If any failed confirmation incremented the counter, anyone who could name a
device id could spend five requests and revoke a working phone.

So exactly one outcome counts: a wrong code against a device that is pending and still holds a live
code. An unknown device id is not counted, and neither is a confirmation for a device that is
already active or already revoked — there is no code there to brute force, so there is nothing for a
lockout to protect and nothing for it to break. The only device a lockout can revoke is one that is
mid-pairing, which is the device whose code is under attack.
`TestPairingLockoutCannotRevokeADeviceItDoesNotProtect` drives twice the budget at an active device
and at a fistful of invented ids, and proves both survive.

The counter is stored on the pending record and moves only through `Registry.RecordPairingFailure`,
which refuses to touch a device that is not pending with a live code — so it can never accumulate
against an active or revoked one, and a remote caller cannot create records at all. Reading it is a
bounded registry **read**, never the cross-process write lock, so a stranger repeating a spent guess
cannot stall `mora companion pair` or `revoke` on the Mac
(`TestPairingConfirmSpendsNoWriteLockOnALockedOutCaller`).

### The accepted cost: a pairing-window denial of service

Say this plainly rather than leaving it to be discovered. **A stranger who knows a pending device id
can end that pairing in five requests.** The id is not a secret — `mora companion pair` prints it,
the QR payload carries it, and it is short — so anyone who can reach the confirmation route and has
seen or guessed one can spend the budget with five wrong codes and have the kernel revoke the
pairing. That is a denial of service on the *pairing window*, deliberately accepted, and it is the
price of the property in the section above: a budget that only the real holder of the code could
exhaust is not a budget at all.

What it cannot do is the part that matters. It cannot revoke a **paired** device — the counter moves
only for a device that is pending with a live code — it cannot read anything, and it cannot make the
code guessable: the budget bounds the attempts, and the code's 160 bits bound the guessing.

The mitigations are the short window and the operator:

- `PairingTTL` is 10 minutes. There is no long-lived target here; outside a window there is nothing
  to exhaust.
- The remedy is one command. `mora companion pair` issues a fresh code and a fresh id, and
  `mora companion status` shows the window and the device awaiting it, so an operator watching a
  pairing fail can see which one died.
- The revocation writes a `revoked` receipt, so a pairing that ended this way is visible in the audit
  trail rather than silently gone.

**There is deliberately no per-source limiter**, and that is N04's finding rather than an omission.
Behind Tailscale Serve the listener sees `127.0.0.1` as the peer for every phone: the client address
does not survive the proxy, and the only place the real one appears is `X-Forwarded-For`, which is
attacker-settable on any path that is not the proxy. A per-IP throttle would therefore collapse into
one bucket shared by every device — throttling the legitimate phone on the attacker's behalf — or
would key on a header the attacker chooses. The limits that exist are the ones that can be trusted:
one confirmation in flight listener-wide, the timing bucket, and the per-window attempt budget.

### What the exemption does and does not cost

The exemption is from the credential check and nothing else. The DNS-rebinding host guard still runs
first and does not care that the route is public
(`TestPairingRouteIsStillBehindTheHostGuard`), the size guards still run, the decode is still strict
inbound, and `last_seen_at` is still stamped only on a `2xx` — which on this route is the device's
first-seen.

It costs one thing worth naming: the pairing path is *discoverable* without a token, because a `405`
and a `404` are distinguishable. It is a published route printed by `mora companion pair`, so there
was never a secret there to keep.
