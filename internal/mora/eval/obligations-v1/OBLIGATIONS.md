# Obligation contract, version 1

This document defines the labels used by Mora's rendered-output exam. A reader must be able to decide every label from this contract and the rendered memories alone. The ledger is not allowed to redefine these terms to fit what the current implementation happens to extract.

Prior-art note: the single-source ledger, deterministic regeneration, recorded hashes, and forbidden-real-domain checks use AMD GAIA issue #848 and its historical `tests/fixtures/email/generate_mbox.py` at commit `0c197ef3` (MIT) as design evidence only. Mora imports no GAIA code or corpus.

## What counts as an obligation

An obligation is a concrete action that one person has undertaken, been asked, or been assigned to perform for another person. The evidence must identify an action and enough context to determine who owes it and who benefits from, requested, or is waiting on it. A polite request can be an obligation even without a question mark. A statement can be an obligation when it clearly commits a person to a future action.

An obligation can be open, closed, superseded, or duplicated. Those labels describe the same underlying obligation at different stages; they do not turn irrelevant prose into an obligation.

Vague intentions, aspirations, status descriptions, observations, and facts without an accountable next action are not obligations. An introductory fragment such as "one more thing" is not independently an obligation when the actionable sentence follows it.

## Owner and counterparty

The owner is the person who owes the action. The counterparty is the person to whom the action is owed: normally the requester, recipient, beneficiary, or person waiting for completion.

The grammatical speaker is not automatically the owner. "Can you send the draft?" is owned by the addressee, not the sender. "I will send the draft" is owned by the speaker. A message describing an action assigned to a third person is owned by that third person, even when the message is sent to the user.

Self-spoken text is evidence of a self-owned obligation only when it contains an actual commitment. It must not be treated as an inbound request merely because it appears in a conversation with somebody else.

## Direction

Direction is stated from the user's point of view:

- `owed_by_self`: the user is the owner.
- `owed_by_counterparty`: somebody else is the owner and the user is the counterparty.

Direction follows the owner, not the channel, sender, meeting section, or surface on which the obligation appears. If the owner is not the user, labeling the obligation as `owed_by_self` is wrong even when the user received the message.

## Due time

An obligation is explicitly due when its evidence gives a calendar date or timestamp. It is relatively due when the evidence gives a time relative to the message, event, or another stated anchor, such as "tomorrow", "by Friday", or "before the review". It has no due time when the evidence contains no defensible deadline.

A phrase is not converted into an invented timestamp. Relative due language remains relative unless its anchor makes a precise time mechanically resolvable. Urgency, importance, or recency alone does not create a due date.

## Lifecycle

An obligation is `open` after it is made or requested and before trustworthy evidence says it no longer remains due.

An obligation is `closed` when later evidence confirms that the promised action was completed, delivered, cancelled with acceptance, or otherwise discharged. A thank-you or acknowledgement closes an obligation only when it clearly refers to the completed action. Closure evidence may be in a different channel from the opening evidence.

An obligation is `superseded` when later evidence replaces it with a materially different action, owner, counterparty, or due term. The replacement is a new or revised obligation; the superseded version must not be presented as current.

Transitions are ordered by time. The ledger's terminal state must agree with its last transition. Silence, age, or disappearance from a later message does not close an obligation.

## Duplicates

Two pieces of evidence are duplicates when they refer to the same underlying action, owner, counterparty, and lifecycle, even if they appear in different memories or channels. A duplicate points to one canonical obligation and must not create a second current brief item.

Similar wording is not enough: two separately owed deliveries remain two obligations. A forwarded or quoted copy of the same ask is normally duplicate evidence, not a new obligation.

## Evidence and attribution

Every label points to a specific subject or body region in one rendered memory. The quoted evidence must itself support the label; nearby names or unrelated text cannot supply missing ownership or action semantics. Later transition evidence points to its own rendered region and may cross channels.

An obligation must be attributed through a real relationship in the source metadata, such as sender, recipient, participant, attendee, or organizer. A name appearing only in prose is not enough to make that memory evidence for the named person.

## Non-obligations

The following classes must never surface as obligation lines by themselves:

- `footer`: legal notices, confidentiality notices, unsubscribe text, company boilerplate, and repeated mail footers.
- `marketing`: promotional calls to action, newsletters, sales language, and generic invitations not directed as a personal accountable ask.
- `notification`: automated RSVP, delivery, scheduling, membership, and service-status messages that report an event without assigning a human action.
- `url_shard`: a URL, meeting link, tracking token, path fragment, or other link residue without an independently stated obligation.
- `self_spoken`: the user's own conversational text when it is not a commitment, including questions or acknowledgements that assign nothing to the user.
- `lead_in`: fragments that introduce or connect to an actionable sentence but do not themselves specify the action.
- `bystander`: a real action assigned to or owed between third parties when the user is merely copied, informed, or mentioned. A third-party obligation can exist in the ledger while still being forbidden from the user's brief.
- `trivia`: facts, greetings, opinions, logistics, social remarks, and status details with no accountable future action.

Quoted replies, forwarded material, signatures, disclaimers, and footers may contain obligation-looking words. Their presence is intentional negative evidence. The authored source of the action and its lifecycle determine the label; copying the words does not make a new obligation.

## Surface expectation

An open, non-duplicate obligation is expected on a surface only when the ledger explicitly names that surface. Closed, superseded, duplicate, third-party-only, and non-obligation evidence must not appear as a current user obligation unless a test intentionally identifies the resulting leak.

The daily and meeting surfaces may project different fields, but projection does not change the gold owner, direction, due information, lifecycle, closure, or evidence source. A missing product field is reported as missing or unknown; the ledger must not infer a value from section placement merely to make the current product look complete.
