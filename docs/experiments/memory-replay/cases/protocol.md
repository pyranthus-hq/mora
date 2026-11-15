# Qualification protocol (public)

This protocol coordinates with the frozen contract in
[`../contract/`](../contract/) ([#541](https://github.com/pyranthus-hq/mora/issues/541)).
Schema field shapes are authoritative in
`internal/mora/pilotreplay/contract`. This document describes **process** only.

## Goals

1. Qualify **one** failed coding-agent task with a checkable outcome, **or**
   record an explicit **no-go** / **prospective_capture** decision.
2. Pair it with two controls under the same material constraints:
   - **valid-memory control** — a constraint that must survive the reviewed edit;
   - **no-relevant-memory control** — a task whose success must not require the edit.
3. Produce a go/no-go packet for the bounded experiment in [#540](https://github.com/pyranthus-hq/mora/issues/540).

## Public vs private

| Surface | Allowed |
| --- | --- |
| Public repo (`cases/` package + this docs tree) | Synthetic manifests, rejection fixtures, redacted eligibility summary, this protocol |
| Private package (owner-chosen location) | Real candidate inventory, traces, dirty trees, permission records, frozen inputs |

The private package **stays outside** the public repository and live vault.
Publishing, training, and external sharing are **separate** permissions from
minimum-necessary private evaluation.

## Private inventory (Adit / authorized-owner handoff)

**Not performed in the public half.** An authorized owner should:

1. Review a small available candidate set (**roughly 5–10** incidents **if
   available**). Do **not** invent cases to meet a quota. Do **not** select only
   expected wins.
2. For each candidate, attempt recovery of: task, observed failed output,
   cutoff, repository commit + relevant dirty changes, dependencies/tool
   behavior, memory snapshot bytes, actual delivered context, model/harness
   settings, and known gaps.
3. Confirm permission for minimum-necessary private evaluation.
4. Preserve failed / unfavorable candidates in the **private** selection record.
5. Freeze inputs against the [#541](https://github.com/pyranthus-hq/mora/issues/541)
   schema when packaging. If historical bytes are missing, specify
   **prospective_capture**; a reconstruction may exercise plumbing but is
   **ineligible** for historical attribution.

Public agents must not invent real cases, commit private data, run models, or
spend money to complete this inventory.

## Go / no-go outcomes

Close [#542](https://github.com/pyranthus-hq/mora/issues/542) with exactly one:

| Outcome | Meaning | Unblocks |
| --- | --- | --- |
| **(1) Eligible** | Frozen case/control package + permission record + declared limitations | Real-case work on [#544](https://github.com/pyranthus-hq/mora/issues/544) / [#545](https://github.com/pyranthus-hq/mora/issues/545) only after this passes |
| **(2) No-go / prospective_capture** | Missing evidence explained; optional prospective capture plan | Does **not** promote dependents out of `status:blocked` |

An issue closed as no-go must **not** be interpreted as an accepted case.
Qualification never authorizes model execution, spend, or outreach.

### Public-half interim decision

Until the private packet exists, the public summary decision is
`public_protocol_only`: protocol + synthetic fixtures shipped; private go/no-go
deferred to Adit / authorized owner.

## What never goes public

- Actual traces, prompts, tool results, or vault exports from real incidents
- Private repository identities, absolute host/vault paths, credentials
- Unredacted permission documents beyond a boolean / status enum in the summary
- Claims that memory **caused** the failure or that a repair **helps**

Closure records controls, exclusions, missing inputs, and eligibility. It never
asserts causation or efficacy.

## Offline manifest checklist

Before treating a (private) case as packaging-complete:

- [ ] Required bytes and hashes present (snapshot, dirty files, delivered parts)
- [ ] Temporal cutoff set; post-cutoff forbidden to contender/repair
- [ ] Repository reconstruction (commit + relevant dirty) recorded
- [ ] Memory snapshot **≠** delivered context (separate ids and hashes)
- [ ] Rejection paths exercised in public fixtures: missing delivered context,
      missing dirty files, unknown permissions, post-cutoff repair evidence
- [ ] Hidden outcome material stored separately from contender/repair inputs
      (checker sensitivity is [#544](https://github.com/pyranthus-hq/mora/issues/544))

Public synthetic fixtures under `internal/mora/pilotreplay/cases/` exercise this
checklist without real incidents.

## Handoff note (copy for PR / issue)

> Private inventory of 5–10 candidates + permission review needs Adit /
> authorized owner. Do not invent cases to meet quota. Public PR closure =
> protocol + fixtures only; go/no-go for the private packet is a separate
> decision. Private package stays outside the public repo.
