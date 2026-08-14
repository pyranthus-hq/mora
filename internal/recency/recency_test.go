package recency

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"regexp"
	"testing"
	"time"
)

func recencyFutureEvent() memory.Memory {
	return memory.Memory{
		ID: "cal_event_fixture", Scope: "global", Type: "event", Title: "Chelsea fixture",
		Source: "calendar", CreatedAt: "2027-01-14T15:00:00Z", Provider: "calendar",
		ProviderID: "primary/abc", ContentHash: "h-cal", LastSynced: "2026-07-29T10:00:00Z",
		Text: "When: 2027-01-14T15:00:00Z", Meta: map[string]any{"occurred_at": "2027-01-14T15:00:00Z"},
	}
}

func recencyGmailThread() memory.Memory {
	return memory.Memory{
		ID: "gmail_thread_fixture", Scope: "global", Type: "email", Title: "Re: contract",
		Source: "gmail", CreatedAt: "2026-06-01T09:00:00Z", Provider: "gmail",
		ProviderID: "t1", ContentHash: "h-mail", LastSynced: "2026-07-31T08:00:00Z",
		Text: "From: neil@example.com\n\nbody",
		Meta: map[string]any{
			"occurred_at": "2026-06-01T09:00:00Z",
			"messages": []map[string]string{
				{"message_ref": "t1#0", "at": "2026-05-20T09:00:00Z"},
				{"message_ref": "t1#1", "at": "2026-06-01T09:00:00Z"},
			},
		},
	}
}

func recencyLocalNote() memory.Memory {
	return memory.Memory{
		ID: "mem_local_note", Scope: "global", Type: "insight", Title: "Local note",
		Source: "mcp", CreatedAt: "2026-07-30T12:00:00Z", Text: "a durable note",
	}
}

func recencyPDFAttachment() memory.Memory {
	return memory.Memory{
		ID: "att_deck_fixture", Scope: "global", Type: "source", Title: "deck.pdf",
		Source: "/tmp/deck.pdf", CreatedAt: "2027-01-14T15:00:00Z", Provider: "gmail",
		ProviderID: "t1", ContentHash: "h-att", Text: "slide text",
	}
}

func recencyIMessageChat() memory.Memory {
	return memory.Memory{
		ID: "imessage_chat_fixture", Scope: "global", Type: "imessage", Title: "Riya",
		Source: "imsg", CreatedAt: "2026-07-28T18:00:00Z", Provider: "imessage",
		ProviderID: "c1", ContentHash: "h-imsg", LastSynced: "2026-07-28T19:00:00Z",
		Text: "Riya: see you then", Meta: map[string]any{"occurred_at": "2026-07-28T18:00:00Z"},
	}
}

func recencyCorruptSyncEvent() memory.Memory {
	return memory.Memory{
		ID: "cal_corrupt_fixture", Scope: "global", Type: "event", Title: "Corrupt sync stamp",
		Source: "calendar", CreatedAt: "2027-03-02T15:00:00Z", Provider: "calendar",
		ProviderID: "primary/corrupt", ContentHash: "h-corrupt", LastSynced: "2026-07-30 10:00:00",
		Text: "When: 2027-03-02T15:00:00Z", Meta: map[string]any{"occurred_at": "2027-03-02T15:00:00Z"},
	}
}

func recencyGoogleEventWithCreation() memory.Memory {
	return memory.Memory{
		ID: "cal_created_fixture", Scope: "global", Type: "event", Title: "Board offsite",
		Source: "calendar", CreatedAt: "2027-02-10T16:00:00Z", Provider: "calendar",
		ProviderID: "primary/offsite", ContentHash: "h-offsite", LastSynced: "2026-07-27T09:00:00Z",
		Text: "When: 2027-02-10T16:00:00Z",
		Meta: map[string]any{
			"occurred_at":       "2027-02-10T16:00:00Z",
			"source_created_at": "2026-07-26T11:30:00Z",
		},
	}
}

