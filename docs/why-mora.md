# Why Mora vs. the alternatives

> The short version lives in the [README](../README.md#why-mora). This is the full
> comparison — how the cloud tools actually work, what Mora does differently, and why
> it matters — with sources. Landscape as of mid-2026.

Most "AI + your email/calendar" tools work the same way under the hood: when you ask a question, the assistant makes a **live API call to a cloud service**, pulls down whatever it needs for that one query, and reasons over it on a remote server. That's fine for "what's on my calendar tomorrow." It's the wrong shape for a *memory* — a durable, cross-source picture of who you talk to, what you've committed to, and what's been said over months.

Mora is built the other way around. It maintains a **persistent local corpus** of human-readable Markdown on your Mac, indexes it in pure-Go SQLite (`modernc.org/sqlite`, no CGO), materializes a **deterministic entity graph** across sources, and serves it to *any* MCP agent. Nothing is fetched per-query from a cloud; nothing leaves the machine.

## How the cloud tools actually work in 2026

- **Claude Desktop / Claude.ai Google connectors** — Connect Gmail, Calendar, and Drive; Claude calls the relevant tool live when a question needs it. Data retrieved during a session is **stored on Anthropic's servers** alongside the chat (deleted when you delete the chat). Anthropic states it doesn't train on connector data, and access mirrors your existing Google permissions. There is **no iMessage**, no persistent local index, and no cross-source graph you own — each chat starts from API fetches.
- **ChatGPT / Codex connectors + Memory** — Connecting a Google app, per OpenAI's own docs, **"may create an indexed copy and sync the content"** to ChatGPT's servers; with Memory enabled, information accessed from connected apps can be **saved into your ChatGPT Memory**. Convenient, but the index and the memory both live in OpenAI's cloud and are opaque — you can't grep them, diff them, or hold them offline. No iMessage.
- **Generic MCP Gmail servers** (GongRzhe, navbuildz, Google's own `gmailmcp.googleapis.com`, etc.) — These translate a request into **real-time Gmail API calls** and explicitly do **not** keep a local corpus. Great for live read/write actions; useless as a memory. No calendar+email+messages fusion, no entity graph, no offline searchable history.
- **Personal-memory apps (Rewind / Limitless)** — Rewind began local-first, but Limitless pivoted to a **"Confidential Cloud"** (data encrypted but processed off-device), and **Meta acquired Limitless in December 2025**, disabling Rewind's Mac capture on Dec 19, 2025. The local-first promise in this category effectively evaporated. Mora keeps it.

## What Mora does that none of them do

- **A local corpus, not a per-query fetch.** Mora backfills Gmail (thread-level), Google Calendar, iMessage, and the filesystem once, into Markdown files you own (`mora connect google`, `mora ingest`). Search is instant and offline; freshness is explicit (`mora sync status`).
- **A real entity graph, not a flat search.** On every index rebuild, Mora compiles people, scopes, tags, `[[wikilinks]]`, and categories into a graph — `PARTICIPATED_IN` / `ATTENDED` / `EMAILED` / `MENTIONS` edges, bi-temporal stamps, fan-out caps — **in the same SQLite transaction as the index (atomic)**. It's deterministic (no NER, no LLM in the loop), and the same person is resolved across email, calendar, and iMessage via connector identity capture (handle↔name, from/to, participants).
- **Hybrid retrieval that fuses sources.** `mora search` blends BM25 (FTS5), cosine over per-memory vectors, and 1-hop graph expansion using **Reciprocal Rank Fusion**. It degrades gracefully to plain FTS5 when no semantic vectors exist.
- **Synthesis you can trust, rented from your agent.** `mora think` (CLI and MCP tool) returns an envelope: cited evidence + a deterministic **"what the vault does NOT know"** gap analysis + a synthesis prompt your agent's own model turns into a cited answer. The gaps are computed *before* any model runs — Mora won't quietly hallucinate coverage it doesn't have.

## The comparison

| Capability | **Mora** | Claude Desktop connectors | Codex + ChatGPT | Generic MCP Gmail |
|---|---|---|---|---|
| Where your data lives | Local Markdown + SQLite on your Mac | Anthropic cloud (per-chat) | OpenAI cloud (indexed copy + Memory) | Nowhere — live API fetch only |
| Egress / telemetry | **Zero egress, zero telemetry** | Data sent to + stored on Anthropic | Data synced to + indexed by OpenAI | Each query hits Google's API |
| Persistent corpus you can grep | **Yes** (human-readable files) | No | Opaque, cloud-only | No |
| Gmail (read-only) | **Yes** (`gmail.readonly`, thread-level) | Yes | Yes | Yes |
| Google Calendar (read-only) | **Yes** (`calendar.readonly`) | Yes | Yes | Often |
| **iMessage** | **Yes** (local `chat.db`, read-only) | **No** | **No** | **No** |
| Cross-source entity graph (people resolved across sources) | **Yes** (atomic, deterministic) | No | No | No |
| Hybrid retrieval (BM25 + vectors + graph, RRF) | **Yes** | N/A (live search) | N/A | N/A |
| Works offline | **Yes** | No | No | No |
| Agent-agnostic | **Yes** (any MCP client) | Claude only | ChatGPT/Codex only | Any MCP client |
| Cost / account | **$0, single binary, no account** | Subscription + Google auth | Subscription + Google auth | Free, but Google auth |
| Zero-setup web UI | No (CLI) | **Yes** | **Yes** | No |

*(To be fair: the cloud tools win on zero-setup and a polished web UI, and they can take write actions — send mail, create events — which Mora deliberately does not. Mora is read-only by design.)*

## Why this matters for you

- **iMessage is the moat.** Your most candid, highest-signal conversations live in iMessage, and **no cloud assistant can legally or technically reach them** — there's no API. Mora reads your local `chat.db` read-only and folds those threads into the same graph as your email and calendar. That's context the cloud simply cannot have.
- **Nothing leaves the Mac.** Read-only scopes, loopback-only optional Ollama (it refuses any non-loopback URL — a hard zero-egress guard), and a local-only usage log you can disable (`DO_NOT_TRACK=1`). For portfolio or personal data you don't want syncing to anyone's servers, this is the only posture that holds.
- **A memory, not a lookup.** Because the corpus is persistent and graphed, Mora answers "who was in the loop on X across email and messages" or "what did I commit to with this person" — questions that require *fused history*, not a fresh per-query fetch.
- **One memory, any agent.** The same vault and the same `mora mcp serve` work with Claude Code *and* Codex. You're not locked into one vendor's cloud memory; your context is portable because it's just files on disk.
- **Honest about what it knows.** `mora think` ships an explicit "what the vault does not know" with every answer, computed deterministically before synthesis — so the agent's cited answer is grounded, not confidently wrong.
- **$0 and self-contained.** One static binary, no Python, no daemon-in-the-cloud, no subscription. `go build`, `mora init`, done.

Sources: [Claude Google Workspace connectors](https://support.claude.com/en/articles/10166901-use-google-workspace-connectors), [ChatGPT Google data controls FAQ](https://help.openai.com/en/articles/10408842-google-app-for-chatgpt-data-controls-faq), [Apps/connectors in ChatGPT](https://help.openai.com/en/articles/11487775-connectors-in-chatgpt), [GongRzhe Gmail-MCP-Server](https://github.com/GongRzhe/Gmail-MCP-Server), [Google Gmail MCP reference](https://developers.google.com/workspace/gmail/api/reference/mcp), [Meta acquires Limitless / Rewind sunset](https://winbuzzer.com/2025/12/05/meta-acquires-ai-wearables-startup-limitless-kills-pendant-sales-and-sunsets-rewind-app-xcxwbn/), [Limitless privacy / Confidential Cloud](https://www.limitless.ai/privacy)
