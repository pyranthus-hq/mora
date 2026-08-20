package mora

import (
	"encoding/hex"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/memory"
	memfile "github.com/pyranthus-hq/mora/internal/memoryfile"
	"io/fs"
	mrand "math/rand/v2"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func renderMemory(m Memory) ([]byte, error) {
	normalized := m
	normalized.Decision = normalizeDecisionValidity(m)
	return memfile.Render(normalized)
}
func parseMemory(path string) (Memory, error) {
	d, err := memfile.Parse(path)
	if err != nil {
		return Memory{}, err
	}
	m := d
	if m.Type == "decision" {
		m.Decision = normalizeDecisionValidity(m)
	}
	return m, nil
}
func parseMemoryBytes(path string, b []byte) (Memory, error) {
	d, err := memfile.ParseBytes(path, b)
	if err != nil {
		return Memory{}, err
	}
	m := d
	if m.Type == "decision" {
		m.Decision = normalizeDecisionValidity(m)
	}
	return m, nil
}
func memoriesRoot(cfg Config) string              { return memfile.MemoriesRoot(cfg) }
func sourcesRoot(cfg Config) string               { return memfile.SourcesRoot(cfg) }
func memoryPath(cfg Config, m Memory) string      { return memfile.Path(cfg, m) }
func osSafeBase(id string) string                 { return memfile.OSSafeBase(id) }
func allMemoryFiles(cfg Config) ([]string, error) { return memfile.All(cfg) }

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
	return atomicio.Write(memoryPath(cfg, m), body, 0o644)
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
		if f.Active() && !searchFilterPasses(f, m) {
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
	return atomicio.Write(filepath.Join(cfg.VaultDir, "index.md"), []byte(body), 0o644)
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
