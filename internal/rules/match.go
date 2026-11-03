package rules

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Action is one parsed Bash tool call. AgentID is "" on the main thread.
// Command is only ever matched, never logged.
type Action struct {
	SessionID string
	AgentID   string
	ToolUseID string
	Command   string
}

// maxUnlessChecks caps the match positions Hit checks against unless.
const maxUnlessChecks = 64

// Matcher is a rule spec with its regexes compiled.
type Matcher struct {
	ID      string
	Context string
	match   *regexp.Regexp
	exclude *regexp.Regexp
	unless  *regexp.Regexp
}

// NewMatcher checks a spec and compiles its regexes.
func NewMatcher(id string, s Spec) (*Matcher, error) {
	if s.Version != 1 {
		return nil, fmt.Errorf("spec version must be 1, got %d", s.Version)
	}
	if s.Tool != "Bash" {
		return nil, fmt.Errorf("spec tool must be Bash, got %q", s.Tool)
	}
	if s.Match == "" {
		return nil, errors.New("spec match is required")
	}
	if s.Context != "" && s.Context != ContextSubagent {
		return nil, fmt.Errorf("spec context must be empty or %q, got %q", ContextSubagent, s.Context)
	}
	m := &Matcher{ID: id, Context: s.Context}
	var err error
	if m.match, err = compilePattern("match", s.Match); err != nil {
		return nil, err
	}
	if s.Exclude != "" {
		if m.exclude, err = compilePattern("exclude", s.Exclude); err != nil {
			return nil, err
		}
	}
	if s.Unless != "" {
		if m.unless, err = compilePattern("unless", s.Unless); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Hit reports whether the matcher fires for one action.
func (m *Matcher) Hit(a Action) bool {
	if m.Context == ContextSubagent && a.AgentID == "" {
		return false
	}
	text := normalizeSpace(a.Command)
	if m.exclude != nil {
		// Python re.sub(exclude, ' ', text). Literal so "$" is not expanded.
		text = m.exclude.ReplaceAllLiteralString(text, " ")
	}
	if m.unless == nil {
		return m.match.MatchString(text)
	}
	// unless stands in for Python's (?![^;&|\n]*X) at the match end: a match
	// counts only when X is absent from the rest of the same simple command.
	// Each check scans to the end of its simple command, so only the first
	// maxUnlessChecks match positions are tried. Without the cap a long command
	// with many matches costs O(n^2) and runs past the hook's budget.
	for _, loc := range m.match.FindAllStringIndex(text, maxUnlessChecks) {
		seg := text[loc[1]:]
		if i := strings.IndexAny(seg, ";&|\n"); i >= 0 {
			seg = seg[:i]
		}
		if !m.unless.MatchString(seg) {
			return true
		}
	}
	return false
}

// Match returns the ids of the matchers that fire, in id order.
func Match(rules []*Matcher, a Action) []string {
	ids := []string{}
	for _, m := range rules {
		if m.Hit(a) {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
