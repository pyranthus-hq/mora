# Mora

Mora is a **local-first memory CLI**. It pre-loads your Gmail (thread-level, multi-account), Google Calendar, **iMessage**, **Apple Calendar**, and local files (.md, .txt, .docx) into a searchable vault on your Mac, builds a deterministic **entity graph** + **hybrid retrieval** + cited **`mora think`** synthesis on top, and serves all of it to Claude Code, Codex, and any MCP agent — **with zero egress**. Nothing leaves your machine.

---

## Quick start

**Requirements:** Go 1.22+ to build from source; macOS for the iMessage connector. Pure Go, single
static binary, no CGO, no Python — `modernc.org/sqlite` only.

```bash
go build -o mora ./cmd/mora       # or: CGO_ENABLED=0 go build -o mora ./cmd/mora
./mora init                       # create the vault (~/vault/mora)

./mora connect google             # OAuth → backfill Gmail + Calendar (read-only)
./mora connect imessage           # macOS → backfill iMessage (needs Full Disk Access)

./mora search "graduation"        # full-text search the vault
./mora graph                      # see the entity graph (people + topics)
./mora graph "Sam"               # everything about one person, cited
./mora mcp serve                  # serve it all over MCP to Claude Code / Codex
```

Connectors are **opt-in** and **read-only**; the index lives in local SQLite; the only optional
network socket is a loopback-only Ollama embedder (off by default). Detailed setup for each
connector, MCP wiring, and upkeep is below.

---

## How the intelligence works

Mora's retrieval is three deterministic layers that compile from your own data at ingest time — no NER, no cloud, no API key, no model download. Everything below runs in the single static Go binary against a local SQLite index. The only optional network socket is a *loopback-only* Ollama upgrade (covered below), and it refuses any non-loopback URL.

### 1. The entity graph — derived from your real mail and messages, not inferred

An **entity** is a thing your vault refers to repeatedly: a **person**, a **scope** (project/namespace), a **tag**, a `[[wikilink]]`, or a `- [Category]` line. Mora materializes these — and the edges between them — into `entities` / `edges` tables **in the same transaction as the index rebuild** (`buildGraph` in `internal/mora/graph.go`), so the graph is always atomically consistent with the search index and byte-identical across rebuilds.

People are the interesting part, and they come straight from connector identity capture — no name-entity-recognition model is involved. When Gmail, Calendar, and iMessage ingest, they already carry structured identity in each memory's metadata: `from` / `to` / `cc` for mail, `organizer` / `attendees` for calendar, and iMessage handle↔name `participants` pairs. `personRefs` resolves those into canonical `person:<lowercased-identity>` nodes (so `neil@x.com` referenced in 40 threads collapses to one node), and emits edges with bi-temporal stamps and provenance back to the source memory:

- **`PARTICIPATED_IN`** — a person was on a thread or chat
- **`ATTENDED`** — a person was on a calendar event
- **`EMAILED`** — sender → each recipient, mail only
- **`MENTIONS`** — a person *known from metadata* who also appears by name in another message's body, matched by a gazetteer built **from the graph's own person aliases** (`gazetteer.go`) — word-boundary, multi-token names, stoplisted, deterministic tie-break. Still no model.

A blast email with 500 recipients won't explode the graph: person fan-out is capped (`maxParticipantFanout = 64`, and it *warns* rather than silently dropping — the repo's honesty rule), and co-occurrence ("who else was on Sam's threads") is a **query-time self-join**, never materialized, so an N-person thread costs O(N) edge rows, not O(N²).

```bash
mora entities                  # grouped: People / Scopes / Links / Categories / Tags, with counts
mora entities "Sam Rivera"     # the memories that reference one entity
mora entities "neil@x.com"     # same person — exact alias/email/handle lookup
mora entities --json
```

**Concrete example.** You and Sam traded 40 emails and a few iMessages. `mora entities` shows `Sam Rivera  43` under **People**. `mora entities "Sam Rivera"` lists those 43 memories; via MCP, `get_entity` additionally returns his aliases (every address/handle/name variant seen), his `degree`, the incoming edges with their `evidence_id`, and his 1-hop `neighbors` — the people he shares threads or events with. No cloud service can build this, because no cloud service can see your `chat.db`. That's the moat.

### 2. Hybrid retrieval — BM25 + static embeddings + graph expansion, fused by RRF

