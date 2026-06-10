package mora

import (
	"context"
	"testing"
)

func ids(mems []Memory) map[string]bool {
	m := map[string]bool{}
	for _, x := range mems {
		m[x.ID] = true
	}
	return m
}

// TestHybridGraphExpansion proves the killer feature of hybrid retrieval: a query
// that names a PERSON surfaces that person's memories even when the memory body
// shares no lexical terms with the query (FTS alone would miss it). The graph arm
// resolves the name → the person → their 1-hop evidence.
func TestHybridGraphExpansion(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// M1's body shares NO words with the query below; it's reachable only via Neil.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/plan", Scope: "personal", Type: "email", Title: "Q3 logistics",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "Booking the venue and catering for the offsite.",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// A decoy with no relation to Neil.
	if err := writeMemory(cfg, Memory{
		ID: "note/decoy", Scope: "personal", Type: "note", Title: "Groceries",
		CreatedAt: "2026-05-02T00:00:00Z", Text: "milk eggs bread",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	got, err := hybridSearch(ctx, cfg, "what did Neil Patel decide", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !ids(got)["gmail_thread/plan"] {
		t.Fatalf("graph expansion failed to surface Neil's memory via his name; got %v", idList(got))
	}
}

// TestHybridFtsAnchor proves an exact lexical match still ranks at/near the top —
// BM25 remains the correctness anchor, the vector/graph arms only add recall.
func TestHybridFtsAnchor(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	run(t, "write", "--scope", "global", "--type", "note", "--title", "OAuth design", "--text", "PKCE flow and refresh tokens for the auth design")
	run(t, "write", "--scope", "global", "--type", "note", "--title", "Lunch", "--text", "tacos on tuesday")

	got, err := hybridSearch(ctx, cfg, "oauth", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected the OAuth note to surface")
	}
	top := got[0]
	if top.Title != "OAuth design" {
		t.Fatalf("exact-match note should rank first, got %q (%v)", top.Title, idList(got))
	}
}

// TestHybridDeterministic proves the fused ranking is byte-stable across calls.
func TestHybridDeterministic(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	for i, txt := range []string{"alpha beta gamma", "beta gamma delta", "gamma delta epsilon"} {
		run(t, "write", "--scope", "global", "--type", "note", "--title", txt, "--text", txt+" body")
		_ = i
	}
	a, err := hybridSearch(ctx, cfg, "beta gamma", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := hybridSearch(ctx, cfg, "beta gamma", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("nondeterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("nondeterministic order at %d: %s vs %s", i, a[i].ID, b[i].ID)
		}
	}
}

func idList(mems []Memory) []string {
	out := make([]string, len(mems))
	for i, m := range mems {
		out[i] = m.ID
	}
	return out
}
