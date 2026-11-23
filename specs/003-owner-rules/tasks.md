# Tasks: Opt-in project owner rules

## Phase 1 — Establish prerequisites

- [ ] T001 With baseline T003 recorded, map exact rule confirmation,
  retirement, and startup injection in `internal/mora/`. Define the
  active-rule fixture and native loader checks for AC-RULE. Prove the
  supported owner signal and direct-agent denial before T002.

## Phase 2 — Project one confirmed rule

- [ ] T002 [US1] Add the projection and isolation regressions first. Implement
  per-project opt-in, bounded rendering, hand-edit handling, retirement,
  and brief deduplication around `project_bind.go` and `hook.go`.
  Depends on T001. Covers AC-BOUNDARY.

## Phase 3 — Record verification

- [ ] T003 Run the scoped Go regressions and a cross-model review. Have
  a separate tester run AC-RULE and AC-BOUNDARY on Mac and the actual
  Windows client. Record the native loader evidence and observed action
  for each session. Depends on T002.
