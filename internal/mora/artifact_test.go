package mora

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var artifactFixedNow = time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

func artifactCfg(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		VaultDir: filepath.Join(root, "vault"),
		StateDir: filepath.Join(root, "state"),
	}
}

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
