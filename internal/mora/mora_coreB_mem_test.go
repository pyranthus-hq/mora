package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// coreBMemFullMemory returns a Memory with EVERY renderable field populated so a
// render→parse round-trip proves each frontmatter field survives.
func coreBMemFullMemory() Memory {
	return Memory{
		ID:          "mem_full_1",
		Scope:       "project:wink",
		Type:        "decision",
		Title:       "ns: value #tag [x]", // forces quoteYAML on ":", "#", "[", "]"
		Tags:        []string{"t1", "t2"},
		Source:      "src#1", // forces quoteYAML on "#"
		CreatedAt:   "2026-06-30T10:00:00Z",
		Provider:    "gmail",
		Account:     "work",
		ProviderID:  "gmail_thread:abc#1", // forces quoteYAML
		ContentHash: "deadbeef",
		LastSynced:  "2026-06-30T09:00:00Z",
		Truncated:   true,
		DeletedAt:   "2026-06-30T11:00:00Z",
		Text:        "body line one\nbody line two",
		Meta:        map[string]any{"from": "a@b.com", "n": json.Number("123456789012345678")},
	}
}

func coreBMemWriteFile(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.md")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write temp memory: %v", err)
	}
	return p
}

func TestCoreB_MemRenderParseRoundtrip(t *testing.T) {
	m := coreBMemFullMemory()
	body, err := renderMemory(m)
	if err != nil {
		t.Fatalf("renderMemory: %v", err)
	}
	// The meta line must be one canonical JSON line (sorted keys, no raw newline).
	if !strings.Contains(string(body), "\nmeta: {") {
		t.Fatalf("expected a canonical meta line, got:\n%s", body)
	}
	got, err := parseMemory(coreBMemWriteFile(t, body))
	if err != nil {
		t.Fatalf("parseMemory: %v", err)
	}
	if got.ID != m.ID || got.Scope != m.Scope || got.Type != m.Type {
		t.Fatalf("id/scope/type mismatch: %+v", got)
	}
	if got.Title != m.Title {
		t.Fatalf("title did not round-trip: want %q got %q", m.Title, got.Title)
	}
	if got.Source != m.Source {
		t.Fatalf("source did not round-trip: want %q got %q", m.Source, got.Source)
	}
	if got.ProviderID != m.ProviderID {
		t.Fatalf("provider_id did not round-trip: want %q got %q", m.ProviderID, got.ProviderID)
	}
	if strings.Join(got.Tags, ",") != "t1,t2" {
		t.Fatalf("tags did not round-trip: %v", got.Tags)
	}
	if got.CreatedAt != m.CreatedAt || got.Provider != m.Provider || got.Account != m.Account {
		t.Fatalf("created_at/provider/account mismatch: %+v", got)
	}
	if got.ContentHash != m.ContentHash || got.LastSynced != m.LastSynced {
		t.Fatalf("content_hash/last_synced mismatch: %+v", got)
	}
	if !got.Truncated {
		t.Fatalf("truncated did not round-trip: %+v", got)
	}
	if got.DeletedAt != m.DeletedAt {
		t.Fatalf("deleted_at did not round-trip: %+v", got)
	}
	if got.Text != m.Text {
		t.Fatalf("text did not round-trip: want %q got %q", m.Text, got.Text)
	}
	if got.Meta["from"] != "a@b.com" {
		t.Fatalf("meta.from lost: %+v", got.Meta)
	}
	// UseNumber must keep the 19-digit id exact (no float64 precision loss).
	if s := fmt.Sprintf("%v", got.Meta["n"]); s != "123456789012345678" {
		t.Fatalf("meta.n precision lost: %q", s)
	}
}

func TestCoreB_MemRenderMemoryMinimalOmitsOptionalLines(t *testing.T) {
	m := Memory{ID: "mem_min", Scope: "global", Type: "insight", Title: "Plain", Source: "manual", CreatedAt: "2026-06-30T00:00:00Z", Text: "hello"}
	body, err := renderMemory(m)
	if err != nil {
		t.Fatalf("renderMemory: %v", err)
	}
	s := string(body)
	for _, mustHave := range []string{"id: mem_min", "scope: global", "type: insight", "title: Plain", "source: manual", "created_at: 2026-06-30T00:00:00Z"} {
		if !strings.Contains(s, mustHave) {
			t.Fatalf("expected %q in:\n%s", mustHave, s)
		}
	}
	for _, absent := range []string{"provider:", "account:", "content_hash:", "last_synced:", "truncated:", "deleted_at:", "\nmeta:"} {
		if strings.Contains(s, absent) {
			t.Fatalf("did not expect %q in minimal render:\n%s", absent, s)
		}
	}
	// A plain (no special char) title must NOT be quoted.
	if strings.Contains(s, `title: "Plain"`) {
		t.Fatalf("plain title should not be quoted:\n%s", s)
	}
}

func TestCoreB_MemRenderMemoryMetaMarshalError(t *testing.T) {
	m := Memory{ID: "mem_bad", Scope: "global", Title: "T", Text: "b", Meta: map[string]any{"c": make(chan int)}}
	_, err := renderMemory(m)
	if err == nil {
		t.Fatalf("expected renderMemory to fail on unmarshalable meta")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("expected json unsupported-type error, got: %v", err)
	}
}

