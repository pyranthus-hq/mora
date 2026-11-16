# Tasks: Governed friction feedback pilot

## Phase 1 — Establish prerequisites

- [ ] T001 Check source feedback and recovery scope, the prior import
  key, and eligible read access on devbox. Reuse the existing reviewed
  note read-only. Use a disposable synthetic fixture for any write or
  repeat-import check. Stop if devbox share authorization or eligibility
  is missing. Covers AC-FRICTION.

## Phase 2 — Run governed recall

- [ ] T002 [US1] Run the fresh-session DR1 checks, including a wrong-scope
  evidence manifest during refresh. Record the report and recovery
  statuses independently. Depends on T001. Covers AC-FRICTION.

## Phase 3 — Record verification

- [ ] T003 Have a separate tester run the symptom query without the ID.
  Record whether it recalled the correct recovery. Stop for the decision
  on any expansion. Depends on T002. Covers AC-FRICTION.
