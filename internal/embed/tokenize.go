package embed

import (
	"strings"
	"unicode"
)

func TokenizeForScan(s string) (words []string, joinable []bool) {
	var cur strings.Builder
	inWord := false
	sepSeen := false
	sepSpaceOnly := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '-' {
			if !inWord {
				if len(words) == 0 {
					joinable = append(joinable, false)
				} else {
					joinable = append(joinable, sepSeen && sepSpaceOnly)
				}
				inWord = true
			}
			cur.WriteRune(r)
		} else {
			if inWord {
				words = append(words, strings.ToLower(cur.String()))
				cur.Reset()
				inWord = false
				sepSpaceOnly = true
			}
			sepSeen = true
			if r != ' ' && r != '\t' {
				sepSpaceOnly = false
			}
		}
	}
	if inWord {
		words = append(words, strings.ToLower(cur.String()))
	}
	return words, joinable
}
func tokenizeWords(s string) []string { tokens, _ := TokenizeForScan(s); return tokens }
func TokenizeWords(s string) []string { return tokenizeWords(s) }
