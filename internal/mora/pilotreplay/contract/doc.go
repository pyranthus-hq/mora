// Package contract freezes the smallest versioned interface for a local,
// opt-in coding-agent memory replay pilot (#541).
//
// The package is a leaf: types, bounds, validation and synthetic fixtures
// only. It imports the standard library alone and must never import
// internal/mora, so consumers and fixtures can compile against the contract
// without dragging the kernel. No runner, oracle, vault I/O, provider client
// or production integration lives here — those implement against this
// interface in their own directories (#543+).
//
// # What the pilot asks
//
// Whether changing memory changes a later coding outcome while preserving
// valid constraints. Completing this contract neither qualifies a real case
// (#542) nor authorizes a paid model run.
//
// # Schema versioning
//
// Every document carries schema_version. Adding an optional field is MINOR
// and keeps SchemaVersion. Removing, renaming or retyping a field, or
// changing a required enum, requires a SchemaVersion bump.
//
// # Public artifacts
//
// Fixtures and docs under this package and docs/experiments/memory-replay/
// contract/ are synthetic only. Private case paths, contents, source
// identities and credentials must never appear in the public repository.
package contract
