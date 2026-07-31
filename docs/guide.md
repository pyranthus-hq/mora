# The Mora guide

This manual covers each connector, option, and upkeep command. It explains
search, the entity graph, and the tradeoffs with cloud services. For a short
guide, read the [README](../README.md). For code details, read
[`docs/architecture/`](architecture/00-overview.md).

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

**Installer script** (macOS / Linux): downloads the correct release archive,
checks its SHA-256 digest, puts `mora` on your PATH, and starts the vault. On
macOS, the release binary is signed with Apple Developer ID identity
`com.pyranthus.mora` from team `VS8M5VJBZ5` and submitted to Apple's notary
service. The installer checks that identity and Apple's notarized-code
requirement before and after copying the binary. It never removes quarantine or
re-signs the binary. If `mora` is already a regular file on `PATH`, the
installer replaces that exact active file. It refuses to overwrite a symlink or
a Homebrew-managed path; use the package manager or set `PREFIX` explicitly.

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

**From a release tarball:** unpack it and run the bundled installer. This uses
the same script in local mode.

```bash
tar -xzf mora_<version>_<os>_<arch>.tar.gz && ./install.sh   # the tarball you downloaded for your platform
```

**From source** (Go 1.25+; pure Go, no CGO):

```bash
go install github.com/pyranthus-hq/mora/cmd/mora@latest
# or, from a clone:
go build -o mora ./cmd/mora && mv mora /usr/local/bin/mora
```

Source builds report version `dev` (or a git tag like `v0.10.0-60-g2d08334`).
They update themselves only when a newer release exists. `mora upgrade` never
replaces a local build with an older release. Rebuild to update. Source builds
also use a placeholder Google OAuth client. Thus, `mora connect google` needs
your own credentials (see BYO credentials below). The release binary includes
a working client.

**macOS Gatekeeper note:** the standalone `mora` executable is the compatibility
bridge for the existing tarball and `mora upgrade` contracts. A raw executable
cannot carry a stapled notarization ticket, so its first notarization-ticket check
can need an internet connection. Do not clear its quarantine attribute or
ad-hoc re-sign it. Either action discards part of the release trust path; signing
it again also changes the identity macOS uses for protected-data permissions.
You can inspect the release without changing it:

```bash
codesign --verify --strict --verbose=2 ./mora
codesign -dvv ./mora 2>&1 | grep -E '^(Identifier|TeamIdentifier|Authority)='
spctl --assess --type execute --verbose=4 ./mora || true
codesign --verify --strict --verbose=2 -R='notarized' ./mora
```

The `spctl` command asks Gatekeeper to fetch the online ticket. It can return
"not app-like" for a correctly notarized raw CLI, so its exit status is not the
verdict. The final `codesign` command checks that ticket for the binary's exact
code directory. Do not use `spctl --type install`; Apple defines that policy for
installer packages. The release pipeline also launches a quarantined disposable
copy of the native-architecture binary before it publishes the release.

A later release will add a branded `Mora.app`. That is a whole application
bundle, not a new skin for this executable. Its updater must replace the whole
signed bundle so the seal, icon, and permission identity stay intact.

## Windows

Run this from PowerShell:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.ps1 -OutFile $env:TEMP\install-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1
```

The Windows installer downloads `mora_<version>_windows_amd64.zip` and
`checksums.txt` from GitHub releases. It checks the zip with
`Get-FileHash -Algorithm SHA256`. It extracts `mora.exe` to
`%LOCALAPPDATA%\Mora\bin\mora.exe` and adds that directory to your User PATH.
After the install, open a new PowerShell window. PowerShell can then find
`mora` on PATH.

Install a pinned version with:

```powershell
powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1 -Version 0.10.0
```

**SmartScreen note:** the v1 Windows binary has no signature. Windows can show
**Windows protected your PC**. First, make sure the installer printed a checksum
success. Then choose **More info > Run anyway** or run:

```powershell
Unblock-File "$env:LOCALAPPDATA\Mora\bin\mora.exe"
```

Windows supports Gmail, Google Calendar, file-system folders, notes, and local
Ollama embeddings. iMessage, Apple Calendar, and Address Book are only for
macOS. On Windows, they stop with a clear error.

Windows schedules jobs with Task Scheduler:

```powershell
mora schedule install ingest-hourly
mora schedule list
```

Task names use the `Mora\<job>` form, such as `Mora\ingest-hourly`. All
platforms use the same job names: `ingest-hourly`, `index-hourly`,
`pulse-daily`, `doctor-pulse`, `backup-daily`, `git-daily`, and `lint-weekly`.

Uninstall with:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/uninstall.ps1 -OutFile $env:TEMP\uninstall-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\uninstall-mora.ps1
```

