// Package corrections turns transcript records into counts: what a person
// rejected or interrupted, and whether the agent tried it again in a later
// session. Nothing here keeps raw command text.
package corrections

import (
	"crypto/sha256"
	"regexp"
	"strings"
)

// Class says how much the tables know about a simple command.
type Class int

const (
	ClassOther Class = iota
	ClassLow
	ClassKnown
)

// Key is the private identity of a simple command. It stays in memory: never
// write, print or log it.
type Key [16]byte

// Segment is one simple command.
type Segment struct {
	Shape string // table text only; "" for ClassOther
	Class Class
	Key   Key
	Name  Key // hash of the command name, for the fix rule
}

// Parsed is one command string, split into simple commands.
type Parsed struct {
	Segments []Segment
	Primary  int // index, -1 when empty
}

// OtherShape is how a command with no Shape prints.
const OtherShape = "(other command)"

const (
	maxInput     = 64 << 10
	maxSimple    = 32
	heredocStops = " \t\r\n;|&<>()"
)

var (
	longHex = regexp.MustCompile(`[0-9a-fA-F]{7,}`)
	digits  = regexp.MustCompile(`[0-9]+`)
)

// Parse splits a command and names each simple command. Shape is only ever
// built from table strings, so a lexer bug can change a count but cannot leak
// input text. Do not add a path that copies input into Shape.
func Parse(command string) Parsed {
	p := Parsed{Primary: -1}
	flagged := -1
	for _, words := range lex(command) {
		seg, marked, ok := parseSimple(words)
		if !ok {
			continue
		}
		if marked && flagged < 0 {
			flagged = len(p.Segments)
		}
		p.Segments = append(p.Segments, seg)
	}
	p.Primary = primaryIndex(p.Segments, flagged)
	return p
}

// primaryIndex is section 3.6 of the spec: the simple command a call is judged by.
func primaryIndex(segs []Segment, flagged int) int {
	if flagged >= 0 {
		return flagged
	}
	for _, class := range []Class{ClassKnown, ClassOther} {
		for i := len(segs) - 1; i >= 0; i-- {
			if segs[i].Class == class {
				return i
			}
		}
	}
	return len(segs) - 1
}

func parseSimple(words []string) (Segment, bool, bool) {
	words = dropWrappers(words)
	if len(words) == 0 {
		return Segment{}, false, false
	}
	name := commandName(words[0])
	base, class, args := baseShape(name, words[1:])
	shape := base
	var marks []string
	if class == ClassKnown {
		marks = shapeMarks(base, args)
		shape = strings.Join(append([]string{base}, marks...), " ")
	}
	parts := []string{name}
	for _, w := range words[1:] {
		parts = append(parts, keyToken(base, w))
	}
	seg := Segment{Shape: shape, Class: class, Key: hashKey(strings.Join(parts, "\x00")), Name: hashKey(name)}
	return seg, len(marks) > 0, true
}

// dropWrappers removes leading NAME=value tokens and wrappers, with the flags
// that follow a wrapper.
func dropWrappers(words []string) []string {
	for len(words) > 0 {
		if isAssignment(words[0]) {
			words = words[1:]
			continue
		}
		name := commandName(words[0])
		if !wrappers[name] {
			break
		}
		words = words[1:]
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			words = words[1:]
		}
		if name == "timeout" && len(words) > 0 {
			words = words[1:]
		}
	}
	return words
}

