package mora

import (
	"context"
	"testing"
)

// TestGazetteerScanRespectsPunctuation is the P1 regression: a name may only match
// across PLAIN SPACE gaps, so an email address / path / newline-split that strips
// to the same tokens must NOT match.

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
