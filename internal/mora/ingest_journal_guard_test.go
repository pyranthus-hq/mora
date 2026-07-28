package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIngestJournalRefusesUnrootedStateDir: the journal/lease write seam fails
// loudly on an empty or relative StateDir instead of resolving against the
// process cwd — which under `go test` is the package source tree, where PR
// #182 nearly committed a leaked lease file (#184).
func TestIngestJournalRefusesUnrootedStateDir(t *testing.T) {
	for _, cfg := range []Config{
		{StateDir: ""},
		{StateDir: "state"},
		{StateDir: "./state"},
	} {
		if err := ensureIngestLease(cfg, "imessage"); err == nil {
			t.Fatalf("ensureIngestLease(StateDir=%q) succeeded, want refusal", cfg.StateDir)
		} else if !strings.Contains(err.Error(), "absolute state_dir") {
			t.Fatalf("ensureIngestLease(StateDir=%q) error = %q, want absolute-state_dir refusal", cfg.StateDir, err)
		}
		if err := ensureIngestJournalHeader(cfg, "imessage"); err == nil {
			t.Fatalf("ensureIngestJournalHeader(StateDir=%q) succeeded, want refusal", cfg.StateDir)
		}
		// The best-effort path line must be a no-op, not a cwd write.
		journalPublishedPath(cfg, "imessage", "sources/imessage/x.md")
	}
	// Nothing may have materialized relative to the working directory.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "ingest")); !os.IsNotExist(err) {
		t.Fatalf("refused journal writes still created %s (stat err=%v)", filepath.Join(cwd, "ingest"), err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "state")); !os.IsNotExist(err) {
		t.Fatalf("refused journal writes still created %s (stat err=%v)", filepath.Join(cwd, "state"), err)
	}
}

// TestIngestJournalAcceptsAbsoluteStateDir: the guard passes a rooted state
// dir through — lease and header land under StateDir/ingest/<source>.
func TestIngestJournalAcceptsAbsoluteStateDir(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	if err := ensureIngestJournalHeader(cfg, "imessage"); err != nil {
		t.Fatalf("ensureIngestJournalHeader = %v", err)
	}
	if _, err := os.Stat(ingestJournalPath(cfg, "imessage")); err != nil {
		t.Fatalf("journal header missing: %v", err)
	}
	if _, err := os.Stat(ingestLeasePath(cfg, "imessage")); err != nil {
		t.Fatalf("lease missing: %v", err)
	}
	releaseIngestLeasesOwnedHere(cfg)
	if _, err := os.Stat(ingestLeasePath(cfg, "imessage")); !os.IsNotExist(err) {
		t.Fatalf("lease not released (stat err=%v)", err)
	}
}