// rfc3339Grammar is RFC 3339 §5.6 `date-time` transcribed straight from the ABNF
// into a regexp, and it is the INDEPENDENT oracle the strict gate is checked
// against below — deliberately a different mechanism from the hand-rolled
// scanner, so a mistake has to be made twice to pass. It also encodes the two
// documented restrictions the scanner takes beyond the bare ABNF (uppercase `T`
// and `Z` only; no leap second), because those are the intended behavior and an
// oracle that disagreed about them would be testing the wrong contract.
//
//	full-date   = 4DIGIT "-" 2DIGIT "-" 2DIGIT
//	full-time   = 2DIGIT ":" 2DIGIT ":" 2DIGIT [ "." 1*DIGIT ] time-offset
//	time-offset = "Z" / ( ("+" / "-") 2DIGIT ":" 2DIGIT )
//
// It cannot express calendar validity (which days a month has), which is exactly
// the part strictRFC3339 leaves to time.Parse — so it is the oracle for
// strictRFC3339, not for rfc3339Instant.
var rfc3339Grammar = regexp.MustCompile(
	`^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])` +
		`T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(\.\d+)?` +
		`(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$`)

// rfc3339GoTolerates are the strings the PINNED toolchain's
// `time.Parse(time.RFC3339, …)` accepts even though RFC 3339 does not. They are
// the whole reason a syntax gate exists ahead of it, so they are pinned as a set:
// if a future Go tightens up, this test says so out loud rather than letting the
// gate quietly become dead code.
//
// The hour case exists because Go's RFC3339 layout spells the hour `15`, its
// NON-padded verb, so one digit satisfies it; the offset cases exist because Go
// range-checks the offset loosely and then folds `+00:60` into an ordinary
// ±01:00, which is invisible to any check made on the parsed result.
var rfc3339GoTolerates = []struct{ stamp, why string }{
	{"2026-07-31T1:12:34Z", "one-digit hour"},
	{"2026-07-31T01:12:34+00:60", "offset minute 60, folded into +01:00"},
	{"2026-07-31T01:12:34-00:60", "offset minute 60, folded into -01:00"},
	{"2026-07-31T01:12:34+24:00", "offset hour 24"},
	{"2026-07-31T01:12:34-24:00", "offset hour -24"},
	{"2026-07-31T01:12:34,5Z", "comma fractional separator"},
}

// rfc3339Sabotage is the corpus of malformed stamps every derived field is
// attacked with below. Each is a shape a hand edit, a truncated write, an older
// binary, or a non-conforming provider can plausibly leave on disk, and NONE of
// them may reach a consumer: the field is omitted instead.
var rfc3339Sabotage = []struct{ stamp, why string }{
	{"2027-01-14T5:00:00Z", "one-digit hour"},
	{"2027-01-14T15:00:00+00:60", "offset minute 60"},
	{"2027-01-14T15:00:00-00:60", "negative offset minute 60"},
	{"2027-01-14T15:00:00+24:00", "offset hour 24"},
	{"2027-01-14T15:00:00-24:00", "negative offset hour 24"},
	{"2027-01-14T15:00:00,500Z", "comma fractional separator"},
	{"2027-01-14T15:00:00.Z", "fractional dot with no digits"},
	{"2027-01-14T15:00:00.+00:00", "fractional dot with no digits before an offset"},
	{"2027-01-14T15:00:00.", "trailing dot and no zone at all"},
	{"2027-01-14T15:00:00", "no zone at all"},
	{"2027-01-14T15:00:00+0100", "offset without its colon"},
	{"2027-01-14T15:00:00+1:00", "one-digit offset hour"},
	{"2027-01-14T15:00:00+00:0", "one-digit offset minute"},
	{"2027-01-14T15:00:00z", "lowercase zone designator"},
	{"2027-01-14t15:00:00Z", "lowercase date/time separator"},
	{"2027-01-14 15:00:00Z", "space instead of T"},
	{"2027-01-14T15:00:00Z ", "trailing byte after a valid stamp"},
	{"2027-01-14T15:00:60Z", "leap second"},
	{"2027-02-30T15:00:00Z", "well-formed but not a real calendar date"},
	{"2027-01-14", "date only"},
	{"yesterday", "not a timestamp at all"},
	{"", "empty"},
}

