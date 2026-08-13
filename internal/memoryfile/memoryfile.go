// Package memoryfile owns Mora's human-readable Markdown memory codec and vault paths.
package memoryfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// Document is the persisted subset of a Mora memory.
type Document struct {
	ID          string
	Scope       string
	Type        string
	Title       string
	Tags        []string
	Source      string
	CreatedAt   string
	Path        string
	Text        string
	Provider    string
	Account     string
	ProviderID  string
	ContentHash string
	LastSynced  string
	Truncated   bool
	DeletedAt   string
	Decision    *memory.DecisionValidity
	Meta        map[string]any
}

// Render serializes a document as canonical Mora Markdown.
func Render(m Document) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "id: %s\nscope: %s\ntype: %s\ntitle: %s\n", m.ID, m.Scope, m.Type, QuoteYAML(m.Title))
	fmt.Fprintf(&b, "tags: [%s]\nsource: %s\ncreated_at: %s\n", strings.Join(m.Tags, ", "), QuoteYAML(m.Source), m.CreatedAt)
	// Issue #237: gate on EITHER field, not just Provider — a caller (or the
	// cluster-contract test fixture) can legitimately set ProviderID alone, and
	// dropping it silently would break Rule 1 (provider anchor equality), which
	// depends on ProviderID surviving the round-trip. Every production connector
	// already sets both together (memory.MapItem), so this only widens what
	// persists, never narrows it.
	if m.Provider != "" || m.ProviderID != "" {
		fmt.Fprintf(&b, "provider: %s\nprovider_id: %s\n", m.Provider, QuoteYAML(m.ProviderID))
	}
	if m.Account != "" {
		fmt.Fprintf(&b, "account: %s\n", m.Account)
	}
	if m.ContentHash != "" {
		fmt.Fprintf(&b, "content_hash: %s\n", m.ContentHash)
	}
	if m.LastSynced != "" {
		fmt.Fprintf(&b, "last_synced: %s\n", m.LastSynced)
	}
	if m.Truncated {
		fmt.Fprintf(&b, "truncated: true\n")
	}
	if m.DeletedAt != "" {
		fmt.Fprintf(&b, "deleted_at: %s\n", m.DeletedAt)
	}
	if decision := m.Decision; decision != nil {
		decisionJSON, err := json.Marshal(decision)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "decision: %s\n", decisionJSON)
	}
	// Meta is one canonical JSON line. json.Marshal sorts keys and never emits a
	// raw newline, so the value survives the line-split parser and the inner colons
	// survive strings.Cut(line, ":") (which splits on the FIRST colon only).
	if metaJSON, err := memory.CanonicalMeta(m.Meta); err != nil {
		return nil, err
	} else if metaJSON != "" {
		fmt.Fprintf(&b, "meta: %s\n", metaJSON)
	}
	fmt.Fprintf(&b, "---\n\n%s\n", m.Text)
	return []byte(b.String()), nil
}

// Parse reads and parses a memory file. It is a thin wrapper over
// ParseBytes so the rebuild loop, which needs the raw bytes for the content
// manifest's sha256 (B1a), can read the file ONCE and get both the hash and the
// parse from the same bytes — zero extra I/O — instead of ReadFile-ing twice.

func Parse(path string) (Document, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	return ParseBytes(path, b)
}

