# Isolation, access table and runner/oracle separation

## Runner vs oracle

| Package | Visible to contender / repair? | Contents |
| --- | --- | --- |
| `runner_package` | Yes (minimized) | Task inputs only |
| `oracle_package` | **No** | Hidden tests, expected answers, reference patches, post-cutoff evidence |

A case whose runner package declares any of
`contains_hidden_tests`, `contains_expected_answers`,
`contains_reference_patch`, or `contains_post_cutoff_evidence` is **rejected**.

`reference_memory_ceiling` on the oracle package must be **false** for the
initial pilot (future optional work — not an initial condition).

## Default deny

Unless an explicit, reviewed exception is recorded **outside** this contract:

- production writes  
- external actions  
- live-vault access  

## Access table (summary)

Full rows: `DefaultAccessTable()` in the Go package.

| Principal | Oracle / post-cutoff / live vault | Notes |
| --- | --- | --- |
| Contender | deny | Sees condition memory + runner inputs only |
| Repair | deny oracle & post-cutoff | May propose a reviewed memory edit |
| Oracle | allow checker material | May hash-ref contender outputs |
| Runner | deny live vault / prod writes | May hash-ref oracle package for staging |
| Operator | allow run gate / permission | Private package remains redacted in public artifacts |

## Explicit isolation assumptions

See `DefaultIsolationAssumptions()`. Violating any assumption voids historical
attribution for that attempt. Highlights:

1. Contender/repair cannot read oracle contents.  
2. Post-cutoff evidence is unavailable to contender/repair.  
3. Snapshot bytes are not substituted for exposure parts.  
4. Provider/model version gaps are marked; no silent fidelity upgrade.  
5. Workspace reset between attempts.  
6. No production writes, external side-effects or live-vault reads.  
7. Content minimization of dirty files and memory.  
8. Reference-memory ceiling out of scope for the initial three conditions.  
9. Retrieval/harness changes are new conditions or declared confounds.  
10. Public fixtures are synthetic only.
