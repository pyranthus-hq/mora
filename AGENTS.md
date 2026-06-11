# AGENTS.md — Mora

Cross-tool guidance for AI agents and reviewers (any tool that reads `AGENTS.md`).
The deep architecture spec lives in [`docs/architecture/`](docs/architecture/00-overview.md)
— read the overview first; this file adds the review rules enforced on PRs.

## What Mora is

Local-first, agent-agnostic memory CLI: one pure-Go binary that stores
human-readable Markdown memories, indexes them in embedded SQLite, and serves
them to any MCP agent. Read-only connectors (Gmail, Calendar, iMessage), zero
egress. See the [README](README.md) for orientation, [`docs/setup.md`](docs/setup.md)
for every command and connector, and [`docs/architecture/`](docs/architecture/00-overview.md)
for the subsystem spec.

## Review guidelines

When reviewing a PR, **focus on the diff** and report only material issues
(P0/P1). Skip style nits — `gofmt`, `go vet`, and `golangci-lint` are
deterministic CI gates and already own formatting. Cap findings; be terse.

Hard rules — flag any violation as blocking:

1. **No import cycle:** `internal/google` (and every connector package) must
   NOT import `internal/mora`. Connectors return plain `MappedMemory` structs;
   `mora` wires them at the boundary. New connectors follow the same rule and
   live in `internal/<provider>`, importing only `internal/memory`.
2. **Pure Go, no CGO:** the product must build with `CGO_ENABLED=0`. No C
   extensions (no `sqlite-vec` C ext), no `mattn/go-sqlite3` — `modernc.org/sqlite`
   only. The race detector in CI (`CGO_ENABLED=1`) is test-only and never affects
   the release build.
3. **Read-only + zero egress:** Google scopes stay `gmail.readonly` /
   `calendar.readonly`. iMessage opens `chat.db` with `mode=ro` (never `immutable=1`);
   Apple Calendar opens its store `mode=ro&immutable=1` (Calendar.app holds the
   write lock). No connector writes to its source; no telemetry/egress.
4. **Honest-snapshot sync:** never swallow sync errors — surface them
   (freshness is the product's value).
5. **State vs vault:** usage logging and sync cursors live in the **state dir**,
   never the vault. Honor `DO_NOT_TRACK` / `mora usage off`.
6. **Identity vs filename:** `StableID` is provider identity only
   (`gmail_thread/<id>`), never content; files are named with `SafeFilename`
   (`/`→`_`). Any ID lookup must match the SafeFilename form.
7. **Dependency hygiene:** don't `go mod tidy` between adding a dep and its first
   import (tidy prunes unimported requires). Don't add a dependency without a
   clear need; prefer the standard library.
8. **Secrets:** `internal/google/client.json` is a committed NON-SECRET
   placeholder. Real credentials come from `MORA_GOOGLE_CREDENTIALS` at runtime —
   never commit real creds.

