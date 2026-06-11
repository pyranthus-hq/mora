# Setup & operations

> Every connector, option, and maintenance command in one place. For the 2-minute
> happy path, see the [QUICKSTART](../QUICKSTART.md); for what the layers do once
> data is in, see [How it works](how-it-works.md).

## Install

**Installer script** (macOS / Linux — downloads the right release binary, clears
macOS Gatekeeper quarantine, signs it, puts it on your PATH, and initializes the vault):

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

**From a release tarball:** unpack and run the bundled installer (same script, local mode):

```bash
tar -xzf mora_0.6.0_darwin_arm64.tar.gz && ./install.sh
```

**From source** (Go 1.22+; pure Go, no CGO):

```bash
go install github.com/pyranthus-hq/mora/cmd/mora@latest
# or, from a clone:
go build -o mora ./cmd/mora && mv mora /usr/local/bin/mora
```

Source builds report version `dev` and refuse self-update — rebuild to upgrade.

**macOS Gatekeeper note:** if you skipped the installer, the first run of a downloaded
binary may be blocked. Right-click `mora` in Finder and choose **Open**, or clear the
quarantine flag:

```bash
xattr -d com.apple.quarantine ./mora
```

## Initialize

```bash
mora init
```

Creates the vault at `~/vault/mora` (override with `--vault /your/path`). The installer
script runs this for you.

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

**Multiple Google accounts** (personal + work) coexist as separate sources, with separate sync
status, digest sections, and labels:

```bash
mora connect google --account work    # second mailbox → gmail-work / calendar-work sources
```

Each account keeps its own token. Re-authing an account that's already connected (under any label)
is detected by the signed-in address and exits gracefully — one mailbox is never double-ingested.

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

## Connect Apple Calendar (macOS)

```bash
mora connectors enable applecalendar
mora ingest run --source applecalendar
```

Reads the local Calendar store (the calendar group container) **read-only and immutable** — same
Full Disk Access story as iMessage, no login. One memory per event, attendees/organizer captured
for the entity graph, with a 180-day forward window so subscribed holiday calendars don't flood
the vault.

## Add a filesystem source

Point Mora at a project directory to ingest docs and metadata:

```bash
mora sources add filesystem --name myproject --path ~/code/myproject --scope project:myproject
mora ingest run --source myproject
```

Mora ingests curated files only: `.md`, `.json`, `.yaml`, `.toml`, `.txt`, `.csv`, `README`, `go.mod`, `CLAUDE.md`, `AGENTS.md`, and similar metadata files — plus **`.docx`** (Word documents, text extracted with pure-Go stdlib). `.pdf` and other binaries/build artifacts are skipped (PDF text extraction would need a non-pure-Go dependency and OCR for scans).

## Wire MCP into Claude Code and Codex

The one-liners:

```bash
claude mcp add mora -s user -- mora mcp serve     # Claude Code
codex  mcp add mora -- mora mcp serve             # Codex
```

Or use the example configs — `examples/claude-code-mcp.json` (copy to your project's
`.claude/mcp.json`) and `examples/codex-mcp.json`.

`mora mcp serve` exposes 11 tools over JSON-RPC: `write_memory`, `read_memory`, `search_memory`, `list_memory`, `delete_memory`, `context_memory`, `think`, `list_entities`, `get_entity`, `digest`, and `brief` — the last is the session-start what-changed/what-matters briefing. Every `search_memory` / `context_memory` answer also carries a per-source `last_synced` map, so your agent can qualify answers with their data age.

## Explore the entity graph

Mora derives a read-only entity graph from your data — **people** from your mail/messages/calendar,
plus the structure in your vault (scopes, tags, `[[wikilinks]]`, `- [categories]`):

```bash
mora entities                 # people + topics across your memory, with counts
mora entities "Sam"           # the memories that reference one entity
mora graph                    # visual map — top people + topics as proportional bars
mora graph "Sam"              # expand one entity: connections, relationship breakdown, evidence
```

The per-entity view shows co-occurring people, the edge breakdown (`EMAILED` / `PARTICIPATED_IN` /
`ATTENDED` / `MENTIONS`), and the evidence memories — every connection cited by StableID. Agents
get the same view via the `list_entities` and `get_entity` MCP tools. How people are classified,
trusted, and merged across addresses is covered in [How it works](how-it-works.md#1-the-entity-graph--derived-from-your-real-mail-and-messages-not-inferred).

## Browse the vault in Obsidian

Open Obsidian and add the vault directory (default: `~/vault/mora`) as a new vault. All memories, synced emails, and calendar events appear as Markdown files.

## Ongoing use

**Keep data fresh automatically** (launchd on macOS; prints a cron line on Linux):

```bash
mora schedule install ingest-hourly   # hourly background sync of every enabled source
mora schedule install pulse-daily     # 8am daily brief (sync-first, persisted, notification)
```

**Check sync freshness:**

```bash
mora sync status
```

**Morning brief / per-source rundown:**

```bash
mora brief                                                 # what changed / what matters
mora pulse --digest --source imessage --since-hours 168    # "just my texts this week"
```

**Tune context density** (scales default budgets for context/digest/brief; `large` raises the
per-call ceiling to 50k tokens and doubles digest snippet length):

```bash
mora config context large       # small | default | large
mora config embedder ollama     # durable semantic-retrieval opt-in (loopback-only)
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

Direct-binary installs self-update from the public GitHub releases — no token or auth needed.
Homebrew-managed installs are detected and deferred to `brew upgrade`; source/`go build` builds
report `dev` and refuse self-update — rebuild with `git pull && go build`.

## Notes

- Google Drive ingestion is not yet available (deferred to a later release).
- Tokens are stored locally in `~/.config/mora/tokens/` (0600) and are never synced or transmitted.
