# Feature Specification: Scoped setup and project switching

**Feature**: `006-project-setup`

**Status**: Proposed release feature. Full kit setup is conditional.

**Acceptance**: E1 and the setup portion of G.

## User Scenarios and Testing

### User Story 1 — Set up Mora once and switch projects (Priority: P1)

As a new Mora user, I can finish setup after installation through my
agent client. Opening another project picks up its own context through
the same registration.

## Functional Requirements

- **FR-001**. Use one MCP registration and a verified client session
  folder or root to resolve the bound project. A shell directory change
  alone does not rebind a session. Keep explicit overrides deliberate.
- **FR-002**. Setup is idempotent. Preserve unmanaged settings, report
  completed and failed steps, and keep a project's generated instructions
  and skill separate from other projects.
- **FR-003**. Unbound folders get no Mora SessionStart text by default.
  Project isolation applies to new context, search, evidence manifests,
  and any available skill suggestion surface even during refresh.
  Intentionally shared personal scope remains available where configured.
- **FR-004**. A proposed full project kit with a single review step
  depends on the one-skill pilot and separate approval to expand scope.

## Success Criteria

- **AC-SWITCH (E1)**. With distinct notes, rules, and skill markers,
  open fresh sessions in A → B → unbound → A on Mac and Windows through
  one registration. Check the scoped result and evidence manifest with a
  fresh index and during a verified in-progress refresh. Test only new
  Mora output after rebind if the client supports it.
- **AC-SETUP (G)**. After the owner installs, use the actual client to
  configure a disposable project and repeat setup. It needs no operator
  terminal and creates no duplicate registration, hook, or managed file.
  A failed step is visible and a safe retry preserves prior valid state.
