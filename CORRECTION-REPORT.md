# Correction retrieval and budget survival

Base: `a96be4da` (the settled provenance recovery dispatch). This commit contains only the correction retrieval follow-up; the provenance baseline import and its separate fixes are described in `REPORT.md`.

## Implemented

- Search and context reads now attach up to two explicit, same-scope, locally authored correction records to a returned prior record. The correction may be below the ranked page or folded by clustering. The projection reuses the existing disposition scan, checks visible vault records and target validity, and does not write the vault.
- Links are selected after the request's source, time, and disposition exclusions. Shared records are not decorated. Filtered correction text and ID are never attached; a generic omission marker warns that the prior claim may lack correction context. The cap preserves the record behind a visible disposition, and correction order uses parsed RFC3339 instants with an ID tie-break.
- Context prints the correction excerpt and writer's disposition ahead of the prior record's body. The title and all warning lines remain atomic under the byte budget. If they cannot fit, a complete short warning replaces them; the old claim is never rendered alone. CLI search, context, and source-filtered list use the same projection or filter boundary as their MCP counterparts.

## Verification

- `go test ./internal/mora -run 'TestExplicitCorrection' -count=1 -v` passed: off-page correction, clustered correction, same-scope isolation, source-filtered MCP/CLI reads, tight context budgets, mixed timestamp offsets, and cap retention of disposition evidence.
- Focused `internal/search` and `internal/mora` provenance, authority, budget, disposition, and filter tests passed; `go test ./internal/disposition -count=1` passed.
- `CGO_ENABLED=0 go build ./...` passed. No full suite, model call, live vault access, or release action was run.

## Limits and remaining work

- This is read presentation for an explicit `target` relationship. It makes no truth, user adoption, supersession, or latest-wins inference. The correction's excerpt is bounded at 180 runes and at most two links are attached; `correction_omitted` says when filters or that cap leave more context out.
- The ranked page still determines which prior records appear. A prior record absent from retrieval cannot receive a visible link. Whether relevant prior records are retrieved and ordered well is a separate ranking evaluation, not a read-presentation pass.
- The prior dispatch's frozen authority adoption failures remain open and were not relabelled or weakened here. Fresh-agent acceptance after integration belongs to the coordinator.
