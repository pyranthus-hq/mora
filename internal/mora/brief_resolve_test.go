package mora

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// brief_resolve_test.go drives Phase 16-01: the READ side of the dated brief
// artifact — latestBriefPath (newest dated file), briefIsFresh (today-or-
// yesterday UTC), and resolveBrief (read-verbatim-if-fresh else generate a
// DELTA-preview with a 24h-window fallback). RED-first.
//
// The kernel is PURE, LOCAL, deterministic, and watermark-safe by construction:
// every date/freshness decision flows from the INJECTED now (never a fresh
// clock), it makes ZERO network calls, and it NEVER advances the Phase-12
// watermark (advance:false on every build). These tests pin all of that.

// resolveFixedNow is a fixed UTC instant so the resolver tests are clock-
// independent (mirrors artifact_test.go's artifactFixedNow discipline).
var resolveFixedNow = time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

// resolveCfg is the lightweight VaultDir/StateDir fixture for the pure path
// tests (no init / sources needed when we only seed brief files on disk).
func resolveCfg(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		VaultDir: filepath.Join(root, "vault"),
		StateDir: filepath.Join(root, "state"),
	}
}

// seedBriefFile writes <VaultDir>/briefs/<date>-brief.md with the given body,
// returning the path. date is a "2006-01-02" string.
func seedBriefFile(t *testing.T, cfg Config, date, body string) string {
	t.Helper()
	dir := filepath.Join(cfg.VaultDir, "briefs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir briefs: %v", err)
	}
	path := filepath.Join(dir, date+"-brief.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write brief: %v", err)
	}
	return path
}

// --- Task 1: latestBriefPath + briefIsFresh ---------------------------------

// TestLatestBriefPathPicksNewestByFilename: with three dated files seeded in an
// arbitrary creation order, latestBriefPath returns the HIGHEST parsed-filename
// date — selection is by parsed YYYY-MM-DD, NOT os mtime.
func TestLatestBriefPathPicksNewestByFilename(t *testing.T) {
	cfg := resolveCfg(t)
	// Create out of date order so an mtime-based picker would choose wrong.
	seedBriefFile(t, cfg, "2026-06-08", "newest")
	seedBriefFile(t, cfg, "2026-06-06", "oldest")
	seedBriefFile(t, cfg, "2026-06-07", "middle")

	path, dated, ok := latestBriefPath(cfg, resolveFixedNow)
	if !ok {
		t.Fatalf("latestBriefPath ok=false, want true")
	}
	want := filepath.Join(cfg.VaultDir, "briefs", "2026-06-08-brief.md")
	if path != want {
		t.Fatalf("latestBriefPath path = %q, want %q (newest by filename date)", path, want)
	}
	if got := dated.UTC().Format("2006-01-02"); got != "2026-06-08" {
		t.Fatalf("latestBriefPath dated = %q, want 2026-06-08", got)
	}
}

// TestLatestBriefPathIgnoresJunk: non-matching files (no -brief.md suffix, an
// unparseable date) are ignored; the newest PARSEABLE dated brief wins.
func TestLatestBriefPathIgnoresJunk(t *testing.T) {
	cfg := resolveCfg(t)
	seedBriefFile(t, cfg, "2026-06-07", "real")
	dir := filepath.Join(cfg.VaultDir, "briefs")
	for _, name := range []string{"notes.md", "2026-99-99-brief.md", "README.md", "brief.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0o644); err != nil {
			t.Fatalf("write junk %q: %v", name, err)
		}
	}

	path, dated, ok := latestBriefPath(cfg, resolveFixedNow)
	if !ok {
		t.Fatalf("latestBriefPath ok=false, want true (one real brief present)")
	}
	want := filepath.Join(dir, "2026-06-07-brief.md")
	if path != want {
		t.Fatalf("latestBriefPath path = %q, want %q (junk ignored)", path, want)
	}
	if got := dated.UTC().Format("2006-01-02"); got != "2026-06-07" {
		t.Fatalf("latestBriefPath dated = %q, want 2026-06-07", got)
	}
}

// TestLatestBriefPathAbsentDir: no briefs/ dir => ok=false.
func TestLatestBriefPathAbsentDir(t *testing.T) {
	cfg := resolveCfg(t) // VaultDir/briefs does not exist
	if _, _, ok := latestBriefPath(cfg, resolveFixedNow); ok {
		t.Fatalf("latestBriefPath ok=true on absent briefs dir, want false")
	}
}

// TestLatestBriefPathEmptyDir: an existing but empty (or junk-only) briefs/ dir
// => ok=false.
func TestLatestBriefPathEmptyDir(t *testing.T) {
	cfg := resolveCfg(t)
	dir := filepath.Join(cfg.VaultDir, "briefs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, ok := latestBriefPath(cfg, resolveFixedNow); ok {
		t.Fatalf("latestBriefPath ok=true on a briefs dir with no parseable brief, want false")
	}
}

