# Frozen scoring policy

Authoritative constants live in
`internal/mora/pilotreplay/oracle` (`FrozenScoringPolicy`, `FreezeChecker`).

## Identity

| Field | Public synthetic value |
| --- | --- |
| `checker_id` | `oracle-checker-synth-v1` |
| `checker_version` | `1.0.0-synth` |
| `policy_id` | `scoring-policy-synth-v1` |

Changing checker identity, policy, hidden-spec digest or inputs digest
**after observing comparison results** invalidates that labeled evaluation and
requires a newly labeled run.

## Dimensions (graded separately)

| Dimension | Meaning |
| --- | --- |
| `failed_task_outcome` | Did the coding task pass or show the known fault? |
| `valid_memory_constraint` | Was the surviving valid-memory constraint preserved? |
| `unrelated_regression` | Did unrelated guarded behavior still hold? |
| `no_relevant_memory_control` | Did the control succeed without requiring the proposed memory edit? |

Statuses: `pass` | `fail` | `inconclusive` | `error` | `not_applicable`.

## Aggregate rule

Frozen id:
`any_fail_is_fail; all_required_pass_is_pass; else_inconclusive; error_dominates_to_error`

- any dimension `error` → outcome `error` (**never** agent success)
- any dimension `fail` → outcome `fail`
- all applicable dimensions `pass` → outcome `pass`
- otherwise → outcome `inconclusive` (**never** agent success)

`not_applicable` dimensions are ignored in the pass rollup.

## Missing / unsupported / grader error

| Condition | Policy | Outcome |
| --- | --- | --- |
| Absent / nil artifact | `inconclusive_never_success` | `inconclusive`, unscored |
| Malformed / unsupported artifact | `inconclusive_never_success` | `inconclusive`, unscored |
| Grader error | `error_never_success` | `error`, unscored |

An unsupported artifact or failed grader yields **unscored/indeterminate**,
never an agent success.

## Allowed feedback

Only these fields may surface toward contender / repair authoring:

- `outcome_kind`
- `dimension_statuses` (status enums only)
- `checker_id`, `checker_version`, `checker_digest`
- `policy_id`
- `artifact_digest`
- `scored`

**Forbidden** in feedback and in contender/repair payloads:

- gold labels, expected answers, hidden test bodies
- reference patches, post-cutoff evidence
- dimension detail bodies, full `hidden_spec`

## Executable preference

Prefer executable marker and file-token checks. If a residual rubric later
requires judgment, freeze it and require independent blinded review of that
portion; disclose disagreement and limits rather than inventing precision.
The public synthetic suite is fully executable.

## Checker freeze record

`FreezeChecker(hidden)` binds:

- policy digest
- hidden-spec digest (synthetic stand-in in public; real private gold digests
  stay out of the public repo)
- inputs digest (artifact schema + feedback vocabulary + policies)

`CheckerDigest` combines those with checker id/version.

## Non-claims

- `claims_efficacy` must remain `false`
- Sensitivity ≠ repair benefit
- Same incident repeated is not a new independent case
