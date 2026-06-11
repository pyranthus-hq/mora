# Mora — 2-minute quickstart

Mora is a **local-first memory CLI** for AI agents. It indexes your notes, files,
Gmail/Calendar, and **iMessage** into a searchable Markdown vault and serves it to
Claude Code / Codex over MCP — nothing leaves your machine.

## 1. Install

One line — fetches the right release binary and handles macOS Gatekeeper:

```sh
curl -fsSL https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.sh | sh
```

Or, from a downloaded release tarball:

```sh
tar -xzf mora_0.6.0_darwin_arm64.tar.gz   # _amd64 on Intel Macs
./install.sh
```

`install.sh` clears the quarantine flag, ad-hoc-signs the binary, drops it on your
PATH, and initializes the vault at `~/vault/mora`. Confirm:

```sh
mora version
mora doctor
```

## 2. Put something in memory

```sh
# write a note by hand
mora write --scope project:demo --type note --title "Kickoff" --text "Met Neil; agreed to pilot Mora."

# or ingest a folder of docs/notes
mora connectors enable filesystem
mora sources add filesystem --name notes --path ~/Documents/notes --scope personal
mora ingest run --source notes
```

### iMessage (macOS, optional, the showpiece)

```sh
mora connectors enable imessage
mora doctor            # tells you if Full Disk Access is needed
# Grant Full Disk Access to your terminal:
#   System Settings → Privacy & Security → Full Disk Access → enable your terminal
mora sync imessage     # read-only ingest of your local Messages
```

## 3. Search it

```sh
mora search "Neil"
mora context --query "what did I agree with Neil" --budget 1500
```

## 4. Give it to your agent (the point)

Wire Mora into Claude Code / Codex **once**:

```sh
claude mcp add mora -s user -- mora mcp serve
codex  mcp add mora -- mora mcp serve
```

Then ask your agent, cold:

> "Search my memory — what did I last discuss with Neil?"

The agent answers from your local vault. No copy-paste, no cloud.

---

Tokens, messages, and emails never leave your machine. Disable usage logging with
`mora usage off` or `export DO_NOT_TRACK=1`.
