# CLI command regression and mutation matrix — issue #205

Registry: `internal/mora/eval/cli-command-registry.json`

Behavior evidence: `internal/mora/eval/cli-command-evidence.json`

Verifier and real-dispatch witnesses: `internal/mora/cli_registry_test.go`

Mutation replay: `scripts/eval/cli-command-mutation-matrix.sh`

## Scope

The registry contains 124 production command paths:

- 40 canonical top-level verbs;
- 4 top-level aliases;
- 68 second-level subcommands;
- 12 third-level subcommands.

The count comes from the production dispatch sites, not from the help text. The
AST drift gate compares the registry with `Run` and every command-family
dispatcher. A production token added, removed, or renamed without the matching
registry row fails `TestCLIRegistryMatchesProductionDispatch`.

Decision values such as `teach commitment wrong-person`, connector type names,
schedule job names, and configuration values are typed arguments, not command
dispatch paths. Their validation remains covered by their subsystem tests.

## Load-bearing evidence contract

| Claim | Production anchor | Named witness | Result |
|---|---|---|---|
| Every registered path enters the real top-level dispatcher | `Run` plus the nested `cmd*` family dispatcher | `TestCLIRegistryRealRunDispatch/<path>` | CLOSED for 124/124 rows |
| Production and registry cannot drift | each exact `case` or dispatch comparison token | `TestCLIRegistryMatchesProductionDispatch` | CLOSED for 124/124 rows |
| Every row resolves all nine behavior dimensions | exact path set in `cli-command-evidence.json` | `TestCLIRegistryBehaviorEvidence` | CLOSED for 124/124 rows; N/A and platform seams require reasons |
| Every dispatch token is load-bearing at runtime | one renamed production token per isolated recompile | `TestCLIRegistryRealRunDispatch/<path>` | CLOSED for the 123-row development snapshot below; `forget list` was added by the completion audit and awaits the final clean replay |
| Pipe output stays ANSI-free on every probe | real `Run` stdout/stderr buffers | `TestCLIRegistryRealRunDispatch/<path>` | CLOSED for 124/124 rows |
| Representative JSON surfaces are byte-clean and parseable | `config --json`, `connectors list --json`, `loop list --json` | `TestCLIRegistryJSONSurfacesAreByteClean` | CLOSED |
| Hook JSON I/O uses the real dispatcher | `Run` → `cmdHook` → session-start/recall | `TestCLIRegistryHookIOThroughRun` | CLOSED |
| Every durable loop action composes through the real dispatcher | `Run` → `cmdLoop` → register/list/begin/heartbeat/status/done | `TestCLIRegistryLoopLifecycleThroughRun` | CLOSED |
| `serve` reaches its real dispatcher and refuses unsafe port 0 | `Run` → `cmdServe` → `serveLoopbackHTTP` | `TestCLIRegistryPriorityGapsUseRealRun/serve` | CLOSED |
| `upgrade` is driven through `Run` and refuses source builds before egress | `Run` → `cmdUpgrade` | `TestCLIRegistryPriorityGapsUseRealRun/upgrade` | CLOSED |
| `connect` selects google, imessage, and filesystem without fallthrough | `Run` → `cmdConnect` | `TestCLIRegistryPriorityGapsUseRealRun/connect_routes` | CLOSED |
| `loop` JSON uses the real dispatcher | `Run` → `cmdLoop` → `loopList` | `TestCLIRegistryPriorityGapsUseRealRun/loop_json` | CLOSED |
| `unforget` keeps its confirmation boundary through `Run` | `Run` → `cmdUnforget` | `TestCLIRegistryPriorityGapsUseRealRun/unforget_refusal` | CLOSED |

The all-row probe deliberately appends an invalid flag. Network, connector,
git, loopback, and OS-service rows therefore stop at their parser or refusal
boundary. This is the cross-platform dispatch contract, not a claim that macOS,
Windows, GitHub, S3, or Google side effects ran in a generic CI process.

Native behavior remains pinned at explicit injectable seams:

- scheduler rendering and execution use `runtimeGOOS` and
  `runScheduleCommand`;
- `serve http install|uninstall|status` use `runtimeGOOS`,
  `runScheduleCommand`, and `serveHTTPPortFree`;
- connector availability uses `runtimeGOOS` and read-only provider fakes;
- share git and bucket transports use their command and object-store fakes.

The registry marks these rows `native-seam`, `darwin-seam`, `network-seam`,
`git-seam`, `git-or-bucket-seam`, or `loopback-seam`. It never relabels a
cross-platform parser probe as native execution.

`cli-command-evidence.json` is the checked-in row mapping. Its groups enumerate
the exact registry paths they own and resolve success, usage, invalid-input,
JSON, pipe, durable-state, error, refusal, and mutation evidence.
`TestCLIRegistryBehaviorEvidence` requires the expanded path set to equal the
production registry exactly, requires every dimension to resolve, verifies
every named Go witness still exists, and refuses unreasoned N/A/platform rows.
The mapping distinguishes unsupported JSON, stateless commands, and platform
seams from tested behavior instead of treating absence as coverage.

## Production mutation replay

