# 19 — Meeting brief assembly

`mora brief --event-id <calendar-memory-id>` builds a local pre-meeting brief for
one calendar event. The MCP `meeting_prep` tool returns the same `MeetingBrief`
shape; without `event_id`, it selects the next or just-started event for backward
compatibility.

## Assembly pipeline

1. Resolve the event by exact memory/provider id. A missing, non-event, or
   ambiguous id is an error.
2. Read the event's structured attendee identities, exclude only configured
   self email addresses, and sort the remaining identities. If an event has
   attendees but Mora cannot identify the user's own address, assembly fails
   closed rather than risk presenting the user as another attendee.
3. For each exact attendee identity, read the same exact-identity graph
   projection used by `get_entity`. The full evidence set remains available to
   the query-time reranker; display-name fallback is deliberately forbidden, so
   two people with the same name remain separate.
4. Select only evidence that describes the user's unfinished business:
   user-owned obligations or unanswered asks, unresolved decisions/threads,
   explicit staleness guards, and load-bearing shared work context. Personal
   trivia without an actionable relationship to the user is dropped.
5. Hydrate `forgettabilityCandidate` values and call `rankForgettability` once
   over the global cross-attendee pool. Selection is `value_micros` descending,
   then dated evidence and stable id, with a three-line per-attendee cap and a
   budget-bounded global cap. Fixed section order remains a presentation layer;
   within each section the selected lines retain value order.

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

## Time, determinism, and egress

`--at <RFC3339>` injects the assembly time; default is the process clock.
`meeting_prep` exposes the equivalent optional `at` argument. Future-dated
non-event evidence is excluded. Relative-age rendering and ranking both use this
injected instant; ordering has explicit tie-breaks, so a fixed vault/index and
`--at` produce byte-identical JSON and human output.

Assembly reads only the local vault and embedded index. It does not sync, call a
model, use an embedding service, advance a watermark, or write state.
