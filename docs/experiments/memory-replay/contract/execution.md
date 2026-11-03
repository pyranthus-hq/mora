# Execution policy, run gate and planning matrix

## Execution policy

Required flags (fail closed if unset):

- `reset_required`  
- `isolation_required`  
- `content_minimization`  
- positive `default_timeout_seconds`  
- fail-closed `cost.ceiling_usd_micros > 0`  
- `default_deny` including production writes, external actions, live vault  

### Retry / missing-run policy

`missing_run_policy` ∈ {`record_unavailable`, `exclude_with_note`, `fail_closed`}.

Skipped and unavailable attempts must **not** invoke the provider. They are
accounted distinctly from failed and successful outcomes.

### Cost accounting

Currency is `USD` micros. Spend must not exceed the ceiling. Accounting does
not imply authorization — see run gate.

## Run gate (before provider invocation)

`AuthorizeProviderInvocation()` requires all of:

1. `run_permission_granted=true` with `permission_record_ref`  
2. `matrix_frozen=true`  
3. `monetary_ceiling_frozen=true`  
4. valid fail-closed spend ceiling  

Schema tests assert rejection when permission or ceiling is absent.

**This contract does not authorize paid runs.** A shape-valid synthetic gate
in fixtures is for tests only.

## Planning matrix (default only)

| Dim | Default |
| --- | --- |
| Reps per cell | 5 |
| Failure cases | 1 |
| Control cases | 2 |
| Conditions | 3 |
| Max planned runs | **45** |

`is_approved_spend` and `is_statistical_claim` must remain **false** on the
planning default. `must_freeze_before_exec` must be **true**.

Freeze the **actual** matrix and monetary ceiling before model execution.
