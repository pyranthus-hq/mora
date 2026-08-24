# The Mora machine contract

This is the published contract between Mora and a program that calls it — an agent harness, a script,
a scheduler, another tool. It is the one page to read before writing that program.

Everything here is enforced by a named test. Where a claim has no test behind it, this page says so
instead of implying one. Where a capability does not exist yet, this page names the phase that will
deliver it rather than describing it as present.

Contributors looking for the implementation — which function writes the envelope, where the gate
lives, how the styling layer is suppressed — want [08 — CLI and UX](./08-cli-and-ux.md). This page is
the consumer's half.

Code is cited by file plus function or constant name, never by line number. Line citations in this
repository have drifted by 1,300 to 1,900 lines.

---

## 1. The envelope

Every machine payload Mora prints carries two keys:

| Key | Type | Meaning |
|---|---|---|
| `schema` | string | The payload's published name, e.g. `mora.doctor.report`. |
| `schema_version` | integer | A MAJOR-only version. It starts at 1. |

The two keys **merge into** the payload object; they do not wrap it. A field a payload published
before the envelope keeps its name, its type, and its top-level position.

### The versioning rule

- **Adding a field is MINOR.** `schema_version` does not move. Read the fields you know and ignore
  the rest — a payload gaining keys is normal and will happen.
- **Removing a field, renaming one, or changing its type is BREAKING.** It requires a
  `schema_version` bump and a new golden directory (§7).
- There is no minor component. A consumer pins on `schema` plus `schema_version` and nothing else.

### The one breaking shape change

A top-level JSON array cannot carry the envelope, so every command that printed a bare `[ … ]` now
prints an object with that array under a named key. **Nothing else about these payloads changed — the
element objects are identical.**

| Command | Was | Now | Key |
|---|---|---|---|
| `mora search --json` | `[ … ]` | `{ "schema": "mora.search", "schema_version": 1, "memories": [ … ] }` | `memories` |
| `mora list --json` | `[ … ]` | `{ "schema": "mora.list", …, "memories": [ … ] }` | `memories` |
| `mora tasks list --json` | `[ … ]` | `{ "schema": "mora.tasks.list", …, "tasks": [ … ] }` | `tasks` |
| `mora loop list --json` | `[ … ]` | `{ "schema": "mora.loop.list", …, "loops": [ … ] }` | `loops` |
| `mora merge list --json` | `[ … ]` | `{ "schema": "mora.merge.list", …, "pending": [ … ] }` | `pending` |
| `mora teach identity list --json` | `[ … ]` | `{ "schema": "mora.teach.identity.list", …, "pending": [ … ] }` | `pending` |
| `mora connectors list --json` | `[ … ]` | `{ "schema": "mora.connectors.list", …, "connectors": [ … ] }` | `connectors` |
| `mora teach examples --json` | `[ … ]` | `{ "schema": "mora.teach.examples", …, "examples": [ … ] }` | `examples` |
| `mora teach history --json` | `[ … ]` | `{ "schema": "mora.teach.history", …, "entries": [ … ] }` | `entries` |
| `mora sources list --json` | `[ … ]` | `{ "schema": "mora.sources.list", …, "sources": [ … ] }` | `sources` |

One further breaking change that is not a shape move: **`mora index rebuild --json` emits
`mora.index.rebuild`**, not the parent's `mora.index`. One schema name had been covering two shapes.

If you pin one of the payloads above, this is the change to migrate. Everything else is additive.

**Enforced by** `TestContractEveryPayloadIsVersioned` (`internal/mora/contract_envelope_test.go`),
which drives every executable command the registry declares, asserts `schema` equals the name the
registry assigns that path, and fails if executed plus shape-only rows do not add up to the
non-exempt row count. A silently skipped command fails the test rather than shrinking the table.

### MCP tool results carry no envelope

This is deliberate and it is not an oversight. The MCP token budget gate measures `write_memory`'s
worst case at 1,470 of 1,500 tokens, and the envelope costs a measured +30 tokens per object payload
— two keys, paid twice, once in the indented text block and once in the `structuredContent` mirror.
Adding it would leave exactly zero headroom on the gate whose whole purpose is to be the forcing
function.

MCP schema names and versions are published through `mora capabilities --json` under
`capabilities.mcp.schemas` instead. **Enforced by** `TestMCPBudgetCeilings` and `TestMCPBudgetCeilingsUnhealthy`
(`internal/mora/mora_mcp_budget_test.go`), which hold the ceiling the envelope would breach.

---

