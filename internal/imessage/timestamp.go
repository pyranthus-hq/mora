package imessage

import "time"

// cocoaUnixOffsetSec is the Unix-epoch offset of the Apple Cocoa epoch
// (2001-01-01 00:00:00 UTC). message.date in chat.db is measured from this epoch.
const cocoaUnixOffsetSec = 978307200

// cocoaEpochToTime converts a chat.db message.date value to a real time.Time.
//
// Modern macOS stores NANOSECONDS since the Cocoa epoch; older macOS stores
// SECONDS. Magnitude-detect: a nanosecond value for any plausible date is ~10^18,
// while a seconds value is ~10^8-10^9. The 10^12 threshold sits well above any
// plausible seconds value (10^12 s ≈ year 33700) and well below any plausible ns
// value (10^12 ns ≈ 17 minutes past the Cocoa epoch), cleanly splitting the eras.
//
// raw == 0 returns the zero time.Time — never a fabricated date.
func cocoaEpochToTime(raw int64) time.Time {
	if raw == 0 {
		return time.Time{}
	}
	var sec, nsec int64
	if raw > 1_000_000_000_000 { // nanoseconds era
		sec = raw / 1_000_000_000
		nsec = raw % 1_000_000_000
	} else { // seconds era (legacy macOS)
		sec = raw
	}
	return time.Unix(sec+cocoaUnixOffsetSec, nsec).UTC()
}

// timeToCocoaNanos is the inverse used to build the SQL lookback cutoff: it
// converts a real time.Time into Cocoa-epoch nanoseconds for `WHERE m.date >= ?`.
// A zero time maps to 0 (open-ended lookback / all-time).
func timeToCocoaNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return (t.Unix() - cocoaUnixOffsetSec) * 1_000_000_000
}
