package mora

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// coreBMemFullMemory returns a Memory with EVERY renderable field populated so a
// render→parse round-trip proves each frontmatter field survives.
func TestCoreB_MemWriteMemoryRoundtrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	m := Memory{ID: "mem_w1", Scope: "project:wink", Type: "decision", Title: "Written", Source: "manual", CreatedAt: "2026-06-30T12:00:00Z", Text: "written body zebra"}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
	p := memoryPath(cfg, m)
	// Scope ":" must map onto a nested directory.
	if !strings.HasSuffix(filepath.ToSlash(p), "memories/project/wink/mem_w1.md") {
		t.Fatalf("unexpected memory path: %s", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("expected file at memoryPath: %v", err)
	}
	got, err := parseMemory(p)
	if err != nil {
		t.Fatalf("parseMemory: %v", err)
	}
	if got.ID != m.ID || got.Title != m.Title || got.Text != m.Text || got.Scope != m.Scope {
		t.Fatalf("written memory did not round-trip: %+v", got)
	}
}

func TestCoreB_MemWriteMemoryRenderError(t *testing.T) {
	cfg := testCfg(t)
	err := writeMemory(cfg, Memory{ID: "mem_x", Scope: "global", Title: "T", Text: "b", Meta: map[string]any{"c": make(chan int)}})
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("expected writeMemory to surface render error, got: %v", err)
	}
}