## 2. Streams

- **stdout carries the result.** With `--json`, stdout is exactly one JSON document and nothing else.
- **stderr carries diagnostics** — progress, warnings, health banners, error text.
- **A usage error is not a result.** A command that fails before doing any work writes nothing to
  stdout. If you got a document, work happened.
- Colour, spinners, and the banner are suppressed on a non-TTY writer, so piping is byte-clean.

**Enforced by** `TestContractStdoutIsPure` (`internal/mora/contract_stream_test.go`), which drives
every executable non-exempt registry row with `--json` and fails unless stdout parses as one JSON
document. There is no measurement-based escape hatch: a command is exempt only by a declared registry
row, never because a baseline recorded it as unfinished.

### Two commands whose stdout is not ours

`mora hook session-start` and `mora hook recall` emit **Claude Code's** `hookSpecificOutput` envelope,
not Mora's. They are declared `exempt` in the command registry with that reason. Do not expect
`schema`/`schema_version` from them, and do not wrap them.

---

## 3. Exit codes

Three non-zero statuses ship. All three are grandfathered with their exact shipped meanings and are
never re-allocated.

| Exit | Status | Meaning |
|---|---|---|
| `0` | — | Success. |
| `1` | grandfathered | Generic failure. Every error class maps here, **including `doctor --strict` on an unhealthy report**. |
| `2` | grandfathered | `doctor --pulse`: a source, the index, or a producer is unhealthy. Sick, not broken. |
| `10` | grandfathered | `loop begin`: this period already succeeded. The idempotent skip, distinct from a real failure. `loop begin` emits its receipt on stdout *before* returning this status. |
| `3`–`9` | **permanently reserved-unused** | Low single-digit statuses are widely squatted by shells, test runners, and wrapper scripts. Leaving them unallocated means a future Mora status can never be confused with one of those conventions. |
| `11`+ | available | The first allocatable status is 11. None is allocated today. |

**Enforced by** `TestExitCodeAllocationIsGrandfathered` (`internal/mora/error_registry_test.go`),
which fails if the class-to-status mapping ever returns a status inside the reserved band, or a status
below 11 that is not 1, 2, or 10.

### `doctor --strict` exits 1, not 2

This is a decision, not an accident, and the inconsistency is real: on one vault with a missing index,
`doctor --pulse` exits 2 while `doctor --strict` exits 1. Changing it was put to the maintainer and
declined. A process exit status is the one contract in this surface with no additive migration path,
and the machine consumer this taxonomy serves already has a clean signal —
`doctor --json --strict` prints the full report, `"healthy": false` and all, *before* returning the
error. A JSON caller distinguishes unhealthy (report present, exit 1) from crashed (no report, exit 1)
today. Only a shell consumer reading a bare `$?` without `--json` cannot, and that consumer gets a
non-zero either way.

**Branch on zero versus non-zero, and read the document for the detail.** That is what every consumer
inside this repository does.

---

## 4. Error codes

A failure reaches a caller as a typed error carrying `Code`, `Class`, `Source`, and a message. Branch
on the code; never match the message.

The taxonomy is declared in `internal/mora/errors.go` and published in
`internal/mora/eval/error-code-registry.json`. **Enforced by** `TestErrorCodeRegistryMatchesSource`
(`internal/mora/error_registry_test.go`), which parses every non-test source file with `go/ast`,
collects each string literal bound to an `errCode*` identifier, and fails if that set differs from the
registry **in either direction** — a code declared but unpublished, or published with no constant
behind it.

| Code | Class | `error_class` | Retryable | Meaning |
|---|---|---|---|---|
| `usage.unknown_flag` | usage | — | no | Unknown or malformed flag. |
| `usage.unknown_value` | usage | — | no | A flag or positional carried a value the command does not accept. |
| `usage.missing_argument` | usage | — | no | A required positional argument was absent. |
| `connector.malformed_response` | connector | `malformed` | no | A connector process, API, or local database returned a payload Mora could not parse. |
| `connector.unavailable` | connector | `unavailable` | **yes** | A connector's process, binary, database, or endpoint could not be reached. |
| `connector.unauthorized` | connector | `unauthorized` | no | An authentication or permission refusal Mora **directly observed** — never inferred from an error string. |
| `connector.stale` | connector | `stale` | **yes** | A source last succeeded longer ago than its freshness budget allows. |
| `connector.empty` | connector | `empty` | no | A source read cleanly and returned zero items. Declared here; emitted per-source in Phase 2 (ISO-02). |
| `connector.unclassified` | connector | `unclassified` | no | A failure with no typed cause, and the read-time backfill for records persisted before the taxonomy shipped. |
| `consent.required` | consent | — | no | A locally recorded consent gate is closed. Mora read its own governance ledger and observed the refusal, so this is a fact rather than an inference. |
| `data.not_found` | data | — | no | A requested memory, entity, or record does not exist. Declared with no producer yet; Phase 5 or Phase 6 gives it one. |
| `data.corrupt` | data | — | no | A vault or state file exists but could not be decoded. |
| `index.unavailable` | index | — | **yes** | The index database could not be opened. `mora index rebuild` is the repair. |
| `index.schema_mismatch` | index | — | no | The index exists but its tables or columns do not match this build. |
| `internal.unexpected` | internal | — | no | A failure Mora cannot attribute to caller, connector, or data. A Mora bug. |

