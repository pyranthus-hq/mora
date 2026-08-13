package mora

import (
	"strings"
	"testing"
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
