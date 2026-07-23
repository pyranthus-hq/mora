# obligations-v2 validation preregistration

Committed BEFORE any corpus fixture was authored and BEFORE any reader sits.
These thresholds may not be adjusted after results arrive; a threshold change
after first contact with data voids the round and requires a fresh corpus cut.

## Why this document exists

The obligations-v1 human round closed with a diagnostic verdict: five lay
readers consistently diverged from the key on clusters that traced to fixture
defects (unobservable closures, mechanically-dead quotes, unrealistic
forwards, an untaught direction taxonomy). The instrument and scope were
revised repeatedly *during* that round, so no sitting could cleanly validate
the revisions — the train/test boundary was gone. This round fixes that by
declaring corpus requirements, instrument, readers, and pass/fail lines in
advance.

## Sealed-key discipline

- The v2 ledger (and therefore the answer key) is authored by a coding agent,
  headlessly. Its labels are never printed to the custodian's terminal and
  never pasted into a conversation with the custodian before the custodian's
  own sitting is sealed.
- The custodian (Adit) is therefore **blind-eligible** for this round and may
  sit as a reader, provided the sitting seals before any adjudication
  material is read.
- Machine gates (validator, lints, hash manifest) are evaluated by pass/fail
  only; their output must not restate labels.

## Corpus requirements (deficiencies this corpus must repair)

1. **Observable closures.** Any obligation whose expected label is
   already-handled must carry the completion evidence in the same artifact a
   reader is shown for that row. Cross-record-only closure is not a
   human-decidable fixture and may appear only as machine-scored rows.
2. **Evidence-complete negatives.** A quoted or archived ask that is dead
   must be observably dead in the fixture text (e.g., the fulfilled reply is
   visible beneath the quote). "The reader should infer staleness from
   meta-cues" is a fixture bug, not a hard row.
3. **Realistic forwards.** Forward fixtures form minimal pairs: a bare
   forwarded advertisement (negative) versus a forward carrying a personal
   ask line (positive), in real client syntax.
4. **Two-section taxonomy.** The contract separates "things Alex must do"
   from "things Alex is waiting on from others," and the human instrument
   asks about them as two named sections. Direction is taught by the
   question, not inferred.
5. **Realism shapes.** Schema v2 requirements: composite footer-on-real-ask,
   72-column wrapped body carrying a live ask (#136 shape), attributed
   quotes; all leakage gates (`LintLeakage`, `LintDateFingerprint`,
   `LintTitleFingerprint`) pass in CI.

## Validation tiers and acceptance thresholds

**Tier 0 — machine gates (every commit).** Validator, all lints, hash
manifest, mutation rows. Pass/fail; any failure blocks the round.

**Tier 1 — AI reader panel (before any human sits).** Three or more sealed
model readers, given only rules + records + rows (no repo, no key).
- Each reader: Cohen's kappa >= 0.70 versus the key.
- Panel majority vote agrees with the key on >= 95% of rows.
- Any row where the majority disagrees with the key is adjudicated as a
  suspected fixture/key bug before humans sit; if the key is changed, Tier 1
  reruns from scratch.

**Tier 2 — human sittings (once per corpus version).**
- One expert/ICP reader covering BOTH classes, kappa >= 0.75, with per-class
  agreement recorded separately (the v1 round never expert-validated the
  negative class; this round must).
- Two cold lay readers, each kappa >= 0.50 on the full row set.
- Zero rows where all counted human readers unanimously disagree with the
  key. One such row = fixture bug: adjudicate, re-cut, re-sit affected rows.
- Readers used to debug the instrument (pilots) are named as pilots in the
  record and excluded from the counted set.

**Close-out.** The round closes as VALIDATED only if all three tiers pass as
specified above. Any other outcome closes as DIAGNOSTIC with named clusters,
and the clusters become requirements for the next cut — thresholds unchanged.

## Instrument commitments

- Reading-time target 10–15 minutes; practice items gate entry; the question
  names both directions; drafts stored locally only; export is copy-first;
  versioned storage key so no reader ever inherits a stale draft.
- No participant-facing text may name ledger, verdict, scorer, or fixture
  vocabulary (build-time leak assertions).
