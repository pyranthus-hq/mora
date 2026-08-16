package graph

import (
	"reflect"
	"testing"
)

func TestNormalizeGazName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Neil Patel", "neil patel", true},
		{"  Neil   Patel ", "neil patel", true},
		{"O'Brien Quinn", "o'brien quinn", true},
		{"Neil", "", false},              // single token — deferred (too ambiguous)
		{"Support Team", "", false},      // stoplist token
		{"No Reply", "", false},          // stoplist token
		{"Support-Team Jane", "", false}, // hyphenated generic bypasses? must not (codex)
		{"Push activity", "", false},     // new stoplist token
		{"State change", "", false},      // new stoplist token
		{"Author", "", false},            // new stoplist token
		{"Will Brown", "", false},        // common function word "will" (codex)
		{"May Day", "", false},           // common function word "may" (codex)
		{"A Smith", "", false},           // single-rune initial token (codex)
		{"neil@x.com", "", false},        // email, not a name
		{"+15551234567", "", false},      // phone handle
		{"Al B", "", false},              // multi-token but < minGazNameLen ("al b" = 4)
	}
	for _, c := range cases {
		got, ok := normalizeGazName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeGazName(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
func TestGazetteerTieBreakDeterministic(t *testing.T) {
	// Two people share the display name "Sam Jones"; the more-mentioned wins.
	persons := map[string]*personAgg{
		"person:a@x.com": {aliases: map[string]bool{"sam jones": true}, evidence: map[string]bool{"m1": true}},
		"person:b@x.com": {aliases: map[string]bool{"sam jones": true}, evidence: map[string]bool{"m1": true, "m2": true}},
	}
	g := buildGazetteer(persons)
	if g["sam jones"] != "person:b@x.com" {
		t.Fatalf("ambiguous name tie-break = %q, want the more-mentioned person:b@x.com", g["sam jones"])
	}
}
func TestGazetteerScanRespectsPunctuation(t *testing.T) {
	persons := map[string]*personAgg{
		"person:john@x.com": {aliases: map[string]bool{"john doe": true, "john@x.com": true}, evidence: map[string]bool{}},
	}
	g := buildGazetteer(persons)

	if got := gazetteerScan(g, "met John Doe today"); !reflect.DeepEqual(got, []string{"person:john@x.com"}) {
		t.Fatalf("plain-space name should match: %v", got)
	}
	for _, neg := range []string{
		"email john.doe@example.com today", // email address — the common false-positive
		"see /people/john/doe in the repo", // path separators
		"John.\nDoe",                       // newline split
		"John, Doe",                        // comma separator
	} {
		if got := gazetteerScan(g, neg); len(got) != 0 {
			t.Fatalf("must NOT match across punctuation in %q: %v", neg, got)
		}
	}
}
func TestGazetteerScanWordBoundary(t *testing.T) {
	persons := map[string]*personAgg{
		"person:ana@x.com": {aliases: map[string]bool{"ana lee": true, "ana@x.com": true}, evidence: map[string]bool{}},
	}
	g := buildGazetteer(persons)

	if got := gazetteerScan(g, "met Ana Lee for coffee"); !reflect.DeepEqual(got, []string{"person:ana@x.com"}) {
		t.Fatalf("expected a match for 'Ana Lee', got %v", got)
	}
	// Substring must NOT match (word boundary): "Banana Leek" contains "ana le"
	// across tokens but never the tokens "ana"+"lee".
	if got := gazetteerScan(g, "Banana Leek soup is great"); len(got) != 0 {
		t.Fatalf("substring should not match on word boundary: %v", got)
	}
	if got := gazetteerScan(g, "no people here"); len(got) != 0 {
		t.Fatalf("unexpected match: %v", got)
	}
}
