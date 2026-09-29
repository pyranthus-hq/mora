// Package report provides comparison-plan validation, report assembly and
// public-safe methodological summaries for the memory-replay pilot (#545;
// parent #540).
//
// Owned public paths only:
//   - this package (plan validation, assembly, synthetic fixtures, public
//     methodological summary helpers)
//   - docs/experiments/memory-replay/report/
//
// Public files contain plumbing and synthetic fixtures only. Actual inputs,
// repairs, run artifacts, outcomes and spend receipts remain in an authorized
// private experiment location outside the public repository. Real comparison
// waits for private eligibility (#542) and explicit spend authorization (Adit).
//
// Do not edit contract/, cases/, runner/, or oracle/ directories from this
// package. Import the frozen contract (#541) as a leaf dependency. Oracle
// checker identity strings are referenced by value for mismatch checks; this
// package does not invoke the checker or any provider.
//
// # What this package does
//
//   - Validates a ComparisonPlan against explicit spend / run / time ceilings
//     (reject missing ceilings; planning default ≤45 runs is NOT approved spend)
//   - Assembles trial rows while keeping failed/missing trials visible,
//     distinguishing case count from repetition count, rejecting mismatched
//     hashes/scorer versions and preserving negative results
//   - Applies a damage gate when a repair drops valid-memory constraints
//   - Emits a public-safe methodological summary with claims_efficacy=false
//
// # What this package never does
//
//   - Execute real models or call paid/external providers
//   - Place private repairs, artifacts or spend receipts in the public repo
//   - Claim memory-edit efficacy or historical improvement
//   - Authorize spend (Adit-gated; AuthorizeRealComparison fails closed)
//
// Completing this package establishes public report plumbing, not that a
// memory edit helped.
package report
