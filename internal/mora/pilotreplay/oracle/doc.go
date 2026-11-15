// Package oracle freezes a hidden outcome checker and damage controls for the
// memory-replay pilot (#544; parent #540).
//
// Owned public paths only:
//   - this package (checker interfaces, frozen scoring policy, synthetic
//     fixtures, access-boundary tests)
//   - docs/experiments/memory-replay/oracle/
//
// Public files contain checker interfaces and synthetic fixtures only.
// Case-specific private hidden tests, reference patches and gold labels remain
// in an authorized evaluator-only location outside the public repository
// (Adit / evaluator handoff until #542 private eligibility). Do not invent
// private gold in-repo.
//
// Do not edit contract/, cases/, runner/, or report/ directories from this
// package. Import the frozen contract (#541) as a leaf dependency. The runner
// is not a construction dependency: output fixtures exercise the checker
// offline in parallel with #543.
//
// # What this package does
//
//   - Grades a copied/sanitized output artifact from a separate evaluator
//     environment (never inside the contender workspace)
//   - Scores dimensions separately: failed-task outcome, valid-memory
//     constraint preservation, unrelated regressions, no-relevant-memory
//     control
//   - Maps unsupported / absent / malformed artifacts and grader errors to
//     unscored/indeterminate (or error) — never to agent success
//   - Freezes checker identity, digest, inputs, allowed feedback and
//     missing-output policy; changing the checker after seeing results
//     invalidates that comparison
//   - Proves access boundaries: contender- and repair-visible payloads omit
//     gold, reference patches, hidden checks and future/post-cutoff evidence
//
// # What this package never does
//
//   - Expose gold / hidden tests / reference patches to contender or repair
//   - Claim repair benefit or historical efficacy (sensitivity only)
//   - Mutate production, call live models, or qualify a real private case
//   - Place private gold in the public repository
//
// Completing this package establishes checker sensitivity on synthetic
// fixtures, not that a memory edit helped.
package oracle
