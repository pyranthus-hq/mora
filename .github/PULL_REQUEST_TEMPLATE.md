## Problem and outcome

<!-- Link the issue/spec. Explain the user-visible or correctness outcome. -->

Closes #

Quality gate or maintenance boundary:

## What changed

<!-- Keep this to one coherent scope. -->

## Verification

<!-- Exact commands, tests, fixtures, and failure cases exercised. -->

## Trust checklist

- [ ] Connector packages do not import `internal/mora`.
- [ ] Release behavior remains pure Go / `CGO_ENABLED=0`; no C SQLite dependency.
- [ ] Sources remain read-only and any network path is explicit.
- [ ] Sync errors and stale state surface; no success watermark advances on failure.
- [ ] Usage/cursor state stays out of the vault and tracking controls are honored.
- [ ] Stable IDs remain provider identity and filename lookups use `SafeFilename`.
- [ ] No credentials, private data, or real connector fixtures are committed.
- [ ] User-data tests ran under an isolated config/vault/state root.
- [ ] Relevant architecture or guide documentation changed with the behavior.
- [ ] Machine-readable output remains byte-clean and deterministic where promised.

## Product-quality evidence

<!-- For product work: name the scored dimension and show the RED→GREEN evidence. For bounded maintenance, write "not applicable" and why. -->