Fifteen codes across seven classes. **The `permission` class is declared with no code, on purpose.**
Mora currently *infers* Full Disk Access from an error string rather than observing a refusal, and
Phase 3 (DOC-03) owns replacing that inference. Minting a permission code now would publish the same
unverified claim in typed form. No code was invented to make the table look complete.

### Classification is structural

Every rule that assigns a connector code matches a decoder type, a sentinel, or an interface. **None
matches error prose.** A failure whose message merely says "unauthorized" is classified
`connector.unclassified`, deliberately — re-encoding a guess as a typed code makes the guess look like
a fact. **Enforced by** `TestContractConnectorCauseClassificationIsStructural`
(`internal/mora/contract_errors_test.go`).

The one place strings are matched by necessity is `sqliteErrorCode` (`internal/mora/index.go`): the
sqlite driver reports a missing table, a duplicate column, and an unopenable file only as message
text, with no error number and no sentinel. That labeling is confined to that one function.

### `error_class` maps many-to-one into `state`; there is no fourth vocabulary

CON-07's five discriminations are an **orthogonal axis**, not a new state vocabulary. A connector
failure carries both: a `state` a human or a banner reads, and an `error_class` a machine branches on.

| `error_class` | `state` |
|---|---|
| `unavailable` | `failed` |
| `unauthorized` | `failed` |
| `malformed` | `failed` |
| `unclassified` | `failed` |
| `stale` | `stale` |
| `empty` | `fresh`, with `item_count` 0 |

Four `failed` sources are distinguishable from each other without reading prose. The three existing
state vocabularies are **unchanged, and no fourth was introduced**:

- `fresh | stale | failed | never` — per-source health, read by `doctor`, `sync status`, and the banner.
- `healthy | degraded | unhealthy` — the aggregate verdict.
- `fresh | dirty | degraded | failed | never` — index health.

`error_code` was added **beside** the existing free-text `last_error`, which is unchanged in name,
type, and meaning. A record written before this change decodes with an empty `error_code` and is read
as `connector.unclassified` at read time; nothing is rewritten on disk. **Enforced by**
`TestContractSyncStatusErrorCodeIsAdditive` and `TestContractConnectorErrorClasses`
(`internal/mora/contract_errors_test.go`).

---

## 5. `--json` coverage

Every command path is declared in `internal/mora/eval/cli-command-registry.json` with exactly one
`json_contract` classification:

| Classification | Rows | Meaning |
|---|---|---|
| `result` | 38 | The command answers a question. `--json` emits the answer. |
| `receipt` | 58 | The command does something. `--json` emits a receipt of what it did. |
| `exempt` | 35 | The command emits no Mora document, and the row carries a mandatory `reason`. |

131 rows in total. The exemptions are not a loophole — an exempt row without a reason fails the build,
and the reasons fall into a small set of families: **dispatch-only group verbs** (`mora share`,
`mora teach`, `mora loop` …), where a bare invocation is a usage error and a usage error is not a
result; **long-running servers** (`mcp serve`, `serve http`), whose protocol is the contract;
**interactive or self-replacing commands** (`init`, `connectors setup`, `upgrade`); **host mutations
with no result payload** (`schedule install`, `hook install`); and the **Claude Code hook handlers**
described in §2.

**Enforced by** `TestCLIContractMatrix` (`internal/mora/contract_matrix_test.go`), which visits every
one of the 131 rows, classifies each into exactly one bucket, and fails on a row that falls through or
carries a `platform` or `json_contract` value outside the declared vocabulary. The visited count must
equal the row count, so nothing escapes by being overlooked.

### Dash-led arguments

