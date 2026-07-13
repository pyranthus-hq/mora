<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora, the all-remembering eye"/>

# Mora

**Experimental local-first memory and cited meeting context for AI agents.**

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Corpus](https://img.shields.io/badge/corpus-local%20by%20default-0a3d33)](#privacy-boundary)

</div>

> [!WARNING]
> Mora is early alpha software. Its connectors, local corpus, retrieval,
> citations, and identity-review machinery are real. Its meeting output has not
> yet passed a real-data product-quality evaluation. Treat surfaced items as
> historical evidence to inspect, not as verified current obligations. Follow
> the [quality-gated alpha plan](https://github.com/pyranthus-hq/mora/issues/137).

Mora syncs read-only copies of Gmail, Google Calendar, iMessage, Apple Calendar,
and selected files into human-readable Markdown plus a rebuildable SQLite index
on your machine. It exposes that corpus to MCP clients and the shell, so several
agents can retrieve the same history with citations.

Mora does not upload or centrally host your corpus. A connected cloud agent may
send retrieved snippets to its model provider; that behavior is governed by the
agent and its organization policy.

<p align="center">
  <img src="docs/assets/architecture.svg" width="760" alt="Read-only sources flow into a local Markdown vault and SQLite index, then into any MCP client. Backup and sharing are optional network paths."/>
</p>

## Current product hypothesis

Before a client meeting, Mora should show what you owe, what they may owe, and
what changed across Gmail, iMessage, and Calendar—with exact evidence and a
visible warning when a required source is stale.

That is the product hypothesis, not a validated claim. Today Mora retrieves and
cites historical candidate lines. Typed direction and closure, fail-closed
product health, the narrow Before / Teach / Health experience, and real-user
validation are still being built through [seven ordered evidence gates](https://github.com/pyranthus-hq/mora/issues/137).

## What works today

- **Read-only ingestion.** Gmail, Google Calendar, iMessage, Apple Calendar, and
  selected folders become local Markdown memories. iMessage and Apple Calendar
  require macOS.
- **A corpus you own.** Markdown is the source of truth; embedded SQLite, FTS,
  vectors, and the person graph are disposable indexes that can be rebuilt.
- **Cited recall.** `search`, `think`, `brief`, and meeting-prep surfaces return
  stable IDs and dated evidence. Optional Ollama embeddings are loopback-only.
- **Cited meeting context (experimental).** Meeting output surfaces historical
  extracts likely to matter. It does not yet establish obligation owner,
  direction, or closure; inspect each citation before acting.
- **Reviewable identity proposals.** Mora can propose email↔phone joins from
  Address Book evidence, but never applies those joins automatically. Confirm,
  reject, and undo are local and auditable.
- **Agent-agnostic access.** Twelve MCP tools and equivalent CLI commands work
  with any client that can launch a local stdio MCP server.

## Install — experimental

Release artifacts are convenient evaluation builds. macOS artifacts are ad-hoc
signed, not Developer ID signed or notarized. Windows artifacts are unsigned.
The remote installers verify the selected archive against the release
`checksums.txt` and refuse to continue if verification is unavailable.

### macOS / Linux release build

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

The installer downloads the current release, verifies its SHA-256 before
extracting, installs `mora`, and initializes `~/vault/mora`. On macOS it prints
and performs the quarantine removal and ad-hoc signing required by the current
unnotarized build.

### Build from source

```bash
go install github.com/pyranthus-hq/mora/cmd/mora@latest
```

This requires Go 1.25+. Source builds report `dev` and do not self-update. They
ship only the committed non-secret OAuth placeholder, so Google access requires
your own client via `MORA_GOOGLE_CREDENTIALS`. See
[Connect Google](docs/guide.md#connect-google-gmail--calendar).

### Windows

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.ps1 -OutFile $env:TEMP\install-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1
```

The PowerShell installer verifies the release checksum, installs to
`%LOCALAPPDATA%\Mora\bin`, and adds that directory to the user PATH. SmartScreen
may still warn because the binary is unsigned. Platform details and Task
Scheduler commands are in the [Windows guide](docs/windows.md).

## Connect one source

The fastest low-trust path is a folder you choose:

```bash
mora doctor
mora connect filesystem ~/notes
mora search "a project or person"
```

Then add the sources you actually want:

```bash
mora connect google                 # Gmail + Google Calendar, read-only
mora connect google --account work  # optional second Google account
mora connect imessage               # macOS; requires Full Disk Access
mora schedule install ingest-hourly
```

Mora's shared Google OAuth app is currently unverified and subject to Google's
testing-user limit. The trust-first path is your own OAuth client through
`MORA_GOOGLE_CREDENTIALS`; the [guide](docs/guide.md#connect-google-gmail--calendar)
covers both paths.

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

Every MCP tool also has a CLI sibling, so an agent with shell access can use
`mora search`, `mora think`, `mora brief`, and `mora write` without MCP. See the
[wiring guide](docs/guide.md#wire-mora-into-your-agent-mcp).

## Review identity proposals

Mora never automatically joins an email identity to a phone number. On macOS it
can propose matches from Address Book evidence for review:

```bash
mora merge list
mora merge confirm --handle <phone> --email <address>
mora merge reject --handle <phone> --email <address>
mora merge undo <ledger-id>
```

Every decision is local, auditable, and reversible.

## Data layout

Mora keeps data in four places. Only the vault is irreplaceable:

| Path | Holds | Recovery |
| --- | --- | --- |
| `vault_dir` (`~/vault/mora`) | Human-readable memories | Back up explicitly |
| `data_dir` (`~/.local/share/mora`) | SQLite search index | `mora index rebuild` |
| `state_dir` (`~/.local/state/mora`) | Sync watermarks and local usage log | Recreated on sync |
| `config_dir` (`~/.config/mora`) | Settings and OAuth tokens | Reconfigure/re-authenticate |

Run `mora config` to see the resolved paths. The vault is plaintext Markdown;
use full-disk encryption such as FileVault or BitLocker. Advanced opt-in backup
and encrypted sharing exist, but they are not part of the current product
hypothesis; read the [guide](docs/guide.md) before enabling either.

## Privacy boundary

- **Read-only at the source.** Google scopes are `gmail.readonly` and
  `calendar.readonly`; iMessage and Apple Calendar databases are opened
  read-only. Mora never sends mail or changes source records.
- **Local corpus.** The vault, index, tokens, sync state, and usage log remain on
  your machine. The usage log records local operational metadata, not query text
  by default, and honors `mora usage off` / `DO_NOT_TRACK=1`.
- **Explicit network paths.** The Mora process uses the network for enabled
  source sync, release updates, and explicitly enabled backup or sharing.
  Optional Ollama inference is restricted to loopback.
- **Downstream agents are a separate boundary.** After an MCP client retrieves
  context, that client's model and data policy govern it. A cloud-hosted agent
  may transmit retrieved snippets to its provider.
- **Plaintext at rest.** Portability and greppability mean anything that can read
  your home directory may read the vault. Protect the device and avoid putting
  the vault in an unencrypted remote.

## Project status and contributing

The active product plan is the [Mora Home alpha epic](https://github.com/pyranthus-hq/mora/issues/137),
with no ship or payment deadline. Current work is closing the rendered-output
audit and operational replay evidence before commitment-quality implementation.

- [Guide](docs/guide.md) — commands, connectors, and operational details.
- [Architecture](docs/architecture/00-overview.md) — as-built subsystem spec.
- [Contributing](CONTRIBUTING.md) — build, test, and review contract.
- [Security policy](SECURITY.md) — report vulnerabilities privately; never paste
  vault contents or credentials into a public issue.

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory.</sub>
</div>
