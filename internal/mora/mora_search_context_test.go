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

func TestFtsQueryORSemantics(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"single", "oauth", `"oauth"`},
		// Function words ("what", "did") are dropped; content survives.
		{"natural_language", "what did neil say", `"neil" OR "say"`},
		{"empty", "", ""},
		{"all_punctuation", "!?,.;", ""},
		{"collapses_whitespace", "  foo   bar  ", `"foo" OR "bar"`},
		{"stopwords_dropped", "the quick brown fox", `"quick" OR "brown" OR "fox"`},
		// Contractions collapse to their head for the stopword check:
		// "what's"→"what" (stopword, dropped), "the" dropped, "plan" kept.
		{"contraction_head_dropped", "what's the plan", `"plan"`},
		// Question word "when" + linking "is" dropped; proper noun + topic kept.
		{"question_words_dropped", "when is Riya birthday", `"Riya" OR "birthday"`},
		// All-stopword query: never emit an empty MATCH — keep every token.
		{"all_stopword_fallback", "what is the", `"what" OR "is" OR "the"`},
		// Case guard: a capitalized/all-caps function word is a name/acronym and
		// must NOT be dropped (Will the person, WHO the org, IT the dept).
		{"capitalized_name_kept", "Will offsite", `"Will" OR "offsite"`},
		{"allcaps_acronym_kept", "WHO policy", `"WHO" OR "policy"`},
		{"allcaps_dept_kept", "IT roadmap", `"IT" OR "roadmap"`},
		// Possessive on a capitalized name survives whole (no contraction collapse).
		{"capitalized_possessive_kept", "Will's roadmap", `"Will's" OR "roadmap"`},
		// Single-char function words are pure noise and dropped regardless of case.
		{"single_char_dropped", "a cat I saw", `"cat" OR "saw"`},
		{"edge_punct_trimmed", "OAuth, 2.0; flow!", `"OAuth" OR "2.0" OR "flow"`},
		// Edge hyphens are stripped (FTS5 '-' is a NOT/column operator); the
		// inner word survives, quoted — safe, no "fts5: syntax error near -".
		{"edge_hyphen_trimmed", "--help", `"help"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ftsQuery(c.in); got != c.want {
				t.Fatalf("ftsQuery(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The real-world bug: a multi-word natural-language query that includes words
// the memory does NOT contain must still surface the memory (OR), where the old
// space-join (implicit AND) returned nothing.
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