// ParseBytes parses an already-read memory file body.
func ParseBytes(path string, b []byte) (Document, error) {
	text := string(b)
	if !strings.HasPrefix(text, "---\n") {
		return Document{}, errors.New("missing frontmatter")
	}
	parts := strings.SplitN(text[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return Document{}, errors.New("invalid frontmatter")
	}
	m := Document{Path: path, Text: strings.TrimSpace(parts[1])}
	for _, line := range strings.Split(parts[0], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		// Meta is a JSON object value with inner colons/quotes — decode the RAW
		// substring after the first colon, NOT the quote-trimmed val (trimming would
		// not corrupt an object, but decoding the raw form is unambiguous).
		if key == "meta" {
			_, raw, _ := strings.Cut(line, ":")
			// UseNumber so a numeric value (e.g. a 19-digit thread/message id) decodes
			// to json.Number, not a lossy float64 — no silent precision loss in Meta.
			dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
			dec.UseNumber()
			meta := map[string]any{}
			if jerr := dec.Decode(&meta); jerr != nil {
				// Never silently drop data: a corrupt meta: line (hand-edit, partial
				// write) loses the memory's entire structured identity from the graph,
				// so surface it instead of swallowing the error.
				fmt.Fprintf(os.Stderr, "warn: %s: meta frontmatter is corrupt and was ignored: %v\n", path, jerr)
			} else if len(meta) > 0 {
				m.Meta = meta
			}
			continue
		}
		if key == "decision" {
			_, raw, _ := strings.Cut(line, ":")
			var decision memory.DecisionValidity
			if jerr := json.Unmarshal([]byte(strings.TrimSpace(raw)), &decision); jerr != nil {
				return Document{}, fmt.Errorf("decision frontmatter is corrupt: %w", jerr)
			}
			m.Decision = &decision
			continue
		}
		switch key {
		case "id", "scope", "type", "title", "tags", "source", "created_at",
			"provider", "provider_id", "account", "content_hash", "last_synced",
			"truncated", "deleted_at":
			// Known scalar field; decode below.
		default:
			// Preserve forward compatibility: older Mora versions ignore fields
			// they do not understand, including their value syntax.
			continue
		}
		parsedVal, err := ParseFrontmatterScalar(key, val)
		if err != nil {
			return Document{}, err
		}
		val = parsedVal
		switch key {
		case "id":
			m.ID = val
		case "scope":
			m.Scope = val
		case "type":
			m.Type = val
		case "title":
			m.Title = val
		case "tags":
			m.Tags = parseTags(val)
		case "source":
			m.Source = val
		case "created_at":
			m.CreatedAt = val
		case "provider":
			m.Provider = val
		case "provider_id":
			m.ProviderID = val
		case "account":
			m.Account = val
		case "content_hash":
			m.ContentHash = val
		case "last_synced":
			m.LastSynced = val
		case "truncated":
			m.Truncated = val == "true"
		case "deleted_at":
			m.DeletedAt = val
		}
	}
	if m.ID == "" {
		return Document{}, errors.New("missing id")
	}
	return m, nil
}

// MemoriesRoot returns the user-memory tree root.
func MemoriesRoot(cfg config.Config) string { return filepath.Join(cfg.VaultDir, "memories") }

// SourcesRoot returns the connector-source tree root.
func SourcesRoot(cfg config.Config) string { return filepath.Join(cfg.VaultDir, "sources") }

// Path returns the canonical path for a document.
func Path(cfg config.Config, m Document) string {
	scopePath := strings.ReplaceAll(m.Scope, ":", string(os.PathSeparator))
	scopePath = strings.ReplaceAll(scopePath, "/", string(os.PathSeparator))
	return filepath.Join(MemoriesRoot(cfg), scopePath, OSSafeBase(m.ID)+".md")
}

// OSSafeBase converts a memory ID (or an already-SafeFilename'd stable key) into
// a base filename legal on the host OS. On macOS/Linux the input is returned
// unchanged, so existing vaults stay byte-identical and need no migration — the
// deliberate cross-OS trade-off recorded in #56. On Windows
// memory.SanitizeWindowsBase maps the reserved set (IDs derived from user or
// connector content can contain ? : * " < > | which os.CreateTemp rejects with
// "The filename, directory name, or volume label syntax is incorrect"); when
// that actually alters the string, a short deterministic hash of the ORIGINAL is
// appended so two distinct IDs that sanitize alike (e.g. "a?" and "a*") never
// collide onto one file and silently overwrite each other.
//
// The mapping is deterministic, so a later write or ID lookup
// reproduces the same name. The memory's real id lives in frontmatter (read by
// Parse), so the filename need only be stable, not reversible.

func OSSafeBase(id string) string {
	if runtime.GOOS != "windows" {
		return id
	}
	safe := memory.SanitizeWindowsBase(id)
	if safe != id {
		safe += "_" + memory.ContentHash(id)[:8]
	}
	return safe
}

// All returns every Markdown memory path in deterministic order.
func All(cfg config.Config) ([]string, error) {
	var paths []string
	for _, root := range []string{MemoriesRoot(cfg), SourcesRoot(cfg)} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			// Surface walk errors (an unreadable directory must not silently
			// shrink the index to the readable subset); a missing root is the
			// one benign case — a fresh vault simply has no sources/ tree yet.
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", root, err)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func parseTags(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return genericutil.SplitCSV(s)
}

// QuoteYAML returns the canonical frontmatter scalar encoding.
func QuoteYAML(s string) string {
	// Keep the renderer and parseFrontmatterScalar symmetric. Backslashes and
	// quotes are included even when this small frontmatter grammar could carry
	// them unquoted: emitting one canonical quoted representation ensures paths
	// and escaped characters round-trip identically on every OS.
	if s == "" || strings.TrimSpace(s) != s || strings.ContainsAny(s, ":#[]\"\\\n\r\t") {
		return strconv.Quote(s)
	}
	return s
}

// ParseFrontmatterScalar decodes the exact quoted representation emitted by
// QuoteYAML. The previous parser only trimmed quote characters, leaving path
// separators doubled in memory. That changed identity-bearing source paths
// after a render/parse round trip.
//
// A value beginning with a quote is treated as quoted data and must be a valid,
// complete strconv string. Failing closed here prevents malformed frontmatter
// from silently producing a different path or provider identity.

func ParseFrontmatterScalar(key, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if !strings.HasPrefix(value, `"`) {
		return value, nil
	}
	decoded, err := strconv.Unquote(value)
	if err != nil {
		// Legacy quoteYAML did not quote a title merely because it contained a
		// quote. A title such as `"Quoted title` was therefore written raw and
		// the old parser kept the memory readable by trimming quote characters.
		// Retain that cosmetic-field compatibility, but do not extend it to
		// source/provider identity where a changed value can alter matching,
		// governance, or citation behavior.
		if key == "title" {
			return strings.Trim(value, `"`), nil
		}
		return "", fmt.Errorf("%s frontmatter value is corrupt: %w", key, err)
	}
	return decoded, nil
}
