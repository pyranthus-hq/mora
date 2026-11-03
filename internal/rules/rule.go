// Package rules is the pure engine behind the PreToolUse warning hook: rule
// specs and their content hash, RE2 matching with Python whitespace parity,
// the hash split into warned and silent groups, the compiled active set the
// hook reads, the hook log, and the hook input parser.
//
// It must stay light. The hook runs before every Bash call, so this package
// imports no SQLite, no index and nothing from internal/mcp. Only rules a
// human approved by their exact content hash reach active.json (see Compile).
package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxMessageRunes caps a rule message. Steward used the same 200 limit.
const MaxMessageRunes = 200

// ContextSubagent limits a rule to calls made from a subagent.
const ContextSubagent = "subagent"

// Spec is the matching part of a rule. Its JSON form is the `spec:` line of
// the vault rule file, and it is part of the content hash.
type Spec struct {
	Version int    `json:"version"`           // must be 1
	Tool    string `json:"tool"`              // "Bash" only in v1
	Match   string `json:"match"`             // RE2, required
	Exclude string `json:"exclude,omitempty"` // RE2; matches replaced by " " before Match
	Unless  string `json:"unless,omitempty"`  // RE2; checked against the rest of the simple command
	Context string `json:"context,omitempty"` // "" or "subagent"
}

// Rule is one parsed vault rule file. Title, CreatedAt and Source are
// descriptive only and are not part of the hash.
type Rule struct {
	ID, Title, CreatedAt, Source, Message string
	Spec                                  Spec
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// Validate checks the id, the spec and the message. Every regex is checked
// for Python-only syntax first so the error says what to write instead.
func Validate(r Rule) error {
	if !idRE.MatchString(r.ID) {
		return fmt.Errorf("invalid rule id %q: use 1 to 48 lowercase letters, digits or dashes, starting with a letter or digit", r.ID)
	}
	if _, err := NewMatcher(r.ID, r.Spec); err != nil {
		return err
	}
	if err := validateMessage(r.Message); err != nil {
		return err
	}
	for _, f := range []struct{ name, val string }{{"title", r.Title}, {"created_at", r.CreatedAt}, {"source", r.Source}} {
		if hasControl(f.val) || f.val != strings.TrimSpace(f.val) {
			return fmt.Errorf("%s must be a single trimmed line without control characters", f.name)
		}
	}
	return nil
}

func validateMessage(msg string) error {
	switch {
	case msg == "":
		return errors.New("message is required")
	case msg != strings.TrimSpace(msg):
		return errors.New("message must not start or end with whitespace")
	case strings.ContainsAny(msg, "\r\n"):
		return errors.New("message must be a single line")
	case !utf8.ValidString(msg):
		return errors.New("message must be valid UTF-8")
	case utf8.RuneCountInString(msg) > MaxMessageRunes:
		return fmt.Errorf("message is %d characters; the limit is %d", utf8.RuneCountInString(msg), MaxMessageRunes)
	case hasControl(msg):
		return errors.New("message must not contain control characters")
	}
	return nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// Hash is the approval key: sha256 over the id, the spec and the message,
// marshalled with a fixed field order. Title, created_at and source can change
// without dropping an approval.
func Hash(r Rule) string {
	body, _ := json.Marshal(struct {
		ID      string
		Spec    Spec
		Message string
	}{r.ID, r.Spec, r.Message})
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ShortHash returns the first 12 hex digits of a Hash value.
func ShortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}
