package mora

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"io/fs"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// writeMemory renders m and atomicWrites it to its memoryPath at the memory's
// EXISTING id. It is the non-exclusive writer: it overwrites (last-writer-wins),
// so it is NOT used for brand-new user memories — those go through createMemory,
// which is collision-proof against a freshly minted, non-deterministic id. Its
// remaining role is writing a memory at a known, caller-supplied id (test seeding,
// and any caller that already owns the id); connector memories use writeMappedMemory.
func writeMemory(cfg Config, m Memory) error {
	body, err := renderMemory(m)
	if err != nil {
		return err
	}
	return atomicWrite(memoryPath(cfg, m), body, 0o644)
}
func renderMemory(m Memory) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "id: %s\nscope: %s\ntype: %s\ntitle: %s\n", m.ID, m.Scope, m.Type, quoteYAML(m.Title))
	fmt.Fprintf(&b, "tags: [%s]\nsource: %s\ncreated_at: %s\n", strings.Join(m.Tags, ", "), quoteYAML(m.Source), m.CreatedAt)
	// Issue #237: gate on EITHER field, not just Provider — a caller (or the
	// cluster-contract test fixture) can legitimately set ProviderID alone, and
	// dropping it silently would break Rule 1 (provider anchor equality), which
	// depends on ProviderID surviving the round-trip. Every production connector
	// already sets both together (memory.MapItem), so this only widens what
	// persists, never narrows it.
	if m.Provider != "" || m.ProviderID != "" {
		fmt.Fprintf(&b, "provider: %s\nprovider_id: %s\n", m.Provider, quoteYAML(m.ProviderID))
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
	if decision := normalizeDecisionValidity(m); decision != nil {
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

// parseMemory reads and parses a memory file. It is a thin wrapper over
// parseMemoryBytes so the rebuild loop, which needs the raw bytes for the content
// manifest's sha256 (B1a), can read the file ONCE and get both the hash and the
// parse from the same bytes — zero extra I/O — instead of ReadFile-ing twice.
func parseMemory(path string) (Memory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Memory{}, err
	}
	return parseMemoryBytes(path, b)
}

// parseMemoryBytes parses an already-read memory file body.
func parseMemoryBytes(path string, b []byte) (Memory, error) {
	text := string(b)
	if !strings.HasPrefix(text, "---\n") {
		return Memory{}, errors.New("missing frontmatter")
	}
	parts := strings.SplitN(text[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return Memory{}, errors.New("invalid frontmatter")
	}
	m := Memory{Path: path, Text: strings.TrimSpace(parts[1])}
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
			var decision DecisionValidity
			if jerr := json.Unmarshal([]byte(strings.TrimSpace(raw)), &decision); jerr != nil {
				return Memory{}, fmt.Errorf("decision frontmatter is corrupt: %w", jerr)
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
		parsedVal, err := parseFrontmatterScalar(key, val)
		if err != nil {
			return Memory{}, err
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
		return Memory{}, errors.New("missing id")
	}
	if m.Type == "decision" {
		m.Decision = normalizeDecisionValidity(m)
	}
	return m, nil
}
func memoriesRoot(cfg Config) string { return filepath.Join(cfg.VaultDir, "memories") }
func sourcesRoot(cfg Config) string  { return filepath.Join(cfg.VaultDir, "sources") }
func memoryPath(cfg Config, m Memory) string {
	scopePath := strings.ReplaceAll(m.Scope, ":", string(os.PathSeparator))
	scopePath = strings.ReplaceAll(scopePath, "/", string(os.PathSeparator))
	return filepath.Join(memoriesRoot(cfg), scopePath, osSafeBase(m.ID)+".md")
}

// osSafeBase converts a memory ID (or an already-SafeFilename'd stable key) into
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
// The mapping is deterministic, so a later write or findMemory lookup
// reproduces the same name. The memory's real id lives in frontmatter (read by
// parseMemory), so the filename need only be stable, not reversible.
func osSafeBase(id string) string {
	if runtime.GOOS != "windows" {
		return id
	}
	safe := memory.SanitizeWindowsBase(id)
	if safe != id {
		safe += "_" + memory.ContentHash(id)[:8]
	}
	return safe
}
func allMemoryFiles(cfg Config) ([]string, error) {
	var paths []string
	for _, root := range []string{memoriesRoot(cfg), sourcesRoot(cfg)} {
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
func findMemoryRaw(cfg Config, id string) (Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return Memory{}, err
	}
	// Google memories store an ID like "gmail_thread/abc" but are filed under the
	// SafeFilename form "gmail_thread_abc.md", so match both shapes. On Windows
	// the on-disk name is the osSafeBase form (reserved chars mapped + a hash
	// suffix), which matches neither, so add those shapes too; on macOS/Linux
	// osSafeBase is the identity, so osBase/osSafe collapse onto base/safeBase.
	base := id + ".md"
	safeBase := memory.SafeFilename(id) + ".md"
	osBase := osSafeBase(id) + ".md"
	osSafe := osSafeBase(memory.SafeFilename(id)) + ".md"
	// Keep the common provider/write-minted shapes fast, but never make the file
	// name part of memory identity. Imported/materialized vaults may use an
	// arbitrary human filename while frontmatter carries the stable id returned by
	// list/search/graph. Defer those files to a second source-of-truth pass.
	var fallback []string
	for _, path := range files {
		b := filepath.Base(path)
		if b != base && b != safeBase && b != osBase && b != osSafe && !strings.Contains(b, id) {
			fallback = append(fallback, path)
			continue
		}
		m, err := parseMemory(path)
		if err == nil && m.ID == id {
			return m, nil
		}
	}
	for _, path := range fallback {
		m, err := parseMemory(path)
		if err == nil && m.ID == id {
			return m, nil
		}
	}
	return Memory{}, fmt.Errorf("memory not found: %s", id)
}

func findMemory(cfg Config, id string) (Memory, error) {
	m, err := findMemoryRaw(cfg, id)
	if err != nil {
		return Memory{}, err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return Memory{}, err
	}
	if !g.memoryVisible(m.ID) {
		return Memory{}, fmt.Errorf("memory is not current: %s (use `mora teach history --memory-id %s` to audit revisions)", id, id)
	}
	return decorateDecision(m, time.Now()), nil
}

// listMemories backs `mora list`, list_memory, and context_memory's no-query
// "recency briefing" fallback. filters is an optional trailing #241
// source/since_hours pair (zero value — a no-op — when omitted, so every
// pre-#241 call site keeps compiling and behaving unchanged). No pre-rank
// subtlety is needed here: every file is already parsed and predicate-checked
// BEFORE the newest-first sort + limit truncate below, so adding the filter
// predicate alongside the existing scope check can never let a filtered-out
// memory crowd out a matching one.
func listMemories(cfg Config, scope string, limit int, filters ...searchFilters) ([]Memory, error) {
	f := oneFilter(filters)
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	var out []Memory
	g, err := loadGovernance(cfg)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			continue
		}
		// Skip tombstones (connector deleted_at): listMemories backs the browse /
		// session-start surfaces (`mora list`, list_memory, the no-query
		// context_memory fallback), so a deleted item must not resurface there as a
		// "recent memory". Mirrors the search-index + graph/digest/salience skip.
		// findMemory (explicit by-id read) intentionally still resolves tombstones.
		if m.DeletedAt != "" {
			continue
		}
		if !g.memoryVisible(m.ID) {
			continue
		}
		if scope != "" && m.Scope != scope {
			continue
		}
		if f.active() && !f.passes(m) {
			continue
		}
		out = append(out, decorateDecision(m, time.Now()))
	}
	// Recency here is MEMORY-WRITE recency, not event time (#218). Sorting by
	// created_at ranked a connector memory by its provider occurrence instant, so a
	// calendar event months out led "the most recent memories" while everything
	// Mora had just ingested sank. byIngestRecency (`recency.go`) orders by the
	// per-memory write clock instead and is a total, deterministic order.
	sort.Slice(out, func(i, j int) bool { return byIngestRecency(out[i], out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// writeWikiIndex refreshes vault/index.md — the page buildContext injects verbatim
// into every `mora context` / MCP context_memory payload. `updated` is the SAME
// stamp the rebuild wrote into index_meta.indexed_at (B5): deriving the timestamp
// from the committed index (rather than a fresh time.Now()) means the page an agent
// trusts can never claim a freshness the index does not have. Called from the
// rebuild commit path so all rebuild callers refresh it, not just the CLI.
func writeWikiIndex(cfg Config, count int, updated string) error {
	var sections []string
	for _, dir := range []string{"memories", "sources", "meetings"} {
		root := filepath.Join(cfg.VaultDir, dir)
		var n int
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && filepath.Ext(path) == ".md" {
				n++
			}
			return nil
		})
		sections = append(sections, fmt.Sprintf("- %s: %d pages", dir, n))
	}
	body := fmt.Sprintf("# Mora Index\n\n> Generated by `mora index rebuild`.\n> Updated: %s\n> Indexed memories: %d\n\n%s\n", updated, count, strings.Join(sections, "\n"))
	return atomicWrite(filepath.Join(cfg.VaultDir, "index.md"), []byte(body), 0o644)
}
func parseTags(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return splitCSV(s)
}
func newID() string {
	var b [4]byte
	if _, err := randRead(b[:]); err != nil {
		// crypto/rand essentially never fails; if the OS CSPRNG is unavailable,
		// derive the suffix from the PRNG (math/rand/v2, auto-seeded at startup,
		// independent of the OS entropy source) rather than leaving b all-zero.
		// An all-zero suffix would collide on every mint within the same second
		// AND stall createMemory's re-mint retry (identical id each attempt).
		// Memory ids are uniqueness tokens, not secrets, so PRNG entropy suffices.
		// Surface it — never silently degrade — but never fail the write.
		warnRandFallback()
		n := mrand.Uint32()
		b[0], b[1], b[2], b[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	}
	return "mem_" + time.Now().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}
func ContentHash(s string) string {
	// FNV-like small stable hash without another dependency.
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 16)
}
func quoteYAML(s string) string {
	// Keep the renderer and parseFrontmatterScalar symmetric. Backslashes and
	// quotes are included even when this small frontmatter grammar could carry
	// them unquoted: emitting one canonical quoted representation ensures paths
	// and escaped characters round-trip identically on every OS.
	if s == "" || strings.TrimSpace(s) != s || strings.ContainsAny(s, ":#[]\"\\\n\r\t") {
		return strconv.Quote(s)
	}
	return s
}

// parseFrontmatterScalar decodes the exact quoted representation emitted by
// quoteYAML. The previous parser only trimmed quote characters, leaving path
// separators doubled in memory. That changed identity-bearing source paths
// after a render/parse round trip.
//
// A value beginning with a quote is treated as quoted data and must be a valid,
// complete strconv string. Failing closed here prevents malformed frontmatter
// from silently producing a different path or provider identity.
func parseFrontmatterScalar(key, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if !strings.HasPrefix(value, `"`) {
		return value, nil
	}
	decoded, err := strconv.Unquote(value)
	if err != nil {
		return "", fmt.Errorf("%s frontmatter value is corrupt: %w", key, err)
	}
	return decoded, nil
}