// TestLatestBriefPathDeterministic: two calls over the same seeded dir return a
// byte-identical path.
func TestLatestBriefPathDeterministic(t *testing.T) {
	cfg := resolveCfg(t)
	seedBriefFile(t, cfg, "2026-06-08", "a")
	seedBriefFile(t, cfg, "2026-06-07", "b")

	p1, d1, ok1 := latestBriefPath(cfg, resolveFixedNow)
	p2, d2, ok2 := latestBriefPath(cfg, resolveFixedNow)
	if !ok1 || !ok2 {
		t.Fatalf("latestBriefPath ok mismatch: %v / %v", ok1, ok2)
	}
	if p1 != p2 {
		t.Fatalf("latestBriefPath not deterministic: %q vs %q", p1, p2)
	}
	if !d1.Equal(d2) {
		t.Fatalf("latestBriefPath dated not deterministic: %v vs %v", d1, d2)
	}
}

// TestBriefIsFresh pins the four freshness cases (pure today-or-yesterday UTC).
func TestBriefIsFresh(t *testing.T) {
	now := resolveFixedNow // 2026-06-08T12:00:00Z

	cases := []struct {
		name  string
		dated time.Time
		want  bool
	}{
		{"today", time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC), true},
		{"yesterday", time.Date(2026, 6, 7, 23, 0, 0, 0, time.UTC), true},
		{"two-days-ago", time.Date(2026, 6, 6, 9, 0, 0, 0, time.UTC), false},
		{"tomorrow", time.Date(2026, 6, 9, 1, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		if got := briefIsFresh(c.dated, now); got != c.want {
			t.Errorf("briefIsFresh(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBriefIsFreshUTCBoundary: a dated file expressed in a non-UTC zone is
// compared on its UTC day — a yesterday-UTC file at a late local hour still
// reads fresh, and the comparison never depends on a fresh clock.
func TestBriefIsFreshUTCBoundary(t *testing.T) {
	now := resolveFixedNow // 2026-06-08T12:00:00Z, UTC day 2026-06-08
	zone := time.FixedZone("EDT", -4*60*60)
	// 2026-06-07T22:00:00-04:00 == 2026-06-08T02:00:00Z => today's UTC day => fresh.
	dated := time.Date(2026, 6, 7, 22, 0, 0, 0, zone)
	if !briefIsFresh(dated, now) {
		t.Fatalf("briefIsFresh should compare on the UTC day (got stale for a today-UTC file)")
	}
}

// --- Task 2: resolveBrief ----------------------------------------------------

// TestResolveBriefReadsFreshVerbatim: today's persisted brief is returned
// BYTE-FOR-BYTE (os.ReadFile) with generated=false — no re-render.
func TestResolveBriefReadsFreshVerbatim(t *testing.T) {
	cfg := resolveCfg(t)
	const sentinel = "# SENTINEL BRIEF\n\nthis exact body must be returned verbatim, not re-rendered.\n"
	seedBriefFile(t, cfg, "2026-06-08", sentinel) // today's UTC day

	body, generated, err := resolveBrief(cfg, resolveFixedNow, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief: %v", err)
	}
	if generated {
		t.Fatalf("resolveBrief generated=true on a fresh persisted brief, want false (verbatim read)")
	}
	if body != sentinel {
		t.Fatalf("resolveBrief did not return the file verbatim\n--- got ---\n%q\n--- want ---\n%q", body, sentinel)
	}
}

// TestResolveBriefReadsYesterdayVerbatim: yesterday's UTC file is still fresh
// (the UTC-boundary fallback) — read verbatim, generated=false.
func TestResolveBriefReadsYesterdayVerbatim(t *testing.T) {
	cfg := resolveCfg(t)
	const sentinel = "# YESTERDAY BRIEF\nstill fresh across the UTC boundary.\n"
	seedBriefFile(t, cfg, "2026-06-07", sentinel) // yesterday's UTC day

	body, generated, err := resolveBrief(cfg, resolveFixedNow, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief: %v", err)
	}
	if generated {
		t.Fatalf("resolveBrief generated=true on a yesterday persisted brief, want false")
	}
	if body != sentinel {
		t.Fatalf("resolveBrief did not return yesterday's file verbatim:\n%q", body)
	}
}

// TestResolveBriefStaleFileGenerates: a 2-days-old persisted brief is NOT fresh,
// so resolveBrief GENERATES from the local vault (generated=true) and does NOT
// return the stale file's bytes.
func TestResolveBriefStaleFileGenerates(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := resolveFixedNow

	// A stale persisted brief (must NOT be returned).
	const stale = "STALE — DO NOT RETURN THIS"
	seedBriefFile(t, cfg, "2026-06-06", stale) // two days before now's UTC day

	// A real, recent cold-start delta in the vault.
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Recent thread", 2*time.Hour, now)

	body, generated, err := resolveBrief(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief: %v", err)
	}
	if !generated {
		t.Fatalf("resolveBrief generated=false on a stale brief, want true (regenerate)")
	}
	if strings.Contains(body, stale) {
		t.Fatalf("resolveBrief returned the STALE file's content instead of regenerating:\n%s", body)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatalf("regenerated brief is empty, want non-empty")
	}
	if !strings.Contains(body, "Recent thread") {
		t.Fatalf("regenerated delta brief should surface the recent thread; got:\n%s", body)
	}
}

// TestResolveBriefForceRegenBypassesFreshFile: with forceRegen=true (the
// `mora brief --fresh` path), resolveBrief regenerates from the live vault
// (generated=true) even when a FRESH persisted brief exists for today — so an
// ad-hoc brief reflects current data instead of replaying the morning snapshot.
// The default (forceRegen=false) path still reads that fresh file verbatim.
func TestResolveBriefForceRegenBypassesFreshFile(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := resolveFixedNow

	// A FRESH (today UTC) persisted brief the default path WOULD return verbatim.
	const cached = "CACHED MORNING BRIEF — must NOT be returned with --fresh"
	seedBriefFile(t, cfg, "2026-06-08", cached) // today's UTC day

	// A real cold-start delta in the live vault for the regen path to surface.
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Fresh regen thread", 2*time.Hour, now)

	// Control: without forceRegen, the fresh file is returned verbatim.
	body, generated, err := resolveBrief(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief control: %v", err)
	}
	if generated || body != cached {
		t.Fatalf("control: want verbatim cached read (generated=false), got generated=%v body=%q", generated, body)
	}

	// --fresh: bypass the cache and regenerate against the live vault.
	body, generated, err = resolveBrief(cfg, now, briefOpts{forceRegen: true})
	if err != nil {
		t.Fatalf("resolveBrief forceRegen: %v", err)
	}
	if !generated {
		t.Fatalf("resolveBrief generated=false with forceRegen, want true (cache bypassed)")
	}
	if strings.Contains(body, cached) {
		t.Fatalf("forceRegen returned the cached file instead of regenerating:\n%s", body)
	}
	if !strings.Contains(body, "Fresh regen thread") {
		t.Fatalf("forceRegen brief should surface the live delta; got:\n%s", body)
	}
}

// TestResolveBriefGeneratesNoFreshFile: with no persisted brief at all,
// resolveBrief generates a non-empty DELTA brief (generated=true).
func TestResolveBriefGeneratesNoFreshFile(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := resolveFixedNow

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Cold start thread", 3*time.Hour, now)

	body, generated, err := resolveBrief(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief: %v", err)
	}
	if !generated {
		t.Fatalf("resolveBrief generated=false with no persisted brief, want true")
	}
	if !strings.Contains(body, "Cold start thread") {
		t.Fatalf("generated brief should surface the cold-start item; got:\n%s", body)
	}
}

// TestResolveBriefEmptyDeltaFallsBackToWindow is the load-bearing subtlety: when
// the DELTA surfaces ZERO items (the scheduled --advance job already consumed
// today's delta) but the 24h window HAS items, resolveBrief re-builds in WINDOW
// mode so the brief is NEVER useless.
func TestResolveBriefEmptyDeltaFallsBackToWindow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := resolveFixedNow

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	// Seed recent items (inside the 24h window).
	digestSeed(t, cfg, "gmail", "Window only thread", 2*time.Hour, now)

	// Consume the delta: advance the watermark so a subsequent DELTA preview
	// surfaces ZERO items (everything baselined) — exactly the scheduled-job case.
	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, false); err != nil {
		t.Fatalf("seed-advance buildDigest: %v", err)
	}

	// Sanity: a plain DELTA preview now surfaces nothing.
	pre, err := buildDigest(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("delta preview: %v", err)
	}
	if surfacedItemCount(pre) != 0 {
		t.Fatalf("precondition broken: delta still surfaces %d items after advance", surfacedItemCount(pre))
	}

	body, generated, err := resolveBrief(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief: %v", err)
	}
	if !generated {
		t.Fatalf("resolveBrief generated=false on the empty-delta path, want true")
	}
	if !strings.Contains(body, "Window only thread") {
		t.Fatalf("empty-delta fallback should surface the 24h-window item; got:\n%s", body)
	}
}

// TestResolveBriefDoesNotAdvanceWatermark proves the read-only invariant: the
// stored watermark snapshot is BYTE-IDENTICAL across a resolveBrief call (it
// never calls saveBriefSnapshot / passes advance:true).
func TestResolveBriefDoesNotAdvanceWatermark(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := resolveFixedNow

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Watermark guard thread", 2*time.Hour, now)

	// Baseline the watermark on disk first so there is a snapshot file to compare.
	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, false); err != nil {
		t.Fatalf("baseline advance: %v", err)
	}
	snapPath := briefPath(cfg, "gmail")
	before, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot before: %v", err)
	}

	if _, _, err := resolveBrief(cfg, now, briefOpts{}); err != nil {
		t.Fatalf("resolveBrief: %v", err)
	}

	after, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("resolveBrief mutated the watermark snapshot (read-only invariant broken)\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestResolveBriefDeterministic: two resolveBrief calls with the same now over
// the same vault return equal bodies (no clock leak, no mutation).
func TestResolveBriefDeterministic(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := resolveFixedNow

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Deterministic thread", 2*time.Hour, now)

	b1, g1, err := resolveBrief(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief #1: %v", err)
	}
	b2, g2, err := resolveBrief(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("resolveBrief #2: %v", err)
	}
	if g1 != g2 {
		t.Fatalf("resolveBrief generated flag not deterministic: %v vs %v", g1, g2)
	}
	if b1 != b2 {
		t.Fatalf("resolveBrief body not deterministic across two calls with the same now")
	}
}

// TestBriefFallbackWindowHoursConst pins the fixed, deterministic fallback
// window (24h) — the watermark-independent look-back used ONLY on an empty delta.
func TestBriefFallbackWindowHoursConst(t *testing.T) {
	if briefFallbackWindowHours != 24 {
		t.Fatalf("briefFallbackWindowHours = %d, want 24 (fixed deterministic fallback)", briefFallbackWindowHours)
	}
}

// TestBriefGoZeroEgress is the zero-egress / read-only guard (D16-2,
// T-16-01/T-16-02). It parses brief.go's AST — NOT its raw text — so the
// pre-existing watermark-store DOC COMMENTS (which legitimately use words like
// "backfill" to describe cold-start behavior) can never false-positive: comments
// are not AST nodes. It asserts (1) brief.go imports NO network package, and (2)
// NO forbidden network/sync FUNCTION IDENTIFIER is REFERENCED anywhere in the
// file — proving the resolver reads disk + computes and never fetches. The
// behavioral read-only invariant (watermark unchanged) is proven independently by
// TestResolveBriefDoesNotAdvanceWatermark above.
func TestBriefGoZeroEgress(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "brief.go", nil, 0) // mode 0: drop comments.
	if err != nil {
		t.Fatalf("parse brief.go: %v", err)
	}

	// (1) No forbidden import path.
	forbiddenImports := []string{"net/http", "net", "github.com/pyranthus-hq/mora/internal/google", "github.com/pyranthus-hq/mora/internal/imessage"}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbiddenImports {
			if path == bad {
				t.Fatalf("brief.go imports %q — zero-egress invariant forbids any network/connector import", path)
			}
		}
	}

	// (2) No forbidden function identifier referenced (a sync/fetch call would
	// break zero-egress; calling saveBriefSnapshot/acquireBriefLock would break
	// read-only). Comments are excluded by construction (we walk the AST, not the
	// text), so the store half's prose never trips this.
	//
	// saveBriefSnapshot and acquireBriefLock are DEFINED in this file (the
	// watermark-store half) — their FuncDecl.Name identifiers are legitimate, only
	// a *call* from the read side is forbidden. We therefore walk function BODIES
	// only (skipping each FuncDecl's own name + signature), so a definition never
	// trips while any invocation does.
	forbiddenIdents := map[string]bool{
		"backfillGoogleFn":   true,
		"backfillIMessageFn": true,
		"LiveFetcher":        true,
		"saveBriefSnapshot":  true,
		"acquireBriefLock":   true,
	}
	report := func(n ast.Node) {
		ast.Inspect(n, func(x ast.Node) bool {
			switch id := x.(type) {
			case *ast.Ident:
				if forbiddenIdents[id.Name] {
					t.Errorf("brief.go references %q — the resolver must never sync, fetch, or advance the watermark", id.Name)
				}
			case *ast.SelectorExpr:
				if forbiddenIdents[id.Sel.Name] {
					t.Errorf("brief.go references %q — the resolver must never sync, fetch, or advance the watermark", id.Sel.Name)
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			report(fn.Body) // walk the body only — skips fn.Name (the definition).
		}
	}
}

// surfacedItemCount sums len(section.Items) across a digest — the "is the delta
// empty" predicate the resolver uses.
func surfacedItemCount(d Digest) int {
	n := 0
	for _, s := range d.Sections {
		n += len(s.Items)
	}
	return n
}
