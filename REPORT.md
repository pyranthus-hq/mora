# Mora attribution and provenance recovery

## Imported from the frozen baseline

Imported only the read-presentation provenance implementation from `baseline.patch` and `baseline-dirty.tar.gz` at base `3433b997`: derived `evidence` / `document` / `authored` origins; MCP read, search, list and context presentation; conservative decision-adoption wording and later-related hints; complete warning lines under tight context budgets; frontmatter line-break and duplicate-field rejection; the labelled authority harness and synthetic fixtures; and the reviewed `mora.context` golden. The imported filesystem-ingest hunk in `internal/mora/ingest.go` is the one shared seam with ingestion: a render error now fails the walk before manifest cleanup can remove a good prior copy. This hunk is required by the frontmatter safety change and should be reconciled with the ingestion worker's branch by the coordinator.

I did not import companion, embedder, reranker, event, digest, or independent Gmail ingestion/account/status changes. The frozen patch's `json-baseline.json` and `mora.capabilities.json` edits were companion-only and were removed after inspection. The small provenance hunk in `internal/mora/gmail_segments_read.go` covers the `read_memory(evidence_ref=...)` presentation path, which bypasses the ordinary read shaper; it does not change Gmail ingestion or status.

## Completed here

- Added derived provenance to CLI `read --json`, `list --json`, and `search --json`, including shared-read and filtered variants. Updated the six affected read/list/search goldens and their variants. The field is presentation-only and is not persisted to Markdown.
- Added synthetic checks for CLI parity, scoped `evidence_ref` origin, and preserving an existing filesystem record and manifest when rendering rejects forged frontmatter.
- Investigated the three historical adoption failures without altering the frozen scorer or labels. Local metadata inspection found six adoption-labelled IDs whose stored types are fact, correction, insight, or mirrored source rather than decision. That is a record-type mismatch in the evaluator's expectation: the renderer only prints an adoption line for decisions. The separate synthetic rendering test confirms nondecisions receive no adoption line. It does not turn any historical failure into a pass or establish user adoption.

## Verification

- Focused tests passed for origin derivation, frontmatter forgery rejection, context warning and budget edges, authority fixtures, scoped `evidence_ref`, CLI JSON parity, filesystem render failure, MCP byte ceilings, and affected contract goldens.
- `CGO_ENABLED=0 GOCACHE=/private/tmp/mora-provenance-go-cache go build ./...` passed.
- `git diff --check` passed. No full suite, live vault evaluation, model calls, or installed-binary changes were run.

## Limits and handoff

The earlier live authority result remains **ADOPTED: 3 pass, 3 fail**; no revised live metric is claimed. The per-case failure report was not available in this checkout. Six nondecision labels establish why some expectations may be invalid, but they do not identify which of the three historical runs failed, and an actual missing warning on a decision remains possible. The frozen scorer deliberately continues to report failures when a labelled row has no adoption line. Semantic user adoption also remains unrecorded by Mora: a writer's claim or a human label elsewhere cannot make the record say the user adopted it. Same-snapshot live before/after scoring remains separate work and was not run against the private corpus here. Ranking and later-pointer recall remain out of scope.

The current frozen companion golden corpus passed after the provenance changes, and no companion golden was imported. This does not establish that companion content from the earlier dirty checkout was unchanged; the frozen baseline contains unrelated companion edits that this branch deliberately excludes.

Sandboxed Orca orchestration `check` and heartbeat repeatedly returned `runtime_unavailable`; a narrowly escalated `check` reached the coordinator and delivered the review instructions. Drona feedback `feedback-5aa6ff6609725aaf` records the sandboxed tool failure.
