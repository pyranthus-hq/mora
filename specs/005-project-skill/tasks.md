# Tasks: One project skill

## Phase 1 — Establish prerequisites

- [ ] T001 After the rule pilot, pin the one task, fixture, scope, and
  native loader evidence. Identify the existing verified owner approval
  path and test an agent-only approval attempt. Stop before T002 if no
  verified path exists. A new approval surface needs a scope decision.
  Covers AC-SKILL and AC-SKILL-SCOPE.

## Phase 2 — Install the one-skill pilot

- [ ] T002 [US1] Add regressions for the digest, edit, and detach cases. Implement
  the one-skill install in the existing Mora CLI and project scope layer
  in `internal/mora/`. Depends on T001. Covers AC-SKILL-SCOPE.

## Phase 3 — Record verification

- [ ] T003 Cross-review the diff. Have a separate tester run the Mac and
  Windows acceptance with the same task and control prompts. Record whether
  the pilot earns wider scope. Depends on T002. Covers AC-SKILL,
  AC-SKILL-SCOPE.
