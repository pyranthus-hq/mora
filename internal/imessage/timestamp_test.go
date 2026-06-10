package imessage

import (
	"testing"
	"time"
)

func TestCocoaEpoch(t *testing.T) {
	const cocoaUnixOffset = 978307200 // 2001-01-01 UTC in Unix seconds

	t.Run("zero raw yields zero time (no fabricated date)", func(t *testing.T) {
		got := cocoaEpochToTime(0)
		if !got.IsZero() {
			t.Fatalf("cocoaEpochToTime(0) = %v, want zero time", got)
		}
	})

	t.Run("nanoseconds era converts to a correct real date", func(t *testing.T) {
		// A real-shaped modern message.date: 2024-03-15T12:30:45.123456789Z.
		want := time.Date(2024, 3, 15, 12, 30, 45, 123456789, time.UTC)
		secSinceCocoa := want.Unix() - cocoaUnixOffset
		raw := secSinceCocoa*1_000_000_000 + int64(want.Nanosecond())
		got := cocoaEpochToTime(raw)
		if !got.Equal(want) {
			t.Fatalf("cocoaEpochToTime(%d) = %v, want %v", raw, got, want)
		}
		if got.Year() < 2001 {
			t.Fatalf("decoded year %d is before the Cocoa epoch", got.Year())
		}
	})

	t.Run("seconds era converts to a correct real date (legacy macOS)", func(t *testing.T) {
		// Legacy macOS stored seconds since the Cocoa epoch.
		want := time.Date(2014, 7, 4, 9, 0, 0, 0, time.UTC)
		raw := want.Unix() - cocoaUnixOffset // seconds-era magnitude (~10^8)
		got := cocoaEpochToTime(raw)
		if !got.Equal(want) {
			t.Fatalf("cocoaEpochToTime(%d) = %v, want %v", raw, got, want)
		}
	})
}
