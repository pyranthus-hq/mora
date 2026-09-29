// Package cases provides synthetic eligibility fixtures and offline manifest
// validation for the memory-replay pilot case-qualification track (#542).
//
// Owned public paths only:
//   - this package (synthetic examples / rejection fixtures / tests)
//   - docs/experiments/memory-replay/cases/ (qualification protocol)
//
// This package does NOT qualify a real historical incident. It does NOT invent
// private cases to meet a quota. Private inventory (≈5–10 candidates) and
// permission review stay with an authorized owner outside the public repo.
// Closing a public PR here means protocol + fixtures only; go/no-go for the
// private packet is a separate handoff.
//
// Do not edit contract/, runner/, oracle/, or report/ directories from this
// package. Import the frozen contract (#541) as a leaf dependency.
//
// Non-claims: completing this package never asserts that memory caused a
// failure, never authorizes model execution or spend, and never places real
// traces, private repository identities, or private source paths in-repo.
package cases
