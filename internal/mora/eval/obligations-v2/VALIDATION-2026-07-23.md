# obligations-v2 validation round — CLOSED: VALIDATED (one documented deviation)

Closed 2026-07-23 by the custodian. Thresholds per `PREREGISTRATION.md`,
frozen before any fixture was authored; none were adjusted.

## Results by tier

**Tier 0 — machine gates.** Validator (schema-v2 realism shapes), identity
lint, label-leakage lint, date-fingerprint lint, title-fingerprint lint,
render + hash manifest: all green in CI on the committed corpus.

**Tier 1 — sealed AI reader panel.** Five model readers across three vendors
(three Anthropic models, one OpenAI, one Google), each given only the rules
and the 18 records, sheets sealed to disk. Every reader 18/18, Cohen's kappa
1.00, all pairwise cross-reader kappas 1.00, majority == key on 18/18.
Thresholds (each >= 0.70, majority >= 95%): PASS. The OpenAI reader is
counted as supplementary because the same tool authored the ledger; the
panel passes on the four independent readers alone.

**Tier 2 — blind human sittings** (four-way per-record task; instrument =
gated-practice web tool; all readers key-blind):

| Reader | Agreement | kappa | Threshold | Result |
|---|---|---|---|---|
| Expert / ICP reader | 17/18 | 0.92 (positive class 1.00) | >= 0.75, both classes | PASS |
| Cold lay reader | 16/18 | 0.83 | >= 0.50 | PASS |
| Blind custodian (cold, expert) | 15/18 | 0.75 | — (see deviation) | — |

Cross-reader agreement: expert vs lay 0.92. Humans now agree with each
other AND with the key — in the obligations-v1 round they agreed with each
other against it.

## Adjudication

Five rows had at least one human disagreement; **zero rows were unanimous
against the key** (the preregistered stop condition), and adjudication
found **no fixture or key bugs** — every disagreement is derivable from the
contract:

- One row (a self-written note restating a promise recorded canonically in
  another record) drew two of three humans into double-counting the copy.
  The one reader who matched the key flagged it ambiguous. Finding: humans
  double-count copies rather than hunt originals — the v1 "no cross-search"
  behavior reduced from five rows to one. Instrument follow-up: add a
  copies practice item.
- Four remaining disagreements were individual slips (missed in-record
  fulfillment above a quote; staged-but-not-delivered read as done; missed
  in-thread closure; a cc'd third-party dependency read as a waiting item).
  The last one is noted as a product idea (a dependency surface), not an
  obligation-contract change.

## Documented deviation

`PREREGISTRATION.md` requires two cold lay readers. The round closed with
one cold lay reader plus the blind custodian as the second cold reader. The
custodian was fully key-blind (sealed before any row-level material was
disclosed) but is the product's author and is not a lay reader. Custodian
decision, 2026-07-23. Every claim derived from this round carries this
qualification.

## Carried forward

- v7 instrument: copies practice item.
- Product note: cc'd-dependency items are not obligations; possible
  separate surface.
- Running the product against this corpus (typed red rows, product-target
  metrics) is intentionally out of scope for this round.