A dash-led token in a positional slot is a mistyped flag, not a value. Mora refuses it rather than
storing it — `mora tasks add --json` once created a live task literally named `--json`, so a machine
caller asking for JSON silently mutated the vault.

To pass a value that legitimately starts with a dash, end flag interpretation first:
`mora tasks add -- -urgent`.

**Two published exceptions.** `mora search` and `mora think` take **free text**, so a dash-led token
is query input, not a mistyped flag. `mora search foo --limitt 5` searches for the literal string
rather than reporting the typo. This is kept deliberately: refusing it would break a legitimate search
for a term that starts with a dash. Both are non-mutating.

**Enforced by** `TestCLIContractMatrix`, which sweeps the registry rather than a hand-kept list, so a
command added in a later phase is swept the day its row lands; by
`TestContractSharedDashLedGuardIsWitnessed`, which requires the refusal to come from the shared guard
rather than from a downstream lookup miss; and by
`TestContractDashLedQuerySlotsAreDocumentedExceptions`
(`internal/mora/contract_envelope_test.go`), which fails if `search` or `think` ever starts refusing,
so the exception above cannot silently become false.

**A small set of commands ignores an unrecognized trailing token instead of refusing it** — the help
aliases, `usage on|off`, `usage queries on|off`, `teach consent status`, and the two Claude Code hook
handlers. None of them stores the token. They are enumerated in `contractMatrixDashLedIgnored`
(`internal/mora/contract_matrix_test.go`) and the list is closed forward: a command not on it that
starts accepting fails the matrix.

---

## 6. Discovery

`mora capabilities --json` is the single self-describing entry point. One call tells a consumer what
this build supports, without probing.

It publishes every command path with its `json_contract` classification and payload name, every error
code with its class and `error_class`, the allocated and reserved exit codes, every schema name, every
connector with its feature flags, and the MCP tool list with its write policy. Every section except
one is derived at runtime from an embedded registry or a Go table, so the document cannot describe a
Mora the binary is not.

### Fields a later phase will flip

These are reported `unsupported` today because the capability does not exist yet, not because it is
switched off:

| Field | Today | Owner |
|---|---|---|
| `features.repair` | `unsupported` | Phase 3 (DOC-04, DOC-05) |
| `features.deep_link` | `unsupported` | Phase 5 (RET-01) |
| `connectors[].features.deep_link` | `unsupported` for all six connectors | Phase 5 (RET-01) |
| `connectors[].features.incremental_sync` | `unsupported` for all six connectors | Phase 4 (ING-01) |

Flipping one of these changes a **value**, not a schema. The shape is a tri-state in every case, so
the compatibility gate stays green.

**Enforced by** `TestCapabilitiesMatchesRegistries` (`internal/mora/capabilities_test.go`), with one
honest limitation stated rather than buried: for the `commands`, `error_codes`, and `exit_codes`
sections the binary and the test read the same bytes — the binary through `go:embed`, the test from
disk — so a registry edit moves both sides together. That is what makes drift impossible by
construction, and it means the test's value in those sections is catching a **projection** bug (a
filter, a truncation, a dropped field) rather than drift. The sections where it is genuinely
load-bearing are the ones whose source is Go: `connectors`, `mcp.tools`, `mcp.write_policy`, and
`schemas`. Drift in the registry itself is caught by
`TestCLIRegistryMatchesProductionDispatch` and `TestContractGoldenCorpusIsComplete`.

**One claim in the payload has no registry behind it and nothing that would catch it going stale:**
`connectors[].incremental_sync`. Its current value is measured, and the evidence lives in
`capabilitiesIncrementalSync` (`internal/mora/capabilities.go`). Phase 4 owns it.

---

## 7. The compatibility guarantee

### What a pinned consumer can rely on

- A field present in a payload at `schema_version: N` stays present, keeps its name, keeps its JSON
  type, and keeps its top-level position, for the life of version N.
- New fields will appear. Ignore the ones you do not know.
- An array-valued field is never `null`. An empty collection is `[]`.
- Values that are inherently variable — ids, timestamps, absolute paths, byte counts, size estimates
  — are variable. Do not pin them. `mora context`'s `used` is one: it is a token estimate over a
  payload whose timestamps carry the host's zone offset, so the same two memories cost a couple of
  tokens more in a local zone than in UTC. Its presence, its type, and `used <= budget` hold; its
  exact number does not.
