# Tasks: 0.17.1 governance baseline

## Phase 1 — Establish prerequisites

- [ ] T001 Inspect [#666](https://github.com/pyranthus-hq/mora/pull/666)
  with `gh pr view 666 --json state,headRefOid,mergeCommit,statusCheckRollup`
  and `gh pr checks 666`. Identify required jobs, record unresolved
  checks, and map
  the write-path matrix in `internal/mora/`. Covers AC-BASE, AC-POLICY.

## Phase 2 — Verify supported write paths

- [ ] T002 [US1] Run the existing authority and affected write-path tests against
  that SHA. Record exact test names and executed counts. Add a focused
  regression and gate fix for any uncovered blocking gap. Depends on T001.
  Covers AC-POLICY.

## Phase 3 — Record verification

- [ ] T003 Record required checks for the landed SHA and the Mac/Windows
  upgrade smoke. Have a separate tester rerun the authority matrix and record
  results. Depends on T002. Covers AC-BASE, AC-POLICY. This is the release
  baseline receipt used by the 0.18 features.
