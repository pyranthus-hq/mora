package mora

import (
	"strings"
)

// lexicalCoverage is a narrow proof, not a semantic score. A full row contains
// every meaningful query term. Rows cannot pool different halves of a question
// to manufacture direct support.
type lexicalCoverage struct {
	FullRows    int
	FullSources int
}

type lexicalEvidence struct {
	Title  string
	Text   string
	Source string
}

func memoryLexicalCoverage(query string, mems []Memory) lexicalCoverage {
	rows := make([]lexicalEvidence, 0, len(mems))
	for _, m := range mems {
		rows = append(rows, lexicalEvidence{
			Title:  m.Title,
			Text:   m.Text,
			Source: evidenceSource(m),
		})
	}
	return strictLexicalCoverage(query, rows)
}

// returnedMemoryRows keeps full text only for rows that survived MCP budgeting.
// A shared and local row may have the same ID, so identity is (Owner, ID).
// Score/date rollups still use the returned slice itself in searchConfidence.
func returnedMemoryRows(returned, full []Memory) []Memory {
	type rowID struct {
		Owner string
		ID    string
	}
	ids := make(map[rowID]bool, len(returned))
	for _, m := range returned {
		ids[rowID{Owner: m.Owner, ID: m.ID}] = true
	}
	rows := make([]Memory, 0, len(returned))
	for _, m := range full {
		if ids[rowID{Owner: m.Owner, ID: m.ID}] {
			rows = append(rows, m)
		}
	}
	return rows
}

func thinkLexicalCoverage(res ThinkResult) lexicalCoverage {
	rows := make([]lexicalEvidence, 0, len(res.Evidence))
	for _, e := range res.Evidence {
		rows = append(rows, lexicalEvidence{
			Title:  e.confidenceTitle,
			Text:   e.confidenceText,
			Source: e.confidenceSource,
		})
	}
	return strictLexicalCoverage(res.Query, rows)
}

// strictLexicalCoverage proves only literal whole-row coverage. If the result
// is a real semantic paraphrase, this proof is absent and fused confidence is
// capped at moderate. It is not labeled weak merely for using different words.
func strictLexicalCoverage(query string, rows []lexicalEvidence) lexicalCoverage {
	terms := confidenceQueryTerms(query)
	if len(terms) == 0 {
		return lexicalCoverage{}
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
	return lexicalCoverage{FullRows: fullRows, FullSources: len(sources)}
}

// confidenceQueryTerms returns every de-duplicated content token in query
// order. It reuses FTS token and case-aware stopword rules. An all-stopword
// query keeps its tokens, matching ftsQuery's fallback.
func confidenceQueryTerms(query string) []string {
	type token struct {
		term string
		key  string
	}
	var all []token
	for _, word := range strings.Fields(query) {
		term, key := ftsToken(word)
		if key != "" {
			all = append(all, token{term: term, key: key})
		}
	}
	content := make([]token, 0, len(all))
	for _, tok := range all {
		if !ftsIsStopword(tok.term, tok.key) {
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

func addConfidenceWords(out map[string]bool, text string) {
	for _, word := range strings.Fields(text) {
		_, key := ftsToken(word)
		if key != "" {
			out[key] = true
		}
	}
}
