# Keep Mora active across agent sessions

Connecting `mora mcp serve` gives an agent tools. A short global instruction
makes the intended behavior explicit when a new session starts: read the brief,
retrieve before guessing about the user's history, and save durable outcomes.

There is no portable `system.md` shared by every agent. Use the harness's global
user instruction file:

| Agent | Global user instruction file |
| --- | --- |
| Codex | `~/.codex/AGENTS.md` |
| Claude Code | `~/.claude/CLAUDE.md` |
| Gemini CLI | `~/.gemini/GEMINI.md` |

Project `AGENTS.md` and `CLAUDE.md` files are for project-specific rules. Do not
put this block inside a generated or projected section.

## Ready-to-paste continuity block

```markdown
<!-- BEGIN MORA AGENT CONTINUITY -->
## Mora continuity

Mora is the canonical cross-session memory.

- At the start of every session, call Mora's `brief` MCP tool once before substantive work. If Mora MCP is unavailable and the `mora` command exists, run `mora brief`.
- Before answering about the user's people, projects, meetings, past work, decisions, or commitments, retrieve from Mora with `context_memory` or `search_memory`. Never invent missing context.
- When a durable fact, decision, correction, commitment, handoff, or shipped outcome emerges, search to avoid duplicates, then write it to the correct Mora scope with `write_memory` or `mora write`. Do not wait for the user to say "remember this."
- Respect Mora's configured write policy. A proposed memory is pending, not saved; a read-only refusal must not be bypassed. For a completed write, read back the memory by ID and surface any dirty-index or retrieval lag honestly.
- Never save secrets, transient chatter, raw operational telemetry, or speculative claims. Never copy text from a projected `MORA:BEGIN`/`MORA:END` block back into Mora.
<!-- END MORA AGENT CONTINUITY -->
```

## Ask an agent to install it

Paste this prompt into an agent that can edit local files:

```text
Make Mora's cross-session behavior persistent for this agent.

First verify that `mora version` works and that Mora MCP is configured. Find the
correct global user instruction file for this harness: ~/.codex/AGENTS.md for
Codex, ~/.claude/CLAUDE.md for Claude Code, or ~/.gemini/GEMINI.md for Gemini
CLI. If the harness is different or the target is ambiguous, stop and ask me.

Add the exact "Mora agent continuity" block from
https://github.com/pyranthus-hq/mora/blob/main/docs/agent-continuity.md. Preserve
all existing content. Do not put it inside any generated, managed, or
MORA:BEGIN/MORA:END projected section. If the marked continuity block already
exists, update that block in place instead of adding a duplicate. Do not edit a
project repository's instructions.

Verify the file contains exactly one continuity block, run the read-only
`mora brief`, and report the file changed plus any health warning. Do not connect
new data sources, install schedules, change Mora's write policy, or delete any
memory.
```

The instruction file persists across sessions. Restart the agent after the first
installation so the new global rule is loaded. Mora's configured
`mcp_write_policy` still controls whether `write_memory` writes immediately,
creates a pending proposal, or refuses the mutation.

## Optional Drona friction rule

Drona is separate from Mora. Mora stores durable conclusions; Drona records
operational friction for human review. If Drona is installed and useful across
more than one project, add this beside the Mora block:

```markdown
## Drona friction

When a broken tool, missing context, misleading instruction, or repeated
workaround causes real rework, run:

`drona wound "<plain-English problem>" --kind <tool|retrieval|memory|docs|config|workflow>`

Skip ordinary mistakes and secrets. A wound never authorizes edits.
```

Drona must never turn a wound into an automatic prompt, skill, repository, or
Mora change. Its output remains a human-reviewed recommendation.
