---
name: daily-brief-loop
description: Use to run the user's durable daily brief — "run my daily brief", "what changed since yesterday", "morning brief", "catch me up", "daily pulse" — a once-per-day, human-checkpointed, NO-SEND loop that syncs the user's own sources, advances the brief watermark exactly once, and presents the dated brief. Drives the `mora loop` command; idempotent (re-running the same day is a no-op that just reprints today's brief).
---

# Daily Brief Loop

Requires the Mora MCP server (`claude mcp add mora -s user -- mora mcp serve`) and the `mora` CLI on PATH. If the `mora` tools/CLI are not available, say so up front and STOP — there is no web fallback for this loop; the brief is built entirely from the user's own local vault, and fabricating one would be a lie about their day.

## The NO-SEND invariant (read first)

**This loop NEVER sends.** It reads the user's own sources and renders a LOCAL brief. It must not email, reply, draft, post, DM, message, create calendar events/tickets, or trigger any outbound notification — even if the brief surfaces something that "obviously needs a reply." Surfacing "you owe Sam a reply" is in scope; sending that reply is OUT of scope and belongs to a different, explicitly-invoked skill. The ONLY writes this loop performs are local: `mora pulse --brief-file` (the dated vault artifact), the watermark advance, and an optional `write_memory` close-out. If any step would cause egress, do not run it.

This loop is **durable, idempotent, and human-checkpointed**: it runs once per logical day, the second run is a no-op, and you advance Mora's state only at explicit gates — never speculatively.

## The Flow

### Step 1 — Open the loop gate (ALWAYS first)

Run exactly:

```
mora loop begin daily-brief --json
```

Branch on the result — do nothing else until you have:

- **Exit 10**, body `{"skip":"already-succeeded"}` → today's brief already ran and succeeded. Do **NOT** sync, do **NOT** pulse, do **NOT** call the MCP `brief` tool, do **NOT** advance anything. Print today's existing dated artifact verbatim and STOP. Read it from the vault (`mora brief --json` returns the freshest persisted brief; or read `briefs/<today-UTC>-brief.md` directly) and present it with a one-line note: "Already ran today — this is the saved brief." Then end the turn. **Re-running after a skip to "fill gaps" is a contract violation: the dated artifact is the source of truth.**
- **Exit 0** → the gate is open. The JSON body includes a `"run_id"` — **note it now**; you pass it back to `loop done` in Step 4 (`--run <run_id>`) so a late or duplicate close can never clobber a newer run. Proceed to Step 2.
- **Any other non-zero exit** → the loop could not open (lock held, config error). Report the error verbatim and STOP. Do not improvise around a closed gate.

> Guardrail: there is exactly ONE entry point. Never skip `loop begin` and jump straight to `pulse`/sync/`brief` — the begin gate is what makes the day idempotent and what owns the lock. If you did not see exit 0 from `loop begin` in THIS run, you have no authority to sync or advance.

### Step 2 — Run the single advancing pulse (checkpoint each stage)

The day's work is **one** command that does sync → build → advance → persist atomically:

```
mora pulse --digest --sync --advance --brief-file --write
```

What each flag means and why it must be exactly this set:

- `--sync` — refresh the user's enabled sources first so the brief reflects current data. Sync errors are **non-fatal by design** (honest-but-don't-abort): a failed source surfaces as stale/unavailable inside the brief. Do not abort the loop on a sync warning — a partial honest brief beats no brief. Do surface the warning to the user; do not hide it.
- `--advance` — commits the delta watermark. **This is the ONLY surface in all of Mora that advances the watermark, and it must run AT MOST ONCE per logical day.** Advancing twice double-consumes the delta, so the second run's brief looks empty and the day's real changes are silently lost.
- `--brief-file` — persists the dated artifact `briefs/<UTC-date>-brief.md` (same-day re-write overwrites; one file per day).
- `--write` — records the run in the local log.

> Guardrail (the single most dangerous failure): in one run you execute **at most one** command that contains both `mora pulse` and `--advance`. If that command has already executed in this run — even if you are unsure whether it succeeded — **do NOT run it again**. To check what happened, READ the brief artifact (`mora brief --json`) or call `mora loop done daily-brief --fail "<reason>"`; never re-issue the advancing pulse to "retry." A re-issued `--advance` is an un-undoable watermark double-advance.

> Guardrail: do NOT decompose this into separate `mora sync` + `mora pulse --advance` calls, and do NOT add a second `pulse --advance` "just to be sure." The one combined command is the atomic unit of the day.

**Read back via the preview-only tool, not a second advance.** After the advancing pulse, when you need to re-read or re-render the brief during this turn (e.g. to present it, to apply a filter, to answer a follow-up), use the **read-only `brief` MCP tool** (`mcp__mora__brief`) or `mora brief`. These are preview-only by construction — they NEVER sync and NEVER advance the watermark. Treat the MCP `brief` tool as a window onto the already-committed state, never as a substitute for the gated advancing pulse and never as a way to "advance again."

