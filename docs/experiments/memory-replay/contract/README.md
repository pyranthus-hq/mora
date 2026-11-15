# Memory replay pilot — contract

**Issue:** [#541](https://github.com/pyranthus-hq/mora/issues/541) (parent [#540](https://github.com/pyranthus-hq/mora/issues/540))  
**Schema version:** `1` (`mora.pilotreplay.*`)  
**Go package:** `github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract`

This directory explains the frozen, versioned contract for a **local, opt-in
coding-agent memory replay** pilot. The pilot asks whether changing memory
changes a later coding outcome while preserving valid constraints. It does
**not** adopt a product pivot.

Completing this contract:

- does **not** qualify a real case ([#542](https://github.com/pyranthus-hq/mora/issues/542));
- does **not** authorize a paid model run;
- does **not** claim downstream efficacy.

Public artifacts here and under `internal/mora/pilotreplay/contract/` are
**synthetic only**. Private case paths, contents, source identities and
credentials must never appear in the public repository.

## Owned paths

| Path | Role |
| --- | --- |
| `internal/mora/pilotreplay/contract/` | Go types, `Validate` helpers, synthetic fixtures, schema tests, JSON Schema stubs |
| `docs/experiments/memory-replay/contract/` | Human contract (this tree) |

Consumers implement runners, oracles and case packaging in **their own**
directories. Coordinate schema changes through #541. No new repository,
service or production integration is introduced by this contract.

## Documents in this tree

| File | Contents |
| --- | --- |
| [schema.md](./schema.md) | Field-level schemas and fidelity/exposure rules |
| [isolation.md](./isolation.md) | Access table, runner/oracle separation, isolation assumptions |
| [execution.md](./execution.md) | Reset, retry, timeout, cost, run gate, planning matrix |
| [receipt.md](./receipt.md) | How to emit the offline contract receipt |

## Three conditions

1. `original_memory` — frozen snapshot / exposure as recorded  
2. `no_memory` — same task without memory  
3. `reviewed_memory_edit` — one reviewed edit (reference-memory ceiling is **out of scope**)

Changing retrieval or harness requires a **separate condition** or a
**declared confound**.

## Controls (required)

1. **Valid memory control** — a constraint that must survive the edit condition.  
2. **No relevant memory control** — a task whose success must **not** require the proposed edit.

## Suggested planning matrix (not approved spend)

`5` matched reps × (`1` failure + `2` controls) × `3` conditions ≤ **45** planned runs.

This is a planning default only. Freeze the **actual** matrix and **monetary
ceiling** before any provider invocation. See [execution.md](./execution.md).

## Coordination

- Schema coordination with case packaging: [#542](https://github.com/pyranthus-hq/mora/issues/542) (qualification independent).  
- Runner work may begin against this interface when the minimum versioned surface is frozen: [#543](https://github.com/pyranthus-hq/mora/issues/543) (synthetic fixtures).
