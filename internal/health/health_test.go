package health

import (
	"strings"
	"testing"
	"time"
)

func TestThresholds(t *testing.T) {
	for _, typ := range []string{"gmail", "calendar", "applecalendar", "github"} {
		if Threshold(typ) != 24*time.Hour {
			t.Fatalf("%s threshold", typ)
		}
	}
	for _, typ := range []string{"imessage", "filesystem", "future"} {
		if Threshold(typ) != 48*time.Hour {
			t.Fatalf("%s threshold", typ)
		}
	}
}
func TestClassifyStates(t *testing.T) {
	now := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, typ string
		st        *Status
		want      string
		age       int
	}{{"nil", "gmail", nil, Never, 0}, {"empty", "gmail", &Status{LastError: "initial"}, Never, 0}, {"invalid", "gmail", &Status{LastSuccessAt: "bad", LastError: "raw"}, Never, 0}, {"failed-text", "gmail", &Status{LastSuccessAt: now.Add(-time.Hour).Format(time.RFC3339), LastError: "boom"}, Failed, 1}, {"failed-count", "gmail", &Status{LastSuccessAt: now.Add(-time.Hour).Format(time.RFC3339), ErrorCount: 1}, Failed, 1}, {"stale", "gmail", &Status{LastSuccessAt: now.Add(-25 * time.Hour).Format(time.RFC3339)}, Stale, 25}, {"boundary", "gmail", &Status{LastSuccessAt: now.Add(-24 * time.Hour).Format(time.RFC3339)}, Fresh, 24}, {"future", "gmail", &Status{LastSuccessAt: now.Add(time.Hour).Format(time.RFC3339)}, Fresh, 0}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify("key", tc.typ, tc.st, now)
			if got.State != tc.want || got.AgeHours != tc.age {
				t.Fatalf("got=%+v want state=%s age=%d", got, tc.want, tc.age)
			}
		})
	}
}
func TestWorstAndBanner(t *testing.T) {
	if Worst(nil) != nil || Banner([]Source{{Key: "ok", State: Fresh}}) != "" {
		t.Fatal("healthy sources alarmed")
	}
	sources := []Source{{Key: "old", State: Stale, AgeHours: 99}, {Key: "never", State: Never}, {Key: "failed-younger", State: Failed, AgeHours: 2}, {Key: "failed-older", State: Failed, AgeHours: 3, LastError: "boom"}}
	w := Worst(sources)
	if w == nil || w.Key != "failed-older" {
		t.Fatalf("worst=%+v", w)
	}
	want := "🔴 MORA HEALTH: failed-older — no successful sync for 3h (boom). Run: mora doctor"
	if got := Banner(sources); got != want {
		t.Fatalf("banner=%q want %q", got, want)
	}
	if got := Banner([]Source{{Key: "gmail", State: Never}}); got != "🔴 MORA HEALTH: gmail — never synced. Run: mora doctor" {
		t.Fatalf("never banner=%q", got)
	}
}
func TestStateRank(t *testing.T) {
	if StateRank(Failed) != 0 || StateRank(Never) != 1 || StateRank(Stale) != 2 || StateRank(Fresh) != 3 {
		t.Fatal("rank order changed")
	}
}
func TestSanitizeError(t *testing.T) {
	if got := SanitizeError(" \n one\ttwo\r "); got != "one two" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("界", BannerErrorCap+5)
	got := SanitizeError(long)
	if len([]rune(got)) != BannerErrorCap+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("runes=%d suffix=%q", len([]rune(got)), got[len(got)-3:])
	}
}
