# Implementation Plan: 0.18 release validation

**Prerequisites**: [Baseline](../002-governance-baseline/tasks.md) T003,
and accepted cases from the [release map](../mora-0.18.md). For first use,
use the [rules](../003-owner-rules/tasks.md),
[setup](../006-project-setup/tasks.md), and
[budget](../008-startup-budget/tasks.md) acceptance results. Include
[review](../001-cross-platform-review/tasks.md) T008 for the no-terminal
review claim. Include [skill library](../012-skill-library/tasks.md) T005
and the final E2 measurements with the library enabled.

## Delivery order

1. Assemble a case-by-case results matrix for the chosen candidate.
   Record pilots and conditional scope separately from the release gates.
2. Run the blind Mac/Windows first-use exercise and the comparison on
   disposable data. Review the public guide and release notes for
   unsupported claims, including about sync, review, savings, or rules.
3. Check the candidate SHA against `.github/workflows/ci.yml` and the
   release workflow. Give the owner the evidence and open gaps for a
   release decision.
