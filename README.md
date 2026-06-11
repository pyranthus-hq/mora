<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora — the all-remembering eye"/>

# Mora

**Local-first memory for your agents. Nothing leaves your machine.**

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Egress](https://img.shields.io/badge/egress-zero-0a3d33)](docs/why-mora.md)

</div>

Every session with Claude Code or Codex starts from zero: the agent doesn't know who Sam is, what you promised him, or that the invoice was due Friday. Mora fixes that. It's a single binary that backfills your **Gmail, Google Calendar, iMessage, Apple Calendar, and local files** into a private, human-readable vault on your Mac — then serves that memory to **any MCP agent**. Your agent answers from your actual history, with citations. Nothing leaves your machine: no cloud, no account, no API key.

## What it looks like

```console
$ mora think "what did we decide with Sam about pricing?"

Evidence (3):
  [mem_20260610_204655_f3049131] Pricing call with Sam — Sam agreed to $29 one-time
  for the pilot; revisit a subscription tier once we cross 10 seats. He wants the
  invoice before Friday.
  …

Gaps: none detected.

(Pass this evidence + gaps to your agent, or run `mora think … --json` for the synthesis prompt.)
```

On your vault the evidence is your real threads, texts, and meetings — and `Gaps:` is the honesty layer: stale evidence, thin coverage on a person, or a name the vault has never seen gets flagged **before any model writes a word**. Wire Mora into your agent once and the same thing happens inside chat: the agent calls `search_memory`, `think`, and `brief` on its own.

## Why Mora

Cloud assistants fetch your mail per-query, reason over it on their servers — and none of them can read iMessage. Mora keeps a **persistent local corpus** instead, which changes what you can ask: not "what's on my calendar" but *"what did I commit to, and with whom, across email and texts."*

| | **Mora** | Cloud connectors (Claude / ChatGPT) | MCP Gmail servers |
|---|---|---|---|
| Your data lives | **On your Mac** — Markdown + SQLite | Vendor cloud | Nowhere — live fetch |
| iMessage | **Yes** (local `chat.db`, read-only) | No | No |
| One person resolved across email + texts + calendar | **Yes** | No | No |
| Greppable corpus, works offline | **Yes** | No | No |
| Agent lock-in | **None** — any MCP client | One vendor | None |
| Cost | **$0**, no account | Subscription | Free |

**iMessage is the moat.** Your most candid threads have no cloud API — Mora reads the local database read-only and folds those conversations into the same people-graph as your email and calendar. Full comparison, caveats, and sources: [Why Mora vs. the alternatives](docs/why-mora.md).

## Quick start

```bash
# macOS / Linux — fetches the release binary, handles Gatekeeper, sets up the vault
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh

mora connect google                    # OAuth → backfill Gmail + Calendar (read-only)
mora connect imessage                  # macOS — walks you through Full Disk Access
mora schedule install ingest-hourly    # keep it fresh in the background

claude mcp add mora -s user -- mora mcp serve    # wire into Claude Code
codex  mcp add mora -- mora mcp serve            # …or Codex
```

Then ask your agent, cold: *"search my memory — what did I last discuss with Sam?"*

Prefer building from source? `go install github.com/pyranthus-hq/mora/cmd/mora@latest` (pure Go, no CGO). macOS gets the full experience — iMessage and Apple Calendar are local-only stores no cloud can reach; on Linux the Gmail, Google Calendar, and filesystem sources work identically. Walkthrough: [QUICKSTART](QUICKSTART.md) · every connector and option: [Setup & operations](docs/setup.md).

## What you get

- **A vault you own.** Every email thread, conversation, and event becomes a Markdown file on disk — open it in Obsidian, grep it, back it up. SQLite is just the index.
- **A people graph, not a contacts list.** `mora graph "Sam"` shows one person across all sources: threads, events, co-occurring people, every edge cited. Built deterministically from real headers and handles — no NER model, no LLM. Bots and no-reply senders are demoted automatically.
- **Hybrid retrieval.** Full-text + embeddings + graph expansion, rank-fused. The default embedder is pure-Go and deterministic; one command (`mora config embedder ollama`) upgrades to local semantic search — loopback-only, enforced.
- **Synthesis with receipts.** `mora think` returns cited evidence plus an explicit *"what the vault does NOT know"* — your agent writes the answer, grounded instead of confidently wrong.
- **A morning brief.** `mora brief` (or the scheduled 8am pulse) tells you what changed and what matters: unanswered threads, upcoming meetings, open loops — ranked by who actually matters to you.
- **11 MCP tools.** Search, context, think, entities, digest, brief, and write-back — one server, any MCP agent, with per-source data freshness attached to search and context answers.

How the three layers work: [How it works](docs/how-it-works.md) · subsystem-level spec with diagrams: [Architecture](docs/architecture/00-overview.md).

## Privacy model

- **Read-only everywhere.** Google scopes are `gmail.readonly` + `calendar.readonly`; the iMessage and Apple Calendar databases are opened read-only/immutable. Mora never modifies or sends anything.
- **Zero egress, zero telemetry.** No server, no account, no analytics endpoint. The only optional network socket is a local Ollama embedder, and it refuses any non-loopback URL.
- **Everything on your disk.** Plain Markdown + local SQLite; OAuth tokens stay in `~/.config/mora/tokens/` (0600), never transmitted.
- **Usage log is local and content-free** — and `mora usage off` (or `DO_NOT_TRACK=1`) disables even that.

## Docs

| | |
|---|---|
| [QUICKSTART](QUICKSTART.md) | The 2-minute happy path |
| [Setup & operations](docs/setup.md) | Every connector, multi-account Google, scheduling, config, upgrading |
| [Why Mora](docs/why-mora.md) | Full comparison vs. cloud connectors and memory apps, with sources |
| [How it works](docs/how-it-works.md) | The entity graph, hybrid retrieval, and the `think` envelope |
| [Architecture](docs/architecture/00-overview.md) | 13 subsystem docs — diagrams + `file:line` citations |

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory — every scrap of it hoarded, nothing ever given away. Fitting, for a vault that never phones home.</sub>
</div>
