package mora

import (
	"context"
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

// TestGazetteerScanRespectsPunctuation is the P1 regression: a name may only match
// across PLAIN SPACE gaps, so an email address / path / newline-split that strips
// to the same tokens must NOT match.
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

// TestGazetteerEmailInBodyNoEdge is the end-to-end P1 regression: a person known
// from metadata, whose email appears in ANOTHER memory's body, gets no false
// MENTIONS edge.
func TestGazetteerEmailInBodyNoEdge(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "hi",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "body",
		Meta: map[string]any{
			"from":  []string{"john.doe@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"john.doe@example.com": "John Doe"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Another memory mentions John's EMAIL ADDRESS (not his spaced name) in its body.
	if err := writeMemory(cfg, Memory{
		ID: "note/n1", Scope: "personal", Type: "note", Title: "note",
		CreatedAt: "2026-05-02T00:00:00Z", Text: "Forwarded from john.doe@example.com per the thread.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	edges := readEdges(t, cfg)
	if hasEdge(edges, "memory:note/n1|MENTIONS|person:john.doe@example.com|note/n1") {
		t.Fatal("email address in a body must not create a false MENTIONS edge (codex P1)")
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

// TestGazetteerBodyMentionEdge proves a person known only from metadata gets a
// MENTIONS edge when their full name appears in another memory's body.
func TestGazetteerBodyMentionEdge(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Establish Neil Patel as a known person via email metadata. He is the sender,
	// so his self-presented name is a trusted alias (A2 provenance) and thus enters
	// the gazetteer.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "hi",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "body",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// A separate note mentions the full name in its body, no Meta.
	if err := writeMemory(cfg, Memory{
		ID: "note/n1", Scope: "personal", Type: "note", Title: "standup",
		CreatedAt: "2026-05-02T00:00:00Z", Text: "Spoke with Neil Patel about the launch.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	edges := readEdges(t, cfg)
	if !hasEdge(edges, "memory:note/n1|MENTIONS|person:neil@example.com|note/n1") {
		t.Fatal("expected a gazetteer MENTIONS edge from the note body to Neil Patel")
	}
	// The mention counts as evidence: Neil's mention_count is now 2 (email + note).
	ents := readEntities(t, cfg)
	if mc := ents["person:neil@example.com"].mentionCount; mc != 2 {
		t.Fatalf("mention_count = %d, want 2 (metadata + body mention)", mc)
	}
}

// TestGazetteerNoDuplicateForParticipant proves a metadata participant is NOT also
// given a redundant MENTIONS edge when their name appears in the same body.
func TestGazetteerNoDuplicateForParticipant(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "hi",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "Thanks, Neil Patel here, see below.",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	edges := readEdges(t, cfg)
	if hasEdge(edges, "memory:gmail_thread/t1|MENTIONS|person:neil@example.com|gmail_thread/t1") {
		t.Fatal("a metadata participant must not get a redundant MENTIONS edge")
	}
	if !hasEdge(edges, "memory:gmail_thread/t1|PARTICIPATED_IN|person:neil@example.com|gmail_thread/t1") {
		t.Fatal("participant edge missing")
	}
}