Keyword search misses paraphrase ("launch" vs "shipping"); pure vector search drifts off exact terms and proper nouns. `hybridSearch` (`internal/mora/hybrid.go`) runs three retrievers and fuses them:

1. **FTS5 / BM25** — the exact-match correctness anchor. Proper nouns, IDs, and literal phrases always rank.
2. **Static-embedding cosine** — recall for paraphrase, over per-memory vectors in `mem_vectors`. Zero-similarity rows are dropped so the vector arm never drags in unrelated memories.
3. **1-hop graph expansion** — if the query *names a person* (gazetteer + exact alias match), pull that person's evidence memories into the candidate pool. GraphRAG-lite, no LLM.

The three ranked lists are merged with **Reciprocal Rank Fusion**, `score = Σ 1/(k + rank)`, `k = 60`. RRF is *rank*-based, so it fuses BM25's unbounded scores with cosine's `[0,1]` without any normalization, and dampens the head so no single arm dominates.

**Be honest about the embedder.** Today it is a pure-Go, deterministic **feature-hashing static embedder** (`staticEmbedder` in `embed.go`): it hashes word tokens and character trigrams into a fixed 256-dim space (signed hashing trick, TF-weighted, L2-normalized). Cosine then tracks shared lexical + subword features — so "launching" and "launch" share signal. This is *not* a vendored transformer (no model2vec/potion weights baked in); it's the deterministic, $0, single-binary floor. It sits behind an `Embedder` interface, so a real potion checkpoint or an **Ollama** model (`nomic-embed-text`) drops in unchanged. Ollama is strictly opt-in (`MORA_EMBEDDER=ollama`), and `chooseEmbedder` **refuses any non-loopback `MORA_OLLAMA_URL`** — memory text never leaves the machine, and an unreachable daemon degrades to the static embedder with a warning, never an error.

**Graceful degradation.** On a pre-I2 index with no `mem_vectors` table, `hybridSearch` is simply FTS-only — search still works. As soon as vectors exist, the cosine and graph arms light up automatically. The model id is stored per vector, so changing embedders triggers a clean re-embed rather than silently mixing incompatible vectors.

```bash
mora search "the pricing thing Sam mentioned" --json   # hybrid by default; FTS-only if no vectors
```

### 3. `mora think` — a synthesis envelope your own agent fills in

`mora think` (`think.go`) does **not** contain an LLM and holds no API key. It returns a *synthesis envelope* — everything an agent needs to write a cited answer — and rents the actual prose generation from the agent that called it (via MCP). The envelope has three parts:

- **Cited evidence** — the top hybrid-retrieval hits, each with `stable_id`, scope, timestamp, fused score, and a snippet, so every downstream claim is attributable.
- **Gap analysis — "what the vault does NOT know"** — computed deterministically *before any model runs*, the trust feature that guards against confidently-wrong RAG. Three honest signals: **stale** (freshest matching memory older than 30 days), **thin coverage** (a distinctively-named person in the query has fewer than 2 memories), and **coverage holes** (a real-name-shaped phrase in the query that resolves to no entity at all). The gap logic reuses the gazetteer's eligibility guards, so a title-cased question phrase like "What Did" is never mistaken for a missing person.
- **A synthesis prompt** — a ready-to-run instruction: *answer using only this evidence, cite every claim with its `[stable_id]`, and surface the known gaps in a "What the vault does not know" section.*

**Concrete example.** `mora think "what did we decide with Sam about pricing?"` retrieves the relevant threads as cited evidence, and if the freshest is two months old it adds a `stale` gap; if Sam has only one memory it flags thin coverage. Your agent reads the envelope and composes the answer — grounded, cited, and honest about its blind spots. Mora did the retrieval and the gap accounting for $0; the agent paid for the sentence-writing.

```bash
mora think "what did we decide with Sam about pricing?"
mora think "..." --json        # full envelope: evidence + gaps + synthesis_prompt
```

**Over MCP** (`mora mcp serve`), the same three layers are exposed as tools any MCP-capable agent (Claude Code, Codex) can call: **`think`**, **`list_entities`**, and **`get_entity`**.

Relevant source: `internal/mora/entities.go`, `graph.go`, `graph_read.go`, `hybrid.go`, `embed.go`, `embed_ollama.go`, `gazetteer.go`, `think.go`. Verified: 229 tests green under `-race`, `go vet` + `golangci-lint` clean, `CGO_ENABLED=0` builds.

