package search

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const snippetTermCap = 12

func snippetTerms(query string) [][]rune {
	var out [][]rune
	seen := map[string]bool{}
	for _, f := range strings.Fields(query) {
		term, key := Token(f)
		if key == "" || seen[key] || IsStopword(term, key) {
			continue
		}
		kr := []rune(key)
		if len(kr) < 2 {
			continue
		}
		seen[key] = true
		out = append(out, kr)
		if len(out) == snippetTermCap {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
func earliestQueryMatch(r []rune, query string) int {
	terms := snippetTerms(query)
	if len(terms) == 0 {
		return -1
	}
	lower := make([]rune, len(r))
	for i, c := range r {
		lower[i] = unicode.ToLower(c)
	}
	isWord := func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }
	for _, t := range terms {
		for i := 0; i+len(t) <= len(lower); i++ {
			if i > 0 && isWord(lower[i-1]) {
				continue // mid-word — not a token start
			}
			hit := true
			for j, tc := range t {
				if lower[i+j] != tc {
					hit = false
					break
				}
			}
			if !hit {
				continue
			}
			if i+len(t) < len(lower) && isWord(lower[i+len(t)]) {
				continue // prefix of a longer word ("dan" in "abundant")
			}
			return i
		}
	}
	return -1
}
func MatchSnippet(text, query string, n int) string {
	flat := strings.Join(strings.Fields(text), " ")
	r := []rune(flat)
	if len(r) <= n {
		return flat
	}
	pos := earliestQueryMatch(r, query)
	leadIn := n / 3 // context ahead of the match so the term doesn't open the window cold
	if pos < 0 || pos <= leadIn {
		// No body match, or the match already sits inside the head window.
		return strings.TrimSpace(string(r[:n])) + "…"
	}
	start := pos - leadIn
	if start+n > len(r) {
		start = len(r) - n
	}
	for start > 0 && start < pos && r[start-1] != ' ' {
		start++ // never open mid-word
	}
	end := start + n
	if end > len(r) {
		end = len(r)
	}
	out := "…" + strings.TrimSpace(string(r[start:end]))
	if end < len(r) {
		out += "…"
	}
	return out
}

// SnippetMemories flattens and match-centers bounded search previews without graph metadata.
func SnippetMemories(mems []memory.Memory, query string, maxRunes int) []memory.Memory {
	if mems == nil {
		return nil
	}
	out := make([]memory.Memory, len(mems))
	for i, m := range mems {
		full := strings.Join(strings.Fields(m.Text), " ")
		if utf8.RuneCountInString(full) > maxRunes {
			m.Text = MatchSnippet(m.Text, query, maxRunes)
			m.Truncated = true
		} else {
			m.Text = full
		}
		m.Meta = nil
		out[i] = m
	}
	return out
}

// Terms selects bounded, distinctive query terms for snippet matching.
func Terms(query string) [][]rune { return snippetTerms(query) }

// EarliestMatch returns the earliest whole-token query match.
func EarliestMatch(r []rune, query string) int { return earliestQueryMatch(r, query) }