func TestCoreB_MemSearchMemories(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	run(t, "write", "--scope", "project:wink", "--type", "decision", "--title", "OAuth path", "--text", "use oauth token flow zebra")
	run(t, "write", "--scope", "global", "--title", "Cooking", "--text", "pasta recipe zebra")

	ctx := context.Background()
	// Term present in both bodies -> both scopes returned.
	all, err := searchMemories(ctx, cfg, "zebra", "", 10)
	if err != nil {
		t.Fatalf("searchMemories: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 hits for zebra, got %d: %+v", len(all), all)
	}

	// Scope filter restricts to one memory.
	scoped, err := searchMemories(ctx, cfg, "zebra", "project:wink", 10)
	if err != nil {
		t.Fatalf("searchMemories(scoped): %v", err)
	}
	if len(scoped) != 1 || scoped[0].Scope != "project:wink" || scoped[0].Title != "OAuth path" {
		t.Fatalf("scope filter failed: %+v", scoped)
	}

	// Term unique to one body.
	oauth, err := searchMemories(ctx, cfg, "oauth", "", 10)
	if err != nil {
		t.Fatalf("searchMemories(oauth): %v", err)
	}
	if len(oauth) != 1 || oauth[0].Title != "OAuth path" {
		t.Fatalf("expected only the OAuth memory, got %+v", oauth)
	}
	if !strings.Contains(oauth[0].Text, "oauth token flow") {
		t.Fatalf("expected full body text in result, got %q", oauth[0].Text)
	}

	// Limit clamps the returned rows.
	one, err := searchMemories(ctx, cfg, "zebra", "", 1)
	if err != nil {
		t.Fatalf("searchMemories(limit=1): %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("expected limit=1 to return 1 row, got %d", len(one))
	}

	// Empty query short-circuits to zero results (FTS5 rejects an empty MATCH).
	none, err := searchMemories(ctx, cfg, "", "", 10)
	if err != nil {
		t.Fatalf("searchMemories(empty): unexpected err %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected empty query to return no results, got %+v", none)
	}
}

func TestCoreB_MemSearchMemoriesRebuildsMissingIndex(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	run(t, "write", "--scope", "global", "--title", "Idx", "--text", "solitary needle term")

	// Deleting index.db forces the rebuild-on-missing branch of searchMemories.
	if err := os.Remove(dbPath(cfg)); err != nil {
		t.Fatalf("remove index db: %v", err)
	}
	hits, err := searchMemories(context.Background(), cfg, "needle", "", 10)
	if err != nil {
		t.Fatalf("searchMemories after rebuild: %v", err)
	}
	if len(hits) != 1 || hits[0].Title != "Idx" {
		t.Fatalf("expected rebuild to re-find the memory, got %+v", hits)
	}
}

// coreBMemSeedListVault writes four memories directly (controlled CreatedAt /
// tombstone) into a fresh vault and returns the config.
func coreBMemSeedListVault(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	seed := []Memory{
		{ID: "mem_old", Scope: "global", Type: "insight", Title: "Old", Source: "manual", CreatedAt: "2026-01-01T00:00:00Z", Text: "old body"},
		{ID: "mem_new", Scope: "global", Type: "insight", Title: "New", Source: "manual", CreatedAt: "2026-03-01T00:00:00Z", Text: "new body"},
		{ID: "mem_proj", Scope: "project:x", Type: "insight", Title: "Proj", Source: "manual", CreatedAt: "2026-02-01T00:00:00Z", Text: "proj body"},
		{ID: "mem_dead", Scope: "global", Type: "insight", Title: "Dead", Source: "manual", CreatedAt: "2026-04-01T00:00:00Z", DeletedAt: "2026-05-01T00:00:00Z", Text: "dead body"},
	}
	for _, m := range seed {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed writeMemory %s: %v", m.ID, err)
		}
	}
	// A malformed file in the memories tree must be skipped, not fail listing.
	if err := os.WriteFile(filepath.Join(memoriesRoot(cfg), "junk.md"), []byte("not a memory"), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	return cfg
}

func TestCoreB_MemListMemories(t *testing.T) {
	cfg := coreBMemSeedListVault(t)

	// No scope, no limit: newest-first, tombstone + malformed skipped.
	all, err := listMemories(cfg, "", 0)
	if err != nil {
		t.Fatalf("listMemories: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 live memories, got %d: %+v", len(all), all)
	}
	if all[0].ID != "mem_new" || all[1].ID != "mem_proj" || all[2].ID != "mem_old" {
		t.Fatalf("expected newest-first order, got %s,%s,%s", all[0].ID, all[1].ID, all[2].ID)
	}
	for _, m := range all {
		if m.ID == "mem_dead" {
			t.Fatalf("tombstone must not surface in listMemories")
		}
	}

	// Scope filter drops the project memory.
	globals, err := listMemories(cfg, "global", 0)
	if err != nil {
		t.Fatalf("listMemories(global): %v", err)
	}
	if len(globals) != 2 {
		t.Fatalf("expected 2 global memories, got %+v", globals)
	}
	for _, m := range globals {
		if m.Scope != "global" {
			t.Fatalf("scope filter leaked %+v", m)
		}
	}

	// Limit clamps to the newest N.
	limited, err := listMemories(cfg, "", 2)
	if err != nil {
		t.Fatalf("listMemories(limit=2): %v", err)
	}
	if len(limited) != 2 || limited[0].ID != "mem_new" || limited[1].ID != "mem_proj" {
		t.Fatalf("limit clamp failed: %+v", limited)
	}
}

func TestCoreB_MemFindMemory(t *testing.T) {
	cfg := coreBMemSeedListVault(t)

	got, err := findMemory(cfg, "mem_new")
	if err != nil {
		t.Fatalf("findMemory(mem_new): %v", err)
	}
	if got.Title != "New" || got.Text != "new body" {
		t.Fatalf("findMemory returned wrong memory: %+v", got)
	}

	// findMemory intentionally still resolves tombstones (explicit by-id read).
	dead, err := findMemory(cfg, "mem_dead")
	if err != nil {
		t.Fatalf("findMemory(mem_dead): %v", err)
	}
	if dead.DeletedAt == "" {
		t.Fatalf("expected tombstone to resolve with DeletedAt set: %+v", dead)
	}

	// Missing id -> descriptive error.
	_, err = findMemory(cfg, "does_not_exist_zzz")
	if err == nil || err.Error() != "memory not found: does_not_exist_zzz" {
		t.Fatalf("expected not-found error, got: %v", err)
	}
}

func TestCoreB_MemAllMemoryFilesWalkErrorSurfaces(t *testing.T) {
	skipOnWindows(t, "chmod 0000 does not block WalkDir on Windows (read-only attribute, not an ACL deny), so the walk error can't be provoked")
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; walk error unreachable")
	}
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// An unreadable subdirectory must surface as an error (the index must not
	// silently shrink to the readable subset), which both listMemories and
	// findMemory propagate from allMemoryFiles.
	locked := filepath.Join(memoriesRoot(cfg), "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod locked: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, err := listMemories(cfg, "", 0); err == nil || !strings.Contains(err.Error(), "walking") {
		t.Fatalf("expected listMemories walk error, got %v", err)
	}
	if _, err := findMemory(cfg, "anything"); err == nil || !strings.Contains(err.Error(), "walking") {
		t.Fatalf("expected findMemory walk error, got %v", err)
	}
}

