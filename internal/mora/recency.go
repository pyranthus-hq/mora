package mora

import (
	"time"
)

// Browse recency (#218).
//
// `mora list`, MCP `list_memory`, and the no-query `context_memory` briefing all
// advertise themselves as "recent memories", but they ranked by `created_at` —
// which, for every connector memory, is the provider's OCCURRENCE time
// (`memory.MapItem` copies `Item.OccurredAt`, and `writeMappedMemory` preserves
// it across rewrites): a calendar event's start, or a thread's newest message.
// A fixture six months out therefore outranked everything Mora had actually
// learned that week, and the browse surface stopped browsing. Worse, agents read
// that single `created_at` as "when did I learn this" and stated it as fact.
//
// The helpers below give those surfaces an honest per-memory write clock to rank
// by, and split the three distinct instants `created_at` was conflating so a
// caller can say which one it means. Every one of them is derived from data Mora
// already persisted, and every one of them declines to answer rather than
// substituting a timestamp that means something else.
//
// The contract every derivation below obeys: a published `event_start`,
// `source_created_at`, or `indexed_at` is a valid RFC3339 instant, or the field
// is absent. Frontmatter is plain text on disk — a hand-edited file, a truncated
// write, or an older binary can leave a stamp that does not parse — and a
// consumer that reads these fields as machine timestamps must never be handed a
// string it cannot parse. Absence is the only honest alternative: a value that
// fails to validate does NOT fall through to a different clock, because the
// substitute would answer a different question than the field asks.

