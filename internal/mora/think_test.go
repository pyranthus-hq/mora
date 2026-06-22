package mora

import (
	"context"
	"strings"
	"testing"
	"time"
)

func gapsJoined(g ThinkGaps) string {
	return strings.Join(append(append(append([]string{}, g.Stale...), g.ThinCoverage...), g.CoverageHoles...), "\n")
}

// TestThinkEvidenceAndStaleness proves think retrieves cited evidence and flags
// staleness deterministically (relative to the injected now).
func TestThinkEvidenceAndStaleness(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	run(t, "write", "--scope", "global", "--type", "note", "--title", "OAuth design", "--text", "PKCE flow and refresh tokens")
	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)

	res, err := buildThink(ctx, cfg, "oauth", "", 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evidence) == 0 || res.Evidence[0].Title != "OAuth design" {
		t.Fatalf("expected OAuth evidence, got %+v", res.Evidence)
	}
	if res.Evidence[0].StableID == "" {
		t.Fatal("evidence must carry a StableID for citation")
	}
	// Staleness is deterministic relative to the injected now; use a scope whose
	// only memory is explicitly old (2020) so the freshest match is stale.
	if err := writeMemory(cfg, Memory{ID: "old/a", Scope: "old", Type: "note", Title: "Ancient OAuth", Text: "oauth historical", CreatedAt: "2020-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	res2, err := buildThink(ctx, cfg, "oauth historical", "old", 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Gaps.Stale) == 0 {
		t.Fatalf("expected a staleness gap for a 2020 memory, got gaps: %s", gapsJoined(res2.Gaps))
	}
	if !strings.Contains(res2.SynthesisPrompt, "stable_id") {
		t.Fatal("synthesis prompt must instruct citation by stable_id")
	}
}

// TestThinkCoverageHole proves a person named in the query but unknown to the
// vault is surfaced as a coverage hole.
func TestThinkCoverageHole(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	run(t, "write", "--scope", "global", "--type", "note", "--title", "note", "--text", "some unrelated content")

	res, err := buildThink(ctx, cfg, "what did Zelda Fitzgerald say", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gapsJoined(res.Gaps), "Zelda Fitzgerald") {
		t.Fatalf("expected a coverage hole for the unknown person, got: %s", gapsJoined(res.Gaps))
	}
}

// TestThinkThinCoverage proves a known person with little evidence is flagged.
func TestThinkThinCoverage(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Neil appears in exactly one memory -> thin (threshold is 2).
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "hi",
		CreatedAt: "2026-06-01T00:00:00Z", Text: "quick note",
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
	res, err := buildThink(ctx, cfg, "what is Neil Patel working on", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gapsJoined(res.Gaps), "Neil Patel") || len(res.Gaps.ThinCoverage) == 0 {
		t.Fatalf("expected a thin-coverage gap for Neil, got: %s", gapsJoined(res.Gaps))
	}
}

// TestThinkNoFalseHoleForQuestionPhrase is the codex I3 regression: a title-cased
// question phrase must NOT be mistaken for an unknown entity.
func TestThinkNoFalseHoleForQuestionPhrase(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	run(t, "write", "--scope", "global", "--type", "note", "--title", "plan", "--text", "the launch plan content")

	res, err := buildThink(ctx, cfg, "What Should We Do About The Plan", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Gaps.CoverageHoles {
		if strings.Contains(h, "What") || strings.Contains(h, "Should") || strings.Contains(h, "About") {
			t.Fatalf("question phrase wrongly flagged as a coverage hole: %q", h)
		}
	}
}

// TestThinkThinCoverageIgnoresFirstNameOnly is the codex I3 regression: thin
// coverage must not fire just because a query shares a person's first-name token.
func TestThinkThinCoverageIgnoresFirstNameOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "hi",
		CreatedAt: "2026-06-01T00:00:00Z", Text: "note",
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
	// "neil" appears as a bare token but NOT the full name "Neil Patel".
	res, err := buildThink(ctx, cfg, "what is the neil river restoration project", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps.ThinCoverage) != 0 {
		t.Fatalf("first-name-only token must not trigger thin coverage: %v", res.Gaps.ThinCoverage)
	}
}

// TestThinkNoMatch proves an empty retrieval is reported honestly.
func TestThinkNoMatch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	res, err := buildThink(context.Background(), cfg, "nonexistent topic xyzzy", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evidence) != 0 {
		t.Fatalf("expected no evidence, got %d", len(res.Evidence))
	}
	if len(res.Gaps.CoverageHoles) == 0 {
		t.Fatal("expected a 'no memory matched' coverage hole")
	}
}

// TestThinkGraphOnlyEvidenceCaveat is the B3 guard: a query naming a person whose
// only matches come via the people-graph (FTS+vec both miss them) STILL returns
// those memories (the killer feature, TestHybridGraphExpansion) but `think` flags
// them with an honest low-confidence retrieval caveat instead of presenting them
// as a direct answer.
func TestThinkGraphOnlyEvidenceCaveat(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// M1 is reachable ONLY via Neil's person edge — its body shares no terms with
	// the query below (Neil's name lives in Meta, not the text/title/FTS).
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
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	res, err := buildThink(ctx, cfg, "what did Neil Patel decide", "", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Killer feature preserved: the graph-only memory STILL surfaces.
	var surfaced bool
	for _, e := range res.Evidence {
		if e.StableID == "gmail_thread/plan" {
			surfaced = true
		}
	}
	if !surfaced {
		t.Fatalf("graph-only person memory must still surface (killer feature); evidence=%v", res.Evidence)
	}
	// B3: but it is honestly flagged as association-only, naming the person.
	caveat := strings.Join(res.Gaps.RetrievalCaveats, " ")
	if !strings.Contains(caveat, "Neil Patel") || !strings.Contains(caveat, "people-graph association") {
		t.Fatalf("expected a graph-only retrieval caveat naming Neil, got: %q", caveat)
	}
	if !strings.Contains(res.SynthesisPrompt, "people-graph association") {
		t.Fatalf("synthesis prompt must carry the caveat:\n%s", res.SynthesisPrompt)
	}
}

// TestThinkNoCaveatWhenDirectlySupported proves the caveat is low-false-positive:
// when ANY returned memory is a direct lexical (FTS) hit — even for a person-named
// query — no association-only caveat fires.
func TestThinkNoCaveatWhenDirectlySupported(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Neil's memory whose BODY contains the query's topic word ("venue") — so FTS
	// directly supports it; it is not association-only.
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
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	res, err := buildThink(ctx, cfg, "what did Neil Patel say about the venue", "", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps.RetrievalCaveats) != 0 {
		t.Fatalf("FTS-supported evidence must not be flagged association-only: %v", res.Gaps.RetrievalCaveats)
	}
}

func TestThinkDeterministic(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	run(t, "write", "--scope", "global", "--type", "note", "--title", "alpha", "--text", "alpha beta gamma")
	run(t, "write", "--scope", "global", "--type", "note", "--title", "beta", "--text", "beta gamma delta")
	now := time.Now()
	a, _ := buildThink(ctx, cfg, "beta gamma", "", 5, now)
	b, _ := buildThink(ctx, cfg, "beta gamma", "", 5, now)
	if a.SynthesisPrompt != b.SynthesisPrompt {
		t.Fatal("think synthesis prompt is not deterministic")
	}
}
