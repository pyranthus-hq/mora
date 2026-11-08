package rules

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The Steward prototype ran Python re. Go's regexp is RE2, so the Python-only
// features fail here with a message that says what to write instead.
// regexp.Compile is still the final gate after this scan.
var (
	errLookahead  = errors.New("lookahead is not supported by Go RE2. Put the forbidden text in `unless` (checked against the rest of the same simple command) or `exclude`")
	errLookbehind = errors.New("lookbehind is not supported. Use a consumed class such as `(?:^|[^A-Za-z0-9_./~$-])`")
	errAtomic     = errors.New("atomic groups (?> are not supported by Go RE2")
	errPossessive = errors.New("possessive quantifiers such as *+ are not supported by Go RE2")
	errBackref    = errors.New(`backreferences such as \1 or (?P=name) are not supported by Go RE2`)
	errBigZ       = errors.New(`\Z is not supported by Go RE2. Use \z`)
)

var countedRE = regexp.MustCompile(`^\{\d*(,\d*)?\}`)

// pythonOnly scans a pattern for Python re syntax that RE2 lacks. It tracks
// character classes and escapes so that, for example, `[(?=]` or `\(?=` is
// not reported as a lookahead.
func pythonOnly(p string) error {
	inClass := false
	prevQuant := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if inClass {
			switch c {
			case '\\':
				if i+1 < len(p) && p[i+1] == 'Z' {
					return errBigZ
				}
				i++
			case '[':
				// POSIX class such as [:alpha:] inside a bracket expression.
				if i+1 < len(p) && p[i+1] == ':' {
					if j := strings.Index(p[i+2:], ":]"); j >= 0 {
						i += j + 3
					}
				}
			case ']':
				inClass = false
			}
			continue
		}
		quant := false
		switch c {
		case '\\':
			if i+1 < len(p) {
				n := p[i+1]
				if n >= '1' && n <= '9' {
					return errBackref
				}
				if n == 'Z' {
					return errBigZ
				}
				i++
			}
		case '[':
			inClass = true
			// A ']' right after '[' or '[^' is a literal, not the end.
			if i+1 < len(p) && p[i+1] == '^' {
				i++
			}
			if i+1 < len(p) && p[i+1] == ']' {
				i++
			}
		case '(':
			rest := p[i+1:]
			switch {
			case strings.HasPrefix(rest, "?="), strings.HasPrefix(rest, "?!"):
				return errLookahead
			case strings.HasPrefix(rest, "?<="), strings.HasPrefix(rest, "?<!"):
				return errLookbehind
			case strings.HasPrefix(rest, "?>"):
				return errAtomic
			case strings.HasPrefix(rest, "?P="):
				return errBackref
			}
			// The '?' that opens a group flag is not a quantifier.
			if strings.HasPrefix(rest, "?") {
				i++
			}
		case '*', '+', '?':
			switch {
			case prevQuant && c == '+':
				return errPossessive
			case prevQuant && c == '?':
				// lazy marker, as in *?
			default:
				quant = true
			}
		case '{':
			if m := countedRE.FindString(p[i:]); m != "" {
				i += len(m) - 1
				quant = true
			}
		}
		prevQuant = quant
	}
	return nil
}

// compilePattern runs the Python-only scan and then regexp.Compile.
func compilePattern(field, p string) (*regexp.Regexp, error) {
	if err := pythonOnly(p); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return re, nil
}

// pySpace reports the characters that Python's str \s matches but Go's \s
// ([\t\n\f\r ]) does not.
func pySpace(r rune) bool {
	switch {
	case r == '\v', r >= 0x1c && r <= 0x1f, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// normalizeSpace maps every pySpace character to an ASCII space before any
// regex runs, so `cd /tmp && make` hits here as it does in Python.
// \b stays ASCII-only in RE2; that gap is not closed.
func normalizeSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if pySpace(r) {
			return ' '
		}
		return r
	}, s)
}