// TestStrictRFC3339IsTheGrammarNotTimeParse pins the validation seam itself
// (#218). Every derived browse field is published VERBATIM, so the question the
// gate answers is not "can Go make an instant out of this" but "is this a string
// the consumer's parser will also accept" — and those differ. `time.Parse` alone
// is not an acceptable oracle anywhere in this file, so the checks here are
// against an explicit transcription of the ABNF.

func TestStrictRFC3339IsTheGrammarNotTimeParse(t *testing.T) {
	// Valid forms that must survive untouched: the gate rejects, it never rewrites,
	// so a legal offset or fraction has to come back exactly as it went in.
	t.Run("valid forms are accepted and preserved byte-for-byte", func(t *testing.T) {
		for _, s := range []string{
			"2026-07-31T01:12:34Z",
			"2026-07-31T00:00:00Z",
			"2026-07-31T23:59:59Z",
			"2026-07-31T01:12:34.5Z",
			"2026-07-31T01:12:34.000000001Z",
			"2026-07-31T01:12:34.123456789012345Z", // more precision than Go keeps
			"2026-07-31T01:12:34-04:00",
			"2026-07-31T01:12:34+05:30",
			"2026-07-31T01:12:34+23:59",
			"2026-07-31T01:12:34-23:59",
			"2026-07-31T01:12:34+00:00",
			"2026-07-31T01:12:34-00:00",
			"2026-07-31T01:12:34.75-04:00",
			"2028-02-29T12:00:00Z", // leap day
		} {
			if !strictRFC3339(s) {
				t.Errorf("strictRFC3339(%q) = false, want true — a legal stamp was rejected", s)
				continue
			}
			if _, ok := rfc3339Instant(s); !ok {
				t.Errorf("rfc3339Instant(%q) rejected a legal stamp", s)
			}
			// The published value is the input, never a re-render: an offset or a
			// fraction Mora was handed is the offset or fraction it hands on.
			m := memory.Memory{ID: "m", Type: "event", Meta: map[string]any{"occurred_at": s}}
			if got, ok := eventStartOf(m); !ok || got != s {
				t.Errorf("eventStartOf published %q (ok=%v), want the input %q unchanged", got, ok, s)
			}
		}
	})

	// The gap this commit closes, stated as a fact about the toolchain: these parse
	// and are still not RFC 3339, so the gate must reject them.
	t.Run("forms time.Parse tolerates are still rejected", func(t *testing.T) {
		for _, tc := range rfc3339GoTolerates {
			if _, err := time.Parse(time.RFC3339, tc.stamp); err != nil {
				t.Errorf("time.Parse now rejects %q (%s) — this toolchain no longer needs the gate for it; re-check the seam's doc comment", tc.stamp, tc.why)
			}
			if rfc3339Grammar.MatchString(tc.stamp) {
				t.Errorf("the ABNF oracle accepted %q (%s) — the oracle is wrong", tc.stamp, tc.why)
			}
			if strictRFC3339(tc.stamp) {
				t.Errorf("strictRFC3339(%q) = true (%s), want false — a stamp Go tolerates would be republished verbatim", tc.stamp, tc.why)
			}
			if _, ok := rfc3339Instant(tc.stamp); ok {
				t.Errorf("rfc3339Instant accepted %q (%s)", tc.stamp, tc.why)
			}
		}
	})

	// Every sabotage shape, judged against the ABNF transcription rather than
	// against Go. `2027-02-30` is the one case the regexp cannot settle: it is
	// syntactically perfect and simply is not a date, which is deliberately
	// time.Parse's half of the job.
	t.Run("sabotage corpus agrees with the ABNF transcription", func(t *testing.T) {
		for _, tc := range rfc3339Sabotage {
			if got, want := strictRFC3339(tc.stamp), rfc3339Grammar.MatchString(tc.stamp); got != want {
				t.Errorf("strictRFC3339(%q) = %v but the ABNF oracle says %v (%s)", tc.stamp, got, want, tc.why)
			}
			if _, ok := rfc3339Instant(tc.stamp); ok {
				t.Errorf("rfc3339Instant(%q) = ok (%s), want rejected", tc.stamp, tc.why)
			}
		}
		if !strictRFC3339("2027-02-30T15:00:00Z") {
			t.Error("2027-02-30T15:00:00Z is syntactically valid; the calendar check belongs to time.Parse, not the syntax gate")
		}
	})

	// Nothing in the corpus, valid or not, may panic the byte-indexed scanner.
	t.Run("no input can panic the scanner", func(t *testing.T) {
		for _, s := range []string{
			"", "Z", "2026", "2026-07-31T", "2026-07-31T01:12:3",
			"2026-07-31T01:12:34", "2026-07-31T01:12:34+", "2026-07-31T01:12:34+0",
			"2026-07-31T01:12:34.", "2026-07-31T01:12:34.1", "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
			"20é26-07-31T01:12:34Z", "2026-07-31T01:12:34é",
			"2026-07-31T01:12:34ééé",
		} {
			if strictRFC3339(s) {
				t.Errorf("strictRFC3339(%q) = true, want false", s)
			}
		}
	})
}