The uninstaller removes `%LOCALAPPDATA%\Mora` and the User PATH entry. It also
deletes scheduled tasks under `\Mora\`. It keeps your vault and config unless
you pass `-Purge`. That option removes Mora data paths after you confirm.

## Initialize

```bash
mora init
```

This creates the vault at `~/vault/mora`. To use a different path, set
`--vault /your/path`. The installer script runs this command for you.

## Connect Google (Gmail + Calendar)

```bash
mora connect google
```

This opens a browser for OAuth consent. On first use, the OAuth app is
**unverified**. Continue through the Google warning with
**Advanced → Go to Mora (unsafe)**. Mora asks for read-only Gmail and Calendar
scopes. It never changes your data.

**WSL users:** `mora connect google` prints a URL but does not open a browser.
Paste the URL into your Windows browser and approve access. The consent redirect
goes to `127.0.0.1`. WSL2 sends it to Mora, so you do not need to paste it back.
Keep the command open until it prints "Connected."

**BYO credentials:** To use your own Google Cloud OAuth client, set:

```bash
export MORA_GOOGLE_CREDENTIALS=/path/to/oauth_client.json
```

**Multiple Google accounts** (personal + work) stay as separate sources. Each
has its own sync status, digest section, and label:

```bash
mora connect google --account work    # second mailbox → gmail-work / calendar-work sources
```

Each account keeps its own token. Mora finds an account that is already
connected by its signed-in address, under any label. It then exits cleanly.
This keeps Mora from ingesting one mailbox twice.

Gmail gets the **last 90 days** by default. Widen the window with
`mora connect google --since-days 365`. Mora saves the window on the source, so
later `mora sync google` runs use it. Calendar always gets a fixed window of
about six months back and three months ahead.

## Connect iMessage (macOS)

```bash
mora connect imessage                    # enable iMessage, check Full Disk Access, then backfill
mora connect imessage --since-days 365   # widen the backlog window (negative = all-time)
```

Mora reads the local Messages database (`~/Library/Messages/chat.db`) in
**read-only** mode. It sends no data. macOS protects this file with
**Full Disk Access**, which it associates with a signed executable target:

1. Run `mora connect imessage`. If access is missing it prints exactly what to do.
2. Open **System Settings → Privacy & Security → Full Disk Access**, add your `mora` binary (or the
   terminal you run it from), and toggle it on.
3. Re-run `mora connect imessage`. `mora doctor` reports the access status any time.

If Full Disk Access was granted to an older ad-hoc-signed Mora, the first move
to the official Developer ID-signed executable is an identity change. macOS can
require you to remove the stale entry, add the installed `mora` again, and grant
access once. Later standalone releases keep the same Developer ID team and
identifier so in-place upgrades have a stable designated requirement.

The planned `Mora.app` migration changes the protected-data target from a raw
executable to an application bundle. Plan for one final Full Disk Access grant
to the app during that migration. Keep the old entry until `mora doctor` and an
iMessage sync pass through the app. Routine app upgrades must replace the whole
bundle, and Mora will not claim that they preserve access until a real version
N to N+1 upgrade proves it without a re-grant.

Contact names come from your address book. Thus, iMessage usually gives the
cleanest name-to-handle map of any source.

## Connect Apple Calendar (macOS)

```bash
mora connectors enable applecalendar
mora ingest run --source applecalendar
```

Mora reads the local Calendar store (the calendar group container) as
**read-only and immutable**. It needs the same Full Disk Access as iMessage, but
no login. Mora writes one memory for each event. It gets the attendees and
organizer for the entity graph. A 180-day forward window keeps subscribed
holiday calendars from filling the vault.

## Add a filesystem source

Point Mora at a folder to ingest documents and metadata. The one-step command
works like `connect google` / `connect imessage`. It adds, enables, and indexes
the source:

```bash
mora connect filesystem ~/code/myproject          # add + enable + index now
mora connect filesystem ~/Documents --name docs   # name it (default: the folder's base name)
```

Run it again on the same folder to index changes. You can connect two different
folders; each gets its own source. Use the longer form to add an off source
first and ingest it later:

```bash
mora sources add filesystem --name myproject --path ~/code/myproject --scope project:myproject
mora connectors enable filesystem
mora ingest run --source myproject
```

The command order does not matter. `sources add` uses the type's consent. A
source added while filesystem is on starts on and can ingest at once. You do not
need to enable it again. If you run `mora connectors enable filesystem` before
you set a folder, Mora enables nothing. A file-system source needs a path.
Instead, Mora points you to the two commands above.

Mora ingests only selected file types: `.md`, `.json`, `.yaml`, `.toml`,
`.txt`, `.csv`, `README`, `go.mod`, `CLAUDE.md`, `AGENTS.md`, and similar
metadata files. It also ingests **`.docx`** (Word documents) and **`.pdf`**.
Pure-Go libraries extract their text without CGO. Mora indexes only text it can
read. A scanned, image-only PDF gives no text because Mora has no OCR. Mora
skips other binaries and build files.

## Manage connectors

See what is connected, what is enabled, and what still needs a login:

```bash
mora connectors list            # every connector, its enabled state, and whether it needs auth
mora connectors list --json     # the same, machine-readable
mora connectors setup           # interactive menu to pick and enable connectors
```

The catalog is `gmail`, `calendar`, `filesystem`, `imessage`, and
`applecalendar`. You must enable a connector and give consent. `enable` runs the
OAuth or access check but gets **no data**. `disable` stops later syncs but does
not delete indexed data.

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

You can also use the example configs. Copy `examples/claude-code-mcp.json` to
your project's `.claude/mcp.json`. Codex can use
`examples/codex-mcp.json`.

`mora mcp serve` gives 12 tools over JSON-RPC: `write_memory`,
`read_memory`, `search_memory`, `list_memory`, `delete_memory`,
`context_memory`, `think`, `list_entities`, `get_entity`, `digest`, `brief`,
and `meeting_prep`. `brief` shows what changed and what matters at session
start. `brief --event-id <id>` and `meeting_prep` build the same cited
pre-meeting view. This view has past candidate lines, open threads, source age,
and material shared context.

Mora ranks these candidates across attendees and shows them as dated evidence.
They are not current truth or a verified commitment ledger. Mora does not yet
find obligation owner, direction, or closure with enough trust. Check each
citation and each red source-health warning before you act.

`meeting_prep` takes `event_id` and an optional RFC3339 `at` seam. If you omit
`event_id`, it selects the next event. `digest` and the session-start `brief`
also take `entity`/`scope`/`since_days`. Use them to select one person,
namespace, or time window. Each `search_memory` / `context_memory` answer also
has a per-source `last_synced` map. Your agent can use it to state the data age.

## Use Mora from the shell

Each MCP tool has a CLI command. You can read and write the vault from a
terminal. The commands use the same search as the agent:

```bash
mora search "OAuth status" --scope project:acme --json   # search the vault (the search_memory tool)
mora write --scope project:acme --type decision --title "Chose OAuth" --text "..." \
  --as-of 2026-07-25T12:00:00Z --durability working \
  --flip-conditions "security review fails;provider terms change"   # save a decision with its validity
