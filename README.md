<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora — the all-remembering eye"/>

# Mora

**Local memory for MCP agents.**

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Egress](https://img.shields.io/badge/egress-zero%20by%20default-0a3d33)](docs/guide.md#why-not-just-use-a-cloud-connector)

</div>

Mora backfills your **Gmail, Google Calendar, iMessage, Apple Calendar, and local files** into a vault of plain Markdown files plus a SQLite index on your machine, and serves it over MCP to Claude Code, Codex, or any other MCP client. Agents answer from your actual history — people, commitments, decisions — with citations. There is no server, account, or telemetry; the only network connections are to the sources you sync, GitHub during `mora upgrade`, an optional localhost Ollama embedder, and — only if you opt in — a private git remote you control for vault backup (`mora sync git`).

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

- **Your history as plain files you own.** Every email thread, text conversation, and calendar event becomes one Markdown file on your disk — open them in [Obsidian](docs/guide.md#browse-the-vault-in-obsidian), `grep` them, back them up like any folder. The search database is just an index: delete it and `mora index rebuild` recreates it from the files.
- **Your documents, searchable.** Point Mora at a folder and it indexes your notes and text files and pulls the words out of Word docs and PDFs — and a PDF someone texts you over iMessage becomes a searchable memory of its own. Mora only indexes text it can actually read: a scanned, image-only PDF yields nothing rather than garbage. OCR it yourself and drop the text into a [files source](docs/guide.md#add-a-filesystem-source).
- **One view of each person.** `mora graph "Sam"` gathers a person across email, texts, and calendar — shared threads, meetings, the people they show up with — and every claim cites the memory it came from. Built with [conservative rules, not an AI model](docs/guide.md#1-the-entity-graph--derived-from-message-metadata): it never guesses that two people are the same, and no-reply robots are filed as services, not people.
- **Search that gets what you meant.** Keyword search works out of the box. For "find the thing I'm describing, not the exact words I used," `mora config embedder ollama` turns on semantic search via [Ollama](https://ollama.com) — still entirely on your machine. [How retrieval works](docs/guide.md#2-hybrid-retrieval--bm25--embeddings--graph-expansion-fused-by-rrf).
- **A morning briefing.** `mora brief`: what's new, what you haven't answered, what's coming up — ranked by who actually matters to you, not whoever emailed last. Narrow it to one person with `mora brief --entity "Riya"`, or get a cited prep pack for your next meeting with `mora prep`.
- **A backup you control (opt-in).** `mora sync git` pushes the vault to a private git remote you choose — GitHub, GitLab, self-hosted, even a bare repo on a USB drive. It only ever pushes, fails loudly rather than guessing, and never includes the index or your login tokens. Restore is `git clone` + `mora index rebuild`. [Details](docs/guide.md#back-up-the-vault-off-device-opt-in).
- **Plugs into your AI agent.** 12 MCP tools let Claude Code, Codex, or any MCP-capable agent search your memory, read the brief, prep for a meeting (`meeting_prep`), and save new facts back (`write_memory`). Every search answer says how fresh each source is, so an agent can't silently lean on stale data. [Wiring guide](docs/guide.md#wire-mora-into-your-agent-mcp).

## Privacy model

- **Read-only.** Google scopes are `gmail.readonly` and `calendar.readonly`; the iMessage and Apple Calendar databases are opened read-only. Mora cannot send, modify, or delete anything.
- **All data local.** Vault, index, and OAuth tokens (`~/.config/mora/tokens/`, 0600) stay on disk. No analytics endpoint; the local, content-free usage log is disabled by `mora usage off` or `DO_NOT_TRACK=1`.
- **Zero egress by default.** Mora runs no server and never hosts your data. The one opt-in exception is `mora sync git`, which pushes the vault to a private remote you control — and `mora doctor` warns whenever the vault is a git repo. For ciphertext at the remote, layer [git-remote-gcrypt](https://spwhitton.name/tech/code/git-remote-gcrypt/) over it.

## Docs

**[The guide](docs/guide.md)** — connectors, MCP wiring, daily use, how retrieval works, the cloud comparison. **[docs/architecture/](docs/architecture/00-overview.md)** — contributor spec: 13 subsystem docs with diagrams and `file:line` citations.

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory.</sub>
</div>
