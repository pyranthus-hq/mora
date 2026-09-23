# Mora recovery core integration — 2026-09-23

## Candidate

This isolated checkout combines the provenance read work (`a96be4da`), explicit correction presentation (`0eeb4c06`), and the reviewed ingestion code commits `47095d1c` and `dd2958b8`. The ingestion commits landed here as `34c61c36` and `5e87dbcf`; the ingestion worker's report commits were excluded. `internal/mora/ingest.go` retains both the Gmail mailbox ownership and sync-status checks and the filesystem render failure guard that prevents an existing record and manifest from being lost on a render error. Integration test and lint fixes are in `c23c0f55` and `a1270777`.

The candidate binary is `.cache/recovery/mora`, built from source commit `a1270777e781b6ba753319234c781c046ee51eb1` with `CGO_ENABLED=0`. Its SHA-256 is `24258935095934968207bc43c34875cbcceccfa9b7797d986e1f483949fa3802`. The binary and its isolated smoke vault are local artifacts; neither is installed or committed. The later report-only commit does not change the binary source.

## Review findings

- The Go package graph compiles without connector-to-`internal/mora` import cycles, and the candidate binary builds with CGO disabled. No new dependency or network-writing connector path was introduced.
- Gmail duplicate detection uses the observed mailbox and normalized fetch selection; it checks before spending quota. Sync-status inspection distinguishes malformed receipts from filesystem manifests and preserves malformed files while surfacing diagnostics. The filesystem render error fails before manifest cleanup.
- Correction links come only from visible, same-scope, locally authored records with an explicit `target`. The read projection excludes governance-hidden, pending-delete, and tombstoned corrections. Source and time filters cannot expose a correction's ID or text across the filtered read; an omission marker is used when applicable. The three newly added visibility regressions pass.
- A linked correction is labelled as its writer's assertion. Context warns that a writer's disposition does not establish truth or user adoption. Related evidence remains `later_related_evidence`; no read path inferred supersession or closure from recency. The original authority scorer and labels were not edited. The historical adoption result reported by the provenance worker remains 3 pass and 3 fail, with no same-snapshot reevaluation here.
- One contract golden wrongly called a synthetic calendar record `evidence` even though it lacks a provider ID. It now expects `authored`, matching the derived provenance rule. The CLI correction test now decodes JSON fields instead of asserting substrings over an envelope.

## Verification results

| Check | Exact result |
| --- | --- |
| `env GOCACHE=/private/tmp/mora-recovery-go-cache go test ./...` inside sandbox | Exit 1: local HTTP listeners could not bind (`operation not permitted`), affecting existing loopback tests. This run did not assess the full suite. |
| Same `go test ./...` outside sandbox | Exit 1: all listed packages except `internal/mora` passed. `internal/mora` failed `TestCLIContractProseExemptionsAreDeclared` and `TestActivitySearchCombinedVariantContract`; both causes were fixed after this run. |
| `go test ./internal/mora -run 'TestExplicitCorrection|TestCLIContractProseExemptionsAreDeclared|TestActivitySearchCombinedVariantContract' -count=1` | Pass after the two fixes. |
| `go test ./internal/memory ./internal/ingest ./internal/mora -run 'Test.*Status|TestExplicitCorrection|TestCLIContractProseExemptionsAreDeclared|TestActivitySearchCombinedVariantContract' -count=1` | Pass after the status lint fix. |
| `go vet ./...` | Pass on final Go source. |
| `golangci-lint run ./...` | Initial exit 1 on staticcheck QF1001 in imported status inspection; final run: `0 issues.` |
| `CGO_ENABLED=0 go build -o .cache/recovery/mora ./cmd/mora` | Pass on final Go source. |
| `integration-fixtures/recovery-smoke.sh <isolated-root>` | Pass against the final binary; shell syntax check and `git diff --check` pass. |

The unrestricted complete suite was not repeated after its two test-only fixes and the equivalent Boolean rewrite for lint. The focused repeats and static gates pass; a final full-suite green result remains unclaimed. The race suite was not run. No model call or private-vault evaluation was made.

## Synthetic CLI evidence

The committed [fixture](integration-fixtures/recovery-input.md), [smoke script](integration-fixtures/recovery-smoke.sh), and [observed summary](integration-fixtures/recovery-observed.json) use only synthetic Acorn text. The last run's raw CLI receipts are retained locally under `.cache/recovery/smoke-v4/`: `ingest.json`, `search.json`, `read.json`, `correction-write.json`, `restart-search.json`, `restart-read.json`, `restart-context.json`, and `summary.json`. The smoke pins `MORA_CONFIG_DIR`, `MORA_VAULT`, state, vault, HOME, and the static embedder inside that isolated root. Each CLI command starts a separate process.

The ingest receipt reports one examined and one materialized filesystem item, zero failures, and `status: success`. Search and read return stable source ID `src_a077a8b7003e602d` with `provenance: document`. After a separate-process correction write, fresh-process search attaches correction `mem_20260923_001250_33460678` with `provenance: authored` and writer disposition `outdated`. Fresh-process context places the explicit correction and the warning that this does not establish truth or user adoption before the older launch claim. This is deterministic local CLI behavior with fixed input and assertions; it is not a live-source, native UI, other-machine, or release acceptance result.

## Remaining gate

The coordinator can combine the platform work with this candidate, run a final complete suite if a green full-suite receipt is required, and review packaging and native acceptance separately. The synthetic fixture is available for a fresh agent to inspect the read surface without using the live vault.
