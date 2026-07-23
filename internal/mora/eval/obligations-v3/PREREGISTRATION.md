# Obligations-v3 validation round — preregistered thresholds
Frozen 2026-07-23 BEFORE corpus authoring begins. Round shape approved by Adit: AI panel + one expert sitting; lay cohort skipped (documented deviation).

## Round shape and deviation
- Tier 1: AI panel, 5 sealed readers across >=2 vendors (claude / codex / antigravity), packet = rules + records only, no labels.
- Tier 2: ONE expert sitting (Anirudh, the v2 expert), blind to the key.
- DEVIATION from the v2 round: no lay cohort. Rationale: v3 changes thread RENDERING realism (per-message Gmail evidence), not the labeling contract; lay generalization of the contract was established in the v2 round (lay 0.83). This deviation is preregistered, not post-hoc.

## Tier 0 — machine gates (all must pass before any reader sits)
- exam.Validate green on the v3 ledger; all lints green (Lint, LintCorpus, LintLeakage, LintDateFingerprint, LintTitleFingerprint); CORPUS.sha256 matches rendered bytes; corpus-integrity test green; deterministic re-render byte-identical.

## Tier 1 — AI panel (gates the expert sitting)
- Each reader: 4-class kappa >= 0.70 vs key.
- Majority vote == key on >= 95% of records.
- Any majority-vs-key mismatch is adjudicated as a suspected fixture/key bug BEFORE the expert sits; if the key changes, Tier 1 reruns from scratch.

## Tier 2 — expert (gates VALIDATED)
- Expert 4-class kappa >= 0.75.
- Binary involvement kappas (must-do-involvement, waiting-involvement) each >= 0.70.
- Zero rows where panel majority AND expert unanimously disagree with the key.

## Corpus content requirements (authoring contract)
- >= 18 artifacts, >= 12 commitments, >= 8 non-obligations; four-class gold derivable (MUST_DO / WAITING / BOTH / NEITHER).
- >= 3 multi-message two-way Gmail threads where per-message author->addressee pairing is REQUIRED to recover direction (union rendering ambiguous), including >= 1 counterparty-authored request that creates a self obligation and >= 1 self-authored ask that creates a waiting item.
- >= 1 reported-speech commitment (owner = reported actor, not the author).
- v2 realism shapes preserved: wrapped authored mail, attributed quoted reply, forwarded, composite artifact with open canonical commitment; >= 1 cross-channel closure.
- Labeling contract text (OBLIGATIONS.md rules incl. the DAILY trailing-7x24h rule) unchanged from v2 except where SchemaV3 rendering requires new wording; any contract change beyond rendering VOIDS the lay-skip rationale and must halt the round.
