<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora, the all-remembering eye"/>

# Mora

**Local-first memory for AI agents: typed, cited commitments from your own mail, messages, and calendars.**

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Corpus](https://img.shields.io/badge/corpus-local%20by%20default-0a3d33)](#privacy-boundary)

</div>

> [!WARNING]
> Mora is alpha software. Its connectors, local corpus, search, citations,
> typed commitments, health checks, and human corrections work, and its
> meeting and daily output passes a strict rendered-output exam on a frozen
> synthetic corpus. It has not yet been validated on other people's real
> data. Treat each surfaced item as cited evidence to check, not as a
> verified current obligation.

Mora syncs read-only copies of Gmail, Google Calendar, iMessage, Apple Calendar,
and selected files. It stores them as readable Markdown and a rebuildable
SQLite index on your machine. MCP clients and the shell can use this corpus.
Several agents can then find the same history with citations.

Mora does not upload your corpus by default or host it for you. Backup and share
commands can send selected data to places you control. A cloud agent can also
send retrieved text to its model provider. The agent and its group policy
control that action.

<p align="center">
  <img src="docs/assets/architecture.svg" width="760" alt="Read-only sources flow into a local Markdown vault and SQLite index, then into any MCP client. Backup and sharing are optional network paths."/>
</p>

## What Mora does

Before a meeting, Mora shows what you owe and what the other person owes you —
each item typed with its owner, direction, due time, and lifecycle state, and
each one cited to the exact message, text, or event it came from. The daily
brief carries the same typed obligation lane. When a required source is stale
or a connector fails, Mora says so instead of guessing: freshness is
fail-closed, and a gap is reported as a gap, never papered over.

These claims are exam-backed, not aspirational. Every release must pass a
frozen rendered-output exam that scores extraction, citation coverage,
counterparty identity, direction, due time, lifecycle, and closure on the real
assembly paths — plus a planted-mutation audit that proves each production
gate has a test that turns red when it is disabled. The strict product target
went from 148 failures to zero and is now a ratchet: any regression fails the
build. See [evaluation and testing](docs/architecture/09-eval-and-testing.md).
Validation on other people's real data has not happened yet; that boundary is
stated here so nobody has to discover it.

## What works today

- **Read-only ingestion.** Mora turns Gmail, Google Calendar, iMessage, Apple
  Calendar, and selected folders into local Markdown memories. iMessage and
  Apple Calendar require macOS.
- **A corpus you own.** Markdown is the source of truth; embedded SQLite, FTS,
  vectors, and the person graph are indexes that you can rebuild.
- **Cited recall.** `search`, `think`, `brief`, and meeting-prep surfaces return
  stable IDs and dated evidence. Optional Ollama embeddings are loopback-only.
- **Typed, cited commitments.** Meeting and daily output surfaces obligations
  with owner, direction, due time, lifecycle state, and closure linkage — every
  line backed by a materialized commitment inventory and an exact citation.
  Untyped candidates never render as obligations.
- **Fail-closed health.** Stale sources, dirty indexes, and failed syncs
  surface as loud warnings on every read path. Mora refuses to present a gap
  as an empty result.
- **Reviewable identity proposals.** Mora can propose email↔phone joins from
  Address Book evidence. Mora never applies these joins on its own. Confirm,
  reject, and undo actions stay local and leave an audit trail.
- **Agent-agnostic access.** Twelve MCP tools and equivalent CLI commands work
  with any client that can launch a local stdio MCP server.

## Install — experimental

Release files are test builds. macOS files have ad-hoc signatures, not
Developer ID signatures or notarization. Windows files have no signatures.
The remote installers check the selected archive against the release
`checksums.txt`. They stop if they cannot do this check.

### macOS / Linux release build

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

The installer downloads the current release and checks its SHA-256. It then
extracts and installs `mora`, and starts `~/vault/mora`. On macOS, it also
prints and runs the quarantine removal and ad-hoc signing steps. The current
build needs these steps because it is not notarized.

### Build from source

```bash
go install github.com/pyranthus-hq/mora/cmd/mora@latest
```

This needs Go 1.25+. Source builds report `dev` and do not update themselves.
They include only the committed non-secret OAuth placeholder. For Google
access, use your own client through `MORA_GOOGLE_CREDENTIALS`. See
[Connect Google](docs/guide.md#connect-google-gmail--calendar).

### Windows

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.ps1 -OutFile $env:TEMP\install-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1
```

The PowerShell installer checks the release checksum. It installs to
`%LOCALAPPDATA%\Mora\bin` and adds that directory to the user PATH. SmartScreen
can still warn because the binary has no signature. See the
[Windows guide](docs/windows.md) for platform details and Task Scheduler
commands.

## Connect one source

For the fastest low-trust start, choose a folder:

```bash
mora doctor
mora connect filesystem ~/notes
mora search "a project or person"
```

Then add only the sources that you want:

```bash
mora connect google                 # Gmail + Google Calendar, read-only
mora connect google --account work  # optional second Google account
mora connect imessage               # macOS; requires Full Disk Access
mora schedule install ingest-hourly
```

Google has not verified Mora's shared OAuth app. Google also limits its test
users. For more control, use your own OAuth client through
`MORA_GOOGLE_CREDENTIALS`. The [guide](docs/guide.md#connect-google-gmail--calendar)
explains both paths.

## Wire it into an agent

```bash
claude mcp add mora -s user -- mora mcp serve
codex mcp add mora -- mora mcp serve
```

Any other MCP client can launch the same local stdio server:

```json
{
  "mcpServers": {
    "mora": { "command": "mora", "args": ["mcp", "serve"] }
  }
}
```

Each MCP tool also has a CLI command. An agent with shell access can use
`mora search`, `mora think`, `mora brief`, and `mora write` without MCP. See
the [wiring guide](docs/guide.md#wire-mora-into-your-agent-mcp).

## Teach Mora

Mora never joins an email identity to a phone number on its own. On macOS, it
can use Address Book evidence to propose matches for review. The queue explains
the corroboration and lists the memories each merge would affect before you
confirm it:

```bash
mora teach identity list
mora teach identity confirm --handle <phone> --email <address> --yes
mora teach identity reject --handle <phone> --email <address>
mora teach identity undo <ledger-id>
```

You can also correct Mora's derived commitments and your own authored memories:

```bash
mora teach commitment wrong-direction --memory-id <id> --direction owed_by_self --yes
mora teach commitment already-closed --memory-id <id> --yes
mora teach memory correct --id <id> --title "Corrected title" --text "Corrected text" --yes
mora teach memory retract --id <id> --yes
mora teach history --memory-id <id>
```

Each decision stays local, preserves its evidence and history, and can be
reversed with `mora teach undo <ledger-id>`. Connector evidence is immutable;
memory correction applies only to authored memories. See
[Teach and human correction](docs/architecture/21-teach.md).

## Data layout

Mora keeps data in four places. Only the vault cannot be rebuilt:

| Path | Holds | Recovery |
| --- | --- | --- |
| `vault_dir` (`~/vault/mora`) | Human-readable memories | Back up explicitly |
| `data_dir` (`~/.local/share/mora`) | SQLite search index | `mora index rebuild` |
| `state_dir` (`~/.local/state/mora`) | Sync watermarks and local usage log | Recreated on sync |
| `config_dir` (`~/.config/mora`) | Settings and OAuth tokens | Reconfigure/re-authenticate |

Run `mora config` to see the full paths. The vault is plain Markdown. Use
full-disk encryption such as FileVault or BitLocker. You can also opt in to
backup and encrypted sharing. These paths are outside the current product
hypothesis. Read the [guide](docs/guide.md) before you enable either one.

## Privacy boundary

- **Read-only at the source.** Google scopes are `gmail.readonly` and
  `calendar.readonly`; iMessage and Apple Calendar databases are opened
  read-only. Mora never sends mail or changes source records.
- **Local corpus.** The vault, index, tokens, sync state, and usage log remain on
  your machine. By default, the usage log stores local run data but not query
  text. It honors `mora usage off` / `DO_NOT_TRACK=1`.
- **Explicit network paths.** The Mora process uses the network for enabled
  source sync and release updates. It also uses the network for backup or
  sharing when you enable them.
  Optional Ollama inference is restricted to loopback.
- **Downstream agents are a separate boundary.** After an MCP client retrieves
  context, its model and data policy apply. A cloud agent can send retrieved
  text to its provider.
- **Plaintext at rest.** Any process that can read your home directory can read
  the vault. Protect the device. Do not put the vault in an unencrypted remote.

## Project status and contributing

Mora is developed against live daily use — the maintainers run it on their own
mail, messages, and calendars every day, and what breaks or misleads in that
use is what gets fixed next. Work lands through per-change issues and PRs with
test evidence; there is no public roadmap. It has no ship or payment deadline.

- [Guide](docs/guide.md) — commands, connectors, and operational details.
- [Architecture](docs/architecture/00-overview.md) — as-built subsystem spec.
- [Contributing](CONTRIBUTING.md) — build, test, and review contract.
- [Security policy](SECURITY.md) — report vulnerabilities privately; never paste
  vault contents or credentials into a public issue.

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory.</sub>
</div>
