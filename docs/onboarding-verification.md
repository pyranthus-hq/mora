# Verified resumable onboarding

Issue #293 defines onboarding as a reconciler, not a success banner. Mora is
**verified** only when it has observed the current underlying condition for
each required step. A receipt records what a prior run did, but is never proof
that a condition remains true. A later run always evaluates before it acts.

## Target checklist

The complete reconciler will use these ordered steps. Each step has a real
postcondition, not a configuration-only proxy.

1. **Installed product** — observe the running version and, for an app install,
   the signed/notarized Mora.app identity.
2. **Local foundation** — verify config, vault marker, local directories, a
   committed healthy index, and token storage disjoint from the vault. This
   reuses the doctor index and token-boundary checks.
3. **Connector readiness** — verify the selected connector is enabled and can
   actually read its protected source. Google requires a working read-only
   token; iMessage and Apple Calendar require an actual protected read, not a
   claim that Full Disk Access was clicked.
4. **Initial data** — verify the approved ingest committed and its index
   rebuild is healthy.
5. **MCP registration** — use only official `claude mcp ...` or `codex mcp ...`
   commands. A differently-targeted existing `mora` name is a conflict, not an
   overwrite opportunity.
6. **MCP protocol** — run a local protocol smoke test against the registered
   server.
7. **Scheduled work** — verify approved refresh and update jobs are installed
   and loaded.
8. **Update policy** — verify the chosen policy and a successful latest-update
   check.
9. **Retrieval** — verify a bounded search result, or truthfully report that
   there are no indexed memories yet.

Steps that request OAuth, Full Disk Access, MCP changes, schedules, update
policy, or data pulls require the approval appropriate to that step. Mora never
edits third-party client settings directly and never grants platform permission
on the user's behalf.

## First slice in this PR

This PR implements only step 2 as three resumable foundation checks:

- `local_layout`: config.toml, vault marker, and all required local directories;
- `committed_index`: the existing doctor index freshness and manifest checks;
- `credential_storage`: a token directory that exists outside the vault, using
  `doctor.PathsDisjoint`.

`mora setup --plan` observes those checks without writes. In a terminal,
`mora setup` rechecks before every action, creates only the missing local layout
or token directory, rebuilds only an unhealthy/missing index, then verifies the
result. A non-interactive invocation is read-only unless it explicitly selects
each mutation with `--local-layout`, `--committed-index`, or
`--credential-storage`; it never accepts a broad `--yes`. It does not launch
OAuth, ingest a source, contact a client CLI, install a schedule, or make an
update check. It prints **Foundation setup verified**, never **Setup complete**.

`mora setup status --json` emits a versioned `mora.setup.status` receipt. Its
stable first-slice exit behavior is: exit 0 when evaluation completed (including
an incomplete result), exit 1 for usage, I/O, or invalid/corrupt receipt errors.
`complete` stays false until later checklist slices exist; callers must inspect
per-step state rather than infer usability from exit status.

## Resume state and failure UX

After every verified foundation action, `mora setup` writes a sanitized,
versioned, atomic `0600` receipt at
`<StateDir>/setup/foundation-receipt.json`. It contains only step IDs, states,
and bounded evidence text—no tokens, source content, queries, account IDs, or
credentials. The receipt is deliberately outside the vault.

On rerun, successful steps are skipped only after current verification passes.
A run interrupted after local layout but before index rebuild resumes with the
layout intact and continues at the missing index. A corrupt or unknown receipt
fails closed with a recovery error rather than being overwritten. If a token
directory overlaps the vault, setup refuses to relocate it automatically and
names the required manual recovery.

The next slices must extend this same evaluator and receipt model for protected
reads, explicit approved mutations, official MCP CLI invocation, schedules,
updates, and retrieval. They must also add an operation lease before any
non-idempotent external action is introduced.
