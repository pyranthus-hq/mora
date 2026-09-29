# Schema surface (v1)

Authoritative validation lives in Go: package
`internal/mora/pilotreplay/contract`. JSON Schema stubs under
`internal/mora/pilotreplay/contract/schemas/` mirror field shapes for review.

Every top-level document carries:

```json
{ "schema": "mora.pilotreplay.<name>", "schema_version": 1 }
```

## Case (`mora.pilotreplay.case`)

| Field | Purpose |
| --- | --- |
| `case_id`, `role` | Identity; role is `failure`, `valid_memory_control`, or `no_relevant_memory_control` |
| `eligibility` | `decision` ∈ {`eligible`,`no_go`,`prospective_capture`}; permission + private-package gates |
| `cutoff` | RFC3339 cutoff; `post_cutoff_forbidden_to_contender` must be true |
| `repository` | Commit SHA + relevant dirty files (path, content sha256, status) |
| `harness` | Model/provider/toolset/harness ids; **provider version status** must be explicit |
| `memory_snapshot` | Stored memory **bytes/version** (hash, length, record ids, fidelity) |
| `delivered_context` | Exact delivered prompt/tool-result **exposure** parts (separate from snapshot) |
| `runner_package` | Contender-visible paths only; hidden oracle flags must be false |
| `oracle_package` | Hidden tests / expected answers / reference patches / post-cutoff refs |
| `controls` | Both control statements for failure cases |
| `isolation_assumptions` | Explicit assumptions list |

### Memory snapshot vs delivered context

- `memory_snapshot` = what was stored (vault/export bytes + version).  
- `delivered_context` = what the agent actually received (prompt / tool results).  
- A memory hash alone does **not** establish exposure.  
- If exposure bytes are missing, set `exposure_availability=missing` and a
  `missing_reason`. Do **not** claim `fidelity=faithful_historical`.

### Fidelity

| Value | Meaning |
| --- | --- |
| `faithful_historical` | Exact historical bytes/version known |
| `reconstruction` | Rebuilt approximation |
| `partial` | Some parts exact, some missing |
| `unknown` | Not established |

**Forbidden:** silently upgrading `reconstruction` (or synthetic material) to
`faithful_historical`. Mark missing provider versions with
`provider_version_status=missing|approximate`.

## Condition (`mora.pilotreplay.condition`)

Kinds: `original_memory`, `no_memory`, `reviewed_memory_edit`.

`reviewed_memory_edit` requires `memory_edit_ref` and `edit_reviewed_by`.
If `harness_unchanged` or `retrieval_unchanged` is false, `declared_confounds`
must be non-empty (or use a separate condition).

## Attempt (`mora.pilotreplay.attempt`)

Statuses: `pending`, `running`, `succeeded`, `failed`, `timed_out`, `skipped`,
`unavailable`.

- `skipped` / `unavailable` require `skip_reason` and **must not** set
  `provider_invoked=true`.  
- `failed` / `timed_out` require `error_code`.
- `succeeded` requires `reset_observed` and `isolation_held`.  
- Do not treat skips/timeouts/failures as successful outcomes.

## Outcome (`mora.pilotreplay.outcome`)

Kinds: `pass`, `fail`, `error`, `skipped`, `unavailable`, `timed_out`,
`inconclusive`. Pairing rules reject e.g. a skipped attempt with `pass`.

## Run gate (`mora.pilotreplay.run_gate`)

Checked **before** provider invocation. Absent run permission, unfrozen
matrix, unfrozen monetary ceiling, or non-positive fail-closed spend ceiling
→ reject.

## Report / receipt

Reports and receipts must set `claims_efficacy=false`. Receipts carry
`example_hashes` and `unresolved_limits` only.
