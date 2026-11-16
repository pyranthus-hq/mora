# Tasks: Bounded startup and diagnostics

## Phase 1 — Establish prerequisites

- [ ] T001 Pin the in-scope catalog and capture fixture output in the
  existing MCP and hook tests in `internal/mora/`. Include a non-ASCII
  note and omitted relevant note. Covers AC-MCP, AC-HOOK.

## Phase 2 — Implement bounded output

- [ ] T002 [US1] Add threshold and authority regressions, then wire profile,
  hook, and diagnostics budgets in `internal/mora/`. Depends on T001.
  Covers AC-MCP, AC-HOOK, AC-STATUS.

## Phase 3 — Record verification

- [ ] T003 After scoped setup T003 and the final catalog selection,
  have the change cross-reviewed and rerun the fixtures independently
  on Mac and Windows. Record each limit, observed value, and explicit
  recall result. Depends on T002. Covers all three cases.
