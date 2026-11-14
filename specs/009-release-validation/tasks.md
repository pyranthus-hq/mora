# Tasks: 0.18 release validation

## Phase 1 — Establish prerequisites

- [ ] T001 Build the acceptance results matrix for the candidate SHA.
  Resolve which proposed cases have entered release scope. Attach the
  actual-client evidence, including [skill library](../012-skill-library/tasks.md)
  T005 results for S001, and leave deferred, failed, or untested rows
  visible. Covers AC-FIRST-USE, AC-RELEASE, AC-COMPARE, S001.

## Phase 2 — Run release acceptance

- [ ] T002 [US1] Have a separate tester run the Mac and Windows first-use
  flows and the plain-file recall comparison on the same fixtures.
  Record times, actions, and scores. Depends on T001. Covers
  AC-FIRST-USE and AC-COMPARE.

## Phase 3 — Record verification

- [ ] T003 Update `docs/guide.md` and release notes for supported behavior.
  Cross-review the evidence and verify required CI jobs at the candidate
  SHA. Hand off to the release owner. Depends on T002. Covers AC-RELEASE.
