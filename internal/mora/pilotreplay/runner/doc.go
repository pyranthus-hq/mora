// Package runner implements a resettable, local, opt-in coding-agent memory
// replay runner for the pilot (#543; parent #540).
//
// Owned paths only:
//   - this package (adapter, disposable workspace, synthetic contender, tests)
//   - docs/experiments/memory-replay/runner/
//
// The runner consumes the frozen #541 contract and may use #542 synthetic
// eligibility manifests. It does NOT edit contract/, cases/, oracle/, or
// report/ directories. Request contract amendments instead of editing those.
//
// # What this package does
//
//   - Prepares disposable repository/workspace, config, vault/index and
//     harness-home copies; resets each trial and verifies clean initial hashes
//   - Admits a caller-selected private case package without copying it into
//     public fixtures or default logs; validates before starting a process
//   - Injects each condition and records exact delivered context separately
//     from the underlying memory snapshot; tool-result reads are captured or
//     marked explicitly unavailable
//   - Emits attempt receipts (model/harness id, condition/rep, digests,
//     start/end, outputs, exit/failure class, usage/cost/unknowns)
//   - Preserves timeouts, unavailable providers and failed attempts; no
//     silent retries
//   - Enforces approved invocation and monetary/run/time ceilings; terminates
//     before provider execution when any gate is missing
//   - Keeps paid/external model calls impossible in ordinary test runs via a
//     synthetic local contender and a stub external provider that never starts
//     unless an authorized gate is present (and still does not spend)
//
// # What this package never does
//
//   - Hidden grading inside the contender workspace
//   - Gold / future / post-cutoff evidence available to repair
//   - Production vaults, connector stores or real external write credentials
//   - Scoring efficacy or claiming historical success from a runner receipt
//   - Real private-case execution without #545 authorization
//
// A runner receipt establishes infrastructure readiness only.
package runner
