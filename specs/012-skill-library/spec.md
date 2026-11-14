# Feature Specification: Skill library and task-based discovery

**Feature**: `012-skill-library`
**Status**: Mora 0.18 release scope. Implementation and acceptance are open.
**Acceptance**: S001 in the [release map](../mora-0.18.md).

Add skills from chat, choose project or global scope, and find skills,
best practices, repositories, and tools by describing a task. Reuse the
approval and native loading flow from the one-project-skill pilot.

### Requirements

- **S01 Chat intake.** Accept pasted skill text, an attached skill folder,
  or a repository link. Use the agent client's supported attachment and link
  access, and existing installers for fetching packages. Identify the skill's
  purpose, required files, dependencies, and source revision. Pasted text uses
  a content hash as its revision identity. Fetched folders also retain their
  source commit or version and a hash of the complete contents. Preserve source
  content and attribution. Show missing files or unavailable sources before
  activation. Return a stable ID and the configured write outcome.
- **S02 Scope suggestion.** Suggest the bound project when the skill depends
  on its stack, conventions, or client material. Suggest global scope for
  reusable guidance that applies across projects. Explain the suggestion in
  one sentence and let the owner select or change it in chat. Ask for a target
  when the project cannot be resolved or applicability is unclear. Global
  means available across the owner's projects on the configured host. Wider
  sharing follows the existing share policy. Apply the configured owner review flow
  to the proposed scope and exact revision, honoring any approval already valid
  for that binding. Record approval and materialization on each host through
  the existing host-specific lifecycle. An agent's scope suggestion carries
  agent provenance.
- **S03 Managed lifecycle.** Reuse Mora's rule and skill governance for
  approval, binding, version history, pinning, retirement, and rollback.
  Compare incoming content with existing records before adding it. Repeating
  an identical import reuses the record. A changed source creates a reviewable
  revision. Review covers the whole skill folder, including scripts. Use
  standard SKILL.md folders and the client's native loader. Detach or rollback
  preserves unrelated and locally edited files and reports conflicts.
- **S04 Resource library.** Store skills, best-practice notes, repository
  references, and tool references with their kind, scope, purpose, applicability,
  source, revision or version where available, review state, and last verified
  date where known. Link overlapping resources and show which version is active.
  A repository or tool reference can be recommended before installation, with
  its availability stated. Imported text remains source material until the
  applicable review flow authorizes it as agent guidance.
- **S05 Discovery by task.** From a normal agent session, accept a task
  description and the current project context, then retrieve up to five
  applicable resource descriptions and pointers. Search the authorized project
  and global scopes. Prefer an applicable project-specific choice when it overrides global guidance.
  Return each resource's purpose, reason for matching, scope, source, active
  revision or version, review state, known limits, and availability. Use recorded
  verification and outcomes to explain recommendations. Mark unknown freshness
  or effectiveness and allow a no-match result. Exclude retired and unapproved
  skill revisions from active guidance, while keeping them available in
  review/history. Saved repository and tool references may appear as unverified
  research leads with that status visible. A recommendation carries no approval
  to install or execute a tool or to adopt its instructions.
- **S06 Discovery and loading.** Keep automatic startup and prompt-recall output within
  the 0.18 E2 limits of 2,000 characters for bound SessionStart and three notes
  within 800 UTF-8 bytes for prompt recall, including health text. Retrieve the
  full approved skill when the task needs it, and load the pinned revision
  through the native loader.
  Resolve global and project bindings before materializing Mora-managed skills
  into the project's loader location, so an overridden global skill is omitted
  there. Report any conflicting unmanaged global installation for resolution.
  An eligible resource omitted from startup remains findable by explicit task
  search. Record recommendation, loading, and observed task outcome separately
  so later recommendations can use evidence of usefulness. Resource references
  retain their scope through search results, evidence manifests, and loading.

### Acceptance test — skill intake and discovery S001

Use disposable projects A and B, global fixtures, and fresh agent sessions.
Record the Mora revision, client, host, index state, prompts, resource IDs,
and native loader evidence. Run the supported chat flow on Mac and the actual
Windows pilot client. Confirm that each client supports chat intake, the
configured owner review, and native loading. Record unsupported capabilities
as open for that host. Set up and approve the same fixtures independently on
each host. The project-text milestone exercises the applicable portions of
checks 1–4. Full release acceptance covers all five checks.

1. **Intake and scope.** Paste a project-specific skill and verify the suggested
   project and reason. Review it in chat and read back the exact approved
   revision and binding. Repeat with reusable guidance, select global scope,
   and verify its binding. Also import a complete attached folder and a pinned
   repository link through chat. Verify the full approved file set and recorded
   revisions for both. An unbound ambiguous input asks for a target. Exercise
   the configured pending or refused path with an agent-authored candidate.
2. **Find and use.** In five fresh matching sessions per supported host, ask
   for the task using paraphrases without a skill name or ID. Require discovery
   and native loading of the pinned revision in at least four sessions, with
   the expected output marker. An unrelated task loads neither fixture skill.
   Retired and unapproved revisions never become active guidance.
3. **Scope and precedence.** In B, require zero A-only resource IDs, contents,
   and evidence references, including during an index refresh. The global skill
   remains available in both projects. Where A has an applicable override,
   recommend and load that revision without also loading conflicting global
   instructions. Use native loader records to verify the selected revision and
   absence of the overridden revision. Check explicit task search for an item
   omitted by a fixture exceeding the startup or prompt recall budget.
4. **Lifecycle.** Submit identical content twice and require one record.
   Import an edited revision and verify the old approval still refers only to
   the old content. Approve the update, then verify fresh-session loading,
   retirement, and rollback to an approved revision. Detach preserves unmanaged
   and locally edited files and reports any conflict.
5. **Resource selection.** Use tasks whose answer keys call for each of the
   four kinds, plus an unrelated task. Require the intended resource and reason,
   scope, source, review state, and known availability in the bounded shortlist.
   Include an unverified reference, a stale reference, and an unavailable tool
   to verify honest status and absence of automatic installation or execution,
   plus a no-match case. Measure the serialized startup and prompt-recall
   output against E2 with the library enabled. A folder fixture with a missing
   referenced file stays incomplete. Re-importing a pinned repository fixture is
   idempotent, while
   changed content creates a new revision for review.