func TestCoreB_MemBuildContext(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	items := []Memory{{Title: "ZZUNIQUEITEM", Text: "zebra item body"}}

	// Budget <= 0 short-circuits.
	if got := buildContext(cfg, items, 0, false); got != "" {
		t.Fatalf("budget 0 should yield empty, got %q", got)
	}

	// hasQuery=true: items lead the wiki preamble.
	q := buildContext(cfg, items, 100000, true)
	if !strings.Contains(q, "# ZZUNIQUEITEM") || !strings.Contains(q, "zebra item body") {
		t.Fatalf("expected item title+text present, got:\n%s", q)
	}
	if !strings.Contains(q, "# priority-map.md") {
		t.Fatalf("expected wiki preamble present, got:\n%s", q)
	}
	if strings.Index(q, "# ZZUNIQUEITEM") > strings.Index(q, "# priority-map.md") {
		t.Fatalf("with a query, items must lead the wiki preamble")
	}

	// hasQuery=false: wiki leads.
	nq := buildContext(cfg, items, 100000, false)
	if idxWiki, idxItem := strings.Index(nq, "# index.md"), strings.Index(nq, "# ZZUNIQUEITEM"); idxWiki < 0 || idxWiki > idxItem {
		t.Fatalf("with no query the wiki must lead: wiki=%d item=%d", idxWiki, idxItem)
	}

	// Tiny budget forces truncation: bytes bounded by budget, item never reached
	// because the wiki preamble alone overflows.
	small := buildContext(cfg, items, 100, false)
	if len(small) == 0 || len(small) > 100 {
		t.Fatalf("expected 0 < len <= 100, got %d", len(small))
	}
	if strings.Contains(small, "ZZUNIQUEITEM") {
		t.Fatalf("tiny budget should be exhausted by the wiki, item leaked in:\n%s", small)
	}
}

func TestCoreB_MemSnippetMemories(t *testing.T) {
	if snippetMemories(nil, "q") != nil {
		t.Fatalf("nil input should return nil")
	}

	// Short body: whitespace flattened, Meta dropped, not truncated.
	short := []Memory{{Title: "S", Text: "  hello   world\n\nfoo  ", Meta: map[string]any{"k": "v"}}}
	so := snippetMemories(short, "hello")
	if len(so) != 1 || so[0].Text != "hello world foo" {
		t.Fatalf("expected flattened body, got %q", so[0].Text)
	}
	if so[0].Truncated {
		t.Fatalf("short body should not be flagged truncated")
	}
	if so[0].Meta != nil {
		t.Fatalf("snippetMemories must drop Meta, got %+v", so[0].Meta)
	}

	// Long body with the query term buried near the end.
	longText := strings.Repeat("filler word ", 40) + "NEEDLE marker here"
	if utf8.RuneCountInString(longText) <= searchSnippetLen {
		t.Fatalf("test fixture too short: %d", utf8.RuneCountInString(longText))
	}
	hit := snippetMemories([]Memory{{Text: longText}}, "NEEDLE")
	if !hit[0].Truncated {
		t.Fatalf("long body should be flagged truncated")
	}
	if !strings.Contains(hit[0].Text, "NEEDLE") {
		t.Fatalf("query term should center the snippet window, got %q", hit[0].Text)
	}
	if utf8.RuneCountInString(hit[0].Text) > searchSnippetLen+2 {
		t.Fatalf("snippet exceeded the clip length: %d", utf8.RuneCountInString(hit[0].Text))
	}
	if len(hit[0].Text) >= len(longText) {
		t.Fatalf("snippet should be shorter than the full body")
	}

	// Same body, empty query -> head clip (no body match), so the buried NEEDLE
	// is NOT in the preview: proves the query drives the window.
	head := snippetMemories([]Memory{{Text: longText}}, "")
	if !head[0].Truncated {
		t.Fatalf("long body should still be truncated for empty query")
	}
	if strings.Contains(head[0].Text, "NEEDLE") {
		t.Fatalf("head clip should not reach the buried term, got %q", head[0].Text)
	}
}
