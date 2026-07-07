package mora

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// artifactFixedNow is a fixed UTC instant used so the dated-path/content tests
// are clock-independent (mirrors brief_test.go's fixedNow discipline).
var artifactFixedNow = time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

func artifactCfg(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		VaultDir: filepath.Join(root, "vault"),
		StateDir: filepath.Join(root, "state"),
	}
}

// TestBriefArtifactPathFixedNow pins the deterministic path shape:
// <VaultDir>/briefs/<UTC-date>-brief.md from the injected now.
func TestBriefArtifactPathFixedNow(t *testing.T) {
	cfg := artifactCfg(t)
	got := briefArtifactPath(cfg, artifactFixedNow)
	want := filepath.Join(cfg.VaultDir, "briefs", "2026-06-08-brief.md")
	if got != want {
		t.Fatalf("briefArtifactPath = %q, want %q", got, want)
	}
}

// TestBriefArtifactPathUTCRoll proves the date is UTC-canonicalized from the
// injected now: a late local evening (23:30 -04:00) lands on the NEXT UTC day,
// deterministically, with no fresh-clock dependence (D13-3).
func TestBriefArtifactPathUTCRoll(t *testing.T) {
	cfg := artifactCfg(t)
	zone := time.FixedZone("EDT", -4*60*60)
	lateLocal := time.Date(2026, 6, 8, 23, 30, 0, 0, zone) // == 2026-06-09T03:30:00Z
	got := briefArtifactPath(cfg, lateLocal)
	want := filepath.Join(cfg.VaultDir, "briefs", "2026-06-09-brief.md")
	if got != want {
		t.Fatalf("briefArtifactPath(late local) = %q, want UTC-rolled %q", got, want)
	}
}

// TestBriefArtifactPathUnderVault asserts the artifact lives under
// VaultDir/briefs and NEVER under StateDir (the watermark's home). briefs/ is a
// new vault subdir, sibling to sources/.
func TestBriefArtifactPathUnderVault(t *testing.T) {
	cfg := artifactCfg(t)
	got := briefArtifactPath(cfg, artifactFixedNow)

	briefsDir := filepath.Join(cfg.VaultDir, "briefs")
	if dir := filepath.Dir(got); dir != briefsDir {
		t.Fatalf("artifact dir = %q, want %q (VaultDir/briefs)", dir, briefsDir)
	}
	rel, err := filepath.Rel(cfg.StateDir, got)
	if err == nil && rel != "" && !filepath.IsAbs(rel) && rel[0] != '.' {
		// rel that does not start with ".." would mean got is inside StateDir.
		if len(rel) < 2 || rel[:2] != ".." {
			t.Fatalf("artifact path %q must NOT live under StateDir %q (watermark home)", got, cfg.StateDir)
		}
	}
}

// sampleDigest builds a small, fully-populated Digest for the write tests.
func sampleDigest(marker string) Digest {
	return Digest{
		Generated:  artifactFixedNow.UTC().Format(time.RFC3339),
		SinceHours: 0,
		Sections: []DigestSection{
			{Source: "gmail", State: "new", Items: []DigestItem{{Title: marker}}},
		},
		Freshness:  map[string]string{"gmail": "fresh"},
		StaleTasks: []string{"task-" + marker},
	}
}

// globBriefs returns every *-brief.md under <VaultDir>/briefs.
func globBriefs(t *testing.T, cfg Config) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(cfg.VaultDir, "briefs", "*-brief.md"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

// TestWriteBriefArtifactBodyMatchesRender asserts the artifact body is EXACTLY
// renderDigest(d, defaultContextTokens*charsPerToken) — one source of truth with
// the human brief / MCP digest — and that the returned path is the dated path.
func TestWriteBriefArtifactBodyMatchesRender(t *testing.T) {
	cfg := artifactCfg(t)
	d := sampleDigest("alpha")

	path, err := writeBriefArtifact(cfg, d, artifactFixedNow)
	if err != nil {
		t.Fatalf("writeBriefArtifact: %v", err)
	}
	if want := briefArtifactPath(cfg, artifactFixedNow); path != want {
		t.Fatalf("returned path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	want := renderDigest(d, defaultContextTokens*charsPerToken)
	if string(got) != want {
		t.Fatalf("artifact body != renderDigest output\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWriteBriefArtifactIdempotentOverwrite proves a same-now double write leaves
// exactly ONE file for that day, carrying the SECOND render's content — no
// -brief-1.md, no append, no proliferation (SC#4).
func TestWriteBriefArtifactIdempotentOverwrite(t *testing.T) {
	cfg := artifactCfg(t)

	if _, err := writeBriefArtifact(cfg, sampleDigest("first"), artifactFixedNow); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := writeBriefArtifact(cfg, sampleDigest("second"), artifactFixedNow); err != nil {
		t.Fatalf("second write: %v", err)
	}

	files := globBriefs(t, cfg)
	if len(files) != 1 {
		t.Fatalf("expected exactly ONE *-brief.md for the day, got %d: %v", len(files), files)
	}
	got, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if want := renderDigest(sampleDigest("second"), defaultContextTokens*charsPerToken); string(got) != want {
		t.Fatalf("idempotent write did not carry the SECOND content")
	}
}

// TestWriteBriefArtifactDistinctDays asserts a different now yields a second,
// separately-dated file so two distinct days coexist.
func TestWriteBriefArtifactDistinctDays(t *testing.T) {
	cfg := artifactCfg(t)
	day2 := artifactFixedNow.Add(24 * time.Hour)

	if _, err := writeBriefArtifact(cfg, sampleDigest("d1"), artifactFixedNow); err != nil {
		t.Fatalf("day1 write: %v", err)
	}
	if _, err := writeBriefArtifact(cfg, sampleDigest("d2"), day2); err != nil {
		t.Fatalf("day2 write: %v", err)
	}

	files := globBriefs(t, cfg)
	if len(files) != 2 {
		t.Fatalf("expected two dated briefs, got %d: %v", len(files), files)
	}
}

// TestWriteBriefArtifactDoesNotAdvanceWatermark is the SC#4 invariant: persisting
// the artifact must NOT create or advance the Phase-12 watermark at
// <StateDir>/brief/. A write touches the vault only.
func TestWriteBriefArtifactDoesNotAdvanceWatermark(t *testing.T) {
	cfg := artifactCfg(t)

	if _, err := writeBriefArtifact(cfg, sampleDigest("nowm"), artifactFixedNow); err != nil {
		t.Fatalf("writeBriefArtifact: %v", err)
	}

	// No per-instance watermark file may exist (the brief/ dir must stay absent
	// or empty — the write never calls saveBriefSnapshot).
	if _, err := os.Stat(briefPath(cfg, "gmail")); !os.IsNotExist(err) {
		t.Fatalf("watermark file for 'gmail' exists after a write (err=%v) — artifact write must not advance the watermark", err)
	}
	wmDir := filepath.Join(cfg.StateDir, "brief")
	if entries, err := os.ReadDir(wmDir); err == nil && len(entries) > 0 {
		t.Fatalf("StateDir/brief is non-empty after a write: %v — must not advance the watermark", entries)
	}
}

// TestWriteBriefArtifactMode asserts the artifact is 0644 — human-readable vault
// content (like memories under sources/), NOT the 0600 secret watermark.
func TestWriteBriefArtifactMode(t *testing.T) {
	cfg := artifactCfg(t)
	path, err := writeBriefArtifact(cfg, sampleDigest("mode"), artifactFixedNow)
	if err != nil {
		t.Fatalf("writeBriefArtifact: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	assertPermUnix(t, fi.Mode(), 0o644)
}
