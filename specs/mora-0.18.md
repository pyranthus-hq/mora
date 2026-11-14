# Mora 0.18 scope and acceptance

The release aims to make Mora usable from an agent client and a local review
window on macOS and Windows after the owner installs it. Every workstream has
a Spec Kit package below. Proposed acceptance stays open until the underlying
decision is approved and evidence is recorded. The feature numbers are
identifiers. The numbered list under Build first gives execution order.

## Build first

1. **Start with baseline T001.** Verify [PR #666](https://github.com/pyranthus-hq/mora/pull/666)
   and close the baseline receipt in [002-governance-baseline/tasks.md](002-governance-baseline/tasks.md)
   before starting 0.18 implementation. Required CI and the authority matrix
   must match the landed revision.
2. **Build one opt-in project rule first.** Run the confirm, fresh-session
   load, and retire test on the actual Windows client and Mac. Verify the
   existing local owner flow while the app is pending. If it cannot prove
   confirmation, use the desktop proof spike to unblock rule activation.
3. **Prove desktop review before expanding it.** Start the review app's
   Mac/Windows owner-verification spike. Enable app decisions only on
   a host that can prove exact-revision human approval. Verify G1 before
   claiming no-terminal review on that host.
4. **Test capture origin and the one-skill path.** Typed capture is a proposed release
   feature. Typed facts gain owner standing only on a proven client. Keep the skill pilot to one recurring
   task. Use those results for scoped setup and project switching. Budget
   fixtures can start on the baseline. Recheck budgets against the final
   catalog and actual scoped setup.
5. **Build the skill library.** After the one-skill pilot and scoped setup,
   add skills from chat, project and global scope, and task-based discovery
   for skills, best practices, repositories, and tools. Verify S001 on Mac
   and Windows.
6. **Run both-host release acceptance last.** Use the chosen build and
   approved scenarios, and record evidence for the release owner.

D1 handoff persistence is an independent pilot after the baseline, using an
authorized share. The friction feedback check is a companion pilot and can use
existing read paths. Full kits, 15-minute sync,
and friction import expansion each require a separate scope decision.

See [Mora 0.19](mora-0.19.md) for NightShift factory integration.

## Feature packages

| 0.18 item | Workstream | Package | Status and dependency | Cases |
| --- | --- | --- | --- | --- |
| Prerequisite | Governance baseline | [Spec](002-governance-baseline/spec.md) · [Plan](002-governance-baseline/plan.md) · [Tasks](002-governance-baseline/tasks.md) | First. Record a reviewed green baseline. | F, H |
| 1 | Owner rules | [Spec](003-owner-rules/spec.md) · [Plan](003-owner-rules/plan.md) · [Tasks](003-owner-rules/tasks.md) | First 0.18 slice after baseline. One opt-in project rule before widening. Supplies the E1 rule fixture. | A, F |
| 2 | Typed capture | [Spec](004-typed-capture/spec.md) · [Plan](004-typed-capture/plan.md) · [Tasks](004-typed-capture/tasks.md) | Proposed. Gate by actual client provenance. Rule drafts reuse owner rules. A desktop fallback claim needs G1 on that host. | B, F |
| 3 | Review app | [Spec](001-cross-platform-review/spec.md) · [Plan](001-cross-platform-review/plan.md) · [Tasks](001-cross-platform-review/tasks.md) | Spec Kit pilot and proposed review path. Baseline first, then exact-revision approval proof on each OS. | F, G1 |
| 4 | Project skill | [Spec](005-project-skill/spec.md) · [Plan](005-project-skill/plan.md) · [Tasks](005-project-skill/tasks.md) | Pilot after the rule lifecycle. One skill, one task. Supplies the E1 skill fixture. | C |
| 5 | Project setup | [Spec](006-project-setup/spec.md) · [Plan](006-project-setup/plan.md) · [Tasks](006-project-setup/tasks.md) | Proposed. Full acceptance uses rule and skill fixtures. Full project kits are conditional. | E1, G |
| 6 | Session handoffs and sync | [Spec](007-session-handoffs/spec.md) · [Plan](007-session-handoffs/plan.md) · [Tasks](007-session-handoffs/tasks.md) | D1 pilot after baseline. D depends on a wider sync decision. | D1, D |
| 7 | Startup and diagnostics | [Spec](008-startup-budget/spec.md) · [Plan](008-startup-budget/plan.md) · [Tasks](008-startup-budget/tasks.md) | Proposed. Final measurements use the chosen catalog and scoped setup. | E, E2 |
| 8 | Skill library | [Spec](012-skill-library/spec.md) · [Plan](012-skill-library/plan.md) · [Tasks](012-skill-library/tasks.md) | 0.18 release scope after the one-skill pilot and scoped setup. | S001 |
| 9 | Release validation | [Spec](009-release-validation/spec.md) · [Plan](009-release-validation/plan.md) · [Tasks](009-release-validation/tasks.md) | Last. Validate approved scope on the release candidate. | G, H, H1 |
| Companion | Friction feedback | [Spec](010-friction-feedback/spec.md) · [Plan](010-friction-feedback/plan.md) · [Tasks](010-friction-feedback/tasks.md) | Pilot. One reviewed report. No release dependency. | DR1 |

## Open any package

From the repository root in Bash, set the directory of the feature you want.
For the first task, use:

```sh
export SPECIFY_FEATURE_DIRECTORY=specs/002-governance-baseline
SPECIFY_FEATURE_NO_PERSIST=1 bash .specify/scripts/bash/check-prerequisites.sh --json --require-tasks --include-tasks
```

Use the same directory with the repo's Codex or Claude Code Spec Kit skills.
Use Bash on macOS, Git Bash, or WSL. Desktop acceptance still uses the
actual client and backend specified in the feature.

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
| C | Pilot | Attach one hand-authored approved skill for a recurring project task. Record the matching prompt, an unrelated control prompt, and native loader evidence. It loads in at least four of five matching fresh sessions and never on the unrelated control. Run on both Mac and Windows. |
| D | Conditional on wider sync | For the broader sync proposal, a Mac-origin rule and skill show as waiting on Windows within 15 minutes of saving both eligible records with both hosts online. Approve the exact revision on Windows. A matching repository is offered its kit. With explicit owner opt-in for a single-owner private remote, sync excludes local state, credentials, and connector data by default, keeps both conflict versions, and reports failure and last success in status. The private remote stores unencrypted files. Scope eligibility must be set before enabling sync. |
| D1 | Pilot | Save a short project-scoped handoff eligible for an existing scoped share on Windows. After the receiver applies the share and refreshes its index, a fresh Mac agent recalls it. Repeat in reverse and include an offline retry. Each destination sees one record with original scope and provenance. Held and rejected fixtures stay out. Verify the persisted handoff, explicit search, and startup recall independently. |
| E | Proposed release | An unbound session receives zero Mora SessionStart text by default. Measure the MCP overhead separately as in E2. If global rules are enabled for the test, measure that file separately. |
| E1 | Proposed release | With one registration, start fresh sessions in A → B → unbound → A. Use distinct project-scoped note, rule, and skill fixtures. Search, evidence manifests, startup context, and skill suggestions exclude the other project’s markers. Record health for a fresh index run and a run with an in-progress refresh. Shared personal facts in B use their intended scope. If the client supports rebind in an open session, check new Mora output after the rebind. A shell directory change is insufficient. |
| E2 | Proposed release | Project-profile MCP initialize uses at most 500 estimated tokens and the proposed seven-tool catalog at most 1,500, at most 2,000 combined before rules. Resolve any catalog change separately while preserving the budget. Bound SessionStart includes its project marker within 2,000 characters. Prompt recall stays within three notes and 800 UTF-8 bytes, and an omitted in-scope note remains findable through explicit search. Estimate tokens at four characters each. |
| F | Proposed release | Exercise MCP, CLI, handoff, correction, task, and imported-file paths that can introduce agent content. Under the configured write policy, verify each attempt is tentative, held, or refused. No case gains owner standing or activates a rule. The baseline covers existing write paths. After desktop review is available, run the separate G1 owner-proof and receipt controls. |
| G | Proposed release | On Mac and Windows, start the timer at the end of installation and stop when the first rule-follow action is observed in a fresh session. The operator uses no terminal after install. Record the elapsed time without claiming a time target, and record first use and second-day use separately. |
| G1 | Spec Kit pilot | Run the [review app acceptance](001-cross-platform-review/quickstart.md) on Mac and Windows. Launch directly and via the platform entry when available, Keep and Discard, Cancel, and verify scoped fresh-session recall with agent provenance intact. The linked AC-STALE checks cover changed or incomplete previews, expiry, replay, and a local caller without owner proof. |
| S001 | Release | On Mac and Windows, add a skill from chat, approve its exact revision and project or global binding, discover and load it by task, and resolve project overrides. Verify scoped discovery of skills, best practices, repositories, and tools, with lifecycle, budget, and access controls. Run all [skill library acceptance checks](012-skill-library/spec.md#acceptance-test--skill-intake-and-discovery-s001). |
| H | Release prerequisite | The candidate commit is on main and all required jobs, including Windows, are green before tagging. |
| H1 | Pilot comparison | Rerun the shared-repo decision-recall prompts against a plain `DECISIONS.md`. Mora meets or exceeds its correct-answer count on the same fixture. Record the score separately from H. |
| DR1 | Companion pilot | Verify idempotent, scoped recall of one reviewed report from a fresh agent, including devbox eligibility and an index refresh. See [friction acceptance](010-friction-feedback/spec.md#success-criteria). |

## Saahith design-partner walkthrough

Use the Big Data Bowl project and a second disposable project to walk through
client setup and scoped notes, one confirmed sub-agent rule, typed capture,
and one approved skill for a recurring task. Verify the rule and skill in a
fresh session, then switch projects and verify isolation. The setup guide
should show where the managed project context goes in the client's instruction
file, such as `CLAUDE.md`, and how to verify it in a fresh session.

On Mac, open review directly and from the Mora Status menu bar. On Windows,
launch from Start. Review and decide on the exact note, then verify recall.
Check backup and sync health, last success, recovery guidance, and one alert
per unresolved failure. Run the [desktop checks](001-cross-platform-review/quickstart.md)
and record the Mac and Windows results separately.

The portability tests cover one project skill on each host and one eligible
handoff in each direction. After the one-skill walkthrough, run the chat
intake and task-based library checks in S001. Automatic 15-minute
cross-device sync remains conditional. The approved backup design uses one
private vault for authorized backup and pulls, with separate scoped shares.
