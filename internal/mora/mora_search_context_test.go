package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- ftsQuery: OR-of-quoted-CONTENT-tokens (stopword-filtered) -------------
// Function words are dropped before the OR so they can't dilute bm25 ranking
// (measured: FTS recall@5 0.591→0.667 on the real-query golden set). Content
// terms are preserved verbatim and quoted; an all-stopword query falls back to
// every token so the MATCH is never empty.

func TestSearchNaturalLanguageReturnsHit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "global", "--type", "note",
		"--title", "Offsite planning", "--text", "We discussed the Q3 offsite in Tahoe with the team.")
	run(t, "write", "--scope", "global", "--type", "note",
		"--title", "Grocery list", "--text", "milk eggs bread")

	// "decide"/"about"/"what" are NOT all present in the offsite memory; under the
	// old implicit-AND this returned []. With OR + bm25 it must surface the offsite note.
	out := run(t, "search", "what did we decide about the offsite", "--json")
	if !strings.Contains(out, "Offsite planning") {
		t.Fatalf("expected NLP query to surface 'Offsite planning', got:\n%s", out)
	}
}

// ---- buildContext: query items must not be starved by the wiki preamble -----

func bigWiki(t *testing.T, cfg Config, name string, n int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg.VaultDir, name), []byte(strings.Repeat("W", n)), 0o644); err != nil {
		t.Fatalf("write wiki %s: %v", name, err)
	}
}

func TestBuildContextQueryItemsNotStarved(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// Wiki preamble larger than the whole budget — would consume it all under the old code.
	bigWiki(t, cfg, "priority-map.md", 5000)

	items := []Memory{{Title: "QUERYHIT", Text: "the relevant retrieved memory"}}
	out := buildContext(cfg, items, 2000, true /* hasQuery */)

	if !strings.Contains(out, "QUERYHIT") {
		t.Fatalf("query items starved by wiki: 'QUERYHIT' not in output:\n%s", out)
	}
	if len(out) > 2000 {
		t.Fatalf("output %d bytes exceeds budget 2000", len(out))
	}
}

func TestBuildContextNoQueryWikiFirst(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.VaultDir, "priority-map.md"), []byte("PRIORITYMAP-BRIEFING"), 0o644); err != nil {
		t.Fatalf("write wiki: %v", err)
	}
	items := []Memory{{Title: "ITEMHIT", Text: "some item"}}
	out := buildContext(cfg, items, 2000, false /* no query: session briefing */)

	if !strings.Contains(out, "PRIORITYMAP-BRIEFING") {
		t.Fatalf("session briefing must include wiki preamble, got:\n%s", out)
	}
	// wiki should lead the output
	if i := strings.Index(out, "PRIORITYMAP-BRIEFING"); i < 0 || i > strings.Index(out+"ITEMHIT", "ITEMHIT") {
		t.Fatalf("expected wiki to precede items in no-query mode:\n%s", out)
	}
}

func TestBuildContextRuneSafeTruncation(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// Multi-byte content (emoji=4 bytes, CJK=3 bytes) so a naive byte cut would split a rune.
	items := []Memory{{Title: "T", Text: strings.Repeat("🚀中", 50)}}
	for _, budget := range []int{10, 17, 23, 42, 100} {
		out := buildContext(cfg, items, budget, true)
		if len(out) > budget {
			t.Fatalf("budget %d: output %d bytes exceeds budget", budget, len(out))
		}
		if !utf8.ValidString(out) {
			t.Fatalf("budget %d: output is not valid UTF-8: %q", budget, out)
		}
	}
}
