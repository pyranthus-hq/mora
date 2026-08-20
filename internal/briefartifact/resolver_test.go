package briefartifact

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var resolveFixedNow = time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

type resolveConfig struct{ VaultDir string }

func resolveCfg(t *testing.T) resolveConfig {
	t.Helper()
	return resolveConfig{VaultDir: filepath.Join(t.TempDir(), "vault")}
}
func seedBriefFile(t *testing.T, cfg resolveConfig, date, body string) string {
	t.Helper()
	dir := filepath.Join(cfg.VaultDir, "briefs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, date+"-brief.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
func latestBriefPath(cfg resolveConfig, _ time.Time) (string, time.Time, bool) {
	return Latest(cfg.VaultDir)
}
func briefIsFresh(dated, now time.Time) bool { return IsFresh(dated, now) }

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

func TestLatestBriefPathIgnoresJunk(t *testing.T) {
	cfg := resolveCfg(t)
	seedBriefFile(t, cfg, "2026-06-07", "real")
	dir := filepath.Join(cfg.VaultDir, "briefs")
	if err := os.Mkdir(filepath.Join(dir, "2026-06-09-brief.md"), 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestLatestBriefPathAbsentDir(t *testing.T) {
	cfg := resolveCfg(t) // VaultDir/briefs does not exist
	if _, _, ok := latestBriefPath(cfg, resolveFixedNow); ok {
		t.Fatalf("latestBriefPath ok=true on absent briefs dir, want false")
	}
}

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

func TestBriefIsFreshUTCBoundary(t *testing.T) {
	now := resolveFixedNow // 2026-06-08T12:00:00Z, UTC day 2026-06-08
	zone := time.FixedZone("EDT", -4*60*60)
	// 2026-06-07T22:00:00-04:00 == 2026-06-08T02:00:00Z => today's UTC day => fresh.
	dated := time.Date(2026, 6, 7, 22, 0, 0, 0, zone)
	if !briefIsFresh(dated, now) {
		t.Fatalf("briefIsFresh should compare on the UTC day (got stale for a today-UTC file)")
	}
}