func TestIndexedAtIsNeverFabricated(t *testing.T) {
	cases := []struct {
		name  string
		mem   memory.Memory
		want  string
		wantK bool
	}{
		{
			name:  "connector memory uses its ingest write stamp",
			mem:   recencyFutureEvent(),
			want:  "2026-07-29T10:00:00Z",
			wantK: true,
		},
		{
			name:  "locally minted memory has created_at as its write clock",
			mem:   recencyLocalNote(),
			want:  "2026-07-30T12:00:00Z",
			wantK: true,
		},
		{
			name: "provider memory without last_synced has no honest clock",
			mem:  recencyPDFAttachment(),
		},
		{
			name: "empty memory claims nothing",
			mem:  memory.Memory{ID: "bare"},
		},
		{
			// The dangerous case: created_at parses, so a fallback would look like a
			// fix. It is the event time.
			name: "unparseable last_synced is unknown, not a reason to use created_at",
			mem:  recencyCorruptSyncEvent(),
		},
		{
			name: "unparseable created_at on a local memory is unknown",
			mem:  memory.Memory{ID: "mem_bad_local", Source: "mcp", CreatedAt: "2026-07-30 12:00:00"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, ok := indexedAtOf(tc.mem)
			if ok != tc.wantK || got != tc.want {
				t.Fatalf("indexedAtOf = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantK)
			}
			// Whatever it reports, it is never the event occurrence time of a
			// connector memory.
			if ok && tc.mem.Provider != "" && got == tc.mem.CreatedAt {
				t.Fatalf("indexedAtOf returned the connector occurrence time %q as an indexing time", got)
			}
		})
	}
}

func TestBrowseRowsSplitConflatedTimestamps(t *testing.T) {
	cases := []struct {
		name              string
		mem               memory.Memory
		wantEventStart    string
		wantSourceCreated string
		wantIndexedAt     string
	}{
		{
			name:           "calendar event exposes its start, not a provider creation time",
			mem:            recencyFutureEvent(),
			wantEventStart: "2027-01-14T15:00:00Z",
			wantIndexedAt:  "2026-07-29T10:00:00Z",
		},
		{
			name:              "gmail thread exposes its opening message as source creation",
			mem:               recencyGmailThread(),
			wantSourceCreated: "2026-05-20T09:00:00Z",
			wantIndexedAt:     "2026-07-31T08:00:00Z",
		},
		{
			name:          "imessage chat has neither an event start nor a creation time",
			mem:           recencyIMessageChat(),
			wantIndexedAt: "2026-07-28T19:00:00Z",
		},
		{
			name:          "local note carries only its write clock",
			mem:           recencyLocalNote(),
			wantIndexedAt: "2026-07-30T12:00:00Z",
		},
		{
			name: "pdf attachment carries none of the three",
			mem:  recencyPDFAttachment(),
		},
		{
			name:              "google calendar event exposes creation and start as different instants",
			mem:               recencyGoogleEventWithCreation(),
			wantEventStart:    "2027-02-10T16:00:00Z",
			wantSourceCreated: "2026-07-26T11:30:00Z",
			wantIndexedAt:     "2026-07-27T09:00:00Z",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rows := decorateBrowseRecency([]memory.Memory{tc.mem})
			if len(rows) != 1 {
				t.Fatalf("decorateBrowseRecency returned %d rows, want 1", len(rows))
			}
			got := rows[0]
			if got.EventStart != tc.wantEventStart {
				t.Fatalf("event_start = %q, want %q", got.EventStart, tc.wantEventStart)
			}
			if got.SourceCreatedAt != tc.wantSourceCreated {
				t.Fatalf("source_created_at = %q, want %q", got.SourceCreatedAt, tc.wantSourceCreated)
			}
			if got.IndexedAt != tc.wantIndexedAt {
				t.Fatalf("indexed_at = %q, want %q", got.IndexedAt, tc.wantIndexedAt)
			}
			// Backward compatibility: created_at is untouched by the split.
			if got.CreatedAt != tc.mem.CreatedAt {
				t.Fatalf("created_at changed: %q, want the persisted %q", got.CreatedAt, tc.mem.CreatedAt)
			}
		})
	}
}

