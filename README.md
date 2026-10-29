<div align="center">

<img src="docs/assets/mora-eye.svg" width="150" alt="Mora"/>

# Mora

<!-- D2: hero line pending Adit -->
**You fix your AI agent. It forgets. Mora keeps the fix.**

Mora saves the corrections you give your AI coding agents as plain files on your Mac.
Every fix has a history and an undo. One setting makes writes through Mora's agent tools wait for your approval.

[![CI](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml/badge.svg)](https://github.com/pyranthus-hq/mora/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pyranthus-hq/mora?color=2fbf9a)](https://github.com/pyranthus-hq/mora/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

</div>

```text
# Save a fact
$ mora write --scope project:demo --type decision --title "Deploy target" --text "We deploy the API to Fly.io in the iad region."
mem_20261028_043936_69e92049    project:demo    Deploy target

# Correct it. Mora keeps both versions.
$ mora teach memory correct --id mem_20261028_043936_69e92049 --title "Deploy target" --text "We moved the API to Render in Ohio on Oct 20." --yes
correct mem_20261028_043936_69e92049 with revision mem_20261028_043937_c05c4d32 (entry gov_20261028_043937_6d7a1060)
reverse with `mora teach undo gov_20261028_043937_6d7a1060`

# Change your mind. One command puts it back.
$ mora teach undo gov_20261028_043937_6d7a1060
undid Teach decision gov_20261028_043937_6d7a1060

$ mora teach history --memory-id mem_20261028_043936_69e92049
gov_20261028_043937_6d7a1060  undone  correct  target=mem_20261028_043936_69e92049  2026-10-28T04:39:37-04:00
```

The `#` lines are notes. The rest is real output from a fresh test vault.

## Install

You need a Mac and [Homebrew](https://brew.sh/).

```sh
brew tap pyranthus-hq/tap
brew install --cask pyranthus-hq/tap/mora
mora init
mora config mcp-write-policy propose
```

The last line makes writes through Mora's agent tools wait for your approval.
From v0.15.3 this is the default, unless you set the policy to `open`. Running the line makes it explicit.
It does not cover the `mora` command itself. An agent that can run Terminal commands can still run `mora write`.
If `mora init` shows a menu, choose no connector for now.

Mora has no window and no Dock icon. It is a command in Terminal.
Other ways to install: [install guide](docs/guide.md#install) (direct installer, Linux, Windows, build from source).

## Try it in 60 seconds

```sh
mora write --scope project:demo --type decision --title "Test command" \
  --text "Run go test -race ./... before every commit."
mora search test --scope project:demo       # lists id, scope and title
mora context --query test --scope project:demo   # shows the saved text and where it came from
```

Default search matches whole words. "dentists" will not find "dentist". Search prints titles only.
Use `mora context --query <word>` to read the text.

Now let Claude Code see your fixes:

```sh
mora hook install    # Claude Code: adds matching memories to each prompt you send
```

Open a new Claude Code session and ask: "What is our test command?"
The prompt must share a word with the saved fix. The hook adds memories from every scope, not only the current project.

Other agents, such as Codex, can call Mora's tools. The agent decides when to look.

```sh
claude mcp add mora -s user -- mora mcp serve    # Claude Code tools
codex mcp add mora -- mora mcp serve             # Codex tools
```

<!-- VERIFY (Oct 29): the hook route passed a sandbox test (2026-10-28, Claude Code, haiku model) with a
     different rule and prompt. This exact prompt, a clean Mac and the tools-only (MCP) route for Codex are
     not tested. Run it on a second macOS user account. If the agent does not use the saved fix, cut the
     "Open a new Claude Code session" sentence. -->

## Agents propose. You approve.

With the approval setting on, an agent that saves a memory through Mora's agent tools creates a proposal.
Nothing from those tools enters your memory until you say yes.

```text
$ mora mcp proposals list
p_mem_20261028_043937_6c52e01e  2026-10-28T04:39:37-04:00  [project:demo/fact] Test command

$ mora mcp proposals approve p_mem_20261028_043937_6c52e01e
Approved p_mem_20261028_043937_6c52e01e as memory mem_20261028_043937_fd0ca867.
```

In this mode an agent also cannot delete through those tools. To change the mode, see [write policy](docs/guide.md#mcp-write-policy).
The setting does not limit the `mora` command. If your agent can run Terminal commands, it can run `mora write` or approve a proposal itself.

## What works today

- Plain Markdown files in `~/vault/mora`. You can read them, search them and back them up.
- Save a fact, correct it, see its history, undo it.
- Approval step for every memory an agent writes through Mora's agent tools.
- Works with Claude Code and Codex through MCP (a standard way for an agent to call local tools). The agent decides when to call Mora.
- Claude Code hooks: `mora hook install` adds your brief at session start and related memories to each prompt.
- Encrypted sharing of one scope with other people (`mora share`).
- Search over files, Gmail, Google Calendar, iMessage, WhatsApp, Apple Calendar and GitHub Issues.
  These are optional. Setup is in the [guide](docs/guide.md#connect-data).

## What comes next

These are not in the public build.

- `mora checkup`: reads your agent logs and shows which of your written rules the agents broke. A preview. Under test.
- Fixes shared across a team.

## Safe and local

- Mora runs on your Mac. It does not call an AI model by default.
- Your memories, index and settings stay on your computer. Mora uses the network only for source APIs
  (if you connect them), release checks, and a backup or share target that you pick.
- Connectors read your mail, messages and calendars. They never change the originals.
- The vault is plain text. Turn on FileVault, and do not put the vault in an unencrypted remote.
- A cloud agent is a separate boundary. After it reads a Mora result, its provider rules apply.
- Mora records a local usage log without your query text. Turn it off with `mora usage off` or `DO_NOT_TRACK=1`.

Full list: [privacy boundary](docs/guide.md#privacy-boundary).

## Known limits

Mora is alpha software. We use it every day. We have not yet measured how it works for other people on their own data.

- Default search matches whole words. It does not match plurals. Meaning-based search is an opt-in setting.
- The Claude Code hook adds at most 3 matching memories to each prompt. In a large vault, a fix may not rank in the top 3.
- `mora share` does not scan for secrets. Do not write secrets into a shared scope.
- Read the cited source before you act on a result. A failed or old sync can leave the local copy stale.

## Read more

| I want to | Go to |
| --- | --- |
| Install another way, or on Linux or Windows | [Install](docs/guide.md#install) |
| Let Mora read iMessage, WhatsApp or Apple Calendar | [Full Disk Access](docs/guide.md#full-disk-access-on-macos) |
| Connect Gmail, Calendar, files or GitHub | [Connect data](docs/guide.md#connect-data) |
| Set daily jobs and updates | [Schedules](docs/guide.md#schedules-and-durable-loops), [Update](docs/guide.md#update) |
| Fix or remove a memory | [Corrections and removal](docs/guide.md#corrections-and-removal) |
| Back up or share | [Backup and sharing](docs/guide.md#backup-and-sharing) |
| Use Mora from a browser tool | [Loopback HTTP](docs/guide.md#loopback-http) |
| Uninstall | [Uninstall](docs/guide.md#uninstall) |
| See how search is tested | [Evaluation and testing](docs/architecture/09-eval-and-testing.md) |
| Understand the design | [Architecture](docs/architecture/00-overview.md) |
| Contribute | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Report a security problem | [SECURITY.md](SECURITY.md). Never paste vault data or credentials into a public issue. |

Want an agent to set Mora up for you? Use the prompt in the [guide](docs/guide.md#ask-an-agent-to-set-it-up).

<div align="center">
<sub>Named for <strong>Hermaeus Mora</strong>, keeper of knowledge and memory. Apache-2.0. Built by Pyranthus.</sub>
</div>