- **A few payloads are platform contracts.** `mora connectors list` publishes the catalog for the
  host: Windows drops the macOS-only connectors (iMessage, Apple Calendar, Address Book), so a
  Windows consumer sees four rows where macOS and Linux see six. The field set and the row shape are
  identical everywhere; only membership differs. Those payloads carry one frozen document per
  platform that differs, under `testdata/contracts/v1/goos/<goos>/`, and the shared corpus stays the
  contract for every other host.
- **Collection ordering is not part of the contract.** The frozen corpus normalizes array order, so a
  reordering will not fail the gate and is not guaranteed to you. If you need a stable order, sort.

### What will change without warning

Nothing, by construction. A removal, rename, or retype cannot reach a release without either a
`schema_version` bump or a red test.

### How that is enforced

`internal/mora/testdata/contracts/v1/<schema>.json` holds one frozen document per executable versioned
payload, and `internal/mora/contract_compat_test.go` decodes it in **both** directions:

| Test | Direction | What it proves |
|---|---|---|
| `TestContractCompatAdditiveIsSafe` | today's output → a type built from the v1 golden | Every v1 field still populates. It then injects a field nothing emits today and asserts the pinned consumer's view is byte-identical, so "adding is safe" is measured rather than assumed. |
| `TestContractCompatRemovalIsCaught` | the v1 golden → a type built from today's payload, unknown fields disallowed | A removed or renamed field is an unknown field in that direction and fails. A key-by-key walk runs alongside it for what strict decoding cannot see: inside an array that is empty today, and scalar retypes. |
| `TestContractGoldenCorpusIsComplete` | registry → corpus | A new payload with no golden fails, so nothing escapes the gate. |
| `TestContractGoldenCorpusIsFrozen` | corpus → live output | The committed document must still match what the command emits, comparing against the platform's override where one exists. |
| `TestContractGoldenPathPrefersGOOSOverride` | selection rule | An override is preferred for the platform that owns it; every other schema and platform falls back to the shared corpus. |
| `TestContractGoldenWindowsCatalogMatchesLiveOutput` | Windows override → live output under `GOOS=windows` | The Windows connector catalog is checked from any host, not only on Windows. |
| `TestContractCompatRemedyMessage` | — | Covers the failure text itself, and proves the detector fires on a synthetic removal. |

**Regeneration cannot be used to drop a field.** The generator refuses to write a document that lost a
key the committed golden carries, and the failure message for a dropped key never mentions the
regeneration environment variable. Otherwise the gate would be bypassable by following its own advice:
remove a field, regenerate, go green. The remedy a dropped key produces names the only legal path:

```
field storage_bytes removed from mora.doctor.report: bump mora.doctor.report.schema_version and move the golden to testdata/contracts/v<N>/
```

### The limit of the corpus, stated plainly

50 goldens cover every payload the suite can execute. **45 non-exempt payloads have no compatibility
gate** — they belong to rows that are destructive, network-bound, host-mutating, interactive, or
platform-gated, and fabricating documents for them would freeze shapes no command emits. Those rows
carry only `TestContractEveryPayloadIsVersioned`'s schema-name assertion. A later phase that makes one
of them executable in a test must add its golden in the same change.

The corpus is generated on macOS and verified on macOS, Linux, and Windows in CI. Where a payload
varies by platform, the platform's own frozen document is verified from any host by driving the same
GOOS seam the product reads (`TestContractGoldenWindowsCatalogMatchesLiveOutput`), so a wrong Windows
document fails on a developer's laptop rather than waiting for the Windows runner.

### Contract tests read machine output, never prose

No test in this package asserts a substring over `--json` stdout without being declared, and no
release gate parses Mora's human output. The release regression harness reads `mora config --json` and
`mora version --json`; it previously screen-scraped the human lines, and an annotation change once
defeated that scrape and broke every tagged release for a day.

**Enforced by** `TestCLIContractProseExemptionsAreDeclared`
(`internal/mora/contract_matrix_test.go`), which holds two declared lists — the test files that pin
human text on purpose, proved honest by requiring each to drive no `--json` at all, and the files that
still assert a substring over `--json` stdout. Both fail on a new entrant and on an entry that no
longer applies, so neither list can rot.

---

## Related

- [08 — CLI and UX](./08-cli-and-ux.md) — the implementation: the writer, the styling gate, the
  byte-clean invariant, where a code is attached.
- [06 — MCP server](./06-mcp-server.md) — the MCP surface and its token budgets.
- [09 — Eval and testing](./09-eval-and-testing.md) — how the contract suite fits the wider harness.
- [docs/guide.md](../guide.md) — the user manual, command by command.
