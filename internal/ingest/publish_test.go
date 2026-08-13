package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
)

func publishSeams(pid int) PublishSeams {
	return PublishSeams{ValidToken: func(s string) bool { return len(s) > 2 && s[:2] == "r_" }, NewID: func() string { return "r_minted" }, Clock: func() time.Time { return time.Date(2026, 8, 13, 2, 3, 4, 0, time.FixedZone("x", 3600)) }, CleanPath: func(p string) string { return filepath.Clean(p) }, Lease: leaseSeams(pid, map[int]bool{pid: true})}
}
func TestEnsureJournalHeaderMarksLeaseAndWritesOnce(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	cfg.SetOperationRunID("r_existing")
	seams := publishSeams(42)
	if err := EnsureJournalHeader(cfg, "gmail", seams); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(JournalPath(cfg, "gmail"))
	if err != nil {
		t.Fatal(err)
	}
	want := "run r_existing 2026-08-13T01:03:04Z\n"
	if string(body) != want {
		t.Fatalf("body=%q want=%q", body, want)
	}
	if !LeaseHeld(cfg, "gmail", seams.Lease) {
		t.Fatal("lease missing")
	}
	if err := EnsureJournalHeader(cfg, "gmail", seams); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(JournalPath(cfg, "gmail"))
	if string(body) != want {
		t.Fatalf("idempotence body=%q", body)
	}
}
func TestEnsureJournalHeaderMintsInvalidRunID(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	cfg.SetOperationRunID("bad")
	if err := EnsureJournalHeader(cfg, "gmail", publishSeams(42)); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(JournalPath(cfg, "gmail"))
	if string(body) != "run r_minted 2026-08-13T01:03:04Z\n" {
		t.Fatalf("body=%q", body)
	}
}
func TestEnsureJournalHeaderRejectsRelativeState(t *testing.T) {
	if err := EnsureJournalHeader(config.Config{StateDir: "relative"}, "gmail", publishSeams(42)); err == nil {
		t.Fatal("relative state accepted")
	}
}
func TestRecordPublishedPathBestEffort(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	seams := publishSeams(42)
	RecordPublishedPath(cfg, "gmail", "a/../b.md", seams)
	body, err := os.ReadFile(JournalPath(cfg, "gmail"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "b.md\n" {
		t.Fatalf("body=%q", body)
	}
	RecordPublishedPath(config.Config{StateDir: "relative"}, "gmail", "x", seams)
}
