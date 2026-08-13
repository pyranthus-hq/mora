package search

import "testing"

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
			if got := FTSQuery(c.in); got != c.want {
				t.Fatalf("FTSQuery(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The real-world bug: a multi-word natural-language query that includes words
// the memory does NOT contain must still surface the memory (OR), where the old
// space-join (implicit AND) returned nothing.
func TestFtsQueryStripsEdgeHyphens(t *testing.T) {
	if got := FTSQuery("--help"); got != `"help"` {
		t.Fatalf("FTSQuery(%q) should trim edge dashes to a safe quoted term, got %q", "--help", got)
	}
	if got := FTSQuery("foo-bar"); got != `"foo-bar"` {
		t.Fatalf("FTSQuery should preserve internal hyphens inside the quoted token, got %q", got)
	}
}

func TestParseArgs(t *testing.T) {
	scope, limit, jsonOut, query, err := ParseArgs([]string{"--json", "--scope=project:x", "--limit", "3", "what", "now"})
	if err != nil || scope != "project:x" || limit != 3 || !jsonOut || len(query) != 2 {
		t.Fatalf("ParseArgs=(%q,%d,%v,%v,%v)", scope, limit, jsonOut, query, err)
	}
	for _, args := range [][]string{{"--scope"}, {"--limit"}, {"--limit=bad"}} {
		if _, _, _, _, err := ParseArgs(args); err == nil {
			t.Fatalf("ParseArgs(%v) should fail", args)
		}
	}
}
