# Implementation Plan: Skill library and task-based discovery

Reuse the [one-project-skill pilot](../005-project-skill/tasks.md) T003 and
[scoped setup](../006-project-setup/tasks.md) T003. Use the existing owner
review path on each host and [desktop review](../001-cross-platform-review/spec.md)
where it provides verified owner approval.

1. Start with pasted skill text, a project-scope suggestion, deduplication,
   exact-revision approval, and native loading. Prove discovery from a
   paraphrased task in a fresh session.
2. Extend intake to attached folders and repository links through supported
   client and installer paths. Add global scope, revision history, retirement,
   and rollback. Resolve project overrides before loading approved skills.
3. Extend task discovery to skills, best practices, repositories, and tools
   using existing scoped retrieval. Limit explicit discovery to five results
   and automatic context to the [startup budgets](../008-startup-budget/spec.md).
   Return availability and review state without installing or running a result.
4. Run S001 on Mac and the supported Windows client using disposable fixtures.
   Record approvals, loader evidence, scope controls, and measured budgets
   for each host.

Retrieval comparisons and local-model tuning are optional experiments after
the workflow works. The release uses the chat workflow as its library interface.
