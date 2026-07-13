# Contributing to Mora

Thanks for taking a look. Mora is experimental local-first memory for AI agents: one pure-Go
binary that pulls your Gmail, calendars, iMessage, and files into a Markdown
vault and serves it over MCP. It's open source under Apache-2.0, and I build it
in the open.

The active milestone is product quality before promotion or charging. The most
useful reports are reproducible failures, redacted product-quality examples, and
bounded changes tied to the [alpha gates](https://github.com/pyranthus-hq/mora/issues/137).
If something breaks, feels wrong, or surprises you, use the matching issue form.
PRs are welcome too — this guide covers how to run the same checks as CI.

## Get oriented first

- **[README.md](README.md)** — install, connect your sources, wire it into an agent.
- **[docs/guide.md](docs/guide.md)** — every command, connector, and option.
- **[docs/architecture/](docs/architecture/00-overview.md)** — the contributor spec: subsystem docs with diagrams. Read the overview before a non-trivial change.
- **[AGENTS.md](AGENTS.md)** — the hard rules enforced on every PR (summarized below). Worth reading even if you're not an agent.

## Build and test

You need Go 1.25+ (see `go.mod`). No CGO toolchain, no system SQLite, no other
native deps — that's the whole point. The test suite passes on a fresh clone
with no network, no credentials, no Ollama, and no macOS databases; anything
that needs those is gated and skips by default.

```bash
git clone https://github.com/pyranthus-hq/mora
cd mora
go build -o mora ./cmd/mora      # build the binary
go test ./...                    # run the suite
```

## Match CI before you push

CI runs five jobs. You can reproduce all of the blocking ones locally:

```bash
# 1. Formatting — must print nothing
gofmt -l .

# 2. Vet
go vet ./...

# 3. Tests with the race detector. The race detector is the one place CGO is
#    allowed: it's test-only and never touches the release build.
CGO_ENABLED=1 go test -race -count=1 -covermode=atomic ./...

# 4. Lint. The .golangci.yml is v2 format, so you need golangci-lint v2
#    (CI pins v2.12.2). v1 cannot read the config.
golangci-lint run --timeout=5m

# 5. Pure-Go cross-arch build — proves the single-binary guarantee holds.
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o mora ./cmd/mora
```

Two more jobs run on PRs and are advisory, not blocking: a binary-size diff
against `main` (a small static binary is the product, so size regressions get a
comment) and a `gitleaks` secret scan over full history. The secret scan is the
one advisory job worth treating as blocking yourself — see rule 8 below.

If `gofmt -l .`, `go vet`, the race tests, and `golangci-lint` are all clean
locally, CI will be green.

## The hard rules

These are enforced on PRs and flagged as blocking. Full text in
[AGENTS.md](AGENTS.md); the load-bearing ones:

1. **No import cycle.** Connector packages (`internal/google`, etc.) must not import `internal/mora`. They return plain `MappedMemory` structs; `mora` wires them at the boundary. New connectors live in `internal/<provider>` and import only `internal/memory`.
2. **Pure Go, no CGO.** The product builds with `CGO_ENABLED=0`. No C extensions, no `mattn/go-sqlite3` — `modernc.org/sqlite` only. The race detector's `CGO_ENABLED=1` is test-only.
3. **Read-only sources and explicit network boundaries.** Google scopes stay `gmail.readonly` / `calendar.readonly`. Connectors never write to their source, and Mora operates no hosted corpus or telemetry service. Source sync, `mora upgrade`, and opt-in backup/share use the network. After an MCP client retrieves context, that client's model and data policy apply.
4. **Honest-snapshot sync.** Never swallow a sync error — surface it. Freshness is the product's value.
5. **State vs vault.** Usage logging and sync cursors live in the state dir, never in the vault. Honor `DO_NOT_TRACK` / `mora usage off`.
6. **Identity vs filename.** `StableID` is provider identity only, never content; files are named with `SafeFilename`. Any ID lookup must match the SafeFilename form.
7. **Dependency hygiene.** Prefer the standard library. Don't add a dependency without a clear need, and don't `go mod tidy` between adding a dep and its first import.
8. **Secrets.** `internal/google/client.json` is a committed non-secret placeholder. Real credentials come from `MORA_GOOGLE_CREDENTIALS` at runtime. Never commit real credentials.

## Sending a PR

- Open an issue first for anything beyond a small fix, so we can agree on the approach before you write it.
- Keep the diff focused. One change per PR.
- Add or update tests for behavior you change. Match the existing table-driven style.
- If you touch a subsystem documented in `docs/architecture/`, update that doc in the same PR.
- Write a clear PR description: what changed, why, and how you verified it.

Reviews focus on the diff and on the rules above. Style nits are owned by
`gofmt`, `go vet`, and `golangci-lint`, so they won't come up in review — just
run them first.

## Reporting bugs

Use the bug or product-quality issue form with what you ran, what you expected,
and what happened. `mora doctor --json`, source freshness, the failing command,
and your OS and Go version help after aggressive redaction. Never paste vault
content, credentials, tokens, personal identifiers, or source databases.

Report suspected vulnerabilities privately through [the security policy](SECURITY.md).