> Guardrail: the MCP `brief` tool / `mora brief` are read-only previews. They are the right tool for reading back and for follow-up questions. They are the WRONG tool for "running the brief" — the real run is the single `pulse --advance` in Step 2, which already happened.

### Step 3 — Present the brief, then the honesty line

Present the rendered brief to the user. Then, in the dining-concierge style, end with an explicit **"What Mora does NOT know"** line that names the real gaps, for example:

- Sources that failed to sync this run (from the `--sync` warnings) and are therefore showing stale/last-good data — name them.
- Source families that are not connected at all (so a whole channel of the day is invisible).
- The watermark boundary: this brief covers changes since the last brief; anything older than the prior watermark is not re-surfaced here.

Cite only what the brief actually contains. Do not invent items, do not infer sentiment or "priority scores" the brief did not produce, and do not paste private message bodies or phone numbers into the summary beyond what the brief already renders.

### Step 4 — Close the loop (only after presenting)

Mark the loop done — but only **after** the brief has actually been presented to the user in Step 3:

- Success: `mora loop done daily-brief --ok --run <run_id>`
- Failure (the pulse errored, the gate was closed, or no valid brief could be presented): `mora loop done daily-brief --fail "<short reason>" --run <run_id>`

`<run_id>` is the value from Step 1's `loop begin` JSON. If `loop done` reports the run was **superseded**, a newer run has replaced yours — do not retry, just stop. (`--run` is optional for manual CLI use, but the loop ALWAYS passes it so a stalled run can't close over a fresh one.)

> Guardrail: never call `loop done --ok` before the brief is on screen. "Done --ok" asserts the human saw today's brief; calling it early breaks the idempotency contract (tomorrow's begin gate trusts this flag) and would make a future same-day run skip with nothing real to show.

> Guardrail: if anything between `loop begin` (exit 0) and a presented brief goes wrong and you must stop, close with `loop done daily-brief --fail "<reason>"` — do not leave the loop open. A loop opened with `begin` must always be closed with `done` (`--ok` or `--fail`) in the same run.

Then **offer** `write_memory` to close the loop durably (do not force it): a one-line memory of today's brief — the date, the headline items, anything the user reacted to or asked you to remember. This sharpens future briefs and gives tomorrow's run continuity. Write it only if the user wants it; cite it as a saved memory, never as something already known.

## Decision table — what runs when

| Situation | What you run | What you must NOT run |
|---|---|---|
| Start of the loop | `mora loop begin daily-brief --json` | anything before it |
| begin → exit 10 `already-succeeded` | print today's `briefs/<today>-brief.md` (via `mora brief`), STOP | sync, `pulse`, MCP `brief`, any `--advance` |
| begin → exit 0 | one `mora pulse --digest --sync --advance --brief-file --write` | a second `--advance`; split sync+pulse |
| Reading back / follow-up question after pulse | `mora brief` or MCP `brief` (read-only) | any `pulse --advance` |
| Sync warning during pulse | continue, surface it in the "does NOT know" line | aborting the loop |
| After presenting the brief | `mora loop done daily-brief --ok --run <run_id>` | `--ok` before presenting |
| Pulse errored / can't present | `mora loop done daily-brief --fail "<reason>" --run <run_id>` | leaving the loop open; `--ok` |
| User wants today remembered | offer `write_memory` | inventing memories; forcing the write |

## Silent-violation checklist (the contract this skill enforces)

1. **Skipping the begin gate** → always run `mora loop begin daily-brief --json` first; no sync/pulse/brief without seeing its exit code in this run.
2. **Ignoring the exit-10 skip** → on `{"skip":"already-succeeded"}`, print the existing dated brief and STOP; never proceed to a real run.
3. **Double-advance** → at most ONE `pulse … --advance` per run; never re-issue it, even under uncertainty.
4. **Retry-by-re-advancing** → to recover from doubt, READ the artifact or `loop done --fail`; never rerun the advancing pulse.
5. **Using the read-only `brief` as the real run** → MCP `brief`/`mora brief` are preview-only; they never replace the gated `pulse --advance`.
6. **Hiding sync errors** → sync failures are non-fatal but must appear in the "does NOT know" line, not be swallowed.
7. **Any send/notify** → NO-SEND invariant: never email, reply, draft, post, DM, message, create events/tickets, or notify outbound.
8. **`done --ok` before presenting** → only mark success after the human has seen the brief.
9. **Leaving the loop open** → every `begin` (exit 0) is closed by `done` (`--ok` or `--fail`) in the same run.
10. **Re-running after a skip to "fill gaps"** → after `already-succeeded`, the dated artifact is the single source of truth; run nothing else.

## Privacy

- The brief is built entirely from the user's local vault and stays local — no egress (the loop's NO-SEND invariant covers both replies and any external query).
- Output contains real names, message context, and calendar detail — never paste it into public demos, posts, or screenshots.
- Task-minimal: present what the brief renders; do not pull or quote extra private threads beyond it, and never include phone numbers.