func isAssignment(w string) bool {
	name, _, found := strings.Cut(w, "=")
	if !found || name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		letter := c == '_' || c|0x20 >= 'a' && c|0x20 <= 'z'
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func commandName(w string) string {
	return strings.TrimSuffix(lastElement(w), ".exe")
}

func lastElement(w string) string {
	w = strings.TrimRight(w, `/\`)
	return w[strings.LastIndexAny(w, `/\`)+1:]
}

// baseShape returns the shape before flags and the tokens after it. Every
// string it returns in the first position is a table value.
func baseShape(name string, rest []string) (string, Class, []string) {
	if low, ok := lowWords[name]; ok {
		return low, ClassLow, nil
	}
	if table, ok := subNames[name]; ok {
		for i := 0; i < len(rest); i++ {
			switch {
			case valueFlags[rest[i]]:
				i++
			case strings.HasPrefix(rest[i], "-"):
			default:
				if sub, ok := subWords[name][rest[i]]; ok {
					return table + " " + sub, ClassKnown, rest[i+1:]
				}
				return table, ClassKnown, rest[i+1:]
			}
		}
		return table, ClassKnown, nil
	}
	if plain, ok := plainWords[name]; ok {
		return plain, ClassKnown, rest
	}
	return "", ClassOther, nil
}

// shapeMarks returns the table flags and cluster that args hold, in table
// order. Input tokens are only looked up, and what comes back is table text.
func shapeMarks(base string, args []string) []string {
	var marks []string
	if flags, ok := shapeFlags[base]; ok {
		seen := map[string]bool{}
		for _, a := range args {
			if alias, ok := shapeAlias[base+" "+a]; ok {
				a = alias
			}
			seen[a] = true
		}
		for _, flag := range strings.Fields(flags) {
			if seen[flag] {
				marks = append(marks, flag)
			}
		}
	}
	letters := shapeClusters[base]
	kept := ""
	for i := range letters {
		if hasClusterLetter(args, letters[i:i+1]) {
			kept += letters[i : i+1]
		}
	}
	if kept != "" {
		marks = append(marks, "-"+kept)
	}
	return marks
}

func hasClusterLetter(args []string, letter string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") &&
			strings.ContainsAny(a, letter+strings.ToUpper(letter)) {
			return true
		}
	}
	return false
}

// keyToken normalizes one argument for the Key, as section 3.5 of the spec says.
func keyToken(base, w string) string {
	switch {
	case strings.ContainsAny(w, " \t\r\n\f\v"):
		return "Q"
	case strings.HasPrefix(w, "-"):
		w, _, _ = strings.Cut(w, "=")
		if alias, ok := shapeAlias[base+" "+w]; ok {
			w = alias
		}
	case strings.ContainsAny(w, `/\`):
		w = lastElement(w)
	}
	return digits.ReplaceAllString(longHex.ReplaceAllString(w, "H"), "0")
}

func hashKey(s string) Key {
	sum := sha256.Sum256([]byte(s))
	return Key(sum[:16])
}

type heredoc struct {
	delim string
	tabs  bool // <<- strips leading tabs from each line
}

// lexer splits a command into simple commands of tokens, with quotes removed.
// It does not recurse into $(...), backticks or (...).
type lexer struct {
	src     string
	cmds    [][]string
	words   []string
	tok     []byte
	inTok   bool
	pending []heredoc
}

func lex(src string) [][]string {
	if len(src) > maxInput {
		src = src[:maxInput]
	}
	l := lexer{src: src}
	var quote byte
	for i := 0; i < len(src) && len(l.cmds) < maxSimple; i++ {
		c := src[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				l.add(c)
			}
		case c == '\\':
			i++
			if i < len(src) && src[i] != '\n' {
				l.add(src[i])
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else {
				l.add(c)
			}
		case c == '\'' || c == '"':
			quote = c
			l.inTok = true
		case c == '#' && !l.inTok:
			for i+1 < len(src) && src[i+1] != '\n' {
				i++
			}
		case c == ' ' || c == '\t' || c == '\r':
			l.endToken()
		case c == ';' || c == '|':
			l.endCommand()
		case c == '\n':
			l.endCommand()
			i = l.skipHeredocs(i+1) - 1
		case c == '&':
			if (i > 0 && (src[i-1] == '>' || src[i-1] == '<')) || (i+1 < len(src) && src[i+1] == '>') {
				l.add(c)
			} else {
				l.endCommand()
			}
		case strings.HasPrefix(src[i:], "<<<"):
			l.tok = append(l.tok, "<<<"...)
			l.inTok = true
			i += 2
		case strings.HasPrefix(src[i:], "<<"):
			i = l.readHeredoc(i+2) - 1
		default:
			l.add(c)
		}
	}
	l.endCommand()
	return l.cmds
}

func (l *lexer) add(c byte) {
	l.tok = append(l.tok, c)
	l.inTok = true
}

func (l *lexer) endToken() {
	if l.inTok {
		l.words = append(l.words, string(l.tok))
	}
	l.tok, l.inTok = l.tok[:0], false
}

func (l *lexer) endCommand() {
	l.endToken()
	if words := trimParens(l.words); len(words) > 0 && len(l.cmds) < maxSimple {
		l.cmds = append(l.cmds, words)
	}
	l.words = nil
}

func trimParens(words []string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "(") {
		if words[0] = strings.TrimLeft(words[0], "("); words[0] != "" {
			break
		}
		words = words[1:]
	}
	for len(words) > 0 && strings.HasSuffix(words[len(words)-1], ")") {
		last := len(words) - 1
		if words[last] = strings.TrimRight(words[last], ")"); words[last] != "" {
			break
		}
		words = words[:last]
	}
	return words
}

// readHeredoc reads the delimiter word after << and returns where it ends. The
// body starts at the next newline, so the rest of this line is lexed as usual.
func (l *lexer) readHeredoc(i int) int {
	h := heredoc{}
	if i < len(l.src) && l.src[i] == '-' {
		h.tabs = true
		i++
	}
	for i < len(l.src) && (l.src[i] == ' ' || l.src[i] == '\t') {
		i++
	}
	var delim []byte
	for ; i < len(l.src) && strings.IndexByte(heredocStops, l.src[i]) < 0; i++ {
		if c := l.src[i]; c != '\'' && c != '"' && c != '\\' {
			delim = append(delim, c)
		}
	}
	if len(delim) > 0 {
		h.delim = string(delim)
		l.pending = append(l.pending, h)
	}
	return i
}

// skipHeredocs skips each pending body, to a line equal to its delimiter or to
// the end of the input, and returns where lexing goes on.
func (l *lexer) skipHeredocs(i int) int {
	for _, h := range l.pending {
		for i < len(l.src) {
			line, _, _ := strings.Cut(l.src[i:], "\n")
			i += len(line) + 1
			if h.tabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				break
			}
		}
	}
	l.pending = nil
	return min(i, len(l.src))
}
