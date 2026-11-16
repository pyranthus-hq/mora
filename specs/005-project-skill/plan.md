# Implementation Plan: One project skill

**Prerequisite**: [Owner rule pilot](../003-owner-rules/tasks.md) T003
establishes project scope and the native-loader test. If the app is used
for review, use the verified boundary from
[review app](../001-cross-platform-review/tasks.md) T008. A verified local
owner approval can prove the pilot without adding skill cards to G1.

## Delivery order

1. Pick one low-risk recurring task and one hand-authored `SKILL.md`.
   Record the approved folder digest and project. Use the client's existing
   project skill directory. Track installed paths and digests in local
   state so detach can remove only matching managed files.
2. Reuse project binding and local approval state. Detect a changed source
   or installed copy before a fresh load. Quarantine a changed managed
   copy from the loader while preserving its contents for review. If the
   actual client cannot enforce the boundary, keep that pilot open.
3. Test native loading, unrelated tasks, the other project, and detach on
   both pilot hosts. Use the result to decide whether to open the broader
   library or project kit work.
