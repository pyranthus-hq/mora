# obligations-v3 validation round — CLOSED: VALIDATED (two documented instrument findings)

Closed 2026-07-23 by the custodian. Thresholds per `PREREGISTRATION.md`,
frozen before any fixture was authored; none were adjusted. Round shape:
sealed AI panel plus one expert sitting; the lay cohort was skipped as a
preregistered (not post-hoc) deviation, on the rationale that v3 changes
thread rendering realism, not the labeling contract the lay cohort
generalized in the v2 round.

## Results by tier

**Tier 0 — machine gates.** Validator (schema-v3 per-message Gmail
rendering), identity lint, label-leakage lint, date-fingerprint lint,
title-fingerprint lint, corpus lint on rendered bytes, hash manifest, and
deterministic re-render: all green in CI on the committed corpus.

**Tier 1 — sealed AI reader panel.** Five model readers across three
vendors (two Anthropic, two OpenAI, one Google), each given only the rules
and the 18 records, sheets sealed to disk. Round 1 surfaced one row where
all five readers unanimously disagreed with the key; per the preregistered
procedure it was adjudicated before any human sat and found to be a
**derivation bug, not a reader miss**: the gold script counted a
cross-artifact closure that is invisible per-record (the terminal
transition's evidence lives in a different artifact than the opening
request). The derivation was amended to per-record visibility, the key
changed on that one row, and — as preregistration requires on any key
change — Tier 1 was rerun from scratch with fresh readers. Rerun: every
reader 18/18, Cohen's kappa 1.00, majority == key on 18/18. Thresholds
(each >= 0.70, majority >= 95%): PASS.

**Tier 2 — expert sitting.** One expert reader (the v2 expert), key-blind,
four-way per-record task on the gated-practice web instrument: 16/18,
four-class kappa 0.84 (>= 0.75), binary involvement kappas 0.77 (must-do)
and 1.00 (waiting) (each >= 0.70). Zero rows where the panel majority and
the expert unanimously disagree with the key (the panel matched the key on
all 18). All preregistered bars: PASS.

## Adjudication of the two expert disagreements

Both were adjudicated (independent model adjudicator, cross-checked) as
**instrument faults, not expert error, and not key bugs**. Neither changes
the key, so the preregistered key-change Tier-1 trigger does not fire.

- **Instrument-guide contradiction.** The instrument's plain-language guide
  told readers the proof of completion "can sit in a different note — skim
  All records", contradicting the formal per-record contract ("use only
  text and source metadata visible in that record"). The expert faithfully
  followed the guide, found the cross-record fulfillment, and marked the
  row already-done; the row's per-record gold (and the unanimous panel,
  which weighted the formal contract) says the obligation still reads
  open. Fix: align the guide and record-drawer copy to the per-record
  closure rule. The derivation and key are unchanged.
- **Render visibility gap.** In one multi-message Gmail thread the rendered
  body carries a `From:` header only on the first message, so a human
  reader cannot see that the second first-person promise line switches
  speaker; the machine-readable per-message senders exist only in source
  frontmatter, which the panel packet exposed but the human instrument did
  not. The full-visibility gold is correct and unchanged; the expert's
  read is the correct label for what the render actually shows. Fix:
  render a `From:` header on every message of a multi-message thread (a
  corpus-bytes amendment tracked as a follow-up issue); the affected read
  should be re-administered on the corrected render — a re-administration,
  not the preregistration's key-change rerun trigger.

## Documented deviations

1. Lay cohort skipped — preregistered in `PREREGISTRATION.md` before
   authoring, with the rationale recorded there.
2. One mid-round key amendment (the Tier 1 derivation bug above), executed
   exactly per the preregistered adjudication procedure, including the full
   Tier 1 rerun from scratch.

Every claim derived from this round carries these qualifications.

## Post-round corpus amendment — 2026-07-29

Issue #190 corrects the documented render-visibility gap. Renderer
`exam-render-v3.1` emits a visible `From:` header for every message in a Gmail
thread, and `CORPUS.sha256` pins the amended bytes. The ledger and gold key are
unchanged.

The agreement values above remain the historical result of the original
sitting. The affected expert row has not yet been re-administered on the
corrected render. Do not claim that the instrument finding is resolved until
that re-read is recorded.
