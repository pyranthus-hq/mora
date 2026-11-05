package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxRuleFileBytes caps a vault rule file. A real one is well under 1 KiB.
const maxRuleFileBytes = 64 << 10

// RulePath is <VaultDir>/rules/<id>.md. The memory indexer reads only
// vault/memories, so rule files are never indexed.
func RulePath(vaultDir, id string) string {
	return filepath.Join(vaultDir, "rules", id+".md")
}

// Parse reads a vault rule file: simple `key: value` frontmatter between
// "---" lines, then the message body. Unknown keys are ignored; unknown spec
// fields are an error so a typo such as "unles" cannot be dropped silently.
// Parse does not call Validate.
func Parse(data []byte) (Rule, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Rule{}, errors.New("rule file must start with a --- line")
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	body := ""
	switch {
	case end >= 0:
		body = rest[end+len("\n---\n"):]
	case strings.HasSuffix(rest, "\n---"):
		end = len(rest) - len("\n---")
	default:
		return Rule{}, errors.New("rule file frontmatter has no closing --- line")
	}
	var r Rule
	seen := map[string]bool{}
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return Rule{}, fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if seen[key] {
			return Rule{}, fmt.Errorf("frontmatter key %q appears twice", key)
		}
		seen[key] = true
		switch key {
		case "id":
			r.ID = val
		case "title":
			r.Title = val
		case "created_at":
			r.CreatedAt = val
		case "source":
			r.Source = val
		case "spec":
			dec := json.NewDecoder(strings.NewReader(val))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&r.Spec); err != nil {
				return Rule{}, fmt.Errorf("spec: %w", err)
			}
			if dec.More() {
				return Rule{}, errors.New("spec: trailing data after the JSON object")
			}
		}
	}
	if !seen["spec"] {
		return Rule{}, errors.New("rule file has no spec line")
	}
	r.Message = strings.TrimSpace(body)
	return r, nil
}

// Render is the inverse of Parse for a valid rule.
func Render(r Rule) []byte {
	// No HTML escaping, so the file shows && and not \u0026\u0026.
	var spec bytes.Buffer
	enc := json.NewEncoder(&spec)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(r.Spec)
	var b bytes.Buffer
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", r.ID)
	if r.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", r.Title)
	}
	if r.CreatedAt != "" {
		fmt.Fprintf(&b, "created_at: %s\n", r.CreatedAt)
	}
	if r.Source != "" {
		fmt.Fprintf(&b, "source: %s\n", r.Source)
	}
	fmt.Fprintf(&b, "spec: %s\n", bytes.TrimSpace(spec.Bytes()))
	b.WriteString("---\n")
	b.WriteString(r.Message)
	b.WriteString("\n")
	return b.Bytes()
}

// ReadRule reads and parses <VaultDir>/rules/<id>.md, capped in size. It
// returns an error wrapping os.ErrNotExist when the file is missing.
func ReadRule(vaultDir, id string) (Rule, error) {
	if !idRE.MatchString(id) {
		return Rule{}, fmt.Errorf("invalid rule id %q", id)
	}
	f, err := os.Open(RulePath(vaultDir, id))
	if err != nil {
		return Rule{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRuleFileBytes+1))
	if err != nil {
		return Rule{}, err
	}
	if len(data) > maxRuleFileBytes {
		return Rule{}, fmt.Errorf("rule file is larger than %d bytes", maxRuleFileBytes)
	}
	return Parse(data)
}
