<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora, the all-remembering eye"/>

# Mora

**Local-first memory for MCP agents.**

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Egress](https://img.shields.io/badge/egress-zero%20by%20default-0a3d33)](docs/guide.md#why-not-just-use-a-cloud-connector)

</div>

Mora indexes your Gmail (one or several accounts), Google Calendar, iMessage, Apple Calendar, and local files into a vault of Markdown files and a SQLite database on your machine, then serves it over MCP to Claude Code, Codex, or any other MCP client. Point several agents at the same vault and they all answer from your real history, with citations.

It runs locally: no server, no Mora account, no telemetry. The only network connections are to the sources you sync, a local Ollama embedder (optional), GitHub for `mora upgrade`, and a private git remote for backup if you turn it on.

## What it looks like

```console
$ mora think "what did we decide with Sam about pricing?"

Evidence (3):
  [mem_20260610_204655_f3049131] Pricing call with Sam: agreed to $29 one-time
  for the pilot; revisit a subscription tier once we cross 10 seats. He wants the
  invoice before Friday.
  ...

Gaps: none detected.
```

`think` returns cited evidence and a list of gaps (stale results, thin coverage, a name the vault has never seen), computed before any model runs. Mora has no LLM and no API key. The calling agent writes the answer; over MCP it calls `search_memory`, `think`, and `brief` itself.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

The script installs the release binary for your platform, clears the macOS Gatekeeper quarantine (binaries are ad-hoc signed, not notarized), and runs `mora init` (vault at `~/vault/mora`). It does not verify checksums; they are on each [release](https://github.com/pyranthus-hq/mora/releases) if you want to check by hand. From source: `go install github.com/pyranthus-hq/mora/cmd/mora@latest` (Go 1.22+, no CGO; source builds report `dev` and cannot self-update).

Then connect sources and wire in your agents:

```bash
mora connect google                    # OAuth login, then backfill Gmail + Calendar (read-only; ~90 days, --since-days to widen)
mora connect google --account work     # add a second mailbox (gmail-work / calendar-work sources)
mora connect imessage                  # macOS; walks you through Full Disk Access
mora schedule install ingest-hourly    # background sync (launchd; prints a cron line on Linux)

claude mcp add mora -s user -- mora mcp serve    # Claude Code
codex  mcp add mora -- mora mcp serve            # Codex; same vault, same memory
```

Per-connector setup, options, and upgrades are in [the guide](docs/guide.md).

## Why a local corpus

Claude and ChatGPT connectors fetch from Google's APIs per query and process the results server-side. That works for "what's on my calendar tomorrow," but not for "what did I commit to, and to whom" across months, and the context you build in one assistant does not carry to the next. Mora keeps a persistent local corpus instead. It indexes iMessage (which has no cloud API), handles several mail accounts at once, and serves all of it to whatever agent you use.

| | Mora | Cloud connectors | MCP Gmail servers |
|---|---|---|---|
| Data lives | Your disk (Markdown + SQLite) | Vendor cloud | Nowhere (live fetch) |
| iMessage | Yes (local `chat.db`, read-only) | No | No |
| Multiple mailboxes in one view | Yes | No | Per server |
| One person across email + texts + calendar | Yes | No | No |
| Shared by every agent (Claude, Codex, etc.) | Yes | Tied to the vendor | Per client |
| Works offline / greppable | Yes | No | No |
| Cost | $0, no Mora account | Subscription | Free |

Cloud tools win on zero setup, a web UI, and write actions. Mora has none of those. Sources and caveats are in [the guide](docs/guide.md#why-not-just-use-a-cloud-connector).

## What you get

- **Plain files you own.** Every email thread, text conversation, and calendar event is one Markdown file. Open them in [Obsidian](docs/guide.md#browse-the-vault-in-obsidian), `grep` them, or back them up like any folder. The SQLite index is disposable: delete it and `mora index rebuild` recreates it from the files.
- **Documents and PDFs.** Point Mora at a folder and it indexes your notes, text files, Word documents, and PDFs, including a PDF someone sends you over iMessage. It only extracts text it can read; a scanned, image-only PDF yields nothing rather than garbage, so OCR it yourself first. [Add a files source](docs/guide.md#add-a-filesystem-source).
- **One view of each person.** `mora graph "Sam"` pulls a person together across email, texts, and calendar, including mail from different accounts, with shared threads, meetings, and the people they appear with. Every line cites its source. Identity merging uses [conservative rules, not a model](docs/guide.md#1-the-entity-graph--derived-from-message-metadata): it merges only on a shared trusted name plus a corroborating address, and leans toward keeping people separate. Most no-reply senders are filed as services, not people.
- **Keyword and semantic search.** Keyword search (BM25) works out of the box. Run `mora config embedder ollama` to add semantic search through [Ollama](https://ollama.com), which also runs locally. [How retrieval works](docs/guide.md#2-hybrid-retrieval--bm25--embeddings--graph-expansion-fused-by-rrf).
- **A daily brief.** `mora brief` shows new and updated threads, upcoming events, and stale open tasks, ranked by a per-person salience score rather than recency. Filter it to one person with `mora brief --entity "Riya"`, or build a cited prep pack for your next meeting with `mora prep`.
- **Opt-in backup.** `mora sync git` pushes the vault to a private git remote you choose: GitHub, GitLab, self-hosted, or a bare repo on a USB drive. It only pushes, never includes the index or your tokens, and fails loudly rather than guessing. Restore is `git clone` plus `mora index rebuild`. [Details](docs/guide.md#back-up-the-vault-off-device-opt-in).
- **One memory, many agents.** 12 MCP tools let any MCP client search memory, read the brief, prep for a meeting (`meeting_prep`), and write facts back (`write_memory`). `mora mcp serve` is a local stdio process, not a network service, so several agents can share the same vault. Every search result reports how fresh each source is. [Wiring guide](docs/guide.md#wire-mora-into-your-agent-mcp).

## Privacy model

- **Read-only at the source.** Google scopes are `gmail.readonly` and `calendar.readonly`. The iMessage and Apple Calendar databases are opened read-only. Mora never sends, changes, or deletes anything in your accounts. Its MCP `write_memory` and `delete_memory` tools only touch memories in the local vault.
- **Everything stays on disk.** The vault, index, and OAuth tokens (`~/.config/mora/tokens/`, mode 0600) live on your machine. There is no analytics endpoint. The only usage log is a local `events.jsonl` that records tool name, timing, result counts, and your raw query text; turn it off with `mora usage off` or `DO_NOT_TRACK=1`.
- **Zero egress by default.** Mora runs no server and never hosts your data. It reaches the network only to sync your sources, talk to a local Ollama embedder, fetch updates during `mora upgrade`, and push to your git remote if you opt in. `mora doctor` warns when the vault is a git repo. `mora sync git` pushes plaintext Markdown; for an encrypted remote, layer [git-remote-gcrypt](https://spwhitton.name/tech/code/git-remote-gcrypt/) on top.

## Docs

**[The guide](docs/guide.md)** covers connectors, MCP wiring, daily use, retrieval, and the cloud comparison. **[docs/architecture/](docs/architecture/00-overview.md)** is the contributor spec: 13 subsystem docs with diagrams and `file:line` citations.

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory.</sub>
</div>
