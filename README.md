<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora — the all-remembering eye"/>

# Mora

**Local memory for MCP agents.**

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Egress](https://img.shields.io/badge/egress-zero-0a3d33)](docs/guide.md#why-not-just-use-a-cloud-connector)

</div>

Mora backfills your **Gmail, Google Calendar, iMessage, Apple Calendar, and local files** into a vault of plain Markdown files plus a SQLite index on your machine, and serves it over MCP to Claude Code, Codex, or any other MCP client. Agents answer from your actual history — people, commitments, decisions — with citations. There is no server, account, or telemetry; the only network connections are to the sources you sync, GitHub during `mora upgrade`, and an optional localhost Ollama embedder.

## What it looks like

```console
$ mora think "what did we decide with Sam about pricing?"

Evidence (3):
  [mem_20260610_204655_f3049131] Pricing call with Sam — Sam agreed to $29 one-time
  for the pilot; revisit a subscription tier once we cross 10 seats. He wants the
  invoice before Friday.
  …

Gaps: none detected.
```

`think` returns cited evidence plus gaps — stale results, thin coverage, a name the vault has never seen — computed before any model runs. Mora has no LLM and no API key; the calling agent writes the answer, and over MCP it calls `search_memory`, `think`, and `brief` itself.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

The script installs the release binary for your platform, clears macOS Gatekeeper quarantine (binaries are ad-hoc signed, not notarized), and runs `mora init` (vault at `~/vault/mora`). It does **not** verify checksums — they're on each [release](https://github.com/pyranthus-hq/mora/releases) if you want to check manually, or unpack a tarball yourself. From source: `go install github.com/pyranthus-hq/mora/cmd/mora@latest` (Go 1.22+, no CGO; source builds report `dev` and cannot self-update).

Then connect sources and wire in an agent:

```bash
mora connect google                    # OAuth → Gmail + Calendar backfill (read-only scopes)
mora connect imessage                  # macOS; walks you through Full Disk Access
mora schedule install ingest-hourly    # background sync (launchd; prints a cron line on Linux)

claude mcp add mora -s user -- mora mcp serve    # Claude Code
codex  mcp add mora -- mora mcp serve            # Codex
```

Per-connector setup, options, and upgrades: [the guide](docs/guide.md).

## Why a local corpus

Claude and ChatGPT connectors fetch from Google's APIs per query and process results server-side — good for "what's on my calendar tomorrow," not for "what did I commit to, and to whom" across months. Mora keeps a persistent corpus instead, and indexes iMessage, which has no cloud API.

| | Mora | Cloud connectors | MCP Gmail servers |
|---|---|---|---|
| Data lives | Your disk (Markdown + SQLite) | Vendor cloud | Nowhere — live fetch |
| iMessage | Yes (local `chat.db`, read-only) | No | No |
| One person across email + texts + calendar | Yes | No | No |
| Works offline / greppable | Yes | No | No |
| Cost | $0, no account | Subscription | Free |

Cloud tools win on zero setup, a web UI, and write actions; Mora has none of those. Sources and caveats: [the guide](docs/guide.md#why-not-just-use-a-cloud-connector).

## What you get

- **A Markdown vault.** One file per email thread, conversation, or event — grep it, open it in Obsidian, back it up. SQLite is a rebuildable index, not the store.
- **An entity graph.** `mora graph "Sam"` shows one person across sources — threads, events, co-occurring people, each edge citing its source memory. Rule-based over message headers, calendar attendees, and address-book names (no NER model); identity merging is conservative; no-reply senders are filed as services, not people.
- **Hybrid search.** BM25 + embedding + graph expansion, fused by reciprocal rank. The default hash embedder is weak on paraphrase; `mora config embedder ollama` switches to local semantic embeddings via [Ollama](https://ollama.com).
- **A daily brief.** `mora brief`: new and unanswered threads, upcoming meetings, open loops, ranked by contact salience.
- **11 MCP tools**, including write-back (`write_memory`). Search responses carry per-source `last_synced` timestamps.

## Privacy model

- **Read-only.** Google scopes are `gmail.readonly` and `calendar.readonly`; the iMessage and Apple Calendar databases are opened read-only. Mora cannot send, modify, or delete anything.
- **All data local.** Vault, index, and OAuth tokens (`~/.config/mora/tokens/`, 0600) stay on disk. No analytics endpoint; the local, content-free usage log is disabled by `mora usage off` or `DO_NOT_TRACK=1`.

## Current limitations

- iMessage and Apple Calendar are macOS-only and require Full Disk Access.
- The bundled Google OAuth app is unverified — consent shows Google's warning (Advanced → continue), or bring your own client via `MORA_GOOGLE_CREDENTIALS`.
- No Google Drive or GitHub ingestion. The filesystem source skips PDFs; `.docx` works.
- Sync re-fetches a recent window each run rather than tailing changes; `mora sync status` shows per-source freshness.
- Default search is lexical; paraphrase recall needs the Ollama embedder.
- Binaries are not Apple-notarized, and the installer does not verify checksums.
- CLI and MCP only; no web or desktop UI.

## Non-goals

- Write actions against your accounts (sending mail, creating events).
- Hosting your data. There is no Mora server; the vault is plain files, so if you want it on two machines, sync it with whatever you already trust (iCloud Drive, Syncthing, git).
- Replacing your mail client, calendar app, or CRM.

## Docs

**[The guide](docs/guide.md)** — connectors, MCP wiring, daily use, how retrieval works, the cloud comparison. **[docs/architecture/](docs/architecture/00-overview.md)** — contributor spec: 13 subsystem docs with diagrams and `file:line` citations.

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory.</sub>
</div>
