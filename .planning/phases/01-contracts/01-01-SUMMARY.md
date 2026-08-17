---
phase: 01-contracts
plan: 01
subsystem: cli-contracts
tags: [go, cli, json, receipts, capabilities, contract-testing]
requires: []
provides:
  - "Typed CLI errors and versioned top-level JSON receipt envelopes"
  - "A lint command tracer contract with strict stdout/stderr separation"
  - "The mora.capabilities v1 CLI receipt with catalog and MCP support data"
affects: [01-02, 01-03, 01-04, 01-05, 01-06, 01-07, 01-08, 01-09, 01-10]
actuals:
  tokens: 4344
  tasks: 3
  commits: 3
tech-stack:
  added: []
  patterns:
    - "Top-level versioned JSON receipt envelopes"
    - "Split-stream CLI contract tests"
key-files:
  created:
    - internal/mora/errors.go
    - internal/mora/receipt.go
    - internal/mora/contract_test.go
    - internal/mora/capabilities.go
    - internal/mora/capabilities_test.go
  modified:
    - internal/mora/vaultops.go
    - internal/mora/mora.go
    - internal/mora/loop.go
    - internal/mora/cli_registry_test.go
    - internal/mora/eval/cli-command-registry.json
    - docs/architecture/08-cli-and-ux.md
key-decisions:
  - "Receipt envelope keys merge into the payload top level to preserve published field locations."
  - "ExitCodeFor explicitly recognizes moraError while retaining the closed set of Mora-owned error types."
  - "Capabilities is CLI-only and reports repair/deep-link support as unsupported until later phases."
requirements-completed: [CON-01, CON-02, CON-03, CON-04, CON-06]
coverage:
  - id: D1
    description: "mora lint emits a versioned JSON receipt and rejects unknown flags with a typed error."
    requirement: CON-01
    verification:
      - kind: unit
        ref: internal/mora/contract_test.go#TestContractTracerLint
        status: pass
    human_judgment: false
  - id: D2
    description: "Lint JSON output is isolated on stdout while diagnostics stay on stderr."
    requirement: CON-02
    verification:
      - kind: unit
        ref: internal/mora/contract_test.go#TestContractTracerLint
        status: pass
    human_judgment: false
  - id: D3
    description: "mora capabilities publishes the v1 catalog, MCP, and tri-state feature receipt."
    requirement: CON-04
    verification:
      - kind: unit
        ref: internal/mora/capabilities_test.go#TestCapabilitiesContract
        status: pass
    human_judgment: false
status: complete
---

# Phase 01 Plan 01: Lint Contract Tracer Summary

**A typed-error and top-level receipt contract is proven on `mora lint`, with a CLI-only capabilities receipt that exposes the contract spine to agents.**

## Performance

- **Duration:** recovered interrupted execution; closeout completed in this session
- **Started:** unknown (prior executor)
- **Completed:** 2026-08-17T05:56:40Z
- **Tasks:** 3/3
- **Files modified:** 11

## Accomplishments

- Added `moraError`, stable usage codes, and a major-versioned receipt envelope with no new dependencies or exit codes.
- Made `mora lint --json` a strict machine contract, including an empty-array payload and split stdout/stderr behavior proven without prose matching.
- Added `mora capabilities --json` v1 for the lint tracer, six catalog connectors, 12 MCP tools, configured write policy, schemas, and unsupported repair/deep-link features.

## Task Commits

1. **Task 1: Contract spine — typed error, versioned envelope, and stderr separation on `mora lint`** — `b032ee9` (`feat`)
2. **Task 2: Tracer contract test — split-stream capture and prose-free assertions** — `2108631` (`test`)
3. **Task 3: `mora capabilities --json` skeleton with tri-state feature reporting** — `c27fc49` (`feat`)

## Files Created/Modified

- `internal/mora/errors.go` — typed, code-carrying CLI error implementation.
- `internal/mora/receipt.go` — top-level JSON receipt envelope emitter.
- `internal/mora/contract_test.go` — split-stream lint contract coverage.
- `internal/mora/capabilities.go` — capabilities receipt assembly from the registry, connector catalog, and MCP registry.
- `internal/mora/capabilities_test.go` — machine contract coverage for capabilities.
- `internal/mora/vaultops.go`, `internal/mora/mora.go`, and `internal/mora/loop.go` — lint stream wiring, dispatch, and safe exit-code recognition.
- `internal/mora/eval/cli-command-registry.json` and `internal/mora/cli_registry_test.go` — schema v2 and dispatch-contract validation.
- `docs/architecture/08-cli-and-ux.md` — capabilities receipt and tri-state behavior documented with the CLI change.

## Decisions Made

- Receipt fields are **merged**, not nested: `schema` and `schema_version` sit alongside payload fields so future additions retain published top-level payload shapes.
- `ExitCodeFor` was widened only to recognize `moraError`; it still rejects arbitrary third-party `ExitCode()` implementations, preserving subprocess safety.
- The registry drift test accepted the new `capabilities` token after its explicit all-platform row was added at schema version 2.

## Verification

- Passed `go test ./internal/mora/ -run 'TestCapabilitiesContract|TestCLIRegistry|TestContractTracerLint' -count=1`.
- Passed `go test ./internal/mora/ -count=1`, `go vet ./...`, `gofmt -l .`, and `CGO_ENABLED=0 go build ./...`.
- Passed `CGO_ENABLED=1 go test -race -vet=off -count=1 -timeout=20m ./...`.
- Confirmed no dependency diff in `go.mod` or `go.sum`, no plan-owned deletions, and no AI-attribution trailers in the plan commits.

## Deviations from Plan

None - the remaining Task 3 work matched the plan and its acceptance criteria.

## Issues Encountered

The plan's broad `panic(` scan finds four pre-existing test crash fixtures in `internal/mora/*_test.go`; each predates `b032ee9` and intentionally exercises durable crash boundaries. They are unrelated to this plan and no production panic was introduced.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

Plans 02–10 can extend the established receipt, registry, typed-error, and contract-test conventions without changing the tracer schema.

## Self-Check: PASSED

- Confirmed all five created implementation/test files exist.
- Confirmed task commits `b032ee9`, `2108631`, and `c27fc49` exist in history.

---
*Phase: 01-contracts*
*Completed: 2026-08-17*
