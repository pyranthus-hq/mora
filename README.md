<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora, the all-remembering eye"/>

# Mora

**Local-first memory for your AI agents.**

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Egress](https://img.shields.io/badge/egress-zero%20by%20default-0a3d33)](docs/guide.md#why-not-just-use-a-cloud-connector)

</div>

Mora indexes your Gmail (one or several accounts), Google Calendar, iMessage, Apple Calendar, and local files into a **vault** of Markdown files and a SQLite index on your machine, then serves it over MCP (the Model Context Protocol) to Claude Code, Codex, or any other MCP client. Point several agents at the same vault and they all answer from your history, with citations.

It runs entirely on your machine: no server, no signup, no telemetry. iMessage and Apple Calendar need macOS; Gmail, Calendar, and files also run on Linux.

<p align="center">
  <img src="docs/assets/architecture.svg" width="760" alt="Mora architecture: your sources flow into one local vault of Markdown plus a SQLite index, served over MCP to every agent (Claude Code, Codex, Gemini CLI), with an opt-in git sync to another machine."/>
</p>

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

`think` returns cited evidence plus the gaps it found: stale results, thin coverage, a name the vault has never seen. There's no API key and no model to host; your agent does the reasoning and calls `search_memory`, `think`, and `brief` over MCP.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

The script downloads the release binary for your platform, **verifies it against the release `checksums.txt`** before extracting, clears the macOS Gatekeeper quarantine (binaries are ad-hoc signed, not notarized — it prints exactly what it does to Gatekeeper and why), and runs `mora init` (your vault lives at `~/vault/mora`). The checksums file is cosign-signed on each [release](https://github.com/pyranthus-hq/mora/releases) if you want to verify the stronger chain by hand. Update in place later with `mora upgrade` (source builds report `dev` and cannot self-update). Prefer to build it yourself? `go install github.com/pyranthus-hq/mora/cmd/mora@latest` (Go 1.25+, pure Go, no CGO).

### Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/uninstall.sh | sh
```

Removes the `mora` binary and de-registers the MCP server from Claude Code / Codex. Your vault is **preserved by default**; re-run with `--purge` (or `MORA_PURGE=1`) to also delete `~/vault/mora`.

### How Mora is laid out

Mora keeps your data in four places. Only the first is precious:

| Path | Holds | If lost |
| --- | --- | --- |
| `vault_dir` (`~/vault/mora`) | your memories, as Markdown | **back this up** (`mora sync git` mirrors it to a private repo) |
| `data_dir` (`~/.local/share/mora`) | the search index | rebuilt with `mora index rebuild` |
| `state_dir` (`~/.local/state/mora`) | connector sync watermarks | rebuilt on the next sync |
| `config_dir` (`~/.config/mora`) | settings + OAuth tokens | re-create with `mora init` / re-auth |

Run `mora config` to see the resolved paths. Mora will refuse to rebuild the index from a vault that suddenly looks empty or unfamiliar (it could mean your `vault_dir` moved). It tells you how to fix it, or pass `--force` to override.

Then connect your sources:

```bash
mora connect google                    # OAuth login, then backfill Gmail + Calendar (read-only; ~90 days, --since-days to widen)
mora connect google --account work     # add a second mailbox (gmail-work / calendar-work sources)
mora connect imessage                  # macOS; walks you through Full Disk Access
mora schedule install ingest-hourly    # background sync (launchd; prints a cron line on Linux)
```

> **First Google sign-in:** Mora's shared Google app is still going through Google's review, so you'll see a "Google hasn't verified this app" screen. Click **Advanced → Go to Mora** to continue; the access stays read-only and on-device. Prefer your own keys? Set `MORA_GOOGLE_CREDENTIALS=/path/to/client.json` to use your own Google OAuth client instead (optional). See [Connect Google](docs/guide.md#connect-google-gmail--calendar).

Wire Mora into your agents (registers the local MCP server):

```bash
claude mcp add mora -s user -- mora mcp serve    # Claude Code
codex  mcp add mora -- mora mcp serve            # Codex
```

Any other MCP client (Gemini CLI, Cursor, custom hosts) takes the same stdio server as JSON:

```json
{
  "mcpServers": {
    "mora": { "command": "mora", "args": ["mcp", "serve"] }
  }
}
```

Drop that into the client's MCP config (`examples/mcp.json` has a copy). Per-connector setup and options are in [the guide](docs/guide.md).

### No MCP? Use the shell

Every MCP tool is also a CLI command, so any agent with shell access can use Mora without registering the server. Paste this into the agent's instructions file (`AGENTS.md`, `CLAUDE.md`, Cursor rules) to teach it to both **recall from** and **capture to** your vault:

```markdown
## Memory: mora (local)

You have `mora`, a local memory over my Gmail, calendars, iMessage, and files.

RECALL — before answering anything about people, past decisions, projects, or
commitments, query mora first and answer from what it returns:

  mora think "<question>" --json    # cited evidence + an explicit gap check
  mora search "<query>" --json      # search the vault
  mora graph "<person>"             # one person across email, texts, and calendar
  mora brief                        # what changed and what matters lately

Cite every claim with its [stable_id]. If the evidence is insufficient, say so
plainly rather than guessing, and surface any gaps mora reports. Never invent a
memory it did not return.

CAPTURE — when I tell you a durable fact, decision, preference, or commitment
worth recalling later, save it back without being asked:

  mora write --type <fact|decision|insight|task> --title "<title>" --text "<body>"

Capture the signal, not chit-chat: things a future session would want surfaced.
Do not store secrets I mention in passing unless I ask you to.
```

The CLI siblings of every tool (`mora search`, `mora write`, `mora think`, ...) are in [the guide](docs/guide.md#use-mora-from-the-shell).

### Agent skills (optional)

A small plugin of agent-side recipes that pair with the MCP server; they teach an agent to resolve people, shared history, and calendar context from the vault *before* reaching for the web:

```
/plugin marketplace add pyranthus-hq/mora
/plugin install mora@mora
```

First skill: `/mora:dining-concierge`, for outing recommendations grounded in who's coming and where you've actually been. Skills are plain Markdown ([plugins/mora](plugins/mora)); other agents can copy them into their own skills directory.

## Why a local corpus

Mora keeps a persistent local corpus, so you can ask "what did I commit to, and to whom" across months and have the same context whichever assistant you use. It indexes iMessage (which has no cloud API) and handles several mail accounts at once, then serves all of it to whatever agent you use. Cloud connectors fetch from Google's APIs per query and keep nothing between calls.

| | Mora | Cloud connectors | MCP Gmail servers |
|---|---|---|---|
| Data lives | Your disk (Markdown + SQLite) | Vendor cloud | Nowhere (live fetch) |
| iMessage | Yes (local `chat.db`, read-only) | No | No |
| Multiple mailboxes in one view | Yes | No | Per server |
| One person across email + texts + calendar | Yes | No | No |
| Shared by every agent (Claude, Codex, etc.) | Yes | Tied to the vendor | Per client |
| Works offline / greppable | Yes | No | No |
| Cost | $0, no Mora account | Subscription | Free |

## What you get

- **Plain files you own.** Every email thread, text conversation, and calendar event is one Markdown file. Open them in [Obsidian](docs/guide.md#browse-the-vault-in-obsidian), `grep` them, or back them up like any folder. The SQLite index is disposable: delete it and `mora index rebuild` rebuilds it from the files.
- **Documents and PDFs.** Point Mora at a folder and it indexes your notes, text files, Word documents, and text-based PDFs, including a PDF someone texts you. [Add a files source](docs/guide.md#add-a-filesystem-source).
- **One view of each person.** `mora graph "Sam"` pulls someone together across email, texts, and calendar (including mail from different accounts) into shared threads, meetings, and the people they appear with, every line cited. Identity matching is deterministic: it merges the same mailbox written different ways (Gmail dot and +tag variants) and links different addresses only by a shared trusted name with a corroborating address. [How the graph works](docs/guide.md#1-the-entity-graph--derived-from-message-metadata).
- **Keyword and semantic search.** Keyword search (BM25) is built in. Run `mora config embedder ollama` to add local semantic search through [Ollama](https://ollama.com). [How retrieval works](docs/guide.md#2-hybrid-retrieval--bm25--embeddings--graph-expansion-fused-by-rrf).
- **A daily brief.** `mora brief` shows new and updated threads, upcoming events, and stale open tasks, ranked by a per-person salience score. Filter it to one person with `mora brief --entity "Riya"`, or build a cited prep pack for your next meeting with `mora prep`.
- **Off-device backup.** `mora sync git` pushes the vault to any private git remote you control: GitHub, GitLab, self-hosted, or a bare repo on a USB stick. It pushes vault files only; your tokens and the index stay on your machine. Restore with `git clone` and `mora index rebuild`. [Details](docs/guide.md#back-up-the-vault-off-device-opt-in).
- **Shared across agents, or used from your shell.** 12 MCP tools let any MCP client search memory, read the brief, prep for a meeting (`meeting_prep`), and write facts back (`write_memory`). `mora mcp serve` runs as a local stdio process, so several agents share the same vault, and every search result reports how fresh each source is. Every tool is also a plain CLI command (`mora search`, `mora write`, `mora think`), so the vault is just as usable from a terminal. [Wiring guide](docs/guide.md#wire-mora-into-your-agent-mcp).

## Privacy

- **Read-only at the source.** Google scopes are `gmail.readonly` and `calendar.readonly`; iMessage and Apple Calendar are opened read-only. Mora never sends, changes, or deletes anything in your accounts. Its `write_memory` and `delete_memory` tools only touch the local vault.
- **Everything on your disk.** The vault, index, and OAuth tokens (`~/.config/mora/tokens/`, mode 0600) live on your machine. There is no analytics endpoint. The only log is a local usage log of tool name, timing, result counts, and scope. It does **not** record your query text by default (opt in with `mora usage queries on`); turn the log off entirely with `mora usage off` or `DO_NOT_TRACK=1`.
- **Plaintext at rest.** Your vault is unencrypted Markdown on disk. That is what makes it greppable and portable, but it also means anything that can read your home directory can read your memories. Protect it with full-disk encryption (FileVault on macOS), and use [git-remote-gcrypt](https://spwhitton.name/tech/code/git-remote-gcrypt/) for an encrypted off-device backup. Tokens are mode 0600; vault files inherit your filesystem permissions.
- **Bring your own Google keys (optional).** Mora's shared Google app is still in Google's verification queue, so you hit an "unverified app" screen and a ~100-user cap. Point Mora at your own OAuth client with `MORA_GOOGLE_CREDENTIALS=/path/to/client.json` to sidestep both. [Connect Google](docs/guide.md#connect-google-gmail--calendar).
- **Zero egress by default.** No server, no hosting. Mora reaches the network only to sync your sources, fetch updates during `mora upgrade`, and push your opt-in git backup; the optional Ollama embedder is loopback-only. `mora doctor` warns if the vault is a git repo, and [git-remote-gcrypt](https://spwhitton.name/tech/code/git-remote-gcrypt/) gives you an encrypted remote.

## Docs

**[The guide](docs/guide.md)** covers connectors, MCP wiring, daily use, retrieval, and the cloud comparison. **[docs/architecture/](docs/architecture/00-overview.md)** is the contributor spec: 13 subsystem docs with diagrams and `file:line` citations.

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory.</sub>
</div>
