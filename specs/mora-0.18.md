# Mora 0.18 scope and acceptance

The release aims to make Mora usable from an agent client and a local review
window on macOS and Windows after the owner installs it. This map records the
release workstreams and their acceptance checks. Build item 3, case G1, is the
first feature with a Spec Kit [spec](001-cross-platform-review/spec.md),
[plan](001-cross-platform-review/plan.md), and
[tasks](001-cross-platform-review/tasks.md). The remaining entries track release
scope and decisions. Every case below still needs recorded acceptance evidence.
A proposed release check needs owner agreement where its underlying
decision is open.

## Build order

| Item | Workstream | Dependency and first deliverable | Acceptance |
| --- | --- | --- | --- |
| 0 | 0.17.1 baseline | Review the [governance and Windows fixes](https://github.com/pyranthus-hq/mora/pull/666). Confirm which agent write paths use the same policy and hold checks. | F, H |
| 1 | Owner rules | After explicit confirmation and project opt-in, generate Mora's own Claude Code rules file. Keep user-edited files intact, drop retired rules, and avoid delivering rules again in the brief. Global generation remains a separate decision. | A, E1, F |
| 2 | Typed capture and chat setup | Prove that the harness can distinguish a human prompt from agent-launched or headless input before granting owner standing. Keep uncertain input in the ordinary governed path. | B, F, G |
| 3 | Review | One local queue, exact-revision human confirmation, and a terminal fallback. G1 covers the note workflow. Skill approvals and optional in-client prompts depend on their own review and client tests. | F, G1 |
| 4 | Skills and sub-agents | Start with one hand-authored skill for one recurring task. Use the native loader and project scope. A general library, versioning, skills sync, and wider sub-agent support await the pilot. | C, E1 |
| 5 | Project kit and setup | With the rule and skill paths proven, validate one MCP registration, project binding, and idempotent setup. A kit with one review step is a follow-up to the one-skill pilot. | E1, G |
| 6 | Sync and session handoffs | Prove eligible scoped handoffs survive both directions between writable origin vaults first. The broader proposal is 15-minute, single-owner two-way sync through a private Git remote, with explicit conflict handling and per-machine activation. | D, D1 |
| 7 | Startup budget and diagnostics | Verify MCP profiles, context caps, the status cost breakdown, and doctor checks. Confirm the project tool catalog against the chosen review surface. | E, E1, E2 |
| 8 | Release evidence and docs | Run the approved flows on the actual Mac and Windows clients with a disposable vault and synthetic inputs. Report measured results and verify CI for the release commit. | Approved cases below |

## Acceptance matrix

Use synthetic data and record the exact build, client, OS, scope, and index
state. Keep sensitive content out of evidence. For Windows, the actual
pilot client and its supported backend are required. Record native or WSL.
A proposed release check documents the 0.18 target. Conditional cases
require their own scope decision before becoming release criteria.

| Case | Scope | Pass condition |
| --- | --- | --- |
| A | Pilot and proposed release | On the actual Windows client, give two fresh sessions the same task with an explicitly confirmed project rule that requires an observable action, such as using a sub-agent. Both perform it. Retire the rule and verify a third session no longer receives it and does not perform the action in response to the rule. Report the broader five-run check separately, with a target of at least four. Each generated project file stays within 6,000 bytes and reports the omitted rule count. |
| B | Proposed release | Type a synthetic personal fact in one bound project. Retrieve it in a fresh session in another with personal scope intact and without a save confirmation or terminal. Submit the same input through an agent-launched headless run and verify that it lacks owner standing. |
| C | Pilot | Attach one hand-authored approved skill for a recurring project task. Record the matching prompt, an unrelated control prompt, and native loader evidence. It loads in at least four of five matching fresh sessions and never on the unrelated control. |
| D | Conditional on wider sync | For the broader sync proposal, a Mac-origin rule and skill show as waiting on Windows within 15 minutes of saving both eligible records with both hosts online. Approve the exact revision on Windows. A matching repository is offered its kit. With explicit owner opt-in for a single-owner private remote, sync excludes local state, credentials, and connector data by default, keeps both conflict versions, and reports failure and last success in status. The private remote stores unencrypted files. Scope eligibility must be set before enabling sync. |
| D1 | Pilot | Save a short project-scoped handoff eligible for an existing scoped share on Windows. After the receiver applies the share and refreshes its index, a fresh Mac agent recalls it. Repeat in reverse and include an offline retry. Each destination sees one record with original scope and provenance. Held and rejected fixtures stay out. Verify the persisted handoff, explicit search, and startup recall independently. |
| E | Proposed release | An unbound session receives zero Mora SessionStart text by default. Measure the MCP overhead separately as in E2. If global rules are enabled for the test, measure that file separately. |
| E1 | Proposed release | With one registration, start fresh sessions in A → B → unbound → A. Use distinct project-scoped note, rule, and skill fixtures. Search, evidence manifests, startup context, and skill suggestions exclude the other project’s markers. Record health for a fresh index run and a run with an in-progress refresh. Shared personal facts in B use their intended scope. If the client supports rebind in an open session, check new Mora output after the rebind. A shell directory change is insufficient. |
| E2 | Proposed release | Project-profile MCP initialize uses at most 500 estimated tokens and the proposed seven-tool catalog at most 1,500, at most 2,000 combined before rules. Resolve any catalog change separately while preserving the budget. Bound SessionStart includes its project marker within 2,000 characters. Prompt recall stays within three notes and 800 UTF-8 bytes, and an omitted in-scope note remains findable through explicit search. Estimate tokens at four characters each. |
| F | Proposed release | Exercise MCP, CLI, handoff, correction, task, and imported-file paths that can introduce agent content. Under the configured write policy, verify each attempt is tentative, held, or refused. No case gains owner standing or activates a rule. Repeat the G1 stale, expiry, and replay controls. |
| G | Proposed release | On Mac and Windows, start the timer at the end of installation and stop when the first rule-follow action is observed in a fresh session. The operator uses no terminal after install. Record the elapsed time without claiming a time target, and record first use and second-day use separately. |
| G1 | Spec Kit pilot | Run the [review app acceptance](001-cross-platform-review/quickstart.md) on Mac and Windows. Launch directly and via the platform entry when available, Keep and Discard, Cancel, reject stale approval, and verify a fresh agent sees the kept note only in scope with agent provenance intact. |
| H | Release prerequisite | The candidate commit is on main and all jobs, including Windows, are green before tagging. For the pilot comparison, rerun the shared-repo decision-recall prompts against a plain `DECISIONS.md`. Mora meets or exceeds its correct-answer count on the same fixture. |

## Friction feedback pilot

Agents record recurring friction with scope, symptom, expected behavior, a
verified recovery when available, and an evidence reference. Start with one
reviewed report through Mora's governed write path. The pilot checks an
idempotent import, read-back, and explicit recall without the report ID in a
fresh matching session. Run a wrong-scope control and distinguish recovery
of service from repair of the defect. Check devbox visibility and project
isolation during index refresh before expanding import. Preserve agent
provenance and redact private references before import. Keep raw traces at
their source and keep experimental warning and recurrence measurement
outside the production feedback path. This companion pilot does not block
the 0.18 release.
