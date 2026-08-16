package mora

import "github.com/pyranthus-hq/mora/internal/search"

type lexicalCoverage = search.LexicalCoverage

func memoryLexicalCoverage(query string, mems []Memory) lexicalCoverage {
	rows := make([]search.LexicalEvidence, 0, len(mems))
	for _, m := range mems {
		rows = append(rows, search.LexicalEvidence{Title: m.Title, Text: m.Text, Source: evidenceSource(m)})
	}
	return search.StrictLexicalCoverage(query, rows)
}
func returnedMemoryRows(returned, full []Memory) []Memory {
	return search.ReturnedMemoryRows(returned, full)
}
func thinkLexicalCoverage(res ThinkResult) lexicalCoverage {
	rows := make([]search.LexicalEvidence, 0, len(res.Evidence))
	for _, e := range res.Evidence {
		rows = append(rows, search.LexicalEvidence{Title: e.confidenceTitle, Text: e.confidenceText, Source: e.confidenceSource})
	}
	return search.StrictLexicalCoverage(res.Query, rows)
}
