# Tasks: Typed memory capture

## Phase 1 — Establish prerequisites

- [ ] T001 With baseline T003 recorded, compare typed and headless capture
  on the actual Mac and Windows clients. Record the qualifying owner
  signal, who can emit it, and the rejected agent and quoted-text cases.
  Stop owner capture on any unsupported host. Covers AC-PROVENANCE.

## Phase 2 — Wire verified typed capture

- [ ] T002 [US1] Add the scope and origin regressions before wiring the verified
  path in `internal/mora/hook.go` and the existing governance path. Keep
  fallback and rule drafts governed. Requires proof from T001 for each
  enabled client and the rules T002 draft path. If all clients fail T001,
  keep the governed fallback and leave AC-TYPED open.
  Covers AC-TYPED, AC-PROVENANCE.

## Phase 3 — Record verification

- [ ] T003 Review the authority change and have a separate tester run
  both acceptance cases with actual clients. Record unsupported hosts and
  the fallback used. Depends on T002.