func TestDerivedTimestampsOmitMalformedStamps(t *testing.T) {
	corruptGmailSourceCreated := recencyGmailThread()
	corruptGmailSourceCreated.ID = "gmail_bad_source_created"
	corruptGmailSourceCreated.Meta["source_created_at"] = "26-05-20"

	corruptGmailOpener := recencyGmailThread()
	corruptGmailOpener.ID = "gmail_bad_opener"
	corruptGmailOpener.Meta["messages"] = []map[string]string{
		{"message_ref": "t1#0", "at": "May 20 2026"},
		{"message_ref": "t1#1", "at": "2026-06-01T09:00:00Z"},
	}

	emptySourceCreated := recencyGmailThread()
	emptySourceCreated.ID = "gmail_empty_source_created"
	emptySourceCreated.Meta["source_created_at"] = ""

	nonStringSourceCreated := recencyGmailThread()
	nonStringSourceCreated.ID = "gmail_nonstring_source_created"
	nonStringSourceCreated.Meta["source_created_at"] = 1753500000

	commaFraction := recencyFutureEvent()
	commaFraction.ID = "cal_comma_fraction"
	commaFraction.Meta["occurred_at"] = "2027-01-14T15:00:00,500Z"

	badOffset := recencyFutureEvent()
	badOffset.ID = "cal_bad_offset"
	badOffset.LastSynced = "2026-07-29T10:00:00+24:00"

	corruptEventStart := recencyFutureEvent()
	corruptEventStart.ID = "cal_bad_start"
	corruptEventStart.Meta["occurred_at"] = "2027-01-14 15:00"

	corruptLocalWrite := recencyLocalNote()
	corruptLocalWrite.ID = "mem_bad_created"
	corruptLocalWrite.CreatedAt = "yesterday"

	cases := []struct {
		name              string
		mem               memory.Memory
		wantEventStart    string
		wantSourceCreated string
		wantIndexedAt     string
	}{
		{
			name:          "malformed occurred_at yields no event start",
			mem:           corruptEventStart,
			wantIndexedAt: "2026-07-29T10:00:00Z",
		},
		{
			// The fall-through is the trap: msgs[0].At parses and is sitting right
			// there, but it answers "when did this thread start", not "when was this
			// object created at its provider".
			name:          "malformed source_created_at does not fall through to the opening message",
			mem:           corruptGmailSourceCreated,
			wantIndexedAt: "2026-07-31T08:00:00Z",
		},
		{
			// Nor does a bad opener slide to the next message in the thread.
			name:          "malformed opening message time yields no source creation",
			mem:           corruptGmailOpener,
			wantIndexedAt: "2026-07-31T08:00:00Z",
		},
		{
			// created_at parses and is months newer — and is the EVENT time. Borrowing
			// it is exactly the bug #218 removed.
			name:           "malformed provider last_synced yields no indexed_at and never borrows created_at",
			mem:            recencyCorruptSyncEvent(),
			wantEventStart: "2027-03-02T15:00:00Z",
		},
		{
			name: "malformed created_at on a local memory yields no indexed_at",
			mem:  corruptLocalWrite,
		},
		{
			// Presence, not emptiness, is the gate: a connector that recorded an
			// unreadable value has still recorded one, and the gmail opening message
			// answers a different question.
			name:          "empty source_created_at key does not fall through to the opening message",
			mem:           emptySourceCreated,
			wantIndexedAt: "2026-07-31T08:00:00Z",
		},
		{
			name:          "non-string source_created_at key does not fall through either",
			mem:           nonStringSourceCreated,
			wantIndexedAt: "2026-07-31T08:00:00Z",
		},
		{
			// time.Parse accepts a comma fractional separator; RFC 3339 does not, and
			// this value is published verbatim to a consumer whose parser may not.
			name:          "comma fractional separator is not RFC3339",
			mem:           commaFraction,
			wantIndexedAt: "2026-07-29T10:00:00Z",
		},
		{
			// Likewise an offset hour above 23, which time.Parse silently turns into a
			// fixed zone.
			name:           "out-of-range zone offset is not RFC3339",
			mem:            badOffset,
			wantEventStart: "2027-01-14T15:00:00Z",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rows := decorateBrowseRecency([]memory.Memory{tc.mem})
			got := rows[0]
			if got.EventStart != tc.wantEventStart {
				t.Fatalf("event_start = %q, want %q", got.EventStart, tc.wantEventStart)
			}
			if got.SourceCreatedAt != tc.wantSourceCreated {
				t.Fatalf("source_created_at = %q, want %q", got.SourceCreatedAt, tc.wantSourceCreated)
			}
			if got.IndexedAt != tc.wantIndexedAt {
				t.Fatalf("indexed_at = %q, want %q", got.IndexedAt, tc.wantIndexedAt)
			}
			// Whatever survived is machine-parseable — that is the whole contract.
			// Checked through the strict seam, NOT through time.Parse: time.Parse is
			// looser than RFC 3339 (see rfc3339GoTolerates), so using it as the
			// acceptance oracle would sign off on the exact strings this test exists
			// to keep off the wire.
			for _, f := range []struct{ name, val string }{
				{"event_start", got.EventStart},
				{"source_created_at", got.SourceCreatedAt},
				{"indexed_at", got.IndexedAt},
			} {
				if f.val == "" {
					continue
				}
				if !strictRFC3339(f.val) {
					t.Fatalf("%s = %q is not RFC3339 by the grammar", f.name, f.val)
				}
			}
			// created_at is never rewritten to compensate for a dropped field.
			if got.CreatedAt != tc.mem.CreatedAt {
				t.Fatalf("created_at changed: %q, want the persisted %q", got.CreatedAt, tc.mem.CreatedAt)
			}
			// A rejected derivation CLEARS the field. Decorating a row that already
			// carries values must never strand one the derivation just refused.
			poisoned := tc.mem
			poisoned.EventStart, poisoned.SourceCreatedAt, poisoned.IndexedAt = "stale", "stale", "stale"
			redone := decorateBrowseRecency([]memory.Memory{poisoned})[0]
			if redone.EventStart != tc.wantEventStart || redone.SourceCreatedAt != tc.wantSourceCreated || redone.IndexedAt != tc.wantIndexedAt {
				t.Fatalf("decorating a pre-populated row stranded a stale stamp: event_start=%q source_created_at=%q indexed_at=%q",
					redone.EventStart, redone.SourceCreatedAt, redone.IndexedAt)
			}
		})
	}
}

