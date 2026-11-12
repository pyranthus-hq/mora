# Implementation Plan: Scoped setup and project switching

**Prerequisites**: [Baseline](../002-governance-baseline/tasks.md) T003.
Use the [rule](../003-owner-rules/tasks.md) and
[skill](../005-project-skill/tasks.md) T003 fixtures for full acceptance.

## Delivery order

1. Compare the actual clients' session directory and declared-root
   support. Use `internal/mora/setup.go`, `project_bind.go`, `mcp.go`,
   and `hook.go` as the existing setup and scope boundaries. Resolve
   scope before ranking and evidence assembly. Report ambiguous sessions
   rather than inferring a project.
2. Add the minimum repeatable post-install setup. Keep installer and
   app approvals under owner control. Record per-step outcomes and
   preserve unmanaged settings on retry. Reuse the scoped fixtures across
   fresh sessions and during a controlled index refresh.
3. If the owner widens the skill pilot later, spec the kit's rule, skill,
   and sub-agent selection and one review step before implementation.
