package google

import "time"

// Strict RFC 3339 validation for provider-supplied timestamps (#218).
//
// This connector must NOT import internal/mora (mora imports google — see the
// note at the top of types.go), so the identical gate that guards Mora's browse
// timestamps (`rfc3339Instant`/`strictRFC3339` in internal/mora/recency.go) is
// duplicated here rather than shared. The two are deliberately kept equivalent:
// a stamp this package refuses to persist is exactly a stamp that package would
// refuse to publish. Change one and change the other.
//
// The reason a strict gate is needed on the WRITE side, where the value is
// re-rendered rather than passed through, is that Go's tolerance shows up as a
// wrong instant instead of a malformed string. `time.Parse(time.RFC3339, …)` on
// the pinned toolchain (go 1.25.8) accepts a one-digit hour (`…T1:12:34Z`), an
// offset minute of 60 (`…+00:60`, `…-00:60`, silently carried into the next
// hour), an offset hour of 24 (`…+24:00`), and a comma fractional separator
// (`…:00,5Z`). Normalizing any of those to UTC yields a perfectly well-formed
// timestamp that is off by an hour or a day, and nothing downstream can detect
// it. Provider text that is not RFC 3339 is not evidence of a time.

// rfc3339Instant reports the instant s denotes, and whether s is a valid RFC 3339
// date-time at all. Empty and malformed are the same answer — neither is evidence
// of anything — and callers omit the value rather than persist a guess.
//
// The grammar is checked by strictRFC3339 BEFORE time.Parse is allowed to
// interpret anything; time.Parse then runs only to reject a syntactically
// well-formed but non-existent calendar date (`2026-02-30`) and to produce the
// instant.
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
// The `T` and `Z` literals must be UPPERCASE, and a leap second (`:60`) is
// rejected — both match what Go's RFC3339 layout accepts and what Google's API
// emits, so neither can reject a timestamp a healthy provider sent. A fractional
// part must be `.` followed by at least one digit; a bare `…:05.Z` is malformed,
// not a zero fraction. Any valid fraction and any valid offset is accepted
// unchanged — this gate rejects, it never rewrites.
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
// a multi-byte rune (or any other non-digit) in a fixed-width slot is a rejection
// rather than a partial read.
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
