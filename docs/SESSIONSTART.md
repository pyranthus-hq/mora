# Make the brief your session-start default

Mora's whole point is that an agent **does not start cold**. This guide wires the
daily *what-changed / what-matters* brief into the start of every agent session —
one command, one hook, or one MCP tool call — so Claude Code, Codex, or any
MCP-capable agent opens each session already holding your context.

Everything here is **local-only and zero-egress**: `mora brief` makes **no network
call**. It reads the brief the scheduled job already wrote, or generates one on the
spot from memories already on disk. Nothing leaves your machine.

> New to Mora? Run the [Quick start](../README.md#quick-start) first
> (`mora init` → `mora connect google` / `mora connect imessage`). This guide
> assumes the vault exists and at least one source is connected.

---

## What this gives you

The latest brief, loaded automatically at the start of every agent session:

- **What changed** since you last looked — the Phase-12 delta watermark surfaces
  only new-or-updated emails, texts, and calendar items, not a fixed re-dump.
- **What matters most, first** — salience ordering leads with your most-salient
  real threads and people, so the budget never gets eaten by noise.
- **Cited and budget-bounded** — every item carries a stable id the agent can
  `read_memory` to verify; the payload is capped well under the MCP token redline.
- **Optionally, a synthesis prompt** — opt into the envelope and the agent gets a
  grounded, cite-by-id instruction to compose a brief in its *own* model. Mora
  itself runs no model and holds no API key.

The result: instead of re-establishing context every session, the agent answers
from your real mail, messages, and calendar from the first message.

---

## The habit loop (Phase 12 → 16)

The brief is one coherent habit made of **two halves** — a scheduled *write* side
and a session-start *read* side — built over five phases:

```
   ┌─────────────────────── the WRITE side (scheduled, once a day) ───────────────────────┐
   │                                                                                       │
   │  Phase 12  delta watermark    →  Phase 13  dated artifact   →  Phase 14  salience     │
   │  "what changed since last        + scheduled pulse-daily       "what matters most     │
   │   time" (content-hash, not        writes briefs/<date>-          first" (orders the    │
   │   timestamps)                     brief.md every morning        section before cap)   │
   │                                                                                       │
   └───────────────────────────────────────────────────────────────────────┬──────────────┘
                                                                            │ persists today's brief
   ┌──────────────────────── the READ side (every session start) ──────────▼──────────────┐
   │                                                                                       │
   │  Phase 16  `mora brief` / the SessionStart hook / the MCP `brief` tool                │
   │  reads today's persisted brief VERBATIM (or generates one on demand if none exists    │
   │  yet) — optionally with the Phase-15 synthesis envelope. Local-only, read-only,       │
   │  never advances the watermark.                                                        │
   │                                                                                       │
   └───────────────────────────────────────────────────────────────────────────────────────┘
```

| Phase | Piece | What it contributes |
|---|---|---|
| 12 | delta watermark | the *what-changed-since-last-time* set (content-hash diff, not a time window) |
| 13 | dated brief artifact + `pulse-daily` | the scheduled job writes `~/vault/mora/briefs/<date>-brief.md` each day |
| 14 | salience ordering | leads with the most-salient thread/person; high-salience items survive the budget cut |
| 15 | synthesis envelope (opt-in) | a cited, model-free `synthesis_prompt` the agent runs in its own model |
| 16 | `mora brief` / SessionStart | reads today's brief (or generates on demand) at session start — the default |

So a fresh install that installs the scheduled job **and** wires the session-start
pull gets the habit by default: the cron writes the brief each morning; the hook /
tool reads it when a session opens; and if no brief exists yet (first run before
the cron has fired), `mora brief` generates one on the spot.

### Install the scheduled write side

```bash
mora schedule install pulse-daily
```

This installs a `launchd` job that each day syncs your already-enabled, read-only
sources, computes the delta, salience-orders it, and persists
`~/vault/mora/briefs/<UTC-date>-brief.md` (and posts a macOS toast). It is the
*write* half — the only surface that advances the Phase-12 watermark. See
`mora schedule list` for the installed jobs.

---

## Wire it into Claude Code

Claude Code runs `hooks.SessionStart` commands at the start of each session and
injects their stdout as context. Add a `SessionStart` hook to
`~/.claude/settings.json` that runs `mora brief`:

```jsonc
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "mora brief" }
        ]
      }
    ]
  }
}
```

What it does: at session start, Claude Code runs `mora brief`, which reads today's
persisted brief (or generates one locally if none exists yet) and injects it as
session context. The brief lives at `~/vault/mora/briefs/<date>-brief.md`; the hook
just surfaces it. Add `--envelope` to the command
(`"command": "mora brief --envelope"`) to also inject a grounded synthesis prompt.

If you already have a `hooks` block, add the `SessionStart` entry alongside your
existing hooks rather than replacing the block.

---

## Wire it into Codex

Codex picks up Mora the same two ways:

- **As an MCP server (recommended).** Register `mora mcp serve` as an MCP server in
  your Codex config, then instruct the agent to **call the `brief` tool first** at
  the start of a session. The MCP server's own instructions already nudge this — see
  *Wire it into any MCP agent* below.
- **As a startup command.** If your Codex setup supports a session-start / startup
  command, point it at `mora brief` (optionally `mora brief --envelope`) exactly as
  the Claude Code hook does — the stdout becomes the session's opening context.

Either path is local-only: the MCP `brief` tool and the `mora brief` command both
read or generate from the local vault and make no network call.

---

## Wire it into any MCP agent (Desktop, etc.)

For any MCP-capable agent (Claude Desktop, Codex-as-MCP, your own client):

1. Serve Mora over stdio:

   ```bash
   mora mcp serve
   ```

2. At the start of a session, have the agent **call the `brief` tool once** — a
   single tool call that returns the same budgeted, cited, source-grouped brief,
   resolved to the freshest available. The MCP server's instructions already tell
   the agent to *"Call `brief` at the START of a session … before doing anything
   else."*

The `brief` tool takes two optional args:

- `max_tokens` — approximate token budget (default ~6000, max ~20000). The payload
  is budgeted to stay under the 20000-token MCP redline regardless.
- `envelope` — opt-in `true` to also get a `synthesis_prompt` for composing a
  grounded, cited brief. Default `false`; Mora makes no model call either way.

---

## Consent & opt-out

The session-start wiring is **docs + examples only**. Mora does **not** auto-edit
your `~/.claude/settings.json`, your Codex config, or anything else — there is no
code path in Mora that writes your agent config. **You** paste the snippet.

- **To opt in:** add the hook / startup command / call the `brief` tool, as above.
- **To opt out:** simply don't add the hook (or remove it). There is nothing to
  disable because nothing was auto-enabled.
- The existing controls still apply: disable a source with
  `mora connectors disable <type>` so it never feeds the brief, and set
  `DO_NOT_TRACK=1` (or `mora usage off`) to disable Mora's local-only usage log.

---

## Zero-egress guarantee

`mora brief` and the MCP `brief` tool make **no network call**. They either read the
brief the scheduled job already persisted, or generate one from memories already
ingested into your local vault. They never sync, never fetch, and never advance the
delta watermark — they are strictly read-only.

The **only** thing that reaches the network is the scheduled `pulse-daily` job, and
only when it syncs — over your already-enabled, **read-only** sources (Gmail /
Calendar read-only scopes, local iMessage `chat.db`). Nothing leaves your machine
beyond those consented, read-only pulls.

---

## Related

- [Quick start](../README.md#quick-start) — install, connect sources, serve MCP
- [synthesis: think & digest](architecture/07-synthesis-think-digest.md) — the
  `mora brief` read-or-generate kernel + the MCP `brief` tool, cited
- [CLI & UX](architecture/08-cli-and-ux.md) — the `mora brief` command on the
  dispatch surface, the byte-clean invariant
- [sync & freshness](architecture/11-sync-and-freshness.md) — the watermark store,
  the `pulse-daily` launchd job
