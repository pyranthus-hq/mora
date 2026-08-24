# The Mora guide

Mora gives AI agents a shared memory that lives on your computer. This guide
explains setup, daily use, permissions, backup, and repair. For code details,
read the [architecture documents](architecture/00-overview.md).

- [Install](#install)
- [Full Disk Access on macOS](#full-disk-access-on-macos)
- [First setup](#first-setup)
- [Connect data](#connect-data)
- [Give Mora to an agent](#give-mora-to-an-agent)
- [Daily use](#daily-use)
- [Corrections and removal](#corrections-and-removal)
- [Schedules and durable loops](#schedules-and-durable-loops)
- [Update](#update)
- [Uninstall](#uninstall)
- [Backup and sharing](#backup-and-sharing)
- [Settings and environment variables](#settings-and-environment-variables)
- [How search works](#how-search-works)
- [Privacy boundary](#privacy-boundary)
- [Troubleshooting](#troubleshooting)

## Install

Mora is alpha software. Back up important data. Check cited source records
before you act on an answer.

### macOS: signed Mora.app

This is the recommended macOS install. The script downloads the release,
checks its checksum, Apple signature, notarization ticket, app identity,
architecture, and version. It installs `~/Applications/Mora.app` and links the
`mora` command to the app's executable. It does not clear quarantine or sign
the app again.

```bash
(
  set -e
  mora_installer="$(mktemp -t mora-install)"
  trap '/bin/rm -f "$mora_installer"' EXIT
  curl -fsSLo "$mora_installer" https://raw.githubusercontent.com/pyranthus-hq/mora/main/install-app.sh
  sh "$mora_installer"
)
```

The app is the Full Disk Access target for iMessage and Apple Calendar. Install
it before you grant that permission.

#### Homebrew status

The signed-app Homebrew Cask is not published yet. `cmd/gencask` exists so the
future Cask can be reproduced exactly from a release tag and
`checksums-app.txt`, but it does not publish anything and currently refuses to
declare `auto_updates true`. That declaration remains blocked until #291 ships
scheduled update checks and notification behavior. The private legacy Cask is
not a supported workaround: it installs a raw binary and strips quarantine.

`mora upgrade` replaces the whole app bundle. It checks the new bundle before
the swap and checks it again after the swap. It restores the old bundle if the
post-swap check fails. Never replace only
`Mora.app/Contents/MacOS/mora`. That breaks the bundle signature.

Mora v0.12.0 came before whole-app self-update. If you still run that version,
run the `install-app.sh` command above once instead of using its `mora upgrade`.
Later app versions use the whole-bundle update path.

One real signed v0.12.3 to v0.12.4 update preserved iMessage and Apple Calendar
access without another grant on one tested Mac. This is useful evidence, not a
guarantee for every Mac. A valid signature alone does not prove that protected
reads still work. Run `mora doctor` and a protected sync after every update.

### Linux and standalone compatibility

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

The script checks the release SHA-256 checksum. On macOS, new installs should
use `Mora.app` instead. The standalone file remains for old installs and Linux.

### Windows

Run this in PowerShell:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.ps1 -OutFile $env:TEMP\install-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1
```

The installer checks the release checksum, installs `mora.exe` under
`%LOCALAPPDATA%\Mora\bin`, and adds that folder to your user PATH. Open a new
PowerShell window after install.

The Windows file is not signed. SmartScreen may warn. Check that the installer
reported a valid checksum before you choose **More info > Run anyway**. You can
also run:

```powershell
Unblock-File "$env:LOCALAPPDATA\Mora\bin\mora.exe"
```

Windows supports Gmail, Google Calendar, folders, GitHub Issues, notes, and
local Ollama embeddings. iMessage, Apple Calendar, and Address Book need macOS.

### Build from source

Mora needs Go 1.25 or later. It is pure Go and does not need CGO.

```bash
go install github.com/pyranthus-hq/mora/cmd/mora@latest
```

A source build reports version `dev` and does not update itself. Build again to
update it. Source builds include only a non-secret Google OAuth placeholder.
Set `MORA_GOOGLE_CREDENTIALS` to your own client JSON before Google setup.

## Full Disk Access on macOS

iMessage and Apple Calendar are local files. macOS protects them with Full Disk
Access. Mora cannot grant this permission or confirm that you clicked a screen.
It can only test whether the read works.

1. Install the signed `Mora.app`.
2. Open **System Settings**.
3. Open **Privacy & Security**.
4. Open **Full Disk Access**.
5. Press **+**. Choose `~/Applications/Mora.app`. If the file chooser does not
   show that folder, press Command-Shift-G and enter the path.
6. Turn Mora on. If macOS asks, quit and reopen Mora or your terminal.
7. Run `mora doctor`.
8. Run `mora sync imessage` or `mora sync applecalendar`.

If you granted access to an older standalone Mora, keep that old entry until
the checks pass through `Mora.app`. You can then remove the old entry yourself.

Scheduled jobs must use the same app identity. When Mora runs inside the signed
app, `mora schedule install` creates a macOS job that starts the app through
LaunchServices. Reinstall each existing job after you move from a standalone
binary:

```bash
mora schedule install ingest-hourly
mora schedule install pulse-daily
```

The macOS app launcher waits for the command but does not return the inner
command's exit code. Mora therefore records source and producer receipts. Use
`mora doctor` and `mora sync status` as the result, not the launcher's status.

### What Mora reads

- iMessage opens `~/Library/Messages/chat.db` with SQLite `mode=ro`.
- Apple Calendar opens its live SQLite store with `mode=ro` and
  `query_only(1)`. It does **not** use `immutable=1`, because that could ignore
  current write-ahead-log changes.

Mora does not write to either source database.

## First setup

Mora is your local evidence store and your agent is the conversational interface.
After you connect a source, ask a concrete question such as **“what did Sam and I
decide about the launch?”** or **“what's on my calendar next week?”** Reading and
searching retrieve evidence only. Saving a durable memory requires explicit write
consent; you can disable a connector or delete a saved memory whenever you choose.


```bash
mora init
mora doctor
```

The default vault is `~/vault/mora`. To choose another path:

```bash
mora init --vault /absolute/path/to/mora
```

### Four similar words

| Word | Meaning |
| --- | --- |
| Connect | Set up a source, enable it, and get its first data in one flow. |
| Enable | Allow a connector to run. This alone does not mean data was pulled. |
| Ingest | Read enabled sources and write the current data into the local vault. Use it for first load and backfill. |
| Sync | Refresh a source that is already set up. |

`mora connectors disable <name>` stops future reads. It does not remove old
memories. `mora forget` is the lasting local removal tool.

## Connect data

Mora has six connectors: Gmail, Google Calendar, filesystem, iMessage, Apple
Calendar, and GitHub Issues.

See their state with:

```bash
mora connectors list
mora connectors list --json
mora connectors setup
```

### A folder

A folder is the fastest setup because it needs no login:

```bash
mora connect filesystem ~/Documents/notes
mora connect filesystem ~/code/acme --name acme
```

Run the same command again to read changes. Or use:

```bash
mora sync filesystem
```

Mora reads text and common project files, including Markdown, JSON, YAML, TOML,
CSV, Word `.docx`, and text-based PDF files. It has no OCR, so it cannot read a
scanned image-only PDF.

The longer form can add a source before it runs:

```bash
mora sources add filesystem --name acme --path ~/code/acme --scope project:acme
mora sources list --json
mora connectors enable filesystem
mora ingest run --source acme
```

`mora sources list --json` emits the `mora.sources.list` v1 receipt. Its
configured-source array lives under `sources`, and is `[]` when none exist.

The connector verbs also take `--json`:

- `mora connect filesystem <path> --json` emits the `mora.connect.filesystem`
  v1 receipt with the registered source name, the resolved path, the scope, and
  how many files the first walk indexed.
- `mora sync <source> --json` emits a `mora.sync.<source>` v1 receipt with the
  source name and the item count that run pulled.
- `mora ingest run --all --json` (or `--source <name> --json`) emits the
  `mora.ingest.run` v1 receipt with the item count and how many sources failed.
  A run that ingests some items and then fails still emits the receipt before it
  exits non-zero, so the count of what landed is never lost to an error. A run
  that fails before ingesting anything — an unknown source, a disabled source, a
  missing selector — emits no receipt, because a usage error is not a result.

Under `--json` these commands send their progress lines and resumable-failure
warnings to stderr, so stdout stays exactly one JSON document.

### Gmail and Google Calendar

```bash
mora connect google
```

Mora opens a browser for OAuth. OAuth is Google's approval flow. Mora requests
only `gmail.readonly` and `calendar.readonly`. You must review and approve the
screen yourself.

Google may show that Mora's shared OAuth app is unverified or may limit test
users. You can use your own installed-app OAuth client:

```bash
export MORA_GOOGLE_CREDENTIALS=/absolute/path/to/oauth_client.json
mora connect google
```

For a second account:

```bash
mora connect google --account work
```

Each account has its own token and sync state. Gmail gets the last 90 days by
default. Change the saved window with:

```bash
mora connect google --since-days 365
```

Calendar uses a fixed window of about six months back and three months ahead.
Refresh the accounts with `mora sync google`. Remove local Google tokens with
`mora disconnect google`. That command does not delete Gmail or Calendar data.

On WSL, Mora may print the OAuth URL instead of opening it. Open the URL in the
Windows browser. Keep the command running while the browser returns to the
loopback address at `127.0.0.1`.

### GitHub Issues

```bash
mora connect github --repo pyranthus-hq/mora
mora connect github --repo owner/repo --repo owner/second-repo
```

`--repo` is repeatable and sets the repository allowlist. Without it, Mora uses
its built-in default list. Mora reads issues. It does not create, edit, close,
assign, or act on them.

Public repositories work without a token at GitHub's lower rate limit. For a
private repository or a higher limit, give the process a read-only token:

```bash
export MORA_GITHUB_TOKEN=github_pat_...
mora sync github
```

Mora does not save this token. It keeps issue number, title, body, state,
labels, assignees, URL, GitHub times, and local read time. It also keeps an
immutable local receipt for each update in the state directory. A close or
reopen updates the same stable issue memory.

A failed or partial GitHub sync is shown in `mora sync status`. Old indexed data
stays searchable and may be stale.

### iMessage

```bash
mora connect imessage
mora connect imessage --since-days 365
```

By default, Mora ingests the last **365 days**. Connect and sync output state the effective
window; pass `--since-days N` to choose another value. A negative `--since-days` asks for
all available history, which can be large.
Mora reads one conversation into one memory. It uses Address Book locally to
map handles to names. It sends no message data.

If setup reports a permission error, complete the
[Full Disk Access steps](#full-disk-access-on-macos), then run:

```bash
mora sync imessage
```

### Apple Calendar

```bash
mora connectors enable applecalendar
mora ingest run --source applecalendar
```

There is no account login. Full Disk Access is the permission gate. Mora writes
one memory per event and reads attendees and organizer data. A 180-day forward
limit prevents large subscribed calendars from filling the vault.

Refresh it with:

```bash
mora sync applecalendar
```

### Ingest all enabled sources

```bash
mora ingest run --all
```

Mora continues after one source fails so later sources still run and the index
still rebuilds. The final command still returns an error and names each failed
source. This keeps the snapshot honest.

## Give Mora to an agent

### MCP over standard input and output

MCP is a standard way for an agent to call tools. Use:

```bash
claude mcp add mora -s user -- mora mcp serve
codex mcp add mora -- mora mcp serve
```

Or add this to another MCP client:

```json
{
  "mcpServers": {
    "mora": { "command": "mora", "args": ["mcp", "serve"] }
  }
}
```

Mora provides 13 tools: write, read, search, list, calendar events, delete, context,
think, entity list, entity detail, digest, brief, and meeting prep. Use
`calendar_events` for exact date, day, and week questions rather than keyword search.

### Agent Plugins package

Release assets include a portable Agent Plugins 1.0 archive. It contributes the
Mora stdio MCP declaration plus first-party skills for read-only recall, explicit
memory capture, dining recommendations, and the advanced daily brief operator
loop. The package does not contain a Mora binary, credentials, vault data, state,
or generated memories.

Install `mora` separately and make it visible on the client's `PATH`. GUI clients
can inherit a different `PATH` than a terminal; if MCP startup says the `mora`
command is unavailable, fix that executable discovery rather than putting a
machine-specific path in the package.

Read the client's enable screen before installing: a compatible client may start
`mora mcp serve` from `mcp.json`, and retrieved results may be processed by that
client's configured model provider. Prefer `propose` or `readonly` until you are
comfortable with the client's tool-approval UX. Agent skills activate on user
requests; they do not guarantee a session-start brief. The Claude marketplace
wrapper intentionally keeps MCP setup explicit rather than adding an automatic
`.mcp.json` startup declaration.

### Ask Mora what it can do

An agent does not have to guess. One command answers it:

```bash
mora capabilities --json
```

The reply is a single document with these field groups:

| Field | What it holds |
|---|---|
| `mora_version` | The build you are talking to. |
| `commands` | Every command path, with its `kind`, its `platform`, its `json_contract` (`result`, `receipt`, or `exempt`), its `payload` schema name, and, on exempt rows, the `reason` it emits no result document. |
| `connectors` | Every connector Mora can ingest, with its display name, its label, whether it needs a login, whether it is ingesting, whether its items are future-dated, and its feature block. |
| `schemas` | Every published CLI payload schema name and its version. |
| `error_codes` | Every published error code, its class, its connector `error_class` where it has one, its exit code, whether a retry can succeed, and its meaning. |
| `exit_codes` | The allocated process exit codes (1, 2, and 10), the reserved range, and the first code a future release may allocate. |
| `mcp` | The write policy in force on this machine, the 12 MCP tool names, and the payload schema version for each tool. |
| `features` | Top-level support for repair and deep links. |

Support is reported with three words and only three: `supported`, `unsupported`,
and `planned`. Read the word, not the presence of the field.

Repair remains `unsupported`. Deep-link delivery is `supported` at the top
level and for Gmail, Google Calendar, and GitHub; connectors without a stable
provider link remain honestly `unsupported`. Per-connector
`incremental_sync` is `supported` for Gmail, Google Calendar, and filesystem sources. A clean initial
snapshot commits Gmail's History ID or Calendar's sync token in state; later runs
request only provider-reported changes. Expired tokens trigger one bounded full
snapshot and establish a fresh cursor. Filesystem sources persist a size/mtime
manifest and skip unchanged files. Other connectors remain `unsupported`.

Exit codes 3 through 9 are permanently reserved and will never be used, so a
wrapper script can tell a Mora status from one invented by a shell or a test
runner. The next code Mora may allocate is 11.

The document is built from Mora's own registries, so it cannot describe a
command, error code, or schema that the binary does not have.

### MCP write policy

Choose how much authority the agent gets:

```bash
mora config mcp-write-policy open
mora config mcp-write-policy propose
mora config mcp-write-policy readonly
```

- `open` allows `write_memory` and `delete_memory`. It is the current default.
- `propose` stores write proposals in the local config directory. It does not
  add them to the vault before approval. It refuses deletes.
- `readonly` refuses writes and deletes.

Review staged writes locally:

```bash
mora mcp proposals list
mora mcp proposals approve <proposal-id>
mora mcp proposals reject <proposal-id>
```

### Loopback HTTP

A sandboxed browser may not be able to start a local process. It can use the
same Mora tools over HTTP on this computer:

```bash
mora serve http
```

The server binds only to `127.0.0.1:7777`. It creates a bearer token in
`<config_dir>/http.json` with file mode `0600`. Data routes need the token. Mora
checks the Host header, sends no CORS allow header, and leaves `delete_memory`
out of the generic `/call` allowlist.

Print the token without starting the server:

```bash
mora serve http --print-token
```

Choose another loopback port with `--port` or `MORA_PORT`. To keep the server
running as a user service:

```bash
mora serve http install
mora serve http status
mora serve http uninstall
```

`install` uses launchd on macOS, a systemd user service on Linux, and Task
Scheduler on Windows. The status command checks both install state and the
`/healthz` endpoint.

### Claude Code hooks

```bash
mora hook install
mora hook status
mora hook status --json
```

The installer adds two commands to `~/.claude/settings.json`:

- `SessionStart` adds the current brief when a session begins.
- `UserPromptSubmit` adds up to three short related memories for a prompt.

Mora keeps other valid settings and hooks. It refuses to change an unreadable
or invalid JSON settings file. Remove only Mora's managed hooks with:

```bash
mora hook uninstall
```

`mora hook status --json` emits the `mora.hook.status` v1 receipt. Its
`harnesses` field is always an array, including when no hook is installed.

## Daily use

### Brief

```bash
mora brief
mora brief --fresh
mora brief --entity "Sam" --since-days 7
```

The brief shows new or changed items, cited obligations, open local tasks, and
health warnings. `--fresh` rebuilds today's view from the local vault. It does
not sync the network sources. Use a scheduled pulse or run sync first when you
need new source data.

The daily scheduled job writes a dated brief under `briefs/` in the vault:

```bash
mora schedule install pulse-daily
```

If a meeting brief links evidence to the wrong attendee, correct the link:

```bash
mora brief correct --memory-id <id> --attendee <email-or-handle> --confirm
mora brief correct --memory-id <id> --attendee <email-or-handle> --unlink --yes
```

`--confirm` records a positive link. `--unlink` is destructive and needs
`--yes`. Add `--json` for the `mora.brief.correct` v1 receipt: the decision, the
memory id, the source and attendee atoms the ledger keys on, and the entry id.

### Write a memory

Save a note, fact, decision, or insight that you want agents to remember:

```bash
mora write --scope project:acme --type decision --title "OAuth" --text "Use PKCE."
```

Mora stores authored memories as readable Markdown in the vault. They can be
searched, corrected, backed up, and shared. Connector data stays read-only.

`mora write --json` emits the `mora.write` v1 receipt with the saved memory's
id, path, scope, type, and title, plus `index_updated`. A `false` value means
the vault write succeeded but the derived search index needs a later rebuild;
the accompanying human warning stays on stderr.

### Search, read, and think

```bash
mora search "OAuth status" --scope project:acme
mora read <id> --json
mora list --scope project:acme --json
mora context --query "auth" --scope project:acme --budget 6000 --json
mora think "What did Sam decide about pricing?" --json
```

Every one of these emits a document with `schema` and `schema_version`:
`mora.read`, `mora.list`, `mora.search`, `mora.context`, and `mora.think`, all
v1. `mora delete <id> --yes --json` emits `mora.delete` v1.

**Shape change in this release.** `search --json` and `list --json` used to
print a bare JSON array. An array cannot carry the schema envelope, so the rows
now sit under a `memories` key:

```json
{ "schema": "mora.search", "schema_version": 1, "memories": [ … ] }
```

The same move applies to every command that printed a bare array. Old shape →
new shape:

| Command | Was | Now |
|---|---|---|
| `search --json`, `list --json` | `[ … ]` | `{ "memories": [ … ] }` |
| `tasks list --json` | `[ … ]` | `{ "tasks": [ … ] }` |
| `loop list --json` | `[ … ]` | `{ "loops": [ … ] }` |
| `merge list --json`, `teach identity list --json` | `[ … ]` | `{ "pending": [ … ] }` |
| `connectors list --json` | `[ … ]` | `{ "connectors": [ … ] }` |
| `teach examples --json` | `[ … ]` | `{ "examples": [ … ] }` |
| `teach history --json` | `[ … ]` | `{ "entries": [ … ] }` |

Adding a field to any payload is a minor change and never bumps
`schema_version`; removing or renaming one is breaking and does. Read the fields
you know and ignore the rest.

These rules are the same for every command, so they are stated once here rather
than repeated per verb. If you are writing a program against Mora, read
[the machine contract](architecture/22-cli-contracts.md): the envelope, the
stdout/stderr split, the exit codes, the full error-code table, which commands
have no `--json` document and why, and exactly what a pinned consumer may rely
on — with the test that enforces each claim named.

`mora think` makes no model call. It returns cited evidence, coverage gaps, and
a prompt that your agent can use. A result may include
`later_related_evidence`. This means a newer, strongly related record exists.
It does not mean the old record was closed or replaced.

Current-state and open-loop queries also use Mora's typed commitment inventory
when it is available. Owner, direction, due time, lifecycle, and closure fields
are part of the tested brief contract. They are still derived evidence. Read
the citation before you act.

### Meeting prep

```bash
mora brief --event-id calendar_event/abc
mora brief --event-id calendar_event/abc --at 2026-07-10T15:00:00Z
```

The event view shows cited prior context, open items, and source age. `--at`
makes the time-dependent view repeatable for a test or review. MCP clients may
request `meeting_prep` by person name. When no upcoming event matches but a
general next event exists, the response sets `name_fallback: true`; clients
must disclose the fallback instead of presenting it as the named meeting.

### Tasks

```bash
mora tasks add "Reply to Sam about the launch" --pri P0
mora tasks list
mora tasks done "Reply to Sam about the launch"
mora tasks sync --write
```

These tasks live in Mora. They do not change tasks in another service.

`add`, `done`, and `sync` each take `--json` and emit the matching
`mora.tasks.add`, `mora.tasks.done`, or `mora.tasks.sync` v1 receipt. The task
name is the first positional argument, so it can never start with `-`: `mora
tasks add --json` is a usage error, not a task named `--json`.

Two rules follow from that:

- Quote a name that contains spaces. An unquoted extra word is a usage error,
  not a silently shortened name — `mora tasks done alpha beta --json junk` fails
  rather than closing `alpha beta` and dropping `junk`.
- Pass a name that really does start with `-` after `--`: `mora tasks add --
  -urgent` and `mora tasks done --json -- -urgent`.

### Health

```bash
mora sync status
mora doctor
mora doctor --strict
mora doctor --json
mora doctor --pulse
mora doctor --repair --dry-run --json
mora doctor --repair --yes --json
```

`doctor` checks paths, the vault, index, source age, token placement, storage,
backup state, and configured shares. On macOS it also tests protected reads.
`--strict` exits with an error when a critical check fails. `--pulse` checks
freshness and can show a native alert on macOS.

The JSON report separates raw `observed` probe results from typed `diagnosis`.
When Mora cannot prove a cause, it reports `cause_unverified`; it does not infer
a permission problem from connector error prose. `--repair --dry-run --json`
returns the exact safe mutation plan without changing state. Applying that plan
requires `--yes`, records before/after verification for every action, and is
idempotent. Doctor never applies unsafe or destructive repairs.

`mora doctor --json` emits the `mora.doctor.report` v1 receipt — the same report
it always printed, now with `schema` and `schema_version` beside its existing
fields. `mora doctor --pulse --json` emits `mora.doctor.pulse` v1. Neither
command's exit status changed: `--pulse` still exits 2 on an unhealthy report
and `--strict` still exits 1.

Every other machine surface carries the same envelope, including
`mora config --json`, `mora config <key> <value> --json`, the `forget`/`unforget`
receipts, the `teach` verbs, `mora share <verb> --json`, `mora connectors
enable|disable --json`, `mora mcp proposals … --json`, and `mora serve http
status --json`. `mora capabilities --json` lists every published schema name in
its `schemas` array, and its `mcp.schemas` section carries the MCP tool payload
versions — MCP tool results themselves stay unversioned so they do not grow
against the per-call token ceiling.

For agent-facing status checks, `mora version --json` (also `mora --version
--json` and `mora -v --json`) emits the `mora.version` v1 receipt with the
stamped version, commit, build time, Go version, OS, and architecture. `mora
pulse --json` emits the `mora.pulse` v1 receipt with a `sources` array of the
same per-source freshness facts used by Mora's health reporting. Both receipts
include top-level `schema` and `schema_version` fields.

`mora index --json` reports the current `mora.index` v1 status receipt; `mora
index rebuild --json` emits its own `mora.index.rebuild` v1 receipt with the
rebuild subcommand, indexed document count, duration, and resulting index state. `mora sync status --json` emits a
`mora.sync.status` v1 receipt with a deterministic `sources` array. Every
configured source instance appears, including disabled and never-synced
instances, and can be reconciled one-to-one with `mora sources list --json` by
`instance_id`/`name`. Each row keeps the stable instance name in `source` and
`instance_id`, identifies its logical `type` and non-secret account label, and
reports whether it is configured and enabled. It also carries state (`fresh`,
`stale`, `failed`, or `never`), success and attempt timestamps, item and error
counts, and the current typed `error_code` plus free-text `last_error`. Legacy
status files whose configured source was removed remain visible with
`configured: false` instead of being mistaken for current configuration.

Mora shows failed, never-run, or stale sources in briefs and read paths. It
does not turn missing current data into a clean empty answer.

## Corrections and removal

### Teach

Teach records a local human decision and rebuilds the derived view.

```bash
mora teach identity list
mora teach identity confirm --handle <phone> --email <address> --yes
mora teach identity reject --handle <phone> --email <address>
mora teach identity undo <ledger-id>

mora teach commitment not-a-commitment --memory-id <id> --yes
mora teach commitment wrong-person --memory-id <id> --person sam@example.com --yes
mora teach commitment wrong-direction --memory-id <id> --direction owed_by_self --yes
mora teach commitment already-closed --memory-id <id> --yes
mora teach commitment duplicate --memory-id <id> --duplicate-of <commitment-id> --yes

mora teach memory correct --id <id> --title "Corrected" --text "..." --yes
mora teach memory supersede --id <id> --title "Replacement" --text "..." --yes
mora teach memory retract --id <id> --yes
mora teach history --memory-id <id>
mora teach undo <ledger-id>
```

Identity matches are proposals until you confirm them. Connector source files
stay unchanged. Memory revision commands apply only to memories that you wrote.

`mora teach identity confirm --json` and `mora teach identity reject --json`
emit the `mora.teach.identity.confirm` and `mora.teach.identity.reject` v1
receipts with the decided pair, the ledger entry id, the corroborating evidence,
and the affected memory ids. The same command under `mora merge` emits
`mora.merge.confirm` and `mora.merge.reject`. Under `--json` the human review
block is not printed; its facts ride the receipt instead.

`mora teach history --json` emits the `mora.teach.history` v1 receipt. Its
decision log lives under `entries`, and is `[]` when there is no history.

### Delete or forget

`mora delete <id> --yes` removes one local memory. A connector can create it
again on the next sync. Use `forget` for a lasting local block:

```bash
mora forget --chat imessage_chat/<guid> --dry-run
mora forget --chat imessage_chat/<guid> --yes
mora forget --handle +14155550123 --yes
mora forget --email sam@example.com --yes
mora forget list
mora forget list --json
mora unforget <entry-id> --yes
```

Run `--dry-run` first. Forget removes matching local memories and blocks their
return. It never deletes source data from Google, Apple, or GitHub.
`mora forget list --json` emits the `mora.forget.list` v1 receipt with active
entries under `entries`.

## Reviewed retention

Retention is a report-first workflow. The report is read-only and defaults to
memories older than 365 days with a 30-day encrypted local recovery window.
Every candidate needs an explicit decision before execution:

```bash
mora retention report --json
mora retention decide <report-id> <memory-id> --action keep
mora retention decide <report-id> <memory-id> --action change-class --class durable
mora retention decide <report-id> <memory-id> --action compact --summary "Durable conclusion"
mora retention decide <report-id> <memory-id> --action delete
mora retention execute <report-id> --yes --json
mora retention verify --json
mora retention recover <manifest-id> --yes --json
```

Execution refuses incomplete decisions, changed files, or a target set that no
longer matches the reviewed report. Before mutation it writes an AES-GCM
encrypted recovery manifest under Mora's state directory, then rebuilds and
checks the index and graph. Recovery refuses expired manifests and changed
targets. It restores only suppressions created by that retention report.

## Schedules and durable loops

Mora uses launchd on macOS and Task Scheduler on Windows. On Linux it prints a
cron line that you can install.

```bash
mora schedule install ingest-hourly
mora schedule install index-hourly
mora schedule install pulse-daily
mora schedule install doctor-pulse
mora schedule install backup-daily
mora schedule install git-daily
mora schedule install lint-weekly
mora schedule list
mora schedule list --json
```

Run or remove a job with:

```bash
mora schedule run pulse-daily
mora schedule uninstall pulse-daily
```

`mora schedule list --json` emits the `mora.schedule.list` v1 receipt with
schedule entries, cadence, next run, and installed state.

Only `pulse-daily` has a direct `schedule run` command. It uses a durable loop
so a second run for the same day does not advance the brief twice.

### Durable loop commands

A loop is a local lease and result record for automation. Register a loop,
begin one run, send heartbeats while it works, then finish it:

```bash
mora loop register daily-report --cadence daily --command "my-report-command"
mora loop begin daily-report --json
mora loop heartbeat daily-report --run <run-id> --json
mora loop done daily-report --run <run-id> --ok
mora loop done daily-report --run <run-id> --fail "short reason"
```

Inspect one loop or list all loops:

```bash
mora loop status daily-report --json
mora loop list --json
```

The CLI uses `done` for finish and `status` for inspect. A run ID prevents an
old worker from closing a newer run. A crash can leave an uncertain outside
effect. Review the status before you repeat that effect.

`mora loop register --json` emits the `mora.loop.register` v1 receipt with the
stored registration. The loop id is the first positional argument and can never
start with `-`: `mora loop begin --json` is a usage error, not a run of a loop
named `--json`.

## Update

Data refresh and app update are separate.

Refresh data:

```bash
mora sync status
mora sync google
mora sync github
mora sync filesystem
mora sync imessage
mora sync applecalendar
mora reingest --full
```

`reingest --full` rewrites current memories with the latest extraction logic
and rebuilds the graph. Use it after an update that changes extraction.

Choose the automatic-check policy and inspect its local receipt:

```bash
mora upgrade --policy auto    # Mora.app-path default; PR #291 currently checks/notifies only
mora upgrade --policy notify
mora upgrade --policy off     # scheduled checks make zero network or notification calls
mora upgrade --status
mora upgrade --status --json
```

The default is `auto` for a released binary whose resolved executable has the
`Mora.app/Contents/MacOS/mora` path shape, `notify` for another released binary,
and `off` for source/local builds. The status reason is `mora_app_path`: this
stage recognizes layout only and does not claim the app signature was verified.
PR #291's pre-apply stage must verify the real bundle identity before any swap.
The policy and cached status are local. Check receipts live under Mora's state
directory and contain versions, timestamps, and typed outcome codes only—not
GitHub tokens, private paths, source content, or raw error text. A failed check
keeps the last known available version. Update notifications are restrained to
one per version every 72 hours, and a notification failure leaves the cached
warning visible.

`mora schedule install update-daily` installs the daily policy check (including
LaunchServices routing on macOS). `notify` remains check plus notification and
`off` returns before any network, notification, receipt, or lease write. `auto`
can apply only when the running executable resolves inside `Mora.app`, the
installed bundle passes its exact version/architecture/Developer ID/notarization
checks, strict product health passes, the app parent is writable, and those
identity and health observations still pass immediately before swap.

Automatic application downloads the canonical architecture-specific `_app.zip`
and `checksums-app.txt`, verifies the checksum and staged bundle through the
same trust chain as manual whole-app upgrade, and atomically swaps at the same
app path. Launch/version/signature, conditional index-schema rebuild, and strict
health must then pass. A failure rolls the app back; rollback and rebuild
outcomes remain visible in `upgrade --status`. An unwritable app is recorded as
`deferred [app_unwritable]`, falls back to the restrained notification, and is
not retried for the same version. Use the printed Homebrew/manual recovery
command instead. No receipt stores raw errors or recovery paths.

Check or manually update the installed release:

```bash
mora upgrade --check
mora upgrade
```

A signed app install downloads the app ZIP and replaces the full checked
bundle. A standalone install uses the raw release archive. Legacy Homebrew
installs are sent to `brew upgrade`; the new signed-app Cask is not public yet.
Source builds do not self-update.

After update, check:

```bash
mora version
mora doctor
mora sync imessage       # when you use iMessage
mora sync applecalendar  # when you use Apple Calendar
```

## Uninstall

### Signed macOS app

```bash
(
  set -e
  mora_uninstaller="$(mktemp -t mora-uninstall)"
  trap '/bin/rm -f "$mora_uninstaller"' EXIT
  curl -fsSLo "$mora_uninstaller" https://raw.githubusercontent.com/pyranthus-hq/mora/main/uninstall-app.sh
  sh "$mora_uninstaller"
)
```

The script checks that the target is the official signed Mora app. It removes
the app and only PATH links that point to that app. It keeps the vault, config,
state, and any `mora.standalone-backup`. Remove the Full Disk Access entry in
System Settings yourself.

Remove scheduled jobs before deleting the app. The app uninstaller does not
remove them. List every installed job, then uninstall each name shown:

```bash
mora schedule list
mora schedule uninstall <job-name>
mora hook uninstall
mora serve http uninstall
```

### Windows

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/uninstall.ps1 -OutFile $env:TEMP\uninstall-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\uninstall-mora.ps1
```

The script removes the installed program, PATH entry, and Mora scheduled tasks.
It keeps the vault and config unless you pass `-Purge` and confirm.

## Backup and sharing

These commands solve different problems.

### Local backup

```bash
mora backup
mora backup --json
```

This creates a timestamped `.tar.gz` of the vault under
`<state_dir>/backups/`. Mora writes a temporary archive, checks that writing and
closing it succeeded, then publishes it and prints the path. It does not leave
the computer. The archive is plaintext and is not a full config backup. Copy it
to safe storage yourself if you need an off-device copy.
`mora backup --json` emits the `mora.backup` v1 receipt with the archive path,
byte size, and creation time.

Install a daily local archive job with:

```bash
mora schedule install backup-daily
```

### Plaintext git backup

`mora sync git` is a one-way push to a private git remote that you control.

```bash
mora sync git --init --remote git@github.com:you/mora-vault.git
mora sync git
mora schedule install git-daily
```

Or let the authenticated `gh` command create a private GitHub repository:

```bash
mora sync git --init --github
```

Mora does not force-push. It stops on a non-fast-forward error. It ignores the
index, database files, `.DS_Store`, and token directory. It also stops if git
already tracks a protected file.

This path does **not** encrypt the vault. A private repository uses the remote's
access control.

Restore into a new, empty path. Do not clone over your active vault:

```bash
git clone <remote> ~/vault/mora-restored
mora init --vault ~/vault/mora-restored
mora index rebuild
```

Run `mora init --vault` in an interactive terminal. If Mora already points to a
different vault, it shows both paths and asks you to confirm the change. Check
them before you approve it.

### Encrypted sharing

`mora share` sends only memories that you wrote in one scope. It never sends
Gmail, iMessage, calendar, filesystem, or GitHub connector records. Data is
encrypted with age before it goes to a private git remote or user-owned S3/R2
bucket.

The receiver first creates a key:

```bash
mora share keygen
```

The receiver sends the printed public key through a trusted channel. The secret
key stays under `<config_dir>/share/`.

Publish through private git:

```bash
mora share init acme --scope project:acme \
  --recipient age1... \
  --remote git@github.com:you/acme-share.git
mora share preview acme
mora share push acme
```

Subscribe and pull:

```bash
mora share subscribe neil --remote git@github.com:them/acme-share.git
mora share pull neil
mora share list
mora share remove neil --yes
```

Received data has its own index beside the vault. Search results name the
publisher. Received data does not join your person graph or local backup.

### S3 or R2 bucket sharing

Bucket sharing uses the same encryption and scope rules. It adds signed
manifests, content hashes, and a version check. The bucket still sees object
sizes and update times. The bucket locator acts like a read credential, so keep
it private.

Set credentials in the environment. The default prefix is `MORA_SHARE`:

```bash
export MORA_SHARE_ACCESS_KEY_ID=...
export MORA_SHARE_SECRET_ACCESS_KEY=...
```

Standard `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` are fallback names.
Use `--secret-ref TEAM` to read `TEAM_ACCESS_KEY_ID` and
`TEAM_SECRET_ACCESS_KEY` instead.

Publish:

```bash
mora share fingerprint
mora share init acme --scope project:acme --recipient age1... \
  --via r2 --bucket my-bucket --endpoint https://<account>.r2.cloudflarestorage.com \
  --prefix shares/acme
mora share preview acme
mora share push acme
```

Subscribe:

```bash
mora share subscribe neil --via r2 --bucket my-bucket \
  --endpoint https://<account>.r2.cloudflarestorage.com \
  --prefix shares/acme --confirm-pin <publisher-fingerprint>
mora share pull neil
```

Confirm the publisher fingerprint through another trusted channel. A signed
manifest proves who published it only after you confirm the first key. The
publisher gets the value from `mora share fingerprint`; do not copy it from the
bucket or the receiver's first response.

### Sharing storage and cleanup

Mora limits the whole product footprint to 15 GiB by default. A pull that would
cross the limit stops and prints the exact larger value you can choose.

```bash
mora share gc
mora share gc neil
mora share storage-limit 20GiB
```

Garbage collection removes old or incomplete local share generations. It does
not remove the published head. Raising the limit is an explicit local choice;
`mora doctor` still reports 15 GiB as its recommended ceiling.

Removing a subscriber stops future pulls and deletes its local received corpus.
It cannot erase a copy that another person already pulled. Git history may also
keep old encrypted files.

## Settings and environment variables

Show current paths and main settings:

```bash
mora config
```

The default paths are:

| Path | Contents |
| --- | --- |
| `~/vault/mora` | Plain Markdown memories. This is the source of truth. |
| `~/.local/share/mora` | Rebuildable SQLite index and received share data. |
| `~/.local/state/mora` | Sync state, usage log, receipts, and local backups. |
| `~/.config/mora` | Settings, OAuth tokens, HTTP token, and share keys. |

Useful config commands:

```bash
mora config context small
mora config context default
mora config context large
mora config embedder static
mora config embedder ollama
mora config mmr on
mora config mcp-write-policy propose
```

Context profiles set default tool budgets. `small` uses about 3,000 tokens,
`default` uses about 6,000, and `large` uses about 12,000. The large profile
allows an explicit per-call maximum of 50,000 tokens. The small and default
profiles keep a 20,000-token ceiling.

MMR is an optional duplicate-reducing rerank. It works only with the semantic
Ollama embedder.

### Main environment variables

| Variable | Use |
| --- | --- |
| `MORA_VAULT` | Select another absolute vault path for this process. It is not saved to `config.toml`. |
| `MORA_CONFIG_DIR` | Move the whole Mora setup root for an isolated run. Defaults for vault, data, and state move under it. |
| `MORA_GOOGLE_CREDENTIALS` | Path to your Google installed-app OAuth client JSON. |
| `MORA_GITHUB_TOKEN` | Read-only GitHub token. Mora does not save it. |
| `MORA_EMBEDDER` | `static` or `ollama`. A set value overrides config. |
| `MORA_OLLAMA_URL` | Ollama URL. Mora refuses a non-loopback address. |
| `MORA_OLLAMA_MODEL` | Ollama embedding model name. |
| `MORA_PORT` | Port for `mora serve http`. Default is `7777`. |
| `MORA_SHARE_ACCESS_KEY_ID` | Bucket share access key. A custom prefix is allowed. |
| `MORA_SHARE_SECRET_ACCESS_KEY` | Bucket share secret key. A custom prefix is allowed. |
| `DO_NOT_TRACK=1` | Disable local usage logging. |
| `MORA_LOG_QUERIES=1` | Keep raw search query text in the local usage log. Off by default. |
| `NO_COLOR` or `MORA_NO_COLOR` | Disable terminal color. |
| `MORA_NO_BANNER` | Hide the setup banner. |
| `MORA_NO_NOTIFY` | Disable macOS brief and health notifications. |

Installed schedules copy `MORA_GOOGLE_CREDENTIALS`, `MORA_CONFIG_DIR`, and
`MORA_VAULT` when needed because OS jobs do not load your shell profile. The
HTTP service copies `MORA_CONFIG_DIR`, `MORA_VAULT`, and `MORA_PORT`.

Tell Mora about your other email addresses in `config.toml`:

```toml
self_emails = "you@work.com, you@icloud.com"
```

Mora uses these addresses to assign meeting commitments. If it cannot identify
you safely, it does not guess.

### Local usage log

```bash
mora usage report
mora usage report --json
mora usage queries on
mora usage queries off
mora usage off
mora usage on
```

The log stays at `<state_dir>/usage/events.jsonl`. By default it keeps command
names, times, counts, sizes, and timing data. It does not keep query text,
memory text, IDs, excerpts, attachment paths, or vault paths. Query logging is
a separate opt-in. Mora does not send the usage log anywhere.
`mora usage report --json` emits the `mora.usage.report` v1 receipt, including
whether tracking is disabled.

## How search works

The Markdown vault is the source of truth. `index.db` is a local cache that Mora
can rebuild:

```bash
mora index rebuild
```

Search has two routes.

### Default static route

The default static embedder uses:

1. full-text search over each parent memory; and
2. bounded full-text search over message segments inside Gmail and iMessage.

This route is good for exact words, names, issue numbers, and phrases. The
static hash vectors are built for compatibility, but the default route does not
use them as semantic search.

### Semantic route

When a real semantic Ollama embedder is active, Mora combines four ranked
lists:

1. parent full-text search;
2. vector similarity;
3. one-hop person-graph expansion; and
4. Gmail/iMessage message-segment full-text search.

It joins these lists with Reciprocal Rank Fusion, or RRF. RRF scores the
position of a result in each list. This avoids comparing raw full-text and
vector scores as if they had the same scale.

Enable local semantic search with:

```bash
mora config embedder ollama
```

Mora uses `nomic-embed-text` by default. It accepts only a loopback Ollama URL.
If Ollama is not available, Mora warns and falls back to the static route.

### Entity graph

Mora builds people from connector metadata such as sender, recipient,
organizer, attendee, and iMessage contact handle. It also indexes scopes, tags,
and wiki links.

```bash
mora entities
mora entities "Sam"
mora graph
mora graph "Sam"
mora entities --json
mora graph --json
```

Mora uses strict rules for identity merges. Address Book evidence can create a
proposal, but only `mora teach identity confirm` applies it.
The JSON receipts are `mora.entities` v1 and `mora.graph` v1; their arrays now
live under the named `entities` key alongside `schema` and `schema_version`.

## Privacy boundary

The exact boundary is:

- Mora reads enabled sources. It does not write to Gmail, calendars, iMessage,
  files, or GitHub Issues.
- Google uses read-only OAuth scopes. GitHub Issues uses read-only API calls.
- iMessage uses SQLite `mode=ro`. Apple Calendar uses `mode=ro` plus
  `query_only(1)` and reads the live write-ahead log.
- The vault, index, tokens, sync state, and local usage log stay on the computer
  by default.
- Source sync and release update need the network.
- `mora sync git` sends plaintext to the remote you choose.
- `mora share` sends age-encrypted authored notes to the private git or bucket
  target you choose. Cloud storage still sees sizes and timing.
- `mora serve http` is loopback-only, but its bearer token must still be kept
  private.
- Optional Ollama requests are loopback-only.
- A cloud MCP client may send retrieved text to its own model provider. Mora
  cannot control that client's data policy.
- The local vault is plaintext. Protect the disk and your operating-system
  account.

## Troubleshooting

### Results look old

```bash
mora sync status
mora doctor --strict
```

Fix the named source, then run its `mora sync <source>` command. Mora keeps old
indexed data when a sync fails, so a search can work while still being stale.

### iMessage or Apple Calendar is denied

Use the [Full Disk Access steps](#full-disk-access-on-macos). Make sure the
entry is `Mora.app`, not only Terminal or an old standalone file. Reinstall
scheduled jobs after an app migration.

### Search index is missing or dirty

```bash
mora index rebuild
mora doctor
```

The index is a cache. Rebuilding it does not rewrite the source Markdown.

### Google setup uses the placeholder client

Set an installed-app client JSON before connect:

```bash
export MORA_GOOGLE_CREDENTIALS=/absolute/path/to/oauth_client.json
mora connect google
```

If a scheduled Google sync uses this setting, reinstall the schedule while the
variable is exported so the OS job records the path.

### HTTP service is installed but not listening

```bash
mora serve http status
mora doctor
```

Check `<state_dir>/serve-http.err.log`. Make sure `MORA_PORT` is free, then run
`mora serve http install` again.

### Learn more

- [README](../README.md) — short setup and product promise.
- [Architecture](architecture/00-overview.md) — current code contract.
- [Windows details](windows.md) — Windows paths and schedules.
- [Security policy](../SECURITY.md) — private vulnerability reporting.
