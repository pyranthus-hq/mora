package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
)

// TestIngestJournalRefusesUnrootedStateDir: the journal/lease write seam fails
// loudly on an empty or relative StateDir instead of resolving against the
// process cwd — which under `go test` is the package source tree, where PR
// #182 nearly committed a leaked lease file (#184).
func TestIngestJournalRefusesUnrootedStateDir(t *testing.T) {
	for _, cfg := range []config.Config{
		{StateDir: ""},
		{StateDir: "state"},
		{StateDir: "./state"},
	} {
		if err := EnsureLease(cfg, "imessage", testLeaseSeams()); err == nil {
			t.Fatalf("ensureIngestLease(StateDir=%q) succeeded, want refusal", cfg.StateDir)
		} else if !strings.Contains(err.Error(), "absolute state_dir") {
			t.Fatalf("ensureIngestLease(StateDir=%q) error = %q, want absolute-state_dir refusal", cfg.StateDir, err)
		}
		if err := EnsureJournalHeader(cfg, "imessage", testPublishSeams()); err == nil {
			t.Fatalf("ensureIngestJournalHeader(StateDir=%q) succeeded, want refusal", cfg.StateDir)
		}
		// The best-effort path line must be a no-op, not a cwd write.
		RecordPublishedPath(cfg, "imessage", "sources/imessage/x.md", testPublishSeams())
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
	cfg := config.Config{StateDir: t.TempDir()}
	if err := EnsureJournalHeader(cfg, "imessage", testPublishSeams()); err != nil {
		t.Fatalf("ensureIngestJournalHeader = %v", err)
	}
	if _, err := os.Stat(JournalPath(cfg, "imessage")); err != nil {
		t.Fatalf("journal header missing: %v", err)
	}
	if _, err := os.Stat(LeasePath(cfg, "imessage")); err != nil {
		t.Fatalf("lease missing: %v", err)
	}
	ReleaseLeasesOwnedHere(cfg, testLeaseSeams())
	if _, err := os.Stat(LeasePath(cfg, "imessage")); !os.IsNotExist(err) {
		t.Fatalf("lease not released (stat err=%v)", err)
	}
}

func testLeaseSeams() LeaseSeams {
	return LeaseSeams{PID: os.Getpid, ProcessAlive: func(int) bool { return true }}
}
func testPublishSeams() PublishSeams {
	return PublishSeams{ValidToken: func(string) bool { return false }, NewID: func() string { return "op_test" }, Clock: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }, CleanPath: filepath.ToSlash, Lease: testLeaseSeams()}
}
