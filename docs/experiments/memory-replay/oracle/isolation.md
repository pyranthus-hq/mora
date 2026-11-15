# Oracle isolation and access boundaries

## Evaluator environment

Hidden checks run from a **separate evaluator environment** over a
**copied/sanitized** output artifact (`PrepareEvaluatorEnv`):

- Temporary directory outside the contender workspace
- Artifact JSON rewritten without gold-like marker keys
- `file_contents` materialized under `files/` for path-token checks
- Environment removed after grading (`Close`)

The contender process never mounts hidden specs, expected answers or reference
patches.

## Access table (oracle-relevant)

Aligned with the contract default access table (#541):

| Principal | Resource | Access |
| --- | --- | --- |
| Contender | runner package / delivered context / condition snapshot | allow (minimized) |
| Contender | hidden tests / expected answers / reference patches / post-cutoff | **deny** |
| Repair | hidden tests / expected answers / reference patches / post-cutoff | **deny** |
| Oracle | hidden tests / expected answers / reference patches / post-cutoff | allow |
| Oracle | contender workspace | hash-ref only (no mutation) |
| Runner | oracle package | **hash-ref only** (contents not mounted into contender) |

Helpers: `BuildContenderView`, `BuildRepairView`, `BuildRunnerOracleStaging`,
`AssertNoGoldInJSON`, `ToAllowedFeedback`.

## Public vs private

| Public (this repo) | Private (evaluator-only, outside repo) |
| --- | --- |
| Checker interfaces | Case-specific hidden tests |
| Frozen scoring policy | Reference patches |
| Synthetic fixtures + stand-in hidden spec | Gold labels / expected answers |
| Access-boundary tests | Real incident bytes / credentials |

**Private gold handoff:** Adit / authorized evaluator until #542 private
eligibility. Do not invent private gold to meet a quota.

## Leak canaries

Offline tests assert:

- Contender / repair JSON views omit gold bodies and canary markers
- Runner staging sets `hash_ref_only=true` and `contents_mounted=false`
- Allowed feedback omits Detail / hidden_spec / gold substrings
- Public `HiddenSpec` requires `synthetic=true`

## Named limits

- OS-level enforcement that a live contender process cannot open the evaluator
  temp dir is not proven here (unit isolation only)
- Real private hidden suites are not in this PR
- Establishing sensitivity does not authorize #545 comparison spend