mora read <id> --json                                    # one memory by id
mora list --scope project:acme --json                    # browse memories in a scope
mora delete <id> --yes                                   # remove one memory
mora context --query "auth" --scope project:acme --budget 6000 --json   # token-budgeted context block + per-memory receipts in items[]
mora think "what did Sam decide about pricing?" --json   # cited evidence + gap analysis
```

`mora context` builds one character-limited block for a query. The default is
2000 characters. Omit `--query` for a recent-data brief. Decision memories use
`--as-of`, `--durability provisional|working|standing`,
`--flip-conditions` (semicolon-separated), and optional `--review-by`.
Incomplete, legacy, or expired decisions are marked `needs_review`.

`mora write` writes only to the local vault, never to your connected accounts.

### Teach Mora

Teach records a local, reversible human correction in Mora's governance
ledger, then deterministically rebuilds the derived view:

```bash
mora teach identity list
mora teach identity confirm --handle <phone> --email <address> --yes

mora teach commitment not-a-commitment --memory-id <id> --yes
mora teach commitment wrong-person --memory-id <id> --person sam@example.com --yes
mora teach commitment wrong-direction --memory-id <id> --direction owed_by_self --yes
mora teach commitment already-closed --memory-id <id> --yes
mora teach commitment duplicate --memory-id <id> --duplicate-of <commitment-id> --yes
mora teach commitment useful --memory-id <id> --yes

