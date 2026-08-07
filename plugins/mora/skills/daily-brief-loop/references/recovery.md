# Daily brief recovery and decision table

Read this reference when `mora loop begin`, heartbeat, pulse, presentation, or close does not follow the ordinary success path. The four invariants in `SKILL.md` remain authoritative.

| Situation | What you run | What you must NOT run |
|---|---|---|
| Start of the loop | `mora loop begin daily-brief --json` | anything before it |
| begin → exit 10 `already-succeeded` or `effect-already-committed` | read and present the saved artifact through `mora brief --json`, STOP | sync, `pulse`, MCP `brief`, any `--advance` |
| begin → non-zero `outcome is uncertain` | report it; inspect `mora loop status daily-brief` / the saved artifact read-only; STOP | any same-day `--advance` or automatic retry |
| begin → exit 0 | `mora loop heartbeat daily-brief --run <run_id>`, then one fenced `mora pulse --digest --sync --advance --brief-file --write --loop daily-brief --loop-run <run_id>` | an unfenced or second `--advance`; split sync+pulse |
| Reading back / follow-up question after pulse | `mora brief` or MCP `brief` (read-only) | any `pulse --advance` |
| Sync warning during pulse | continue, surface it in the "does NOT know" line | aborting the loop |
| After presenting the brief | `mora loop done daily-brief --ok --run <run_id>` | `--ok` before presenting |
| Pulse errored / can't present | `mora loop done daily-brief --fail "<reason>" --run <run_id>`; if the effect had started, same-day automatic retry remains blocked | leaving the loop open; `--ok`; retrying `--advance` |
| User wants today remembered | offer `write_memory` | inventing memories; forcing the write |

## Silent-violation checklist (the contract this skill enforces)

1. **Skipping the begin gate** → always run `mora loop begin daily-brief --json` first; no sync/pulse/brief without seeing its exit code in this run.
2. **Ignoring the exit-10 skip** → on `already-succeeded` or `effect-already-committed`, print the existing dated brief and STOP; never proceed to a real run.
3. **Double-advance** → at most ONE `pulse … --advance` per run; never re-issue it, even under uncertainty.
4. **Retry-by-re-advancing** → to recover from doubt, READ the artifact or `loop done --fail`; never rerun the advancing pulse.
5. **Using the read-only `brief` as the real run** → MCP `brief`/`mora brief` are preview-only; they never replace the gated `pulse --advance`.
6. **Hiding sync errors** → sync failures are non-fatal but must appear in the "does NOT know" line, not be swallowed.
7. **Any send/notify** → NO-SEND invariant: never email, reply, draft, post, DM, message, create events/tickets, or notify outbound.
8. **`done --ok` before presenting** → only mark success after the human has seen the brief.
9. **Leaving the loop open** → every `begin` (exit 0) is closed by `done` (`--ok` or `--fail`) in the same run.
10. **Re-running after a skip to "fill gaps"** → after either exit-10 skip, the dated artifact is the single source of truth; run nothing else.
11. **Advancing without an active owner fence** → heartbeat the run, then pass its exact id through `--loop daily-brief --loop-run <run_id>`; never strip those flags from the advancing pulse.

