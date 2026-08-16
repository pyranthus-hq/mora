package search

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
)

type LexicalCoverage struct {
	FullRows    int
	FullSources int
}
type LexicalEvidence struct {
	Title  string
	Text   string
	Source string
}

func StrictLexicalCoverage(query string, rows []LexicalEvidence) LexicalCoverage {
	terms := ConfidenceQueryTerms(query)
	if len(terms) == 0 {
		return LexicalCoverage{}
	}
	sources := map[string]bool{}
	fullRows := 0
	for _, row := range rows {
		words := map[string]bool{}
		addConfidenceWords(words, row.Title)
		addConfidenceWords(words, row.Text)
		full := true
		for _, term := range terms {
			if !words[term] {
				full = false
				break
			}
		}
		if !full {
			continue
		}
		fullRows++
		sources[row.Source] = true
	}
	return LexicalCoverage{FullRows: fullRows, FullSources: len(sources)}
}
func ConfidenceQueryTerms(query string) []string {
	type token struct {
		term string
		key  string
	}
	var all []token
	for _, word := range strings.Fields(query) {
		term, key := Token(word)
		if key != "" {
			all = append(all, token{term: term, key: key})
		}
	}
	content := make([]token, 0, len(all))
	for _, tok := range all {
		if !IsStopword(tok.term, tok.key) {
			content = append(content, tok)
		}
	}
	if len(content) == 0 {
		content = all
	}
	out := make([]string, 0, len(content))
	seen := map[string]bool{}
	for _, tok := range content {
		if seen[tok.key] {
			continue
		}
		seen[tok.key] = true
		out = append(out, tok.key)
	}
	return out
}
func ReturnedMemoryRows(returned, full []memory.Memory) []memory.Memory {
	type rowID struct {
		Owner string
		ID    string
	}
	ids := make(map[rowID]bool, len(returned))
	for _, m := range returned {
		ids[rowID{Owner: m.Owner, ID: m.ID}] = true
	}
	rows := make([]memory.Memory, 0, len(returned))
	for _, m := range full {
		if ids[rowID{Owner: m.Owner, ID: m.ID}] {
			rows = append(rows, m)
		}
	}
	return rows
}
func addConfidenceWords(out map[string]bool, text string) {
	for _, word := range strings.Fields(text) {
		_, key := Token(word)
		if key != "" {
			out[key] = true
		}
	}
}
