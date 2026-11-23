# Tasks: Scoped setup and project switching

## Phase 1 — Establish prerequisites

- [ ] T001 With baseline T003 recorded, verify folder/root and rebind
  semantics on the actual clients. Map scope handling in `internal/mora/`
  and identify unsupported cases. Covers AC-SWITCH.

## Phase 2 — Implement scoped setup

- [ ] T002 [US1] Add isolation and repeated-setup regressions. Implement scope
  resolution and idempotent setup around `project_bind.go`, `mcp.go`,
  `hook.go`, and `setup.go`. Depends on T001. Covers AC-SWITCH,
  AC-SETUP.

## Phase 3 — Record verification

- [ ] T003 Cross-review the implementation. With rule T003 and skill
  T003 fixtures, have a separate tester run both cases on Mac and Windows.
  Record any unsupported rebind, missing suggestion surface, and refresh
  evidence. Depends on T002.