mora teach memory correct --id <id> --title "Corrected" --text "..." --yes
mora teach memory supersede --id <id> --title "Replacement" --text "..." --yes
mora teach memory retract --id <id> --yes
mora teach history --memory-id <id>
mora teach undo <ledger-id>
```

Identity proposals show their typed corroborating evidence and affected-memory
list before confirmation. If one memory opens multiple commitments, pass
`--commitment-id` to select one. Connector evidence is immutable; memory
revision commands apply only to authored memories. Ordinary reads hide
retracted and superseded revisions, while history and original files remain
auditable.

Human corrections are not evaluation data by default. `mora teach examples
--json` refuses until a user runs `mora teach consent enable --yes`, and even
then exports structural verdict fields and an ordinal reference, never raw
memory text, timestamps, or identity-derived references. Teach mutations are
intentionally absent from MCP. See
[architecture: Teach and human correction](architecture/21-teach.md).

### Permanently forget a person or chat

`mora delete` removes one memory now. If it came from a connector, the next
hourly sync restores it. `mora forget` makes the removal last. It removes the
matching memories **and** records a local block. Sync cannot create them again.

```bash
mora forget --chat imessage_chat/<guid> --dry-run   # preview exactly what would be removed
mora forget --chat imessage_chat/<guid> --yes       # forget one conversation/thread/event
mora forget --handle +14155550123 --yes             # forget a 1:1 iMessage counterpart
mora forget --email sam@example.com --yes           # forget a 1:1 email counterpart
mora forget list                                     # show active suppressions
mora unforget <entry-id> --yes                       # reverse a forget
```

Forgetting is **local-only**. It stops Mora from keeping or getting the content
again on this Mac. It also applies to your other devices through `mora sync git`.
It never deletes data at Gmail or Apple.

`--handle`/`--email` remove one-to-one memories with that person. They keep
group threads that include the person. Your address is on each email thread.
Thus, `--email` matches only a thread whose sole *other* party is that address.
For one thread with more people, use `--chat <thread-id>`. Broader email forget
rules will use the identity graph.

Always run `--dry-run` first to see which memories Mora will remove. `unforget`
removes the block. A later sync can then ingest the content again if it is in
the connector's lookback window. See
[architecture: governance ledger](architecture/17-governance-ledger.md).

## Make the brief your session-start default

The brief is a daily *what-changed / what-matters* digest. It shows new or
updated threads since your last view. It sorts them by who matters to you. Each
item has an id that you can cite. A real person's email with a deadline starts
the brief on an **Urgent** shelf. The byte budget never drops it without notice.
The brief has a scheduled **write side** and a session-start **read side**:

```bash
mora schedule install pulse-daily   # write side: syncs, then persists ~/vault/mora/briefs/<date>-brief.md each morning
mora brief                          # read side: print today's brief (generates one locally if none exists yet)
mora brief --fresh                  # regenerate today's brief even if one already exists
```

`pulse-daily` uses Mora's durable `daily-brief` loop. A second scheduler call on
the same day does nothing. The active pulse renews its lease and holds its owner
fence through the full watermark transaction. The run, artifact, and watermarks
must use one logical UTC period.

Before the transaction, Mora saves and fsyncs a durable effect intent. It then
fsyncs the artifact and watermarks, and records the commit checkpoint. If a
crash leaves only the intent, status reports `uncertain`. Mora blocks an
automatic same-day retry to prevent a second advance. Old schedules use the
same gate at run time. A reinstall updates their stored command but is not
needed for safety.

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

This wiring is **docs-only**. Paste the text to opt in, and remove it to opt out.
The optional `mora hook install` command is the one exception. It adds Mora's
hooks to `~/.claude/settings.json` next to your hooks. It will not change a
settings file that it cannot parse.

`mora brief` and the `brief` tool make **no network call**. They read or build
from memories on disk. Only the scheduled sync uses the network. It uses your
enabled, read-only sources.

## Explore the entity graph

Mora builds a read-only entity graph from your data. It gets **people** from
mail, messages, and calendars. It gets scopes, tags, `[[wikilinks]]`, and
`- [categories]` from your vault:

```bash
mora entities                 # people + topics across your memory, with counts
mora entities "Sam"           # the memories that reference one entity
mora graph                    # visual map — top people + topics as proportional bars
mora graph "Sam"              # expand one entity: connections, relationship breakdown, evidence
```

The view for one entity shows people that occur with it. It also shows the edge
types (`EMAILED` / `PARTICIPATED_IN` / `ATTENDED` / `MENTIONS`) and evidence
memories. Each link cites a StableID. Agents get the same view through the
`list_entities` and `get_entity` MCP tools. [How it works](#how-it-works)
explains how Mora sorts, trusts, and joins people across addresses.

## Browse the vault in Obsidian

Open Obsidian and add the vault directory as a new vault. Its default path is
`~/vault/mora`. Memories, synced emails, and calendar events appear as Markdown
files.

## Day to day

**Keep data fresh on a schedule.** Mora uses launchd on macOS and Task
Scheduler on Windows. On Linux, it prints a cron line.

```bash
mora schedule install ingest-hourly   # hourly background sync of every enabled source
mora schedule install pulse-daily     # 8am daily brief (sync-first, persisted, notification)
mora schedule install doctor-pulse    # 9am freshness alarm (native toast + exit 2 when a source is unhealthy)
mora schedule list                    # show which jobs are installed
```

The full set of jobs is `ingest-hourly`, `index-hourly`, `pulse-daily`, `doctor-pulse`, `backup-daily`, `git-daily`, and `lint-weekly`.

On macOS, `schedule install` writes the launchd plist and loads it at once with
`launchctl bootstrap`. You do not need to log out. If this step fails, the
command exits non-zero. It prints the exact `launchctl` command for a manual
load.

**Check sync freshness:**

```bash
mora sync status
```

**Check your install:**

```bash
mora doctor
```

`mora doctor` reports the health of the install. It checks the vault and index,
and that tokens stay outside the vault. It shows the last sign-in for each
Google account, storage size, and whether the vault is a git repo. It also shows
the age of each enabled source. On macOS, it checks Full Disk Access for
iMessage and Apple Calendar.

**Per-source freshness and the health alarm:**

```bash
mora doctor --json      # machine-readable report; each connector gets a `source_fresh:<key>` check
mora doctor --strict    # exit non-zero if any critical check fails, incl. a stale/failed/never-synced source
mora doctor --pulse     # freshness-only check: a native toast + exit 2 when any source is unhealthy, exit 0 when all are fresh
```

A connector can go stale when its background sync keeps failing. A six-day
case of this bug led to `doctor --pulse`. Gmail, Calendar, and Apple Calendar
warn after 24h without a clean sync. iMessage and filesystem warn after 48h.
Any stored sync error warns at once, even before that age.

When a source or index is unhealthy, the red banner starts `mora brief` and
`mora brief --event-id`: `🔴 MORA HEALTH: <source> — no successful sync for <N>h (<error>). Run: mora doctor`.
Background producer liveness issues (ops attention) render as yellow warnings:
`🟡 MORA HEALTH: <producer> has not been produced for <N>h. Run: mora doctor`.
Thus, a stale or dead source cannot be absent from a brief without a warning, and producer liveness is distinguished from data staleness.
Schedule `doctor-pulse` to get the native alert without a manual check.

**Record open tasks so the brief can surface them:**

```bash
mora tasks add "Reply to Sam about the launch" --pri P0   # capture an open loop (name first, then flags)
mora tasks list                                           # current live tasks (--json for machine-readable)
mora tasks done "Reply to Sam about the launch"           # mark one done so it stops resurfacing as stale
mora tasks sync --write                                   # scan memories for open tasks and record them
```

The brief shows live tasks. Stale tasks stay there until you mark them done.

**Morning brief / per-source rundown:**

```bash
mora brief                                                 # what changed / what matters
mora brief --event-id calendar_event/abc --at 2026-07-10T15:00:00Z  # reproducible, fully-cited meeting brief
mora brief --entity "Riya" --since-days 7                  # just one person, last week (preview-only)
mora pulse --digest --source imessage --since-hours 168    # "just my texts this week"
```

**Tune context density.** This scales the default budgets for context, digest,
and brief. `large` raises each call limit to 50k tokens. It also doubles the
digest text length.

```bash
mora config context large       # small | default | large
mora config embedder ollama     # durable semantic-retrieval opt-in (loopback-only)
mora config mmr on              # diversity-aware rerank of hybrid results (off by default; needs embedder ollama)
```

MMR removes near-duplicate hits from a result set. It is off by default. It
works only when the Ollama embedder is on.

**Tell Mora your other email addresses** (`self_emails` in `config.toml`):

```toml
self_emails = "you@work.com, you@icloud.com"
```

Mora knows the mailbox that you approved for Google. The connectors also record
which invitee is you. Google uses `Attendee.Self`, and Apple uses
`Participant.is_self`. But a calendar can invite an address that neither one
covers, such as a Workspace alias or custom domain.

If Mora cannot find you in the invitee list, it will **not guess**. It refuses
to assign meeting items and tells you to add the alias here. Your address list
also keeps Mora from showing your records as another person's unfinished work.

**Re-sync Google data manually:**

```bash
mora sync google
```

**View local usage analytics (stays on your disk; query text is not recorded by default):**

```bash
mora usage report
```

The usage log keeps the tool name, time, result counts, response-envelope byte
size, and compact phase timings where Mora can separate them cleanly. Read
events distinguish full and bounded-match requests; the allowlisted
`evidence_ref` label is reserved for evidence-reference integration. They keep
only counts, truncation, and requested/used budgets. They never keep a memory
id, body, excerpt, match/evidence text, metadata, attachment path, or vault path.
The log stays at `<state_dir>/usage/events.jsonl`; Mora never writes it into your
vault or sends it over the network.

Query text is omitted by default. You can opt in to keep raw *search* query
strings locally, such as for an eval set. This opt-in never causes
`read_memory` content or match/evidence arguments to be retained. You can turn
query retention off at any time:

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

Keep two separate things fresh: **your data** and **the app**.

**Refresh your data:**

```bash
mora sync status                 # per-source freshness — when each connector last pulled
mora sync google                 # re-pull Gmail + Calendar
mora sync filesystem             # re-index enabled filesystem sources
mora sync imessage               # re-read the local Messages DB (macOS)
mora reingest --full             # re-fetch + rewrite memories with the latest metadata AND rebuild the entity graph
```

Run `mora reingest` after you update to a build with better extraction. Better
identity capture is one example. The command rewrites current memories with the
new logic. It also rebuilds the graph in one atomic pass.

**Update the app itself:**

```bash
mora upgrade                     # in-place self-update to the latest release (verifies checksums before swapping)
mora upgrade --check             # just report whether a newer release exists
```

After a successful swap, `mora upgrade` rebuilds the search index with the new
version. A schema change never leaves a stale index. With the default embedder,
Mora rebuilds it on the first read. An Ollama re-index can take minutes. Mora
then tells you to "run `mora index rebuild`" and does not hide the old index.

On macOS, the standalone bridge swaps one notarized Developer ID-signed binary
for another with the same `com.pyranthus.mora` identifier and Apple team. Do not
run `codesign --sign -` on an installed release: that replaces the stable
identity and can invalidate Full Disk Access. The later `Mora.app` line will use
a separate whole-bundle updater; the standalone updater must not extract an app
asset and replace only `Contents/MacOS/mora`, because that would break the app's
signature seal.

Direct-binary installs update from public GitHub releases. They need no token or
login. Mora sends Homebrew installs to `brew upgrade`. Source and `go build`
builds report `dev` and do not update themselves. Rebuild them with
`git pull && go build`. Mora also stops local git builds at or ahead of the
latest release. This includes versions such as `v0.10.0-60-g2d08334` and
`-dirty`. The update runs, with a note, only when the release is newer than the
build's base tag.

## Back up the vault off-device (opt-in)

By default, the vault stays on the machine. For an off-device backup with
history, Mora can push it to a **private git remote you control**. This can be
GitHub, GitLab, your server, or a bare repo on a USB drive.

The flow is **one-way, push-only**. Your local vault stays the source of truth.
Mora shows each git error and never uses `--force`. A non-fast-forward rejection
means that the remote changed. Mora tells you and does not replace it.

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

`--init` writes a safe `.gitignore` for `index.db`, `*.db`, `.DS_Store`, and
`tokens/`. This keeps the rebuildable index and secrets on the machine. It finds
an existing repo through `vault/.git`. Mora does not use a parent repo when the
vault is inside one. It also refuses a `.git` gitfile or symlink that points
elsewhere.

Ignore rules do not apply to git-*tracked* files. If git already tracks index or
token files, sync stops and tells you how to fix it. It does not push them.
Restore on a new machine with
`git clone <remote> ~/vault/mora && mora index rebuild`.

The vault holds decoded iMessages and Gmail threads in **plaintext**. Use a
private remote that you own. Mora runs no server and never selects a place for
you. `mora doctor` warns when the vault is a git repo.

Mora calls your system `git`, and `gh` for `--github`. Your SSH keys, credential
helper, or `gh auth` still work. To encrypt data at rest on the remote, add
[git-remote-gcrypt](https://spwhitton.name/tech/code/git-remote-gcrypt/) to any
remote. The flow stays the same.

## Share memories with someone (opt-in, encrypted)

`mora share` sends **one scope of memories you wrote** with `mora write` or MCP
`write_memory`. It sends them to a **private git remote you control** or a
**user-owned S3/R2 bucket**. It encrypts the data for each recipient with
[age](https://age-encryption.org).

The person you invite gets your notes as a **read-only, separately-indexed
corpus**. Their `mora search` and `mora think` include your memories and name
you as the owner. Mora does not merge them into the other person's vault or
people graph. Mora never shares connector evidence such as Gmail threads,
iMessages, or calendar events. Write the decision as a note and share that.

The receiver starts. Create a key and send the **public** half to the publisher
through a trusted channel. Only the public key travels. There is no key server.

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

The design has these rules and limits:

- **Encryption is mandatory.** `push` refuses to run without at least one
  recipient key, and only `*.md.age` ciphertext ever enters the repo. `mora
  doctor` checks that staging has no plaintext. It also shows each configured
  share. Keep the remote private because it holds your data.
- **A preview before every push.** Mora lists all data before it leaves.
  `mora share preview` shows the full content.
- **Reading someone never rewrites you.** A subscription lives beside the vault
  in Mora's data directory and has its own index. Backups, vault git-sync, the
  entity graph, and `delete_memory` do not see it. `think`'s gap analysis still
  states what *your* vault does not know.
- **Revocation is honest, not magic.** `mora share remove` stops later pushes.
  Git history lasts, and subscribers keep data that they pulled. To stop later
  access, use a new repo and new recipient keys.
- Mora calls your system `git`. Your SSH keys, credential helpers, or `gh auth`
  work in the same way as vault backup.

## How it works

Search has three layers. Mora builds them from your data at ingest time. It
uses no model unless you select the optional Ollama embedder. The Go binary runs
all work against a local SQLite index. For diagrams and `file:line` citations,
see [`docs/architecture/`](architecture/00-overview.md).

### 1. The entity graph — derived from message metadata

An **entity** is a repeated thing in your vault. It can be a **person**, a
**scope** (project/namespace), a **tag**, a `[[wikilink]]`, or a
`- [Category]` line. Mora writes these entities and their edges to the
`entities` / `edges` tables. It does this **in the same transaction as the index
rebuild** with `buildGraph` in `internal/mora/graph.go`. Thus, the graph and
search index always change together. Rebuilds produce the same bytes.

Connectors give Mora the identity data for people. No name-entity-recognition
model takes part. Gmail, Calendar, and iMessage put structured identity in each
memory's metadata. Mail has `from` / `to` / `cc`. Calendar has `organizer` /
`attendees`. iMessage has handle↔name `participants` pairs.

`personRefs` maps these values to standard `person:<lowercased-identity>` nodes.
For example, 40 uses of `neil@x.com` map to one node. It also writes edges with
two time stamps and source-memory proof:

- **`PARTICIPATED_IN`**: a person was on a thread or chat
- **`ATTENDED`**: a person was on a calendar event
- **`EMAILED`**: sender → each recipient, mail only
- **`MENTIONS`**: a person *known from metadata* who also appears by name in another message's body, matched by a gazetteer built **from the graph's own person aliases** (`gazetteer.go`); it is word-boundary, multi-token names, stoplisted, deterministic tie-break. Still no model.

A bulk email with 500 recipients does not fill the graph. Mora limits person
fan-out with `maxParticipantFanout = 64` and warns when it drops data.
Co-occurrence, such as "who else was on Sam's threads," uses a **query-time
self-join**. Mora does not store those joins. Thus, an N-person thread uses O(N)
edge rows, not O(N²).

**Concrete example.** You and Sam sent 40 emails and a few iMessages.
`mora entities` shows `Sam Rivera  43` under **People**.
`mora entities "Sam Rivera"` lists those 43 memories. Through MCP, `get_entity`
also gives his aliases, `degree`, and incoming edges with `evidence_id`. It
lists his 1-hop `neighbors`: people who share threads or events with him.

The person graph is also cleaned so it reflects *people*, not raw addresses:

- **Mora moves automated senders out of the People view.** It marks no-reply,
  receipts, notifications, and "LinkedIn Job Alerts" bots as `service`. You can
  still find them with `get_entity`.
- **Mora trusts aliases by source.** A name becomes a match key only if its
  owner *presented it themselves*. An email sender or iMessage contact can do
  this. Spam mail-merge labels and other people's wrong labels cannot change
  your identity.
- **The same human across addresses collapses into one person.** Gmail dot/`+tag` variants and
  cross-domain matches with a full-name anchor merge. Thus, `get_entity` gives
  the *complete* view for each address. Mora uses strict rules and never joins
  two people from a weak signal.

### 2. Hybrid retrieval — BM25 + embeddings + graph expansion, fused by RRF

Keyword search misses a paraphrase such as "launch" versus "shipping." Pure
vector search can miss exact terms and proper nouns. `hybridSearch`
(`internal/mora/hybrid.go`) runs and joins three search methods:

1. **FTS5 / BM25**: the exact-match check. Proper nouns, IDs, and exact phrases
   always rank.
2. **Static-embedding cosine**: finds paraphrases through per-memory vectors in
   `mem_vectors`. Mora drops zero-similarity rows. They cannot add unrelated
   memories to the vector results.
3. **1-hop graph expansion**: if the query *names a person*, Mora adds that
   person's evidence memories to the candidate set. It uses a gazetteer and an
   exact alias match. This is GraphRAG-lite with no LLM.

`mora search` and `search_memory` use hybrid search only with an active semantic
embedder. You must opt in to Ollama, and Mora must reach it. The default
static-hash embedder uses only FTS. In tests, hybrid search with static-hash
reduced recall@5 from 0.591 to 0.394. Turn on Ollama with
`mora config embedder ollama` to use the cosine and graph methods.

**Weighted Reciprocal Rank Fusion** joins the three ranked lists. Each method
adds `weight / (k + rank)` with `k = 10`. The weights make exact matches lead:
`fts 1.5`, `vec 1`, `graph 1`. RRF uses rank, not raw score. Thus, it can join
unbounded BM25 scores with cosine's `[0,1]` values without normalization. The
weights keep keywords ahead, so a fuzzy vector hit cannot hide a proper noun.

**Embedder limits.** The default is a pure-Go, deterministic **feature-hashing
static embedder**, `staticEmbedder` in `embed.go`. It hashes word tokens and
character trigrams into a fixed 256-dim space. It uses a signed hash, TF
weights, and L2 normalization. Cosine tracks shared words and word parts. Thus,
"launching" and "launch" share a signal.

This is not a semantic model. It tracks tokens and word parts, not meaning, so
it has limited paraphrase recall. An `Embedder` interface lets an **Ollama**
model, `nomic-embed-text`, take its place. You must opt in with
`mora config embedder ollama` or `MORA_EMBEDDER=ollama`. `chooseEmbedder`
**refuses any non-loopback `MORA_OLLAMA_URL`**. Memory text stays on the
machine. If Mora cannot reach the daemon, it warns and uses the static embedder.

**Fallback.** If an index has no `mem_vectors` table, `hybridSearch` uses only
FTS. Search still works. When vectors exist, Mora starts the cosine and graph
methods. Each vector stores its model id. A new embedder causes a clean
re-embed, so Mora does not mix vectors from different models.

### 3. `mora think` — a synthesis envelope your own agent fills in

`mora think` (`think.go`) has **no** LLM and holds no API key. It returns a
*synthesis envelope* with the data an agent needs for a cited answer. The agent
that called Mora through MCP writes the prose. The envelope has three parts:

- **Cited evidence**: the top hybrid-search hits. Each has `stable_id`, scope,
  time stamp, fused score, and text. These fields tie later claims to evidence.
- **Gap analysis ("what the vault does NOT know")**: runs in the same way
  *before any model runs*. It reports three signals. **stale** means the newest
  matching memory is more than 30 days old. **thin coverage** means a named
  person in the query has fewer than 2 memories. **coverage holes** means a
  real-name-shaped phrase maps to no entity.
- **A synthesis prompt**: tells the agent to use only this evidence. It must cite
  each claim with its `[stable_id]`. It must list known gaps in a "What the
  vault does not know" section.

For example, `mora think "what did we decide with Sam about pricing?"` finds the
related threads as cited evidence. If the newest item is two months old, it
adds a `stale` gap. If Sam has one memory, it reports thin coverage. Your agent
reads the envelope and writes the answer with evidence ids and gaps.

## Why not just use a cloud connector?

The main tradeoff is durable, owned data. Mora is not the only local or
cross-source product. Live connectors can be easier to set up and can act on
source systems. Mora builds a corpus on your machine. Several agents can check,
cite, grep, diff, back up, or rebuild it.

The privacy rule is more exact than "nothing leaves this computer":

- Mora calls enabled source APIs to sync data. It calls GitHub for updates. It
  also calls backup or share targets that you set.
- Mora does not operate a hosted corpus or telemetry service. Its optional usage
  log is local, omits query text by default, and can be disabled.
- Optional Ollama inference is restricted to loopback.
- Once an MCP client retrieves context, that client's model and data policy apply.
  A cloud-hosted agent may transmit the retrieved snippets to its provider.

Choose Mora when you want a readable local corpus and read-only source access.
This choice needs more setup, permissions, storage, and upkeep. Choose a hosted
connector when quick access and source actions matter more.

## Notes

- Google Drive ingestion is later work.
- Mora stores tokens in `~/.config/mora/tokens/` (0600). It never syncs or
  sends them.
