<div align="center">

<img src="docs/assets/mora-eye.svg" width="190" alt="Mora, the all-remembering eye"/>

# Mora

**Mora makes the AI agents you use follow the corrections, decisions, and handoffs you have already given them.**

Local, project-scoped rules, decisions, and session handoffs across Claude Code, Codex, and Antigravity — backed by plain Markdown files on your computer, transparent delivery logs (`mora given`), and "save now, review later" governance (`mora review`).

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/pure%20Go-no%20CGO-00ADD8)](go.mod)
[![Corpus](https://img.shields.io/badge/corpus-local%20by%20default-0a3d33)](#privacy-boundary)

</div>

> [!WARNING]
> Mora is alpha software. We use it every day, and its main paths have tests.
> It has not been tested on many other people's real data. Read the cited source
> before you act on a result. A failed or old sync can make the local copy stale.

**Jump to:** [Install](#install) · [60-second quickstart](#60-second-project-quickstart) ·
[The two-sided contract](#the-two-sided-contract) · [Agent write policy](#agent-write-policy) ·
[Personal sources](#personal-sources-and-cross-device-sync) · [Update](#keep-mora-up-to-date) ·
[Privacy](#privacy-boundary)

## Install

### macOS

Install the signed and notarized `Mora.app` and `mora` CLI with [Homebrew](https://brew.sh/):

```sh
brew tap pyranthus-hq/tap
brew install --cask pyranthus-hq/tap/mora
mora version
```

If Mora is already installed, read the [migration steps](docs/homebrew.md#installation-and-updates) before using Brew.

If you do not use Homebrew, install the same signed app directly in `~/Applications/Mora.app`:

```bash
(
  set -e
  mora_installer="$(mktemp -t mora-install)"
  trap '/bin/rm -f "$mora_installer"' EXIT
  curl -fsSLo "$mora_installer" https://raw.githubusercontent.com/pyranthus-hq/mora/main/install-app.sh
  sh "$mora_installer"
)
```

### Linux, Windows, and Go source

Linux and standalone installs:

```bash
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

Windows users can see the [Windows guide](docs/windows.md). To build from source with Go 1.25 or later:

```bash
go install github.com/pyranthus-hq/mora/cmd/mora@latest
```

Source builds report version `dev` and do not self-update. Neither install method sets up connectors, agent hooks, an agent write policy, or a daily update job on its own.

## 60-second project quickstart

### 1. Bind a repository and wire your agents

From your project root:

```bash
mora init
mora hook install --project --scope project:my-app
claude mcp add mora -- mora mcp serve --project-dir "$PWD"
codex mcp add mora -- mora mcp serve --project-dir "$PWD"
```

- `mora hook install --project --scope project:my-app` binds the current folder to `project:my-app` and wires Claude Code lifecycle hooks (`SessionStart`, `UserPromptSubmit`, `SessionEnd`) for this folder only.
- The `--project-dir` in the MCP commands pins that server to one folder, so register it once per project.
- To bind a folder without installing hooks, run `mora project bind --scope project:my-app .`.
- Antigravity hooks are separate and **experimental**: `mora hook install --client antigravity` writes `SessionStart` and `PreInvocation` (no `Stop`) into `~/.gemini/config/hooks.json`. It has not been tested against a real Antigravity install. Under WSL it refuses unless you pass `--hooks-file /mnt/c/Users/<name>/.gemini/config/hooks.json`.

### 2. Record an owner rule and see what your agent gets

```bash
# Save an owner-confirmed decision. It asks "Save this as your own statement? [y/N]"
# and refuses outside your own terminal (a pipe, a script, or an agent session).
mora write --mine --scope project:my-app --type decision \
  --title "Package manager" \
  --text "Use pnpm in this repository, not npm."

# Inspect the exact <2,000-character Project Brief your agent gets at SessionStart
mora brief --project .
```

`--mine` (and `mora review` below) ask a question at a real terminal and refuse inside an agent session or an IDE-agent terminal, so run them in a plain terminal window.

Without `--mine`, `mora write` saves the note but the brief lists it under `Notes, not instructions`, not under `Owner rules`. At a terminal, outside an agent session, it prints one extra stderr line saying so and pointing at `--mine`. Run inside a bound folder, `mora write` defaults to that project's scope and says so on stderr; `--scope` overrides it.

### 3. Inspect delivery, review agent notes, or undo

```bash
mora given                                   # see which notes Mora handed to your agent (with titles)
mora review                                  # keep (confirm as yours) or reject notes agents saved
mora status                                  # folder, health, notes waiting for review, last delivery
mora status --web                            # same, as a read-only page on 127.0.0.1 (URL with a random token is printed)
mora undo <memory-id>                        # remove an agent-written note from the vault
mora teach memory retract --id <id> --yes    # hide an authored memory from retrieval while keeping history
```

### 4. Hand off work across sessions and machines

```bash
mora handoff write --scope project:my-app \
  --title "Auth migration handoff" \
  --direction "Move session cookies to PKCE flow" \
  --why "Legacy token refresh fails on mobile WebViews" \
  --where "PKCE verifier merged; callback handler tests passing" \
  --next "Wire token exchange in internal/auth/callback.go"

mora handoff show --scope project:my-app   # bare `mora handoff` shows the latest too
mora handoff list
```

A `handoff` is a vault memory (`type: handoff`) that carries direction, why, key turns, where the work stands, and the next step into the next session's Project Brief, and across machines if you sync the vault. Other flags: `--project DIR`, `--turns`, `--text`, `--short`, `--json`. Add `--mine` to save it owner-confirmed (it asks at your terminal). Without `--mine`, and for any handoff an agent writes, it is unconfirmed and the brief labels it `Last handoff (unconfirmed)`. The brief shows the newest handoff when you confirmed it. If the newest is unconfirmed, the brief shows your newest confirmed one from the last 14 days instead, with a line saying a newer unconfirmed handoff exists (`mora handoff show` prints it, escaped, and says whether it is confirmed). With no recent confirmed handoff it shows the newest, labelled unconfirmed; and if that one fails the write checks it says only that the newest handoff is withheld, never the text. When a session ends without a handoff, the `SessionEnd` hook records the last five user requests in that bound folder as a crash-safe fallback.

## The two-sided contract

<p align="center">
  <img src="docs/assets/architecture.svg" width="760" alt="Read-only sources and project rules flow into a local Markdown vault and SQLite index, then into any MCP client. Backup and sharing are optional network paths."/>
</p>

### For your agents

1. **Project Brief at `SessionStart` (`<2,000` characters):** When a session starts in a bound project folder, Mora injects two separated blocks instead of a general inbox digest:
   - `Owner rules for project:NAME`: confirmed corrections and decisions from `project:NAME` and `global`, ordered project-first and newest-first.
   - `Notes, not instructions`: up to three recent project decisions you have not confirmed (agent notes, and anything saved without `--mine`), the count of notes waiting for review, and the latest session `handoff`.
   - Notes from `personal` or other `project:*` scopes never appear on their own in a bound project.
2. **Scoped recall on prompts (`UserPromptSubmit` / Antigravity `PreInvocation`):** Searches only the bound project scope and `global` using stemmed full-text search (`porter unicode61`) and metadata reranking, adding up to three short matches within 0.7 seconds. A note is added only if it shares two content words with the prompt, or one word that is in its title or rare in the index. Tentative notes are never added. It skips slash commands and prompts under 12 characters.
3. **Project-aware MCP tools (`mora mcp serve --project-dir "$PWD"`):** Exposes `brief`, `search_memory`, `context_memory`, `read_memory`, `write_memory`, `list_memory`, `think`, `calendar_events`, `list_entities`, `get_entity`, `digest`, `meeting_prep`, and `delete_memory`. With `--project-dir`, `write_memory` defaults an omitted scope to the bound project and `brief` returns the project brief.
4. **Opt-in Bash warning rules (`mora rules`):** Checks Bash tool calls in Claude Code against regex patterns you approved in your own terminal:
   ```bash
   mora rules add --id no-sudo --match '^\s*sudo\b' --message "Do not use sudo here."
   mora rules approve no-sudo --mode warn   # run in your own terminal
   mora hook install --pre-tool
   ```
   When a command matches, the warning returns with the command result and changes what the agent does next. Today this is opt-in, covers Bash calls in Claude Code on one device, and warns without blocking. Rules drafted from your corrections, and Codex warnings, are not built. See [Bash warning rules](docs/guide.md#bash-warning-rules).

### For you (transparency and control)

- **See everything handed to an agent (`mora given`):** Every time a hook or MCP tool hands notes to an agent, Mora appends the timestamp, surface (`hook-session-start`, `hook-recall`, `mcp:<tool>`), caller, scope, and note IDs/titles to `given.log`. It says "given", never "used": Mora cannot prove whether the agent followed the note.
- **See status (`mora status`, `mora status --web`, macOS `Mora Status.app`):** `mora status --web` serves a read-only page on `127.0.0.1` behind a random per-run URL token printed at start. It shows health, notes waiting for review, and the last delivery; it has no Keep/Reject buttons. Reviewing (keep or reject agent notes) happens with `mora review` in a terminal.
- **Plain Markdown on disk (`~/vault/mora`):** Every memory is a readable `.md` file with YAML frontmatter. No AI model is called when Mora saves or searches.
- **Read-only command repeat check (`mora check`):** Counts rejected or interrupted Bash commands in local Claude Code and Codex transcripts and compares repeat rates against a baseline of unrejected commands, making no network connection.

What Mora does **not** do:

- It does not call an AI model when it saves or searches. An optional local Ollama model on loopback can add vector search.
- It does not record full conversations into your vault. Only explicit writes and bounded session handoffs are stored.
- It does not train, own, or fine-tune a model, or "learn you" over time. We are testing a small local model that checks agent actions against your own rules; it is not built yet.

## Agent write policy

This policy governs the MCP `write_memory` and `delete_memory` tools, the loopback HTTP `/call` route, and paired companion captures. It does not gate `mora write`, connectors, tasks, or `mora teach` in your terminal:

| How it is saved | Waits for your approval? |
| --- | --- |
| `mora write` in Terminal | No. Saved at once (`--mine` marks it owner-confirmed after you confirm in your terminal). |
| `mora handoff write` | No. Saved at once as `type: handoff` (`--mine` marks it owner-confirmed). |
| Connectors (files, mail, calendar, messages, GitHub) | No. Saved at once as read-only source evidence. |
| `mora teach ... --yes` corrections | No. Saved at once, and reversible with `mora teach undo`. |
| An agent's `write_memory` call over MCP | **Default (`tentative`):** low-risk notes save at once with a `mora-tentative` tag (`save now, review later` via `mora review`). High-risk notes wait in the proposal queue. Under `propose`, all agent writes wait for approval. |
| Lifecycle hooks (`SessionStart`, `UserPromptSubmit`, `SessionEnd`) | Hooks never write vault memories (`SessionEnd` updates only the per-project handoff state). |

### How `tentative` ("save now, review later") works

By default (`mcp_write_policy = "tentative"`), an agent's `write_memory` call is tentative: it saves immediately with a `mora-tentative` tag when its type is `insight`, `fact`, `decision`, `task`, `correction`, or `handoff` and its text passes Mora's deterministic write checks. It stays tentative until you review it; nothing an agent writes becomes owner-confirmed on its own. Ordinary coding rules such as "always use pnpm" save as tentative. A write is held in the proposal queue (`held_because`) when:

- its type is not one of those; or
- its text matches a credential/secret pattern; contains terminal control characters; contains invisible characters that hide or reorder text (bidirectional overrides, tag characters, zero-width spaces, word joiners, blank fillers) or three or more invisible characters in a row (ordinary emoji, joined emoji, and zero-width joiners in other scripts are fine); matches a short sensitive-word list (medical, passport, and similar); or talks to agents: tells them to auto-approve or accept all proposals or tool calls, to proceed without review or confirmation, to hide something from "the user" or "the owner", to ignore previous instructions, or pipes a download into a shell or interpreter. Ordinary rules such as "never ignore errors" or "always merge with squash" are not held.

A tentative memory appears in `search`, `read`, `list`, and `context` with `"trust":"tentative"`, and tentative decisions and handoffs appear in the Project Brief's `Notes, not instructions` section. They do not enter `Owner rules`, the prompt recall hook, the daily brief, `think`, or `mora share` until confirmed, and a tentative `correction` does not change how other notes are shown.

Run `mora review` in your own terminal to step through saved tentative notes and held proposals (`[k]eep [r]eject [s]kip [q]uit`). Keeping a note removes `mora-tentative` and binds `"trust":"owner-confirmed"` to the exact fields shown. You can also mark a note owner-confirmed in your terminal with `mora write --mine` or `mora confirm <id> --mine`. Plain `mora confirm <id>` only removes the tentative tag; the note becomes ordinary, not owner-confirmed.

**Honest limits:**

- The deterministic hold filter is coarse. It does not catch a false claim written in plain words.
- An agent that can run shell commands can invoke `mora write`, `mora confirm`, or `mora mcp proposals approve` directly, so MCP write policy is not a sandbox against a shell-capable agent.
- `mora review` and `--mine` check stdin and known Claude Code / Codex session variables, not every IDE harness, and a process using a pseudo-terminal or writing vault files directly can forge the owner-confirmed tag. It is a review record, not a cryptographic signature.
- `mora teach memory retract` and `mora teach undo` hide a memory from current retrieval and keep history; `mora undo <id>` deletes an agent-written file and logs the undo. Neither erases text from agents or shared peers that already read it.

### Switching policy (`tentative`, `propose`, `open`, `readonly`)

```bash
mora config mcp-write-policy tentative # default: save low-risk notes with a tag; review later
mora config mcp-write-policy propose   # agents can only propose through MCP; writes wait for approval
mora config mcp-write-policy open      # agent writes save at once; MCP deletes allowed
mora config mcp-write-policy readonly  # agent writes and deletes refused
```

To inspect and decide held proposals directly:

```bash
mora mcp proposals list
mora mcp proposals show <proposal-id>
mora mcp proposals approve <proposal-id>
mora mcp proposals reject <proposal-id>
```

To give a specific local or remote agent its own scope and tool limits, use `mora agents add <name> --read-scopes project:my-app,global` and `mora mcp serve --profile <name>`. See [Agent profiles](docs/guide.md#agent-profiles).

## Personal sources and cross-device sync

Beyond project rules and handoffs, Mora can index your folders, mail, messages, calendars, and GitHub Issues into the same local vault so you or an agent you authorize can search them in one place.

### Add read-only sources

```bash
mora connect filesystem ~/Documents/notes   # local Markdown, text, PDF, and Word files
mora connect google                         # Gmail and Google Calendar (readonly OAuth)
mora connect github --repo owner/repository # GitHub Issues
mora connect imessage                       # macOS; needs Full Disk Access for Mora.app
mora connectors enable whatsapp             # macOS; local WhatsApp Desktop store
mora ingest run --source whatsapp
mora connectors enable applecalendar        # macOS; needs Full Disk Access for Mora.app
mora ingest run --source applecalendar
```

Mora only reads these sources; it never sends, edits, or deletes mail, messages, events, or issues. For macOS `imessage`, `whatsapp`, and `applecalendar`, grant **Full Disk Access** to `/Applications/Mora.app` (Homebrew) or `~/Applications/Mora.app` (direct install) in **System Settings > Privacy & Security > Full Disk Access**, then run `mora doctor`. See the [guide](docs/guide.md#full-disk-access-on-macos).

### Daily personal commands

```bash
mora brief                                      # daily digest of changes, commitments, and health
mora search "What is open with Sam?"            # hybrid search across sources and notes
mora think "What did we decide about pricing?"  # cited evidence plus coverage gaps (no LLM call)
mora brief --event-id calendar_event/abc        # cited meeting prep
mora tasks list                                 # open local tasks
mora doctor                                     # source, index, and configuration health
```

### Backup and sharing are separate choices

Nothing is uploaded by default. When you want off-device backup or cross-machine sync, choose the path that matches your trust boundary:

| Path | Leaves this computer? | Encryption added by Mora? | What it moves |
| --- | --- | --- | --- |
| `mora backup` | No | No | Writes a local `.tar.gz` snapshot in Mora's state directory. |
| `mora sync git` | Yes, if the remote is off-device | No | Pushes the plaintext vault (including tentative notes and handoffs) to a private Git remote you control. |
| `mora share` | Yes | Yes, with `age` | Publishes only confirmed authored memories from one chosen scope to a Git remote or S3/R2 bucket you control. |

See [Backup and sharing](docs/guide.md#backup-and-sharing) before enabling any network path.

## Keep Mora up to date

| Installed with | Update command |
| --- | --- |
| Homebrew Cask | `brew update && brew upgrade --cask pyranthus-hq/tap/mora` |
| Direct signed-app installer | `mora upgrade` |
| Go source build | Run `go install` again. Source builds do not self-update. |

To enable daily release checks for a signed app:

```bash
mora upgrade --policy auto
mora schedule install update-daily
mora upgrade --status
```

After any update, run `mora version` and `mora doctor`.

## Uninstall

```bash
mora schedule list                 # on WSL this prints "cron listing not implemented"; remove your own cron line instead
mora schedule uninstall <each-job-name-shown>
mora hook uninstall
mora serve http uninstall
```

Then run `brew uninstall --cask pyranthus-hq/tap/mora` for a Homebrew install, or the checked `uninstall-app.sh` script in the [guide](docs/guide.md#uninstall) for a direct install. Both leave your vault data in place unless you explicitly purge it.

## Data layout

| Path | Holds | Can Mora rebuild it? |
| --- | --- | --- |
| `~/vault/mora` | Markdown memories, rules, and handoffs | No. Back it up. |
| `~/.local/share/mora` | SQLite FTS5/vector index and received shares | Yes, except received share data must be pulled again. |
| `~/.local/state/mora` | Sync state, `given.log`, usage log, local backups | Usually. |
| `~/.config/mora` | `config.toml`, `projects.json`, OAuth tokens, share keys | No. Reconnect or restore it. |

Run `mora config` to see active paths. `MORA_VAULT` selects another absolute vault path for one process without changing `config.toml`.

## Privacy boundary

- **Read-only connectors:** Google uses `gmail.readonly` and `calendar.readonly`. iMessage uses `mode=ro`. Apple Calendar uses `mode=ro` plus SQLite `query_only(1)`.
- **Local by default:** The vault, SQLite index, tokens, `given.log`, sync state, and usage log stay on this machine unless you configure a connector, release check, backup remote, share remote, or cloud agent tunnel.
- **Network paths move data by design:** Source connectors talk to their APIs, `mora upgrade` checks GitHub Releases, `mora sync git` and `mora share` upload to remotes you configure, and any cloud or local agent that reads a Mora result processes that result under its own model provider's data rules.
- **Zero model calls inside Mora:** Saving, indexing, searching, `mora brief`, `mora think`, and `mora check` make no LLM calls. Optional Ollama embeddings are restricted to a loopback address (`127.0.0.1` / `localhost`).
- **Local usage log:** Omits query text by default. Disable it with `mora usage off` or `DO_NOT_TRACK=1`.
- **Plaintext vault:** Files in `~/vault/mora` are plaintext Markdown on disk. Use FileVault, BitLocker, or Linux disk encryption.

## Search and proof

With the default static embedder, Mora searches parent memories and bounded Gmail/iMessage message segments with stemmed SQLite FTS5 (`porter unicode61`; title and tag hits rank ahead of body hits) and metadata reranking so confirmed project corrections and decisions rank above long source files. The query ORs its content words (common words are dropped) and ranks the best matches first. An index built by an older version gets stemming after `mora index rebuild --if-needed`. With an active loopback Ollama embedder, it joins full-text, vector, person-graph, and message-segment lists using Reciprocal Rank Fusion (RRF).

Mora's frozen evaluation corpora start from a written answer key, pin every byte, run the real brief, search, and meeting code, and verify citations and commitment fields. The fixtures and scores live in [`internal/mora/eval/`](internal/mora/eval/) and [evaluation and testing](docs/architecture/09-eval-and-testing.md).

## More

- [Guide](docs/guide.md): full CLI reference, project bindings, hooks, connectors, and upkeep.
- [Agent skills](plugins/mora/README.md): portable Agent Plugins 1.0 package (`mora-recall`, `memory-capture`, `daily-brief-loop`).
- [Architecture](docs/architecture/00-overview.md): current design and code map.
- [Contributing](CONTRIBUTING.md): build and review rules.
- [Security](SECURITY.md): how to report a security issue. Never paste vault data or credentials into a public issue.

---

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory.</sub>
</div>
