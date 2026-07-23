# The Mora guide

The complete manual: every connector, option, and maintenance command, how
retrieval and the entity graph work, and how Mora compares to the cloud alternatives. The short
version of all of this is the [README](../README.md); contributor-facing internals
live in [`docs/architecture/`](architecture/00-overview.md).

- [Install](#install)
- [Windows](#windows)
- [Initialize](#initialize)
- [Connect Google (Gmail + Calendar)](#connect-google-gmail--calendar)
- [Connect iMessage (macOS)](#connect-imessage-macos)
- [Connect Apple Calendar (macOS)](#connect-apple-calendar-macos)
- [Add a filesystem source](#add-a-filesystem-source)
- [Manage connectors](#manage-connectors)
- [Wire Mora into your agent (MCP)](#wire-mora-into-your-agent-mcp)
- [Use Mora from the shell](#use-mora-from-the-shell)
- [Make the brief your session-start default](#make-the-brief-your-session-start-default)
- [Explore the entity graph](#explore-the-entity-graph)
- [Browse the vault in Obsidian](#browse-the-vault-in-obsidian)
- [Day to day](#day-to-day)
- [Keep Mora up to date](#keep-mora-up-to-date)
- [Back up the vault off-device (opt-in)](#back-up-the-vault-off-device-opt-in)
- [Share memories with someone (opt-in, encrypted)](#share-memories-with-someone-opt-in-encrypted)
- [How it works](#how-it-works)
- [Why not just use a cloud connector?](#why-not-just-use-a-cloud-connector)
- [Notes](#notes)

## Install

**Installer script** (macOS / Linux: downloads the right release binary, clears
macOS Gatekeeper quarantine, signs it, puts it on your PATH, and initializes the vault):

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

**From a release tarball:** unpack and run the bundled installer (same script, local mode):

```bash
tar -xzf mora_<version>_<os>_<arch>.tar.gz && ./install.sh   # the tarball you downloaded for your platform
```

**From source** (Go 1.25+; pure Go, no CGO):

```bash
go install github.com/pyranthus-hq/mora/cmd/mora@latest
# or, from a clone:
go build -o mora ./cmd/mora && mv mora /usr/local/bin/mora
```

Source builds report version `dev` and refuse self-update. Rebuild to upgrade. They also use a placeholder Google OAuth client, so `mora connect google` needs your own credentials (BYO credentials, below); the release binary ships with a working client.

**macOS Gatekeeper note:** if you skipped the installer, the first run of a downloaded
binary may be blocked. Right-click `mora` in Finder and choose **Open**, or clear the
quarantine flag:

```bash
xattr -d com.apple.quarantine ./mora
```

## Windows

Run this from PowerShell:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.ps1 -OutFile $env:TEMP\install-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1
```

The Windows installer downloads `mora_<version>_windows_amd64.zip` from GitHub releases, downloads `checksums.txt`, verifies the zip with `Get-FileHash -Algorithm SHA256`, extracts `mora.exe` to `%LOCALAPPDATA%\Mora\bin\mora.exe`, and adds that directory to your User PATH. Open a new PowerShell window after install so `mora` resolves on PATH.

Install a pinned version with:

```powershell
powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1 -Version 0.10.0
```

**SmartScreen note:** the v1 Windows binary is unsigned. If Windows shows **Windows protected your PC**, verify that the installer printed a checksum success, then choose **More info > Run anyway** or run:

```powershell
Unblock-File "$env:LOCALAPPDATA\Mora\bin\mora.exe"
```

Windows supports Gmail, Google Calendar, filesystem folders, notes, and local Ollama embeddings. iMessage, Apple Calendar, and Address Book are macOS-only and should refuse cleanly on Windows.

Windows schedules jobs with Task Scheduler:

```powershell
mora schedule install ingest-hourly
mora schedule list
```

Task names use the `Mora\<job>` form, for example `Mora\ingest-hourly`. The same job names are available across platforms: `ingest-hourly`, `index-hourly`, `pulse-daily`, `doctor-pulse`, `backup-daily`, `git-daily`, and `lint-weekly`.

Uninstall with:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/uninstall.ps1 -OutFile $env:TEMP\uninstall-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\uninstall-mora.ps1
```

The uninstaller removes `%LOCALAPPDATA%\Mora`, removes the User PATH entry, and deletes scheduled tasks under `\Mora\`. It preserves your vault and config unless you pass `-Purge`, which removes Mora data paths after confirmation.

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

This opens a browser for OAuth consent. On first use the OAuth app is **unverified**, so click through the Google warning via **Advanced → Go to Mora (unsafe)**. Mora requests read-only Gmail and Calendar scopes; it never modifies your data.

**WSL users:** `mora connect google` prints a URL (it won't auto-open a browser). Paste it into your Windows browser and approve. The consent redirect goes to `127.0.0.1`, which WSL2 forwards to Mora automatically, so there is no paste-back. Leave the command running until it prints "Connected."

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
is detected by the signed-in address and exits gracefully, so one mailbox is never double-ingested.

Gmail backfills the **last 90 days** by default. Widen it with `mora connect google --since-days 365`; the window is saved on the source, so later `mora sync google` runs reuse it. Calendar always pulls a fixed window of about six months back and three months forward.

## Connect iMessage (macOS)

```bash
mora connect imessage                    # enable iMessage, check Full Disk Access, then backfill
mora connect imessage --since-days 365   # widen the backlog window (negative = all-time)
```

Mora reads the local Messages database (`~/Library/Messages/chat.db`) **read-only**; nothing is
sent anywhere. macOS gates that file behind **Full Disk Access**, granted *per binary*:

1. Run `mora connect imessage`. If access is missing it prints exactly what to do.
2. Open **System Settings → Privacy & Security → Full Disk Access**, add your `mora` binary (or the
   terminal you run it from), and toggle it on.
3. Re-run `mora connect imessage`. `mora doctor` reports the access status any time.

Contact names come from your own address book, so iMessage usually yields the cleanest
name-to-handle mapping of any source.

## Connect Apple Calendar (macOS)

```bash
mora connectors enable applecalendar
mora ingest run --source applecalendar
```

Reads the local Calendar store (the calendar group container) **read-only and immutable**, the same
Full Disk Access story as iMessage, no login. One memory per event, attendees/organizer captured
for the entity graph, with a 180-day forward window so subscribed holiday calendars don't flood
the vault.

## Add a filesystem source

Point Mora at a folder to ingest docs and metadata. The one-step way mirrors
`connect google` / `connect imessage`, adding, enabling, and indexing in a single command:

```bash
mora connect filesystem ~/code/myproject          # add + enable + index now
mora connect filesystem ~/Documents --name docs   # name it (default: the folder's base name)
```

Re-run it on the same folder anytime to re-index after changes; connecting two
different folders just works (each gets its own source). The longer, explicit form
is still available if you want to stage a source disabled first and ingest later:

```bash
mora sources add filesystem --name myproject --path ~/code/myproject --scope project:myproject
mora connectors enable filesystem
mora ingest run --source myproject
```

The order is forgiving: `sources add` inherits the type's consent, so a source
added while filesystem is already enabled starts enabled and can be ingested
immediately (no re-enable needed). Running `mora connectors enable filesystem`
before any folder is configured enables nothing — a filesystem source is
meaningless without a path — and instead points you at the two commands above.

Mora ingests curated files only: `.md`, `.json`, `.yaml`, `.toml`, `.txt`, `.csv`, `README`, `go.mod`, `CLAUDE.md`, `AGENTS.md`, and similar metadata files, plus **`.docx`** (Word documents) and **`.pdf`**, both text-extracted with pure-Go libraries (no CGO). Mora only indexes text it can actually read: a scanned, image-only PDF yields nothing rather than garbage (there is no OCR). Other binaries and build artifacts are skipped.

## Manage connectors

See what is connected, what is enabled, and what still needs a login:

```bash
mora connectors list            # every connector, its enabled state, and whether it needs auth
mora connectors list --json     # the same, machine-readable
mora connectors setup           # interactive menu to pick and enable connectors
```

The catalog is `gmail`, `calendar`, `filesystem`, `imessage`, and `applecalendar`. Enabling is explicit and consent-gated: `enable` runs the OAuth or access check but pulls **no data**, and `disable` stops future syncs without deleting anything already indexed.

```bash
mora connectors enable calendar     # consent only; backfill with a sync or ingest
mora connectors disable imessage    # stop syncing a source
```

Backfill one source or everything that is enabled:

```bash
mora ingest run --source docs       # one named source (a name that matches nothing is an error, not an empty run)
mora ingest run --all               # every enabled source (what the hourly schedule runs; disabled sources are skipped)
```

## Wire Mora into your agent (MCP)

The one-liners:

```bash
claude mcp add mora -s user -- mora mcp serve     # Claude Code
codex  mcp add mora -- mora mcp serve             # Codex
```

Or use the example configs: `examples/claude-code-mcp.json` (copy to your project's
`.claude/mcp.json`) and `examples/codex-mcp.json`.

`mora mcp serve` exposes 12 tools over JSON-RPC: `write_memory`, `read_memory`, `search_memory`, `list_memory`, `delete_memory`, `context_memory`, `think`, `list_entities`, `get_entity`, `digest`, `brief`, and `meeting_prep`. `brief` is the session-start what-changed/what-matters briefing; `brief --event-id <id>` and `meeting_prep` assemble the same cited pre-meeting view of historical candidate lines, unresolved threads, current staleness metadata, and material shared context. These candidates are heuristically ranked across attendees and rendered as dated evidence, not current truth or a verified commitment ledger. Mora does not yet establish obligation owner, direction, or closure reliably; inspect each citation and any red source-health warning before acting. `meeting_prep` accepts `event_id` plus an optional RFC3339 `at` seam, or selects the next event when `event_id` is omitted. `digest` and the session-start `brief` also accept `entity`/`scope`/`since_days` to narrow to one person, namespace, or window. Every `search_memory` / `context_memory` answer also carries a per-source `last_synced` map, so your agent can qualify answers with their data age.

## Use Mora from the shell

Every MCP tool has a CLI sibling, so you can read and write the vault from a terminal with the same retrieval the agent uses:

```bash
mora search "OAuth status" --scope project:acme --json   # search the vault (the search_memory tool)
mora write --scope project:acme --type decision --title "Chose OAuth" --text "..."   # save a fact
mora read <id> --json                                    # one memory by id
mora list --scope project:acme --json                    # browse memories in a scope
mora delete <id> --yes                                   # remove one memory
mora context --query "auth" --scope project:acme --budget 6000 --json   # token-budgeted context block + bounded items
mora think "what did Sam decide about pricing?" --json   # cited evidence + gap analysis
```

`mora context` assembles a single character-budgeted block for a query (default 2000 characters; omit `--query` for a recency briefing). `mora write` is the only command here that changes anything, and it only writes to the local vault, never to your connected accounts.

### Permanently forget a person or chat

`mora delete` removes one memory now, but for anything that came from a connector the next hourly sync brings it right back. `mora forget` is the durable version: it removes the matching memories **and** records a local suppression so sync can never re-create them.

```bash
mora forget --chat imessage_chat/<guid> --dry-run   # preview exactly what would be removed
mora forget --chat imessage_chat/<guid> --yes       # forget one conversation/thread/event
mora forget --handle +14155550123 --yes             # forget a 1:1 iMessage counterpart
mora forget --email sam@example.com --yes           # forget a 1:1 email counterpart
mora forget list                                     # show active suppressions
mora unforget <entry-id> --yes                       # reverse a forget
```

Forgetting is **local-only**: it stops Mora from holding and re-acquiring the content on this Mac (and your other devices, via `mora sync git`) — it never deletes anything at Gmail or Apple. `--handle`/`--email` act conservatively: they remove one-to-one memories with that counterpart but keep group threads they merely appear in. (Because your own address is on every email thread, `--email` matches only a thread whose sole *other* party is that address — for a specific email thread with more people on it, forget it by `--chat <thread-id>`; broader person-level email forgetting arrives with the identity graph.) Always `--dry-run` first to see exactly which memories a forget will touch. `unforget` reverses the suppression, and future syncs may re-ingest the content again (within the connector's lookback window). See [architecture: governance ledger](architecture/17-governance-ledger.md) for the design.

## Make the brief your session-start default

The brief is a daily *what-changed / what-matters* digest: new-or-updated threads since you last looked,
ordered by who actually matters to you, every item citable by id. A deadline-bearing email from a real
person leads the brief on an **Urgent** shelf above the sections (and never gets silently dropped by the
byte budget). It has a scheduled **write side** and a session-start **read side**:

```bash
mora schedule install pulse-daily   # write side: syncs, then persists ~/vault/mora/briefs/<date>-brief.md each morning
mora brief                          # read side: print today's brief (generates one locally if none exists yet)
mora brief --fresh                  # regenerate today's brief even if one already exists
```

`pulse-daily` enters through Mora's durable `daily-brief` loop: a duplicate same-day scheduler fire is a no-op, and the advancing pulse actively renews its lease and holds its owner fence through the complete watermark transaction. The run, artifact, and watermarks must share one logical UTC period. Mora fsyncs a durable effect intent before entering the transaction, fsyncs the artifact and watermarks, then records the commit checkpoint. If a crash leaves only the intent, status reports `uncertain` and automatic same-day retry is blocked rather than risking a second advance. Existing schedules installed by older Mora versions are runtime-routed through the same gate; reinstalling updates their stored command but is not required for safety.

**Claude Code** runs `SessionStart` hooks and injects their stdout as context. Add to
`~/.claude/settings.json` (alongside any existing hooks):

```jsonc
{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "mora brief" } ] }
    ]
  }
}
```

**Codex / any MCP agent:** register `mora mcp serve` and have the agent call the
`brief` tool first. The server's instructions already nudge this. The tool takes
optional `max_tokens` (default ~6000) and `envelope: true` (adds a grounded,
cite-by-id synthesis prompt; Mora itself runs no model and holds no API key).

This wiring is **docs-only**: Mora never edits your agent config. You paste the
snippet, and removing it is the whole opt-out. `mora brief` and the `brief` tool make
**no network call**; they read or generate from memories already on disk. The only
thing that touches the network is the scheduled sync, over your already-enabled,
read-only sources.

## Explore the entity graph

Mora derives a read-only entity graph from your data: **people** from your mail/messages/calendar,
plus the structure in your vault (scopes, tags, `[[wikilinks]]`, `- [categories]`):

```bash
mora entities                 # people + topics across your memory, with counts
mora entities "Sam"           # the memories that reference one entity
mora graph                    # visual map — top people + topics as proportional bars
mora graph "Sam"              # expand one entity: connections, relationship breakdown, evidence
```

The per-entity view shows co-occurring people, the edge breakdown (`EMAILED` / `PARTICIPATED_IN` /
`ATTENDED` / `MENTIONS`), and the evidence memories, every connection cited by StableID. Agents
get the same view via the `list_entities` and `get_entity` MCP tools. How people are classified,
trusted, and merged across addresses is covered in [How it works](#how-it-works).

## Browse the vault in Obsidian

Open Obsidian and add the vault directory (default: `~/vault/mora`) as a new vault. All memories, synced emails, and calendar events appear as Markdown files.

## Day to day

**Keep data fresh automatically** (launchd on macOS, Task Scheduler on Windows, a printed cron line on Linux):

```bash
mora schedule install ingest-hourly   # hourly background sync of every enabled source
mora schedule install pulse-daily     # 8am daily brief (sync-first, persisted, notification)
mora schedule install doctor-pulse    # 9am freshness alarm (native toast + exit 2 when a source is unhealthy)
mora schedule list                    # show which jobs are installed
```

The full set of jobs is `ingest-hourly`, `index-hourly`, `pulse-daily`, `doctor-pulse`, `backup-daily`, `git-daily`, and `lint-weekly`.

**Check sync freshness:**

```bash
mora sync status
```

**Check your install:**

```bash
mora doctor
```

`mora doctor` reports the health of the install: the vault and index, that your tokens live outside the vault, when you last signed in to each Google account, the storage footprint, whether the vault is a git repo, per-source freshness for every enabled connector, and (on macOS) Full Disk Access for iMessage and Apple Calendar.

**Per-source freshness and the health alarm:**

```bash
mora doctor --json      # machine-readable report; each connector gets a `source_fresh:<key>` check
mora doctor --strict    # exit non-zero if any critical check fails, incl. a stale/failed/never-synced source
mora doctor --pulse     # freshness-only check: a native toast + exit 2 when any source is unhealthy, exit 0 when all are fresh
```

A connector goes stale silently if a sync keeps failing in the background — the six-day version of this bug is why `doctor --pulse` exists. Gmail/Calendar/Apple Calendar alarm after 24h without a clean sync; iMessage/filesystem after 48h. Any recorded sync error (not just age) alarms immediately. When a source is unhealthy, the SAME red banner — `🔴 MORA HEALTH: <source> — no successful sync for <N>h (<error>). Run: mora doctor` — leads both `mora brief` and `mora brief --event-id`, so a stale or dead source is never quietly missing from a brief that otherwise renders with full confidence. Install `doctor-pulse` on a schedule to get the native toast without having to remember to check.

**Record open tasks so the brief can surface them:**

```bash
mora tasks add "Reply to Sam about the launch" --pri P0   # capture an open loop (name first, then flags)
mora tasks list                                           # current live tasks (--json for machine-readable)
mora tasks done "Reply to Sam about the launch"           # mark one done so it stops resurfacing as stale
mora tasks sync --write                                   # scan memories for open tasks and record them
```

Live tasks surface in the brief, and stale ones keep resurfacing until you mark them done.

**Morning brief / per-source rundown:**

```bash
mora brief                                                 # what changed / what matters
mora brief --event-id calendar_event/abc --at 2026-07-10T15:00:00Z  # reproducible, fully-cited meeting brief
mora brief --entity "Riya" --since-days 7                  # just one person, last week (preview-only)
mora pulse --digest --source imessage --since-hours 168    # "just my texts this week"
```

**Tune context density** (scales default budgets for context/digest/brief; `large` raises the
per-call ceiling to 50k tokens and doubles digest snippet length):

```bash
mora config context large       # small | default | large
mora config embedder ollama     # durable semantic-retrieval opt-in (loopback-only)
mora config mmr on              # diversity-aware rerank of hybrid results (off by default; needs embedder ollama)
```

MMR trims near-duplicate hits from a result set; it is off by default and only applies when the Ollama embedder is on.

**Tell Mora your other email addresses** (`self_emails` in `config.toml`):

```toml
self_emails = "you@work.com, you@icloud.com"
```

Mora already knows the mailbox you authorized Google on, and the connectors record which invitee is you (Google's `Attendee.Self`, Apple's `Participant.is_self`). But a calendar often invites an address neither one covers — a Workspace alias, a custom domain. If Mora cannot recognize you among a meeting's invitees it will **not guess**: it refuses to attribute anything for that meeting and tells you to add the alias here. Listing your addresses removes the guesswork, and keeps your own records from being presented as the other person's unfinished business.

**Re-sync Google data manually:**

```bash
mora sync google
```

**View local usage analytics (stays on your disk; query text is not recorded by default):**

```bash
mora usage report
```

The usage log keeps tool name, timing, result counts, and scope, but **not** your
query text. If you want the raw query strings retained locally (e.g. to grow an eval
set), opt in; turn it back off at any time:

```bash
mora usage queries on    # retain raw query text in the local log (off by default)
mora usage queries off   # stop retaining it
```

**Disable usage tracking entirely:**

```bash
mora usage off
# or set the env var:
export DO_NOT_TRACK=1
```

**Revoke Google access and remove tokens:**

```bash
mora disconnect google
```

## Keep Mora up to date

Two independent things stay fresh: **your data** and **the app**.

**Refresh your data:**

```bash
mora sync status                 # per-source freshness — when each connector last pulled
mora sync google                 # re-pull Gmail + Calendar
mora sync filesystem             # re-index enabled filesystem sources
mora sync imessage               # re-read the local Messages DB (macOS)
mora reingest --full             # re-fetch + rewrite memories with the latest metadata AND rebuild the entity graph
```

Run `mora reingest` after upgrading to a build that improves extraction (e.g. better identity capture). It rewrites existing memories with the new logic and rebuilds the graph in one atomic pass.

**Update the app itself:**

```bash
mora upgrade                     # in-place self-update to the latest release (verifies checksums before swapping)
mora upgrade --check             # just report whether a newer release exists
```

After a successful swap, `mora upgrade` automatically rebuilds the search index with the new
version. (A schema change never strands a stale index: with the default embedder Mora
rebuilds one automatically at first read; with the Ollama embedder, where a re-index takes
minutes, it asks with a clear "run `mora index rebuild`" instead of degrading silently.)
Direct-binary installs self-update from the public GitHub releases, with no token or auth needed.
Homebrew-managed installs are detected and deferred to `brew upgrade`; source/`go build` builds
report `dev` and refuse self-update. Rebuild with `git pull && go build`.

## Back up the vault off-device (opt-in)

By default the vault never leaves the machine. If you want an off-device backup with
version history, Mora can push it to a **private git remote you control**: GitHub,
GitLab, a self-hosted server, or a bare repo on a USB drive. The flow is **one-way,
push-only** (your local vault is the source of truth) and **fail-loud**: any git error
surfaces, and a push is never `--force`d. A non-fast-forward rejection means the
remote diverged, and you should know, not have it overwritten.

```bash
# Point at any git remote you control:
mora sync git --init --remote git@github.com:you/mora-vault.git

# …or let Mora create a PRIVATE GitHub repo for you (needs the gh CLI, authenticated):
mora sync git --init --github            # repo name defaults to "mora-vault"; --name overrides

# Subsequent backups — commits + pushes only what changed:
mora sync git

# Automate a daily 3am backup (macOS launchd; prints the equivalent line elsewhere):
mora schedule install git-daily
```

`--init` writes a defensive `.gitignore` (`index.db`, `*.db`, `.DS_Store`, `tokens/`)
so the rebuildable index and anything secret never leave the machine, and it detects
an existing repo via `vault/.git`. It won't adopt a parent repo if your vault lives
inside one, and it refuses a `.git` gitfile or symlink that points elsewhere. If
index or token files are already git-*tracked* (ignore rules don't apply to tracked
files), the sync hard-stops with remediation instead of pushing them. Restore on a
new machine: `git clone <remote> ~/vault/mora && mora index rebuild`.

Know what you're opting into: the vault contains decoded iMessages and Gmail threads
in **plaintext**, so the remote must be private and yours. Mora runs no server and
never picks a destination for you. `mora doctor` warns whenever the vault is a git
repo. Mora shells out to your system `git` (and `gh` for `--github`), so your existing
SSH keys, credential helper, or `gh auth` just work. For ciphertext at rest on the
remote, layer [git-remote-gcrypt](https://spwhitton.name/tech/code/git-remote-gcrypt/)
over any remote; the flow is unchanged.

## Share memories with someone (opt-in, encrypted)

`mora share` publishes **one scope of memories you wrote** (`mora write` / MCP
`write_memory`) to a **private git remote you control** (or a **user-owned
S3/R2 bucket**), encrypted to each
recipient with [age](https://age-encryption.org). The person you invite
subscribes and gets your notes as a **read-only, separately-indexed corpus**:
their `mora search` and `mora think` include your memories, clearly attributed,
but nothing is ever merged into their own vault or people graph. Connector
evidence (Gmail threads, iMessages, calendar events) is never shared — capture
the decision as an authored note and share that.

Receiving side first — generate a key and send the **public** half to the
publisher over any channel you trust (only the public key travels; there is no
key server):

```bash
mora share keygen        # prints your age public key; the secret stays in ~/.config/mora/share/
```

Publishing side:

```bash
# One-time: create the share — a dedicated PRIVATE repo, separate from any vault backup
mora share init acme --scope project:acme \
  --recipient age1... \
  --remote git@github.com:you/acme-share.git   # or --github to create a private repo via gh

mora share preview acme   # the exact files and content that would leave, in full
mora share push acme      # preview + confirm, encrypt, commit, push (never --force)
```

Receiving side:

```bash
mora share subscribe neil --remote git@github.com:them/acme-share.git
mora share pull                # fetch what they've published since (--ff-only)
mora search "launch"           # shared hits appear as "[neil] …"; --json/MCP results carry "owner"
mora read <id>                 # expands a shared search hit to its full text
mora share remove neil --yes   # unsubscribe: deletes the local corpus; your vault was never touched
```

What the design guarantees, and what it honestly cannot:

- **Encryption is mandatory.** `push` refuses to run without at least one
  recipient key, and only `*.md.age` ciphertext ever enters the repo. `mora
  doctor` checks that staging stays plaintext-free and discloses every
  configured share. Keep the remote private anyway — it's your data.
- **A preview before every push.** Nothing leaves without being listed first;
  `mora share preview` shows the full content.
- **Reading someone never rewrites you.** A subscription lives beside the vault
  (under Mora's data dir) with its own index. It is invisible to your backups,
  your vault git-sync, your entity graph, and `delete_memory`. `think`'s gap
  analysis still reports what *your* vault does not know.
- **Revocation is honest, not magic.** `mora share remove` stops future pushes,
  but git history is durable and subscribers keep what they already pulled. To
  cut future access, rotate to a new repo and new recipient keys.
- Mora shells out to your system `git`, so existing SSH keys, credential
  helpers, or `gh auth` just work — same as the vault backup.

## How it works

Retrieval is three layers, all computed from your own data at ingest time with no model involved (the optional Ollama embedder is the one exception, covered below). Everything runs in the Go binary against a local SQLite index. For the subsystem-level spec with diagrams and `file:line` citations, see [`docs/architecture/`](architecture/00-overview.md).

### 1. The entity graph — derived from message metadata

An **entity** is a thing your vault refers to repeatedly: a **person**, a **scope** (project/namespace), a **tag**, a `[[wikilink]]`, or a `- [Category]` line. Mora materializes these (and the edges between them) into `entities` / `edges` tables **in the same transaction as the index rebuild** (`buildGraph` in `internal/mora/graph.go`), so the graph is always atomically consistent with the search index and byte-identical across rebuilds.

People are the interesting part, and they come straight from connector identity capture. No name-entity-recognition model is involved. When Gmail, Calendar, and iMessage ingest, they already carry structured identity in each memory's metadata: `from` / `to` / `cc` for mail, `organizer` / `attendees` for calendar, and iMessage handle↔name `participants` pairs. `personRefs` resolves those into canonical `person:<lowercased-identity>` nodes (so `neil@x.com` referenced in 40 threads collapses to one node), and emits edges with bi-temporal stamps and provenance back to the source memory:

- **`PARTICIPATED_IN`**: a person was on a thread or chat
- **`ATTENDED`**: a person was on a calendar event
- **`EMAILED`**: sender → each recipient, mail only
- **`MENTIONS`**: a person *known from metadata* who also appears by name in another message's body, matched by a gazetteer built **from the graph's own person aliases** (`gazetteer.go`); it is word-boundary, multi-token names, stoplisted, deterministic tie-break. Still no model.

A blast email with 500 recipients won't explode the graph: person fan-out is capped (`maxParticipantFanout = 64`, and it warns rather than silently dropping), and co-occurrence ("who else was on Sam's threads") is a **query-time self-join**, never materialized, so an N-person thread costs O(N) edge rows, not O(N²).

**Concrete example.** You and Sam traded 40 emails and a few iMessages. `mora entities` shows `Sam Rivera  43` under **People**. `mora entities "Sam Rivera"` lists those 43 memories; via MCP, `get_entity` additionally returns his aliases (every address/handle/name variant seen), his `degree`, the incoming edges with their `evidence_id`, and his 1-hop `neighbors`, the people he shares threads or events with.

The person graph is also cleaned so it reflects *people*, not raw addresses:

- **Automated senders are demoted.** no-reply / receipts / notifications / "LinkedIn Job Alerts"
  bots are classified `service` and kept out of the People view (still searchable via `get_entity`).
- **Aliases are trusted by provenance.** A name only becomes a match key if its owner *presented it
  themselves* (as an email sender, or an iMessage contact), so spam mail-merge labels and other
  people's mislabels never pollute who you are.
- **The same human across addresses collapses into one person.** Gmail dot/`+tag` variants and
  cross-domain matches (with a full-name anchor) merge, so `get_entity` returns a *complete* picture
  no matter which address you ask by. Conservative on purpose: it never fuses two different people on
  a weak signal.

### 2. Hybrid retrieval — BM25 + embeddings + graph expansion, fused by RRF

Keyword search misses paraphrase ("launch" vs "shipping"); pure vector search drifts off exact terms and proper nouns. `hybridSearch` (`internal/mora/hybrid.go`) runs three retrievers and fuses them:

1. **FTS5 / BM25**: the exact-match correctness anchor. Proper nouns, IDs, and literal phrases always rank.
2. **Static-embedding cosine**: recall for paraphrase, over per-memory vectors in `mem_vectors`. Zero-similarity rows are dropped so the vector arm never drags in unrelated memories.
3. **1-hop graph expansion**: if the query *names a person* (gazetteer + exact alias match), pull that person's evidence memories into the candidate pool. GraphRAG-lite, no LLM.

`mora search` and `search_memory` take this hybrid path only when a semantic embedder is active (Ollama opted in and reachable). With the default static-hash embedder they stay FTS-only, because hybrid measured worse than plain FTS under static-hash (recall@5 fell from 0.591 to 0.394). Turn on Ollama (`mora config embedder ollama`) to light up the cosine and graph arms.

The three ranked lists are merged with **weighted Reciprocal Rank Fusion**: each arm contributes `weight / (k + rank)` with `k = 10`, and the weights are tuned so the exact-match anchor leads (`fts 1.5`, `vec 1`, `graph 1`). RRF is *rank*-based, so it fuses BM25's unbounded scores with cosine's `[0,1]` without any normalization, and the weights keep the keyword arm slightly ahead so a proper-noun match is never buried under a fuzzy vector hit.

**Embedder limitations.** The default is a pure-Go, deterministic **feature-hashing static embedder** (`staticEmbedder` in `embed.go`): it hashes word tokens and character trigrams into a fixed 256-dim space (signed hashing trick, TF-weighted, L2-normalized). Cosine then tracks shared lexical + subword features, so "launching" and "launch" share signal. This is not a semantic model: it tracks shared tokens and subwords, not meaning, so paraphrase recall is limited. It sits behind an `Embedder` interface, so an **Ollama** model (`nomic-embed-text`) drops in unchanged. Ollama is strictly opt-in (`mora config embedder ollama`, or `MORA_EMBEDDER=ollama`), and `chooseEmbedder` **refuses any non-loopback `MORA_OLLAMA_URL`**. Memory text never leaves the machine, and an unreachable daemon degrades to the static embedder with a warning, never an error.

**Graceful degradation.** On an index with no `mem_vectors` table, `hybridSearch` is FTS-only, and search still works. As soon as vectors exist, the cosine and graph arms light up automatically. The model id is stored per vector, so changing embedders triggers a clean re-embed rather than silently mixing incompatible vectors.

### 3. `mora think` — a synthesis envelope your own agent fills in

`mora think` (`think.go`) does **not** contain an LLM and holds no API key. It returns a *synthesis envelope* (everything an agent needs to write a cited answer) and rents the actual prose generation from the agent that called it (via MCP). The envelope has three parts:

- **Cited evidence**: the top hybrid-retrieval hits, each with `stable_id`, scope, timestamp, fused score, and a snippet, so every downstream claim is attributable.
- **Gap analysis ("what the vault does NOT know")**: computed deterministically *before any model runs*, so coverage limits are reported rather than papered over. Three signals: **stale** (freshest matching memory older than 30 days), **thin coverage** (a distinctively-named person in the query has fewer than 2 memories), and **coverage holes** (a real-name-shaped phrase in the query that resolves to no entity at all).
- **A synthesis prompt**: a ready-to-run instruction: *answer using only this evidence, cite every claim with its `[stable_id]`, and surface the known gaps in a "What the vault does not know" section.*

So `mora think "what did we decide with Sam about pricing?"` retrieves the relevant threads as cited evidence; if the freshest is two months old it adds a `stale` gap, and if Sam has only one memory it flags thin coverage. Your agent reads the envelope and composes the answer, citing the evidence ids and surfacing the gaps.

## Why not just use a cloud connector?

The architectural tradeoff is persistence and ownership, not a claim that Mora is
the only local or cross-source product. Live connectors are often easier to set up
and can act on source systems. Mora instead builds a durable corpus on your machine
that several agents can inspect, cite, grep, diff, back up, or rebuild.

That boundary is narrower than "nothing leaves this computer":

- Mora reaches enabled source APIs to synchronize data, GitHub for updates, and
  explicitly configured backup or sharing destinations.
- Mora does not operate a hosted corpus or telemetry service. Its optional usage
  log is local, omits query text by default, and can be disabled.
- Optional Ollama inference is restricted to loopback.
- Once an MCP client retrieves context, that client's model and data policy apply.
  A cloud-hosted agent may transmit the retrieved snippets to its provider.

Choose Mora when a human-readable local corpus and read-only source posture are
worth the extra installation, permissions, storage, and operational responsibility.
Choose a hosted connector when zero-setup access and source actions matter more.

## Notes

- Google Drive ingestion is not yet available (deferred to a later release).
- Tokens are stored locally in `~/.config/mora/tokens/` (0600) and are never synced or transmitted.
