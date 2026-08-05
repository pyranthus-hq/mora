package mora

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfidenceBM25HighRankPoorCoverageCannotBeStrong(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// A realistic background corpus gives the one repeated term a large BM25
	// magnitude. The row still covers only one part of the five-term question.
	for i := 0; i < 100; i++ {
		if err := writeMemory(cfg, Memory{
			ID:        fmt.Sprintf("answerability-bg-%03d", i),
			Scope:     "global",
			Type:      "note",
			Title:     fmt.Sprintf("Background %d", i),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Source:    "notes",
			Text:      "unrelated workflow notes and ordinary project mail",
		}); err != nil {
			t.Fatalf("seed background: %v", err)
		}
	}
	if err := writeMemory(cfg, Memory{
		ID:        "launch-writing-only",
		Scope:     "global",
		Type:      "note",
		Title:     "Launch writing",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Source:    "notes",
		Text:      "launch launch launch launch copy and announcement wording",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	sc := searchStructured(t, `{"query":"Is Atlas beta readiness complete for launch?","confidence":true}`)
	conf := mustConfidence(t, sc)
	if got := conf["max_score"].(float64); got > confidenceSearchStrongBound {
		t.Fatalf("fixture max_score=%v, want a score inside the old strong BM25 band", got)
	}
	if got := conf["strength"]; got != "moderate" {
		t.Fatalf("strength=%v, want moderate: rank is strong but whole-row lexical proof is absent", got)
	}
}

func TestConfidenceThinkSplitTermSabotageCannotBeStrong(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now().UTC().Format(time.RFC3339)
	mems := []Memory{
		{ID: "gmail/first-half", Scope: "global", Type: "email", Source: "gmail", Title: "Atlas beta readiness", Text: "Atlas beta readiness writing notes.", CreatedAt: now},
		{ID: "imessage/second-half", Scope: "global", Type: "imessage", Source: "imessage", Title: "Readiness complete launch", Text: "Readiness complete launch checklist formatting.", CreatedAt: now},
	}
	for _, m := range mems {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	// The default payload is the pre-#280 shape. Confidence must not add a gap,
	// checks receipt, or synthesis instruction when its knob is off.
	legacy := thinkStructured(t, `{"query":"atlas beta readiness complete launch"}`)
	legacyFalse := thinkStructured(t, `{"query":"atlas beta readiness complete launch","confidence":false}`)
	if !reflect.DeepEqual(legacy, legacyFalse) {
		t.Fatalf("confidence omitted and false changed the think payload\nomitted: %#v\nfalse: %#v", legacy, legacyFalse)
	}
	thinkLegacy := legacy["think"].(map[string]any)
	gapsLegacy := thinkLegacy["gaps"].(map[string]any)
	if len(gapsLegacy) != 1 {
		t.Fatalf("confidence-off gaps changed from the legacy shape: %v", gapsLegacy)
	}
	wantChecks := []string{"staleness", "evidence_density", "source_coverage", "temporal_state", "entity_coverage", "retrieval_support"}
	gotChecks, ok := gapsLegacy["checks_applied"].([]any)
	if !ok || len(gotChecks) != len(wantChecks) {
		t.Fatalf("checks_applied=%v, want legacy receipt %v", gapsLegacy["checks_applied"], wantChecks)
	}
	for i, want := range wantChecks {
		if gotChecks[i] != want {
			t.Fatalf("checks_applied[%d]=%v, want %q", i, gotChecks[i], want)
		}
	}
	if prompt := thinkLegacy["synthesis_prompt"].(string); strings.Contains(prompt, "direct coverage") || strings.Contains(prompt, "KNOWN GAPS") {
		t.Fatalf("confidence-off synthesis prompt changed: %q", prompt)
	}

	sc := thinkStructured(t, `{"query":"atlas beta readiness complete launch","confidence":true}`)
	conf := mustConfidence(t, sc)
	if got := conf["max_score"].(float64); got <= 0 {
		t.Fatalf("max_score=%v, want a positive RRF rank score for the sabotage fixture", got)
	}
	if got := conf["strength"]; got != "moderate" {
		t.Fatalf("strength=%v, want moderate: two three-term halves cannot combine into whole-row proof", got)
	}
}

func TestConfidenceThinkDirectMultiSourceEvidenceCanStayStrong(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now().UTC().Format(time.RFC3339)
	mems := []Memory{
		{ID: "gmail/direct", Scope: "global", Type: "email", Source: "gmail", Title: "Atlas beta readiness", Text: "Atlas beta readiness is complete for launch.", CreatedAt: now},
		{ID: "imessage/direct", Scope: "global", Type: "imessage", Source: "imessage", Title: "Atlas launch check", Text: "Atlas beta readiness is complete for launch.", CreatedAt: now},
	}
	for _, m := range mems {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	sc := thinkStructured(t, `{"query":"Is Atlas beta readiness complete for launch?","confidence":true}`)
	conf := mustConfidence(t, sc)
	if got := conf["strength"]; got != "strong" {
		t.Fatalf("strength=%v, want strong for direct current evidence from two sources", got)
	}
}

func TestStrictLexicalCoverageIsWholeRowAndDeterministic(t *testing.T) {
	rows := []lexicalEvidence{
		{Source: "gmail", Text: "Atlas beta readiness"},
		{Source: "calendar", Text: "Readiness complete launch"},
	}
	query := "atlas beta readiness complete launch"
	a := strictLexicalCoverage(query, rows)
	b := strictLexicalCoverage(query, rows)
	if a != b {
		t.Fatalf("lexical coverage changed across identical calls: first=%+v second=%+v", a, b)
	}
	if a.FullRows != 0 || a.FullSources != 0 {
		t.Fatalf("split terms fabricated whole-row support: %+v", a)
	}
}

func TestConfidenceExactWordCheckUsesEveryQueryTerm(t *testing.T) {
	query := "alpha bravo charlie delta echo foxtrot golf hotel juliet kilo lima mike november"
	terms := confidenceQueryTerms(query)
	if len(terms) != 13 {
		t.Fatalf("confidenceQueryTerms returned %d terms, want all 13: %v", len(terms), terms)
	}

	firstTwelve := []lexicalEvidence{{Source: "gmail", Text: "alpha bravo charlie delta echo foxtrot golf hotel juliet kilo lima mike"}}
	if got := strictLexicalCoverage(query, firstTwelve); got.FullRows != 0 {
		t.Fatalf("a row missing term 13 passed the exact-word check: %+v", got)
	}
	allThirteen := []lexicalEvidence{{Source: "gmail", Text: query}}
	if got := strictLexicalCoverage(query, allThirteen); got.FullRows != 1 {
		t.Fatalf("a row containing all 13 terms failed the exact-word check: %+v", got)
	}
}

func TestConfidenceExactWordCheckKeepsCapitalizedStopwordNames(t *testing.T) {
	query := "Will approve launch"
	terms := confidenceQueryTerms(query)
	if len(terms) != 3 || terms[0] != "will" {
		t.Fatalf("confidenceQueryTerms(%q)=%v, want capitalized Will preserved", query, terms)
	}
	withoutName := []lexicalEvidence{{Source: "gmail", Text: "approve launch"}}
	if got := strictLexicalCoverage(query, withoutName); got.FullRows != 0 {
		t.Fatalf("row missing capitalized name Will passed: %+v", got)
	}
	withName := []lexicalEvidence{{Source: "gmail", Text: "Will approve launch"}}
	if got := strictLexicalCoverage(query, withName); got.FullRows != 1 {
		t.Fatalf("row containing Will failed: %+v", got)
	}
}

func TestConfidenceSearchCoverageUsesOnlyBudgetedReturnedRows(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	returned := []Memory{{
		ID: "partial", Source: "gmail", Title: "Atlas beta", Text: "short returned preview",
		Score: -5, CreatedAt: "2026-08-01T00:00:00Z",
	}}
	fullRows := []Memory{
		{ID: "partial", Source: "gmail", Title: "Atlas beta", Text: "partial text", Score: -5, CreatedAt: "2026-08-01T00:00:00Z"},
		// This direct row ranked well enough to exist before budgeting, but the
		// caller did not receive it. It must not strengthen the returned set.
		{ID: "dropped-direct", Source: "imessage", Title: "Atlas beta readiness complete launch", Text: "all terms", Score: -6, CreatedAt: "2026-08-02T00:00:00Z"},
	}
	conf := searchConfidence(context.Background(), cfg, returned, false, fullRows, fullRows, retrievalTrace{}, "Atlas beta readiness complete launch", now)
	if conf.Strength != "moderate" {
		t.Fatalf("strength=%q, want moderate when the only exact row was budget-dropped", conf.Strength)
	}
	if conf.MaxScore != -5 || conf.MeanScore != -5 {
		t.Fatalf("score stats changed scope: max=%v mean=%v, want returned-row -5/-5", conf.MaxScore, conf.MeanScore)
	}
	if conf.FreshestSourceAt != "2026-08-01T00:00:00Z" {
		t.Fatalf("freshest_source_at=%q, want returned row only", conf.FreshestSourceAt)
	}
}

func TestConfidenceReturnedSharedRowCannotBorrowCollidingLocalText(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	query := "atlas beta readiness complete launch"
	returned := []Memory{{
		ID: "collision", Owner: "team", Source: "gmail", Title: "Atlas beta", Text: "returned shared preview",
		Score: -5, CreatedAt: "2026-08-01T00:00:00Z",
	}}
	full := []Memory{
		// Same stable id, but this is a dropped LOCAL row with direct text.
		{ID: "collision", Source: "imessage", Title: query, Text: "local direct body", Score: -6, CreatedAt: "2026-08-02T00:00:00Z"},
		// This is the shared row the caller actually received; it is partial.
		{ID: "collision", Owner: "team", Source: "gmail", Title: "Atlas beta", Text: "shared partial body", Score: -5, CreatedAt: "2026-08-01T00:00:00Z"},
	}
	local := full[:1]
	rows := returnedMemoryRows(returned, full)
	if len(rows) != 1 || rows[0].Owner != "team" || rows[0].Text != "shared partial body" {
		t.Fatalf("returned shared identity borrowed the colliding local row: %+v", rows)
	}
	conf := searchConfidence(context.Background(), cfg, returned, false, full, local, retrievalTrace{}, query, time.Now())
	if conf.Strength != "moderate" {
		t.Fatalf("strength=%q, want moderate when returned shared row is partial", conf.Strength)
	}
}

func TestConfidenceDirectReturnedSharedRowsCount(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	query := "atlas beta readiness complete launch"
	created := time.Now().UTC().Format(time.RFC3339)
	local := []Memory{
		{ID: "local-a", Source: "notes", Title: "atlas beta readiness", Text: "partial local context", Score: 0.1, CreatedAt: created},
		{ID: "local-b", Source: "calendar", Title: "complete launch", Text: "other partial local context", Score: 0.09, CreatedAt: created},
	}
	shared := []Memory{
		{ID: "shared-a", Owner: "alice", Source: "gmail", Title: query, Text: "direct shared evidence", Score: 0.08, CreatedAt: created},
		{ID: "shared-b", Owner: "bob", Source: "imessage", Title: query, Text: "independent direct shared evidence", Score: 0.07, CreatedAt: created},
	}
	full := append(append([]Memory{}, local...), shared...)
	returned := append([]Memory{}, full...)
	conf := searchConfidence(context.Background(), cfg, returned, true, full, local, retrievalTrace{}, query, time.Now())
	if conf.Strength != "strong" {
		t.Fatalf("strength=%q, want strong when two direct returned shared rows provide two sources", conf.Strength)
	}
}

func TestConfidenceThinkSemanticParaphraseCapsAtModerateNotWeak(t *testing.T) {
	srv := fakeOllama(t, []float64{1, 0, 0, 0})
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")

	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now().UTC().Format(time.RFC3339)
	mems := []Memory{
		{ID: "gmail/paraphrase", Scope: "global", Type: "email", Source: "gmail", Title: "Project Atlas", Text: "The release gate cleared and the product can ship.", CreatedAt: now},
		{ID: "imessage/paraphrase", Scope: "global", Type: "imessage", Source: "imessage", Title: "Go-live approval", Text: "Approval was granted and rollout may proceed.", CreatedAt: now},
	}
	for _, m := range mems {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	sc := thinkStructured(t, `{"query":"What is the Atlas beta launch decision?","confidence":true}`)
	conf := mustConfidence(t, sc)
	if got := conf["scale"]; got != confidenceScaleRRFFused {
		t.Fatalf("scale=%v, want fused semantic path", got)
	}
	if got := conf["strength"]; got != "moderate" {
		t.Fatalf("strength=%v, want moderate: strict lexical proof is absent, but semantic paraphrase is not proven weak", got)
	}
}
