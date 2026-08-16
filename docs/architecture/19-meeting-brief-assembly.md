# 19 — Meeting brief assembly

`mora brief --event-id <calendar-memory-id>` builds a local brief for one
meeting. The MCP `meeting_prep` tool returns the same `MeetingBrief` shape.
Without `event_id`, it selects the next event or one that just started. This
keeps earlier behavior. Both surfaces use the same gated assembly pipeline.
They do not have separate CLI paths.

## Assembly pipeline

1. Resolve the event by exact memory/provider id. A missing, non-event, or
   ambiguous id is an error.
2. Read the event's structured attendee identities, exclude the user, and sort
   the remaining identities. The user is resolved from three sources, in order
   of authority: the event's own `self_email` (Google `Attendee.Self`, Apple
   `Participant.is_self` — the connector records which invitee IS the user),
   the mailbox each Google source was authorized on (`Source.Email`), and any
   aliases declared in `config.toml` `self_emails`. Identity is **declared,
   never inferred** — Mora will not guess self from a display name.

   **Why three sources.** The address a calendar *invites* is routinely not the
   mailbox OAuth was granted on (a Workspace alias, a custom domain, an iCloud
   address). An unrecognized alias fails self-exclusion, so the user is admitted
   as an attendee of their own meeting and their own records are cited back to
   them as the counterparty's unfinished business — wrong-person attribution,
   which is severity-1.

   If Mora cannot tell which invitee is the user, it **gaps rather than
   guesses**: any invitee could BE the user, so it attributes nothing, sets
   `self_unresolved`, and states in `gaps` how to fix it (`self_emails`). It
   does *not* error — an unattributed brief emits zero lines and is exactly as
   safe, whereas erroring would take the whole next-meeting brief down over one
   unresolvable event (`selectNextEvent` is provider-agnostic). Gapping
   suppresses the *claim*. It never destroys the *artifact*.
3. For each exact attendee identity, read the same exact-identity graph
   projection used by `get_entity`, and keep only the evidence this person is a
   **party to** — reached by a relationship edge (`PARTICIPATED_IN`, `EMAILED`,
   `ATTENDED`), never by a body-text `MENTIONS` edge. `buildGraph` emits
   `MENTIONS` only for a gazetteer name-hit on a memory the person is *not* a
   participant of, so a mention-only record is by construction a third party
   writing this person's name: a note reading *"I spoke to Neil about the pilot;
   can you follow up?"* is an ask owed to its **author**, not to Neil.
   `get_entity` correctly pools every rel (a dossier is *about* the person), but
   the brief is about the user's unfinished business **with** them, so it takes
   only the relational slice. Attributing a mention is wrong-person attribution.

   Display-name fallback is deliberately forbidden, so two people with the same
   name remain separate. Shared evidence prefers its sole attendee sender. If a
   group record cannot be assigned to exactly one attendee, it is dropped rather
   than attributed arbitrarily.
4. Select only evidence that describes the user's unfinished business:
   user-owned obligations or unanswered asks, unresolved decisions/threads,
   explicit staleness guards, and load-bearing shared work context. Personal
   trivia without an actionable relationship to the user is dropped. Rendering
   extracts the qualifying sentence/clause itself, so trivia elsewhere in an
   otherwise-actionable thread cannot ride along in the cited line. `internal/meeting` owns the pure sender-authored text and historical-framing mechanics (quote/forward/signature exclusion, URL/noise removal, hard-wrap repair, segmentation, and phrase boundaries); Mora owns identity-aware classification and selection.
5. Hydrate `forgettabilityCandidate` values and call `rankForgettability` once
   over the global cross-attendee pool. Selection is `value_micros` descending,
   then dated evidence and stable id, with a three-line per-attendee cap and a
   budget-bounded global cap. Candidates are greedily admitted by actual
   serialized `MeetingBrief` size with the MCP envelope reserve. Even the
   mandatory event-only shape must fit or assembly fails loudly. Fixed section
   order remains a presentation layer. Within each section the selected lines
   retain value order.

## Citation invariant

`CitedBriefLine` is the only renderable evidence atom. Every line contains a
`BriefCitation` with `memory_id`, `channel`, `source`, and RFC3339 `date`.
`MeetingBrief.validate` runs before human rendering and before the MCP return;
an incomplete citation rejects the whole brief before any line is emitted.
Section headings are fixed labels, not factual claims. Evidence text is a
compact extract from the cited memory, never a generated conclusion.

The dated-historical rail is independent of ranking. `newCitedBriefLine` wraps
every extract with its relative age and past-tense framing (for example,
`~10 months ago, the cited record involving Dana stated: …`). The original
extract is quoted rather than rewritten. `MeetingBrief.validate` recomputes the
required prefix from `as_of` and the citation date and fails closed if a line is
plain present-tense text. This applies equally to human rendering and the JSON
shape returned by MCP.

The event and its attendee roster are likewise carried in `CitedMeetingEvent`
with the calendar memory's citation. The explicit `egress_calls` meter is always
zero.

## The health banner (HEALTH-02)

`MeetingBrief.SourceHealth` is populated once, at build time, by
`sourceHealthAll(cfg, at)` — the SAME per-source freshness snapshot `mora doctor`
and the daily brief read (see [sync-and-freshness](./11-sync-and-freshness.md)).
`renderMeetingBrief` renders `healthBannerFromSources(brief.SourceHealth)` as the
FIRST line, even when there is no upcoming event (`brief.Event == nil`) — a
broken source is worth surfacing on its own, independent of whether a next
meeting happens to exist. Because MCP `meeting_prep` returns the `MeetingBrief`
struct directly, the health snapshot travels with it for free: no extra tool
plumbing, and an agent reading the struct can see a dead corpus without parsing
Markdown. A brief that renders confidently over stale/failed source data is a
WRONG brief — this is a correctness signal riding alongside the citations, not
an ops-only addition.

## Time, determinism, and egress

`--at <RFC3339>` injects the assembly time. Default is the process clock.
`meeting_prep` exposes the equivalent optional `at` argument. Future-dated
non-event evidence is excluded. Relative-age rendering and ranking both use this
injected instant. Ordering has explicit tie-breaks, so a fixed vault/index and
`--at` produce byte-identical JSON and human output.

Assembly reads only the local vault and embedded index. It does not sync, call a
model, use an embedding service, advance a watermark, or write state.