func TestCoreB_MemParseMemoryErrors(t *testing.T) {
	// Nonexistent path -> read error (not a frontmatter error).
	if _, err := parseMemory(filepath.Join(t.TempDir(), "nope.md")); err == nil {
		t.Fatalf("expected read error for missing file")
	} else if strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("expected os read error, got frontmatter error: %v", err)
	}

	cases := []struct{ body, wantErr string }{
		{"no frontmatter here\n", "missing frontmatter"},
		{"---\nid: x\n", "invalid frontmatter"},        // opens but never closes
		{"---\ntitle: X\n---\n\nbody\n", "missing id"}, // closes, but no id
	}
	for _, c := range cases {
		_, err := parseMemory(coreBMemWriteFile(t, []byte(c.body)))
		if err == nil || err.Error() != c.wantErr {
			t.Fatalf("body %q: want error %q, got %v", c.body, c.wantErr, err)
		}
	}
}

func TestCoreB_MemParseMemoryNoColonLineIgnored(t *testing.T) {
	body := "---\nid: mem_nc\nscope: global\njustacomment\ntitle: Hello\n---\n\nbody\n"
	m, err := parseMemory(coreBMemWriteFile(t, []byte(body)))
	if err != nil {
		t.Fatalf("parseMemory: %v", err)
	}
	if m.ID != "mem_nc" || m.Title != "Hello" || m.Scope != "global" {
		t.Fatalf("colon-less line should be skipped, got %+v", m)
	}
}

func TestCoreB_MemParseMemoryMetaEdges(t *testing.T) {
	// Corrupt meta: warn to stderr, ignore, but keep the rest of the memory.
	corrupt := "---\nid: mem_cm\nscope: global\ntitle: T\nmeta: {not valid json\n---\n\nbody\n"
	m, err := parseMemory(coreBMemWriteFile(t, []byte(corrupt)))
	if err != nil {
		t.Fatalf("parseMemory(corrupt meta): %v", err)
	}
	if m.ID != "mem_cm" || m.Meta != nil {
		t.Fatalf("corrupt meta should be dropped, other fields kept: %+v", m)
	}
	// Empty object -> len(meta)==0 -> Meta stays nil.
	empty := "---\nid: mem_em\nscope: global\ntitle: T\nmeta: {}\n---\n\nbody\n"
	m2, err := parseMemory(coreBMemWriteFile(t, []byte(empty)))
	if err != nil {
		t.Fatalf("parseMemory(empty meta): %v", err)
	}
	if m2.Meta != nil {
		t.Fatalf("empty meta object should leave Meta nil, got %+v", m2.Meta)
	}
}

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

func TestCoreB_MemTruncateRunes(t *testing.T) {
	if got := truncateRunes("abc", 0); got != "" {
		t.Fatalf("max<=0 should yield empty, got %q", got)
	}
	if got := truncateRunes("abc", -3); got != "" {
		t.Fatalf("negative max should yield empty, got %q", got)
	}
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("short string should be unchanged, got %q", got)
	}
	if got := truncateRunes("hello", 5); got != "hello" {
		t.Fatalf("exact length should be unchanged, got %q", got)
	}
	if got := truncateRunes("hello world", 5); got != "hello" {
		t.Fatalf("expected ASCII clip to 5, got %q", got)
	}
	// Multibyte: "hé" is bytes h(1)+é(2). max=2 lands inside é, so it must back
	// up to a rune boundary rather than split the rune.
	s := "héllo"
	got := truncateRunes(s, 2)
	if got != "h" {
		t.Fatalf("expected rune-safe backup to %q, got %q", "h", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncateRunes produced invalid UTF-8: %q", got)
	}
	// A whole multibyte string under the limit is returned intact.
	if got := truncateRunes(s, 100); got != s {
		t.Fatalf("multibyte string within limit should be unchanged, got %q", got)
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

func TestCoreB_MemBudgetSearchResults(t *testing.T) {
	mems := []Memory{
		{ID: "a", Title: "Alpha", Text: strings.Repeat("alpha ", 20)},
		{ID: "b", Title: "Beta", Text: strings.Repeat("beta ", 20)},
		{ID: "c", Title: "Gamma", Text: strings.Repeat("gamma ", 20)},
	}

	// Disabled (budget <= 0): everything kept, nothing dropped.
	kept, dropped := budgetSearchResults(mems, 0)
	if len(kept) != 3 || dropped != 0 {
		t.Fatalf("budget<=0 should keep all: kept=%d dropped=%d", len(kept), dropped)
	}
	kept, dropped = budgetSearchResults(mems, -1)
	if len(kept) != 3 || dropped != 0 {
		t.Fatalf("negative budget should keep all: kept=%d dropped=%d", len(kept), dropped)
	}

	// Empty slice: no work, no drops.
	kept, dropped = budgetSearchResults(nil, 100)
	if len(kept) != 0 || dropped != 0 {
		t.Fatalf("empty input: kept=%d dropped=%d", len(kept), dropped)
	}

	// Tiny budget: first row is force-kept, the rest dropped.
	kept, dropped = budgetSearchResults(mems, 10)
	if len(kept) != 1 || kept[0].ID != "a" || dropped != 2 {
		t.Fatalf("tiny budget should keep only the first: kept=%d dropped=%d", len(kept), dropped)
	}

	// Generous budget: keep everything.
	kept, dropped = budgetSearchResults(mems, 1_000_000)
	if len(kept) != 3 || dropped != 0 {
		t.Fatalf("big budget should keep all: kept=%d dropped=%d", len(kept), dropped)
	}
}