The replay script makes one isolated production-site mutation per registry row:
it renames that row's exact dispatch token to `__issue205_mutant__`, recompiles
the package, and runs that row's real-`Run` regression. The witness compares
the registered token with an unknown-token control and must turn red for the
specific behavioral reason. The three `serve http` service actions additionally
pin stable branch-specific output so falling through into the random-token HTTP
server cannot masquerade as a kill. A compile error or unrelated failure does
not count.

Segmented development replay on 2026-07-29:

```text
ALLOW_DIRTY=1 GOCACHE=/tmp/mora-go-cache scripts/eval/cli-command-mutation-matrix.sh
KILLED 1..118
SURVIVED usage queries on

# The auditor had changed the outer `usage on` token instead of `args[1]`.
# After anchoring the nested comparison exactly:
ONLY_PATH='usage queries on'  ... cli-command-mutation-matrix.sh  # KILLED
ONLY_PATH='usage queries off' ... cli-command-mutation-matrix.sh  # KILLED

# The service-action witnesses then exposed random HTTP fallthrough output.
# After adding stable action fingerprints:
ONLY_PATH='serve http install'   ... cli-command-mutation-matrix.sh  # KILLED
ONLY_PATH='serve http uninstall' ... cli-command-mutation-matrix.sh  # KILLED
ONLY_PATH='serve http status'    ... cli-command-mutation-matrix.sh  # KILLED
```

The auditor correctly refused both survivors before its anchors/fingerprints
were repaired. The combined development evidence is 123/123 runtime kills for
that registry snapshot with zero unexplained rows. The later completion audit
added the previously omitted `forget list` path, so the final 124-row registry
still needs one end-to-end clean replay. It is intentionally not immutable
closeout evidence, and the final corrected script has not been replayed
end-to-end as one clean run.
Run it without `ALLOW_DIRTY=1` from the final clean revision; the script refuses
a dirty checkout by default.

## Separate existing mutation campaigns

The CLI matrix does not rewrite the meaning of the two earlier campaigns:

- `scripts/eval/exam-mutation-matrix.sh` owns obligation/exam production
  mutations. PR #208 repaired its strict-target driver and re-anchored all
  decayed witnesses. The two future product rows in
  `obligations-v1/mutation-matrix.md` remain explicitly issue-owned; they are not
  hidden CLI holes.
- `scripts/eval/gate2-mutation-matrix.sh` validates the 93 exact Gate 2 named
  witnesses. The production mutation replays in `mutation-matrix-gate2.md` are
  historical evidence and must not be described as newly replayed by that
  witness-integrity command.

## Final closeout commands

Run from a clean immutable revision:

```sh
GOCACHE=/tmp/mora-go-cache go test ./internal/mora -run '^TestCLIRegistry' -count=1
GOCACHE=/tmp/mora-go-cache scripts/eval/cli-command-mutation-matrix.sh
GOCACHE=/tmp/mora-go-cache scripts/eval/exam-mutation-matrix.sh
GOCACHE=/tmp/mora-go-cache scripts/eval/gate2-mutation-matrix.sh
GOCACHE=/tmp/mora-go-cache go test -count=1 ./...
GOCACHE=/tmp/mora-go-cache CGO_ENABLED=1 go test -race -count=1 -timeout=20m -covermode=atomic ./...
GOCACHE=/tmp/mora-go-cache go vet ./...
GOCACHE=/tmp/mora-go-cache golangci-lint run
GOCACHE=/tmp/mora-go-cache CGO_ENABLED=0 go build ./...
```

Close #205 only after the per-row behavior mapping, the clean mutation replay,
and every final command above is green. A green ordinary test suite alone is
not mutation evidence.

## Development verification — 2026-07-29

These results are from the working-tree snapshot and must not be represented as
clean-revision closeout:

| Command | Result |
|---|---|
| `go test ./internal/mora -run '^TestCLIRegistry' -count=1` | GREEN |
| segmented `ALLOW_DIRTY=1 scripts/eval/cli-command-mutation-matrix.sh` replay | GREEN after two auditor repairs — 123/123 runtime dispatch mutants killed for the earlier snapshot; `forget list` added later and pending final clean replay |
| `scripts/eval/exam-mutation-matrix.sh` | GREEN — all audit groups closed and 23/23 planted mutants killed |
| `scripts/eval/gate2-mutation-matrix.sh` | GREEN — all 93 authoritative named witnesses present and green; this is not a fresh replay of the historical production mutants |
| `go test -count=1 ./...` | GREEN |
| `CGO_ENABLED=1 go test -race -count=1 -timeout=5m ./internal/mora -run '^TestCLIRegistry'` | GREEN — final corrected registry harness |
| `go test -race -count=1 ./...` | TIMEOUT — Go's default 10-minute package limit expired in `TestMCPBudgetCeilingsUnhealthy`; no race report appeared |
| `CGO_ENABLED=1 go test -race -count=1 -timeout=20m -covermode=atomic ./...` | GREEN — the checked-in Linux CI command; `internal/mora` completed in 727.463s |
| `go vet ./...` | GREEN |
| `golangci-lint run` | GREEN — 0 issues |
| `CGO_ENABLED=0 go build ./...` | GREEN |
| `git diff --check` | GREEN |

The 20-minute race timeout is not an ad hoc relaxation: `.github/workflows/ci.yml`
uses that exact command because the Gate 2 multi-process storm tests can exceed
Go's default 10-minute package limit on a loaded runner.