func TestSabotagedStampsAreOmittedFromEveryDerivedField(t *testing.T) {
	sites := []struct {
		field string
		// inject returns a memory carrying the sabotaged stamp at this site, plus the
		// two fields that must still be derivable from the rest of the record.
		inject                            func(stamp string) memory.Memory
		wantEventStart, wantSourceCreated string
		wantIndexedAt                     string
	}{
		{
			// Meta["occurred_at"] on a type:event memory is the event start.
			field: "event_start",
			inject: func(stamp string) memory.Memory {
				m := recencyFutureEvent()
				m.ID = "cal_bad_start"
				m.Meta["occurred_at"] = stamp
				return m
			},
			wantIndexedAt: "2026-07-29T10:00:00Z",
		},
		{
			// A connector that recorded a creation time Mora cannot read has still
			// recorded one: the gmail opening message must NOT be substituted.
			field: "source_created_at",
			inject: func(stamp string) memory.Memory {
				m := recencyGmailThread()
				m.ID = "gmail_bad_source_created"
				m.Meta["source_created_at"] = stamp
				return m
			},
			wantIndexedAt: "2026-07-31T08:00:00Z",
		},
		{
			// The gmail fallback path: a corrupt opener does not slide to the next
			// message in the thread, which answers a different question.
			field: "source_created_at",
			inject: func(stamp string) memory.Memory {
				m := recencyGmailThread()
				m.ID = "gmail_bad_opener"
				m.Meta["messages"] = []map[string]string{
					{"message_ref": "t1#0", "at": stamp},
					{"message_ref": "t1#1", "at": "2026-06-01T09:00:00Z"},
				}
				return m
			},
			wantIndexedAt: "2026-07-31T08:00:00Z",
		},
		{
			// The dangerous one: created_at is right there, parses, and is the EVENT
			// time. It must not be borrowed — and the memory must sort unknown too.
			field: "indexed_at",
			inject: func(stamp string) memory.Memory {
				m := recencyFutureEvent()
				m.ID = "cal_bad_sync"
				m.LastSynced = stamp
				return m
			},
			wantEventStart: "2027-01-14T15:00:00Z",
		},
		{
			// A locally minted memory's created_at IS its write clock, so corrupting
			// it leaves no write clock at all.
			field: "indexed_at",
			inject: func(stamp string) memory.Memory {
				m := recencyLocalNote()
				m.ID = "mem_bad_created"
				m.CreatedAt = stamp
				return m
			},
		},
	}

	realClock := memory.Memory{ID: "gmail_ok", Provider: "gmail", CreatedAt: "2026-01-01T00:00:00Z", LastSynced: "2026-07-31T01:00:00Z"}

	for _, site := range sites {
		for _, bad := range rfc3339Sabotage {
			site, bad := site, bad
			t.Run(fmt.Sprintf("%s/%s", site.field, bad.why), func(t *testing.T) {
				mem := site.inject(bad.stamp)
				got := decorateBrowseRecency([]memory.Memory{mem})[0]

				want := map[string]string{
					"event_start":       site.wantEventStart,
					"source_created_at": site.wantSourceCreated,
					"indexed_at":        site.wantIndexedAt,
				}
				want[site.field] = "" // the attacked field is omitted, whatever it was
				for name, val := range map[string]string{
					"event_start":       got.EventStart,
					"source_created_at": got.SourceCreatedAt,
					"indexed_at":        got.IndexedAt,
				} {
					if val != want[name] {
						t.Fatalf("%s = %q, want %q (sabotaged %s with %q — %s)", name, val, want[name], site.field, bad.stamp, bad.why)
					}
					// A surviving field is a valid instant by the grammar, and is never
					// the sabotaged text smuggled through under another name.
					if val != "" && !strictRFC3339(val) {
						t.Fatalf("%s = %q is not RFC3339 by the grammar", name, val)
					}
					if val != "" && val == bad.stamp {
						t.Fatalf("%s republished the sabotaged stamp %q", name, bad.stamp)
					}
				}
				// created_at is never rewritten to compensate for a dropped field, and
				// is never what a dropped indexed_at falls back to.
				if got.CreatedAt != mem.CreatedAt {
					t.Fatalf("created_at changed: %q, want the persisted %q", got.CreatedAt, mem.CreatedAt)
				}
				if got.IndexedAt != "" && got.IndexedAt == mem.CreatedAt && mem.Provider != "" {
					t.Fatalf("indexed_at = %q is the connector's occurrence time — the #218 substitution came back", got.IndexedAt)
				}

				// A memory whose write clock was sabotaged also SORTS unknown: it drops
				// behind every memory that has a real one, rather than ranking on the
				// event time sitting in its created_at. The row's omission and the
				// sort's verdict come from the same seam and must never disagree.
				if site.field == "indexed_at" {
					if _, ok := ingestRecencyOf(mem); ok {
						t.Fatalf("a sabotaged write clock (%q — %s) was accepted as sortable", bad.stamp, bad.why)
					}
					if byIngestRecency(mem, realClock) {
						t.Fatalf("a sabotaged write clock (%q — %s) outranked a real one", bad.stamp, bad.why)
					}
					if !byIngestRecency(realClock, mem) {
						t.Fatalf("a real write clock lost to a sabotaged one (%q — %s)", bad.stamp, bad.why)
					}
				}
			})
		}
	}
}