---

## Why Mora vs. the alternatives

Most "AI + your email/calendar" tools work the same way under the hood: when you ask a question, the assistant makes a **live API call to a cloud service**, pulls down whatever it needs for that one query, and reasons over it on a remote server. That's fine for "what's on my calendar tomorrow." It's the wrong shape for a *memory* — a durable, cross-source picture of who you talk to, what you've committed to, and what's been said over months.

Mora is built the other way around. It maintains a **persistent local corpus** of human-readable Markdown on your Mac, indexes it in pure-Go SQLite (`modernc.org/sqlite`, no CGO), materializes a **deterministic entity graph** across sources, and serves it to *any* MCP agent. Nothing is fetched per-query from a cloud; nothing leaves the machine.

### How the cloud tools actually work in 2026

- **Claude Desktop / Claude.ai Google connectors** — Connect Gmail, Calendar, and Drive; Claude calls the relevant tool live when a question needs it. Data retrieved during a session is **stored on Anthropic's servers** alongside the chat (deleted when you delete the chat). Anthropic states it doesn't train on connector data, and access mirrors your existing Google permissions. There is **no iMessage**, no persistent local index, and no cross-source graph you own — each chat starts from API fetches.
- **ChatGPT / Codex connectors + Memory** — Connecting a Google app, per OpenAI's own docs, **"may create an indexed copy and sync the content"** to ChatGPT's servers; with Memory enabled, information accessed from connected apps can be **saved into your ChatGPT Memory**. Convenient, but the index and the memory both live in OpenAI's cloud and are opaque — you can't grep them, diff them, or hold them offline. No iMessage.
- **Generic MCP Gmail servers** (GongRzhe, navbuildz, Google's own `gmailmcp.googleapis.com`, etc.) — These translate a request into **real-time Gmail API calls** and explicitly do **not** keep a local corpus. Great for live read/write actions; useless as a memory. No calendar+email+messages fusion, no entity graph, no offline searchable history.
- **Personal-memory apps (Rewind / Limitless)** — Rewind began local-first, but Limitless pivoted to a **"Confidential Cloud"** (data encrypted but processed off-device), and **Meta acquired Limitless in December 2025**, disabling Rewind's Mac capture on Dec 19, 2025. The local-first promise in this category effectively evaporated. Mora keeps it.

### What Mora does that none of them do

- **A local corpus, not a per-query fetch.** Mora backfills Gmail (thread-level), Google Calendar, iMessage, and the filesystem once, into Markdown files you own (`mora connect google`, `mora ingest`). Search is instant and offline; freshness is explicit (`mora sync status`).
- **A real entity graph, not a flat search.** On every index rebuild, Mora compiles people, scopes, tags, `[[wikilinks]]`, and categories into a graph — `PARTICIPATED_IN` / `ATTENDED` / `EMAILED` / `MENTIONS` edges, bi-temporal stamps, fan-out caps — **in the same SQLite transaction as the index (atomic)**. It's deterministic (no NER, no LLM in the loop), and the same person is resolved across email, calendar, and iMessage via connector identity capture (handle↔name, from/to, participants).
- **Hybrid retrieval that fuses sources.** `mora search` blends BM25 (FTS5), cosine over per-memory vectors, and 1–2 hop graph expansion using **Reciprocal Rank Fusion (k=60)**. It degrades gracefully to plain FTS5 when no vectors exist.
- **Synthesis you can trust, rented from your agent.** `mora think` (CLI and MCP tool) returns an envelope: cited evidence + a deterministic **"what the vault does NOT know"** gap analysis + a synthesis prompt your agent's own model turns into a cited answer. The gaps are computed *before* any model runs — Mora won't quietly hallucinate coverage it doesn't have.

### The comparison

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

### Why this matters for you

- **iMessage is the moat.** Your most candid, highest-signal conversations live in iMessage, and **no cloud assistant can legally or technically reach them** — there's no API. Mora reads your local `chat.db` read-only and folds those threads into the same graph as your email and calendar. That's context the cloud simply cannot have.
- **Nothing leaves the Mac.** Read-only scopes, loopback-only optional Ollama (it refuses any non-loopback URL — a hard zero-egress guard), and a local-only usage log you can disable (`DO_NOT_TRACK=1`). For portfolio or personal data you don't want syncing to anyone's servers, this is the only posture that holds.
- **A memory, not a lookup.** Because the corpus is persistent and graphed, Mora answers "who was in the loop on X across email and messages" or "what did I commit to with this person" — questions that require *fused history*, not a fresh per-query fetch.
- **One memory, any agent.** The same vault and the same `mora mcp serve` work with Claude Code *and* Codex. You're not locked into one vendor's cloud memory; your context is portable because it's just files on disk.
- **Honest about what it knows.** `mora think` ships an explicit "what the vault does not know" with every answer, computed deterministically before synthesis — so the agent's cited answer is grounded, not confidently wrong.
- **$0 and self-contained.** One static binary, no Python, no daemon-in-the-cloud, no subscription. `go build`, `mora init`, done.

Sources: [Claude Google Workspace connectors](https://support.claude.com/en/articles/10166901-use-google-workspace-connectors), [ChatGPT Google data controls FAQ](https://help.openai.com/en/articles/10408842-google-app-for-chatgpt-data-controls-faq), [Apps/connectors in ChatGPT](https://help.openai.com/en/articles/11487775-connectors-in-chatgpt), [GongRzhe Gmail-MCP-Server](https://github.com/GongRzhe/Gmail-MCP-Server), [Google Gmail MCP reference](https://developers.google.com/workspace/gmail/api/reference/mcp), [Meta acquires Limitless / Rewind sunset](https://winbuzzer.com/2025/12/05/meta-acquires-ai-wearables-startup-limitless-kills-pendant-sales-and-sunsets-rewind-app-xcxwbn/), [Limitless privacy / Confidential Cloud](https://www.limitless.ai/privacy)

---

## Install

**From source:**

```bash
go build -o mora ./cmd/mora
# move mora onto your PATH, e.g.:
mv mora /usr/local/bin/mora
```

**From release tarball:** unpack and move the `mora` binary onto your PATH.

**macOS Gatekeeper note:** The first run of a downloaded binary may be blocked.
Right-click `mora` in Finder and choose **Open**, or clear the quarantine flag:

```bash
xattr -d com.apple.quarantine ./mora
```

---

## Initialize

```bash
mora init
```

Creates the vault at `~/vault/mora` (override with `--vault /your/path`).

---

## Connect Google (Gmail + Calendar)

```bash
mora connect google
```

This opens a browser for OAuth consent. On first use the OAuth app is **unverified** — click through the Google warning via **Advanced → Go to Mora (unsafe)**. Mora requests read-only Gmail and Calendar scopes; it never modifies your data.

**WSL users:** `mora connect google` prints a URL (it won't auto-open a browser). Paste it into your Windows browser and approve. The consent redirect goes to `127.0.0.1`, which WSL2 forwards to Mora automatically — no paste-back needed. Leave the command running until it prints "Connected."

**BYO credentials:** To use your own Google Cloud OAuth client, set:

```bash
export MORA_GOOGLE_CREDENTIALS=/path/to/oauth_client.json
```

---

## Connect iMessage (macOS)

```bash
mora connect imessage                    # enable iMessage, check Full Disk Access, then backfill
mora connect imessage --since-days 365   # widen the backlog window (negative = all-time)
```

Mora reads the local Messages database (`~/Library/Messages/chat.db`) **read-only** — nothing is
sent anywhere. macOS gates that file behind **Full Disk Access**, granted *per binary*:

1. Run `mora connect imessage`. If access is missing it prints exactly what to do.
2. Open **System Settings → Privacy & Security → Full Disk Access**, add your `mora` binary (or the
   terminal you run it from), and toggle it on.
3. Re-run `mora connect imessage`. `mora doctor` reports the access status any time.

iMessage gives the cleanest contact graph (names come from your own address book), which is why it's
the best surface to lead a demo with — consumer Gmail is inherently noisier.

---

## Add a filesystem source

Point Mora at a project directory to ingest docs and metadata:

```bash
mora sources add filesystem --name myproject --path ~/code/myproject --scope project:myproject
mora ingest run --source myproject
```

Mora ingests curated files only: `.md`, `.json`, `.yaml`, `.toml`, `.txt`, `.csv`, `README`, `go.mod`, `CLAUDE.md`, `AGENTS.md`, and similar metadata files — plus **`.docx`** (Word documents, text extracted with pure-Go stdlib). `.pdf` and other binaries/build artifacts are skipped (PDF text extraction would need a non-pure-Go dependency and OCR for scans).

---

## Wire MCP into Claude Code and Codex

**Claude Code** — add to your project's `.claude/mcp.json` or copy the example:

```bash
cp examples/claude-code-mcp.json .claude/mcp.json
```

**Codex** — copy the example config:

```bash
cp examples/codex-mcp.json ~/codex-mcp.json
# then reference it when starting Codex
```

Both configs run `mora mcp serve`, which exposes 11 tools over JSON-RPC: `write_memory`, `read_memory`, `search_memory`, `list_memory`, `delete_memory`, `context_memory`, `think`, `list_entities`, `get_entity`, `digest`, and `brief` — the last is the session-start what-changed/what-matters briefing.

---

## See the entity graph

Mora derives a read-only entity graph from your data — **people** from your mail/messages/calendar,
plus the structure in your vault (scopes, tags, `[[wikilinks]]`, `- [categories]`):

```bash
mora entities                 # people + topics across your memory, with counts
mora entities "Sam"          # the memories that reference one entity
mora graph                    # visual map — top people + topics as proportional bars
mora graph "Sam"             # expand one entity: connections, relationship breakdown, evidence
```

The **person graph** is compiled deterministically from connector metadata — no NER, no model —
and cleaned so it reflects *people*, not raw addresses:

- **Automated senders are demoted.** no-reply / receipts / notifications / "LinkedIn Job Alerts"
  bots are classified `service` and kept out of the People view (still searchable via `get_entity`).
- **Aliases are trusted by provenance.** A name only becomes a match key if its owner *presented it
  themselves* (as an email sender, or an iMessage contact) — so spam mail-merge labels and other
  people's mislabels never pollute who you are.
- **The same human across addresses collapses into one person.** Gmail dot/`+tag` variants and
  cross-domain matches (with a full-name anchor) merge, so `get_entity` returns a *complete* picture
  no matter which address you ask by. Conservative on purpose: it never fuses two different people on
  a weak signal.

The per-entity view shows co-occurring people, the edge breakdown (`EMAILED` / `PARTICIPATED_IN` /
`ATTENDED` / `MENTIONS`), and the evidence memories — every connection cited by StableID.

Agents get the same view via the `list_entities` and `get_entity` MCP tools.

---

## Browse the vault in Obsidian

Open Obsidian and add the vault directory (default: `~/vault/mora`) as a new vault. All memories, synced emails, and calendar events appear as Markdown files.

---

## Ongoing use

**Check sync freshness:**

```bash
mora sync status
```

**Re-sync Google data manually:**

```bash
mora sync google
```

**View local usage analytics (content-free):**

```bash
mora usage report
```

**Disable usage tracking:**

```bash
mora usage off
# or set the env var:
export DO_NOT_TRACK=1
```

**Revoke Google access and remove tokens:**

```bash
mora disconnect google
```

---

## Keeping Mora up to date

Two independent things stay fresh: **your data** and **the app**.

**Refresh your data (no infra, works today):**

```bash
mora sync status                 # per-source freshness — when each connector last pulled
mora sync google                 # re-pull Gmail + Calendar
mora sync imessage               # re-read the local Messages DB (macOS)
mora reingest --full             # re-fetch + rewrite memories with the latest metadata AND rebuild the entity graph
```

Run `mora reingest` after upgrading to a build that improves extraction (e.g. better identity capture) — it rewrites existing memories with the new logic and rebuilds the graph in one atomic pass.

**Update the app itself:**

```bash
mora upgrade                     # in-place self-update to the latest release (verifies checksums before swapping)
mora upgrade --check             # just report whether a newer release exists
```

- **Homebrew installs** are updated with `brew upgrade pyranthus-hq/tap/mora` (mora detects this and tells you).
- **Direct-binary installs** self-update from the public GitHub releases — `mora upgrade` needs no token or auth.
- Source/`go build` builds report `dev` and refuse self-update — rebuild with `git pull && go build`.

---

## Notes

- Google Drive ingestion is not yet available (deferred to a later release).
- Tokens are stored locally in `~/.config/mora/tokens/` and are never synced or transmitted.
