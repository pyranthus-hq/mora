package search

import (
	"math"
	"strings"
	"testing"
	"time"
)

func testCatalog() Catalog {
	return Catalog{Normalize: func(s string) string {
		if s == "applecal" {
			return "applecalendar"
		}
		return s
	}, Known: func(s string) bool { return s == "gmail" || s == "applecalendar" || s == "filesystem" }, Types: func() []string { return []string{"applecalendar", "filesystem", "gmail"} }, Unsupported: func(s string) (string, bool) {
		if s == "filesystem" {
			return "no provider identity", true
		}
		return "", false
	}}
}
func TestParseFilterReceiptAndNormalization(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	f, err := ParseFilter(map[string]any{"source": " applecal:work ", "since_hours": float64(24)}, now, testCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if f.Source != " applecal:work " || f.SourceFamily != "applecalendar" || f.SourceInstance != "work" || f.SinceHours != 24 || f.Now != now || f.NormalizedSource() != "applecalendar:work" {
		t.Fatalf("f=%+v", f)
	}
}
func TestParseFilterFailures(t *testing.T) {
	cases := []map[string]any{{"source": 1}, {"source": ""}, {"source": "gmail:"}, {"source": "gmail:a:b"}, {"source": "unknown"}, {"source": "filesystem"}, {"since_hours": "1"}, {"since_hours": float64(0)}, {"since_hours": 1.5}, {"since_hours": float64(math.MaxInt64)}}
	for _, args := range cases {
		if _, err := ParseFilter(args, time.Now(), testCatalog()); err == nil {
			t.Errorf("args %v should fail", args)
		}
	}
}
func TestParseSourceKnownUnknownAndInstance(t *testing.T) {
	family, instance, err := ParseSource("gmail:doesnotexist", testCatalog())
	if err != nil || family != "gmail" || instance != "doesnotexist" {
		t.Fatalf("got=(%q,%q,%v)", family, instance, err)
	}
	_, _, err = ParseSource("unknown", testCatalog())
	if err == nil || !strings.Contains(err.Error(), "applecalendar, filesystem, gmail") {
		t.Fatalf("unknown error=%v", err)
	}
}
func TestParseFilterLargeSafeWindow(t *testing.T) {
	const safe = 100000
	f, err := ParseFilter(map[string]any{"since_hours": float64(safe)}, time.Now(), testCatalog())
	if err != nil || f.SinceHours != safe {
		t.Fatalf("f=%+v err=%v", f, err)
	}
}
