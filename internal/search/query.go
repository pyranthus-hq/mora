// Package search owns query parsing and SQLite FTS query compilation.
package search

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ftsStopwords = map[string]bool{
	"a": true, "about": true, "am": true, "an": true, "and": true, "are": true,
	"as": true, "at": true, "be": true, "been": true, "being": true, "but": true,
	"by": true, "can": true, "could": true, "did": true, "do": true, "does": true,
	"doing": true, "for": true, "from": true, "had": true, "has": true, "have": true,
	"how": true, "i": true, "if": true, "in": true, "into": true, "is": true,
	"it": true, "its": true, "me": true, "my": true, "of": true, "on": true,
	"or": true, "our": true, "so": true, "that": true, "the": true, "their": true,
	"them": true, "then": true, "there": true, "these": true, "they": true,
	"this": true, "to": true, "was": true, "we": true, "were": true, "what": true,
	"when": true, "which": true, "who": true, "will": true, "with": true,
	"would": true, "you": true, "your": true,
}

func Token(f string) (term, key string) {
	term = strings.Trim(f, `"':;,.!?()[]{}<>-`)
	if term == "" {
		return "", ""
	}
	key = strings.ToLower(term)
	if i := strings.IndexAny(key, "'’"); i > 0 {
		key = key[:i]
	}
	return term, key
}

// ftsIsStopword decides whether a token is a droppable function word. It is
// deliberately case-aware: a function word is dropped only when written in
// lowercase. An explicit capital or all-caps form (Will, WHO, IT, CAN, AM)
// signals a proper noun or acronym that is discriminative in a real query, so
// it survives — this generalizes past a hand-picked collision list to protect
// every name/acronym (Mora, Neil, GEO, MFA, IP, SF, …). Single-character
// function words ("a", "i") are pure noise and always dropped regardless of case.

func IsStopword(term, key string) bool {
	if !ftsStopwords[key] {
		return false
	}
	if utf8.RuneCountInString(term) == 1 {
		return true
	}
	return term == strings.ToLower(term)
}

// FTSQuery compiles natural language into a safe, recall-oriented FTS5 MATCH expression.
func FTSQuery(q string) string {
	// Build an OR of quoted content tokens. Space-joining (the original behavior)
	// made FTS5 treat the query as an implicit AND of every token, so a
	// natural-language query like "what did neil say about the offsite" matched
	// nothing (it required every word, stopwords included). OR-joining lets any
	// term match while bm25 ranks the best matches first.
	//
	// But a pure OR of *every* token dilutes ranking: stopwords ("the/with/what")
	// match nearly everything, ballooning the candidate pool and letting docs that
	// hit several common words (while missing the rare, meaningful ones) outrank
	// the true match. Measured on Adit's real-query golden set, dropping function
	// words lifts FTS recall@5 0.591→0.667 (and the hybrid surface 0.394→0.439),
	// with no query regressing inside the top-5 cutoff. So we OR only the
	// content terms; if a query is ALL stopwords we fall back to every token so we
	// never emit an empty MATCH (FTS5 errors on ""). Each term is double-quoted so
	// operators/specials (AND, OR, NOT, *, :, -) inside a term can't raise
	// "fts5: syntax error".
	type tok struct{ term, key string }
	var toks []tok
	for _, f := range strings.Fields(q) {
		term, key := Token(f)
		if term == "" {
			continue
		}
		toks = append(toks, tok{term, key})
	}
	content := make([]tok, 0, len(toks))
	for _, t := range toks {
		if IsStopword(t.term, t.key) {
			continue
		}
		content = append(content, t)
	}
	if len(content) == 0 {
		content = toks // all-stopword query: keep everything rather than match nothing
	}
	terms := make([]string, 0, len(content))
	for _, t := range content {
		terms = append(terms, `"`+strings.ReplaceAll(t.term, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}

// ParseArgs parses the CLI search flags while preserving query words.
func ParseArgs(args []string) (string, int, bool, []string, error) {
	scope := ""
	limit := 10
	jsonOut := false
	var query []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--scope":
			if i+1 >= len(args) {
				return "", 0, false, nil, errors.New("--scope requires value")
			}
			i++
			scope = args[i]
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case a == "--limit":
			if i+1 >= len(args) {
				return "", 0, false, nil, errors.New("--limit requires value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return "", 0, false, nil, err
			}
			limit = n
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return "", 0, false, nil, err
			}
			limit = n
		default:
			query = append(query, a)
		}
	}
	return scope, limit, jsonOut, query, nil
}