// rfc3339Instant parses a persisted stamp under that contract, reporting ok=false
// for both the empty and the unparseable case — neither is evidence of anything,
// and callers treat them identically (omit the field, sort into the unknown
// bucket). It is the single validation seam for all three derived fields, so the
// value a row publishes and the instant the sort compares can never disagree.
//
// `time.Parse(time.RFC3339, …)` alone is NOT that check. Go's RFC3339 layout is
// looser than the RFC 3339 grammar, and these fields are published VERBATIM, so
// any form Go tolerates would reach a consumer whose parser does not. Measured on
// the pinned toolchain (go 1.25.8), `time.Parse(time.RFC3339, …)` accepts at
// least:
//
//   - a ONE-DIGIT hour (`…T1:12:34Z`). The layout's hour verb is `15`, which is
//     Go's non-padded form and matches 1–2 digits; every other component uses a
//     zero-padded verb (`01`, `02`, `04`, `05`) and does require two. Go then
//     re-renders it as `01:12:34`, but the stored string is what ships.
//   - an offset MINUTE of 60 (`…+00:60`, `…-00:60`). Go silently carries it into
//     the next hour, so the parsed zone reads as a perfectly ordinary ±01:00 and
//     a range check on the RESULT cannot see the malformed input at all.
//   - an offset HOUR of 24 (`…+24:00`, `…-24:00`), which Go turns into a fixed
//     ±24h zone. RFC 3339 caps the offset hour at 23.
//   - a COMMA as the fractional-second separator (`…:00,5Z`). RFC 3339 allows
//     only `.`.
//
// So the grammar is checked directly, by strictRFC3339, BEFORE time.Parse is
// allowed to interpret anything. time.Parse then runs only to do the two jobs the
// syntax pass deliberately leaves to it: rejecting a syntactically well-formed but
// non-existent calendar date (`2026-02-30`), and producing the instant itself.
func rfc3339Instant(s string) (time.Time, bool) {
	if !strictRFC3339(s) {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// strictRFC3339 reports whether s is EXACTLY an RFC 3339 `date-time`, by the
// grammar rather than by whatever a parser happens to accept:
//
//	date-time     = full-date "T" full-time
//	full-date     = 4DIGIT "-" 2DIGIT "-" 2DIGIT
//	full-time     = 2DIGIT ":" 2DIGIT ":" 2DIGIT [ "." 1*DIGIT ] time-offset
//	time-offset   = "Z" / ( ("+" / "-") 2DIGIT ":" 2DIGIT )
//
// Every component is a FIXED digit count, every separator is a fixed literal, and
// the whole string must be consumed — a trailing byte, a missing one, or a
// one-digit hour is a rejection, not a value to be repaired. Ranges are checked
// here too (month 01–12, day 01–31, hour ≤ 23, minute ≤ 59, second ≤ 59, offset
// hour ≤ 23, offset minute ≤ 59); the day is only bounded syntactically because
// which days a month actually has is time.Parse's job in rfc3339Instant.
//
// Two deliberate restrictions beyond the bare ABNF, both matching what Mora
// itself writes (`time.Format(time.RFC3339)`) and what Go's RFC3339 layout
// accepts, so neither can reject a stamp Mora produced:
//
//   - The `T` and `Z` literals must be UPPERCASE. RFC 3339 permits lowercase via
//     ABNF's case-insensitive literals but explicitly invites applications to be
//     stricter, and a published stamp should have one spelling.
//   - A leap second (`:60`) is rejected. Go rejects it too, so accepting it here
//     would only move the failure to time.Parse; the field is omitted either way,
//     which is the safe direction of the right-or-absent contract.
//
// A fractional part must be `.` followed by at least one digit: a bare `…:05.Z`
// or a trailing `…:05.` is malformed, not a zero fraction. Any valid number of
// fractional digits, and any valid offset, is preserved unchanged — this gate
// rejects, it never rewrites.
func strictRFC3339(s string) bool {
	// "2006-01-02T15:04:05Z" — the shortest legal form, and the fixed prefix
	// through the seconds field is the same length in every form.
	if len(s) < 20 {
		return false
	}
	if _, ok := rfc3339Digits(s, 0, 4); !ok { // date-fullyear
		return false
	}
	month, ok := rfc3339Digits(s, 5, 2)
	if !ok {
		return false
	}
	day, ok := rfc3339Digits(s, 8, 2)
	if !ok {
		return false
	}
	hour, ok := rfc3339Digits(s, 11, 2)
	if !ok {
		return false
	}
	minute, ok := rfc3339Digits(s, 14, 2)
	if !ok {
		return false
	}
	second, ok := rfc3339Digits(s, 17, 2)
	if !ok {
		return false
	}
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return false
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return false
	}

	rest := s[19:]
	if len(rest) > 0 && rest[0] == '.' {
		frac := 0
		for frac+1 < len(rest) && rest[frac+1] >= '0' && rest[frac+1] <= '9' {
			frac++
		}
		if frac == 0 { // "." with nothing after it is not a fraction
			return false
		}
		rest = rest[frac+1:]
	}

	if rest == "Z" {
		return true
	}
	if len(rest) != len("+00:00") {
		return false
	}
	if rest[0] != '+' && rest[0] != '-' {
		return false
	}
	offHour, ok := rfc3339Digits(rest, 1, 2)
	if !ok {
		return false
	}
	offMinute, ok := rfc3339Digits(rest, 4, 2)
	if !ok {
		return false
	}
	return rest[3] == ':' && offHour <= 23 && offMinute <= 59
}

// rfc3339Digits reads exactly n ASCII digits at s[i:i+n]. It is byte-indexed, so
// a multi-byte rune (or any other non-digit) in a fixed-width slot is a
// rejection rather than a partial read.
func rfc3339Digits(s string, i, n int) (int, bool) {
	if i+n > len(s) {
		return 0, false
	}
	v := 0
	for _, c := range []byte(s[i : i+n]) {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	return v, true
}

// indexedAtOf reports when Mora last WROTE this memory into the vault — the
// honest "when did Mora learn this" clock — and whether that clock is knowable
// from the file at all.
//
//   - `last_synced` is stamped per item by `memory.Ingest` immediately before the
//     vault write, and reaches disk only when `writeMappedMemory` actually
//     rewrites the file (the content-hash-equal branch returns early and leaves
//     the previous value in place). The persisted value is therefore the instant
//     of the last successful write of this memory's current content — NOT a
//     sync-attempt time, which lives in the state dir on
//     `memory.SyncStatus.LastAttemptAt` and never touches a memory file.
//   - a memory with no provider was minted locally: `write_memory` / `mora write`
//     (both via `createMemory`), a `mora teach` replacement, or a filesystem
//     source file from `ingestFilesystem`. Every one of those stamps `created_at`
//     from the clock AT the write, so for them `created_at` IS the write clock.
//   - anything else — a provider memory with no `last_synced`, i.e. a PDF
//     attachment memory from `writeAttachmentMemories` (which inherits the
//     PARENT's occurrence-time `created_at`) or a file left by an older binary —
//     has no write clock on disk. It is reported as unknown instead of passing an
//     event time off as an indexing time.
//
// The chosen clock is then VALIDATED, and a malformed one is reported as unknown
// rather than published raw or replaced. A provider memory whose `last_synced`
// does not parse is exactly the case: `created_at` is right there and is a
// perfectly parseable string, but on a connector memory it is the occurrence
// time, so borrowing it would reintroduce the event-time-as-indexing-time bug
// this file exists to remove. Corruption in the write clock means the write clock
// is unknown — not that some other clock takes over.
//
// This is deliberately stricter than `observedAtOf` (`graph.go`), which falls
// back to `created_at` unconditionally: a best-effort observation time is fine
// for ranking an edge, but a timestamp an agent will quote back as "when I
// learned this" has to be right or absent.
func indexedAtOf(m Memory) (string, bool) {
	s := m.LastSynced
	if s == "" && m.Provider == "" {
		s = m.CreatedAt
	}
	if _, ok := rfc3339Instant(s); !ok {
		return "", false
	}
	return s, true
}

// ingestRecencyOf parses the indexed_at clock into the instant the browse order
// sorts on. Because it reads through indexedAtOf, a memory sorts into the unknown
// bucket for exactly the reasons its row omits `indexed_at` — there is no state
// where a memory ranks by a stamp the row declines to show, or shows a stamp the
// sort ignored.
func ingestRecencyOf(m Memory) (time.Time, bool) {
	s, ok := indexedAtOf(m)
	if !ok {
		return time.Time{}, false
	}
	return rfc3339Instant(s)
}

// byIngestRecency orders browse rows newest-WRITTEN first.
//
// It compares parsed instants rather than the raw strings because the two clocks
// are not written in the same zone: `last_synced` is always UTC (`…Z`) while a
// locally minted `created_at` carries the machine's offset (`…-04:00`), and
// lexical comparison across those two forms ranks them wrongly.
//
// Memories with no honest write clock (see indexedAtOf) sort after every memory
// that has one — unknown recency may not claim to be recent, and falling back to
// their `created_at` would put event time back in the ordering this fix exists to
// remove. Ties break on id, so the order is total and reproducible across runs
// regardless of directory-walk order.
func byIngestRecency(a, b Memory) bool {
	at, aok := ingestRecencyOf(a)
	bt, bok := ingestRecencyOf(b)
	if aok != bok {
		return aok
	}
	if aok && !at.Equal(bt) {
		return at.After(bt)
	}
	return a.ID < b.ID
}

// eventStartOf reports a calendar event's START instant.
//
// `Meta["occurred_at"]` is an event start only on an EVENT memory: the google
// (`calendar.go`) and applecal (`applecal.go`) connectors write the event's start
// there, while gmail (`gmail.go`) and imessage (`map.go`) write the thread's
// NEWEST message time under the same key. Restricting to `type: event` is what
// keeps the field from relabelling a message time as a scheduled start.
//
// Both connectors normalize the value through `time.Format(time.RFC3339)` before
// it is persisted, so a stamp that fails to parse here came from an edited or
// damaged file rather than from a connector — and is omitted, not published raw.
func eventStartOf(m Memory) (string, bool) {
	if m.Type != "event" || m.Meta == nil {
		return "", false
	}
	s, _ := m.Meta["occurred_at"].(string)
	if _, ok := rfc3339Instant(s); !ok {
		return "", false
	}
	return s, true
}

// sourceCreatedAtOf reports when the SOURCE object came into existence at its
// provider, using only timestamps Mora actually persisted:
//
//   - `Meta["source_created_at"]` when a connector recorded one directly. Google
//     Calendar does: `calEventToItem` (`internal/google/calendar.go`) normalizes
//     `calendar.Event.Created` — when the event was created at Google, a clock
//     genuinely distinct from its start — into this key. A memory ingested before
//     that landed simply has no such key and omits the field until it is
//     re-ingested; nothing here back-fills it.
//   - for a gmail thread, the `at` of the FIRST entry of `Meta["messages"]` — the
//     thread's opening message, captured in provider order from a full
//     `Threads.Get(format=full)`, which is when the thread began at Gmail.
//
// Everything else is omitted. The applecal connector exposes no trustworthy
// creation clock (the store's `creation_date` tracks the LOCAL replica's row, not
// the event's creation at its origin), and an iMessage conversation persists only
// its newest message time — so for those the value is genuinely unavailable and
// the field is left off rather than filled with a timestamp that means something
// else.
//
// An unusable `Meta["source_created_at"]` yields nothing: it does NOT fall
// through to the gmail opening-message path, which would answer "when did this
// thread start" to a row asking "when was this object created" without saying so.
// The gate is the key's PRESENCE, not its emptiness — a key persisted as `""`,
// `null`, or a non-string is a connector that recorded something unreadable, and
// silently answering from a different clock is the substitution this file
// forbids. Only an absent key means "this connector records no creation time".
func sourceCreatedAtOf(m Memory) (string, bool) {
	if m.Meta == nil {
		return "", false
	}
	if raw, present := m.Meta["source_created_at"]; present {
		s, _ := raw.(string)
		if _, ok := rfc3339Instant(s); !ok {
			return "", false
		}
		return s, true
	}
	if m.Provider == "gmail" {
		if msgs := gmailCommitmentMessages(m); len(msgs) > 0 {
			if _, ok := rfc3339Instant(msgs[0].At); ok {
				return msgs[0].At, true
			}
		}
	}
	return "", false
}

// decorateBrowseRecency returns copies of the browse rows carrying the three
// instants `created_at` used to conflate (#218): `event_start` (when the thing
// happens), `source_created_at` (when the source object was created at its
// provider), and `indexed_at` (when Mora wrote the memory into the vault). Each
// is omitted when it cannot be derived honestly OR when the persisted stamp is
// not a valid RFC3339 instant, so every field a row does carry is machine-
// parseable and an absent field reads as "Mora does not know" — never as a
// substituted stand-in. `created_at` is left byte-for-byte as it was, so existing
// consumers keep the value they have always received.
//
// It must run BEFORE snippetMemories, which drops `Meta` — the source of
// `event_start` and `source_created_at` — to bound the preview row size.
func decorateBrowseRecency(mems []Memory) []Memory {
	if mems == nil {
		return nil
	}
	out := make([]Memory, len(mems))
	for i, m := range mems {
		// Assigned unconditionally, so a failed derivation CLEARS the field rather
		// than leaving whatever the caller passed in. Nothing populates these today
		// (they are never persisted, so no read path can), but the contract belongs
		// to this function: decorating twice, or decorating an already-decorated row,
		// must not be able to strand a value the derivation just rejected.
		m.EventStart, _ = eventStartOf(m)
		m.SourceCreatedAt, _ = sourceCreatedAtOf(m)
		m.IndexedAt, _ = indexedAtOf(m)
		out[i] = m
	}
	return out
}
