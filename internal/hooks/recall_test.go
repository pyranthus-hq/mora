package hooks

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"testing"
	"time"
)

type Memory = memory.Memory

const hookRecallByteLimit = RecallByteLimit

func skipRecallPrompt(s string) bool { return SkipRecallPrompt(s) }
func formatRecallContext(m []Memory, t float64, n time.Time) string {
	return FormatRecallContext(m, t, n)
}
func recallLine(m Memory, n time.Time) string { return RecallLine(m, n) }
func clipRunes(s string, n int) string        { return ClipRunes(s, n) }
func memoryAge(s string, n time.Time) string  { return MemoryAge(s, n) }
func TestHk_SkipRecallPromptStopwordAfterLength(t *testing.T) {
	if !skipRecallPrompt("      continue      ") {
		t.Fatal("a padded stopword must still be skipped via the trim/lowercase switch")
	}
	// A genuine question of the same length must NOT be skipped (the default arm).
	if skipRecallPrompt("what should we do about the migration") {
		t.Fatal("a real prompt must not be skipped")
	}
	// Long slash-command still skipped by the leading-slash guard.
	if !skipRecallPrompt("/compact the whole conversation now") {
		t.Fatal("a slash command must be skipped")
	}
}

func TestHk_FormatRecallContextSkipsBlankLine(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	mems := []Memory{
		{ID: "blank", Source: "test", CreatedAt: now.Format(time.RFC3339), Score: -1}, // no text/title -> blank line
		{ID: "real", Title: "Real one", Source: "test", CreatedAt: now.Format(time.RFC3339), Text: "keep me", Score: -1},
	}
	got := formatRecallContext(mems, 0, now)
	if strings.Contains(got, "id: blank") {
		t.Fatalf("blank-line memory must be skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "id: real") {
		t.Fatalf("real memory must be included, got:\n%s", got)
	}
}

func TestHk_FormatRecallContextByteLimit(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	bigTitle := strings.Repeat("A", 420) // ~420 bytes/line, so line 2 blows the 800 cap
	body := strings.Repeat("word ", 80)
	mems := []Memory{
		{ID: "first", Title: bigTitle, Source: "test", CreatedAt: now.Format(time.RFC3339), Text: body, Score: -1},
		{ID: "second", Title: bigTitle, Source: "test", CreatedAt: now.Format(time.RFC3339), Text: body, Score: -1},
		{ID: "third", Title: bigTitle, Source: "test", CreatedAt: now.Format(time.RFC3339), Text: body, Score: -1},
	}
	got := formatRecallContext(mems, 0, now)
	if len(got) > hookRecallByteLimit {
		t.Fatalf("output %d bytes exceeds cap %d", len(got), hookRecallByteLimit)
	}
	if !strings.Contains(got, "id: first") {
		t.Fatalf("first line must always fit, got:\n%s", got)
	}
	if strings.Contains(got, "id: second") {
		t.Fatalf("second line must be dropped by the byte cap, got:\n%s", got)
	}
}

func TestHk_RecallLineFallbacks(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	created := now.Format(time.RFC3339)

	// Empty text -> title becomes the snippet.
	line := recallLine(Memory{ID: "a", Title: "Only a title", Source: "manual", CreatedAt: created}, now)
	if !strings.Contains(line, "Only a title") {
		t.Fatalf("empty-text line should fall back to title, got %q", line)
	}

	// Empty text AND empty title -> no line at all.
	if got := recallLine(Memory{ID: "b", CreatedAt: created}, now); got != "" {
		t.Fatalf("text-less, title-less memory must render empty, got %q", got)
	}

	// Empty title -> id is used as the title; empty source -> "memory"; scope appended.
	line = recallLine(Memory{ID: "xyz", Source: "", Scope: "proj", CreatedAt: created, Text: "some body"}, now)
	if !strings.Contains(line, "- xyz [memory/proj,") {
		t.Fatalf("title/source fallbacks wrong, got %q", line)
	}
	if !strings.Contains(line, "id: xyz") || !strings.Contains(line, "some body") {
		t.Fatalf("recallLine missing id/snippet, got %q", line)
	}
}

func TestHk_ClipRunes(t *testing.T) {
	if got := clipRunes("short", 100); got != "short" {
		t.Fatalf("under-limit string must be unchanged, got %q", got)
	}
	if got := clipRunes("abcdefghij", 3); got != "abc..." {
		t.Fatalf("clipRunes truncation = %q, want %q", got, "abc...")
	}
	if got := clipRunes("ab cdef", 3); got != "ab..." {
		t.Fatalf("clipRunes must trim trailing space before the ellipsis, got %q", got)
	}
	// Multi-byte runes must be counted by rune, not byte.
	if got := clipRunes("héllo wörld", 4); got != "héll..." {
		t.Fatalf("clipRunes rune counting = %q, want %q", got, "héll...")
	}
}

func TestHk_MemoryAge(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		created string
		want    string
	}{
		{"not-a-timestamp", "unknown"},
		{now.Add(48 * time.Hour).Format(time.RFC3339), "in the future"},
		{now.Format(time.RFC3339), "today"},
		{now.Add(-25 * time.Hour).Format(time.RFC3339), "1d"},
		{now.Add(-72 * time.Hour).Format(time.RFC3339), "3d"},
	}
	for _, c := range cases {
		if got := memoryAge(c.created, now); got != c.want {
			t.Fatalf("memoryAge(%q) = %q, want %q", c.created, got, c.want)
		}
	}
}

func TestRecallCompositionEdges(t *testing.T) {
	if SkipRecallPrompt("this is a substantive prompt") {
		t.Fatal("substantive prompt skipped")
	}
	if PrependBanner("", "body") != "body" || PrependBanner("banner", "") != "banner" || PrependBanner("banner", "body") != "banner\nbody" {
		t.Fatal("banner composition changed")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mems := []Memory{{ID: "1", Text: "one", CreatedAt: now.Format(time.RFC3339)}, {ID: "2", Text: "two", CreatedAt: now.Format(time.RFC3339)}, {ID: "3", Text: "three", CreatedAt: now.Format(time.RFC3339)}, {ID: "4", Text: "four", CreatedAt: now.Format(time.RFC3339)}}
	if got := strings.Count(FormatRecallContext(mems, 0, now), "\n-"); got != 3 {
		t.Fatalf("additional rows=%d want 3", got)
	}
}