func TestBrowseRecencyComparesInstantsNotStrings(t *testing.T) {
	// 2026-07-30T23:00:00-04:00 == 2026-07-31T03:00:00Z, i.e. LATER than the
	// connector row below, but lexically smaller.
	local := memory.Memory{ID: "mem_local_offset", CreatedAt: "2026-07-30T23:00:00-04:00"}
	connector := memory.Memory{ID: "gmail_earlier", Provider: "gmail", CreatedAt: "2026-01-01T00:00:00Z", LastSynced: "2026-07-31T01:00:00Z"}
	if !byIngestRecency(local, connector) {
		t.Fatal("an offset-carrying local write clock lost to an earlier UTC one — the sort compared raw strings")
	}
	if byIngestRecency(connector, local) {
		t.Fatal("byIngestRecency is not antisymmetric across mixed zones")
	}
	// An unparseable stamp is not evidence of recency: it drops behind every
	// memory with a real clock.
	corrupt := memory.Memory{ID: "mem_corrupt", CreatedAt: "not-a-timestamp"}
	if byIngestRecency(corrupt, connector) {
		t.Fatal("an unparseable created_at outranked a real write clock")
	}
	if !byIngestRecency(connector, corrupt) {
		t.Fatal("a real write clock lost to an unparseable created_at")
	}
	// Same for a provider memory whose last_synced does not parse: it sorts unknown,
	// and the parseable created_at beside it (an EVENT time) does not rescue it.
	corruptSync := recencyCorruptSyncEvent()
	if byIngestRecency(corruptSync, connector) {
		t.Fatal("an unparseable last_synced outranked a real write clock")
	}
	if !byIngestRecency(connector, corruptSync) {
		t.Fatal("a real write clock lost to an unparseable last_synced")
	}
	if _, ok := ingestRecencyOf(corruptSync); ok {
		t.Fatal("an unparseable last_synced was accepted as a sortable write clock")
	}
}

func TestExportedRecencySurface(t *testing.T) {
	m := recencyFutureEvent()
	if _, ok := Instant(m.LastSynced); !ok {
		t.Fatal("instant rejected")
	}
	if got, ok := IndexedAt(m); !ok || got != m.LastSynced {
		t.Fatalf("indexed=(%q,%v)", got, ok)
	}
	if _, ok := IngestTime(m); !ok {
		t.Fatal("ingest time missing")
	}
	if !Before(m, recencyPDFAttachment()) {
		t.Fatal("known recency must precede unknown")
	}
	if got, ok := EventStart(m); !ok || got != "2027-01-14T15:00:00Z" {
		t.Fatalf("event=(%q,%v)", got, ok)
	}
	gmail := recencyGmailThread()
	if got, ok := SourceCreatedAt(gmail); !ok || got != "2026-05-20T09:00:00Z" {
		t.Fatalf("source=(%q,%v)", got, ok)
	}
	if got := Decorate([]memory.Memory{m}); len(got) != 1 || got[0].IndexedAt == "" {
		t.Fatalf("decorate=%+v", got)
	}
}
