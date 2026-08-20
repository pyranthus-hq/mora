package search

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"testing"
)

func TestStrictLexicalCoverageIsWholeRowAndDeterministic(t *testing.T) {
	rows := []LexicalEvidence{
		{Source: "gmail", Text: "Atlas beta readiness"},
		{Source: "calendar", Text: "Readiness complete launch"},
	}
	query := "atlas beta readiness complete launch"
	a := StrictLexicalCoverage(query, rows)
	b := StrictLexicalCoverage(query, rows)
	if a != b {
		t.Fatalf("lexical coverage changed across identical calls: first=%+v second=%+v", a, b)
	}
	if a.FullRows != 0 || a.FullSources != 0 {
		t.Fatalf("split terms fabricated whole-row support: %+v", a)
	}
}

func TestConfidenceExactWordCheckUsesEveryQueryTerm(t *testing.T) {
	query := "alpha bravo charlie delta echo foxtrot golf hotel juliet kilo lima mike november"
	terms := ConfidenceQueryTerms(query)
	if len(terms) != 13 {
		t.Fatalf("ConfidenceQueryTerms returned %d terms, want all 13: %v", len(terms), terms)
	}

	firstTwelve := []LexicalEvidence{{Source: "gmail", Text: "alpha bravo charlie delta echo foxtrot golf hotel juliet kilo lima mike"}}
	if got := StrictLexicalCoverage(query, firstTwelve); got.FullRows != 0 {
		t.Fatalf("a row missing term 13 passed the exact-word check: %+v", got)
	}
	allThirteen := []LexicalEvidence{{Source: "gmail", Text: query}}
	if got := StrictLexicalCoverage(query, allThirteen); got.FullRows != 1 {
		t.Fatalf("a row containing all 13 terms failed the exact-word check: %+v", got)
	}
}

func TestConfidenceExactWordCheckKeepsCapitalizedStopwordNames(t *testing.T) {
	query := "Will approve launch"
	terms := ConfidenceQueryTerms(query)
	if len(terms) != 3 || terms[0] != "will" {
		t.Fatalf("ConfidenceQueryTerms(%q)=%v, want capitalized Will preserved", query, terms)
	}
	withoutName := []LexicalEvidence{{Source: "gmail", Text: "approve launch"}}
	if got := StrictLexicalCoverage(query, withoutName); got.FullRows != 0 {
		t.Fatalf("row missing capitalized name Will passed: %+v", got)
	}
	withName := []LexicalEvidence{{Source: "gmail", Text: "Will approve launch"}}
	if got := StrictLexicalCoverage(query, withName); got.FullRows != 1 {
		t.Fatalf("row containing Will failed: %+v", got)
	}
}

func TestReturnedMemoryRowsKeepsOwnerIdentityAndFullOrder(t *testing.T) {
	returned := []memory.Memory{{Owner: "alice", ID: "same"}, {Owner: "bob", ID: "missing"}}
	full := []memory.Memory{{Owner: "bob", ID: "same", Text: "wrong"}, {Owner: "alice", ID: "same", Text: "right"}, {Owner: "bob", ID: "missing", Text: "second"}}
	got := ReturnedMemoryRows(returned, full)
	if len(got) != 2 || got[0].Text != "right" || got[1].Text != "second" {
		t.Fatalf("rows=%+v", got)
	}
}
