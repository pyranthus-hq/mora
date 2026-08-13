package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

func TestPathsKeysAndValidation(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := ValidateStateRoot(cfg); err != nil {
		t.Fatal(err)
	}
	if ValidateStateRoot(config.Config{StateDir: "relative"}) == nil || ValidateStateRoot(config.Config{}) == nil {
		t.Fatal("relative/empty state must fail")
	}
	if got := SourceKey("gmail", "work/account"); strings.Contains(got, "/") {
		t.Fatalf("unsafe key=%q", got)
	}
	if SourceKey("", "") != "unknown" {
		t.Fatal("empty key mismatch")
	}
	if got := JournalPath(cfg, "gmail"); got != filepath.Join(cfg.StateDir, "ingest", "gmail", "journal.log") {
		t.Fatalf("journal=%q", got)
	}
	if got := LeasePath(cfg, "gmail"); got != filepath.Join(cfg.StateDir, "ingest", "gmail", "lease") {
		t.Fatalf("lease=%q", got)
	}
}
func TestScanAndStatusFailClosed(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	dir := filepath.Dir(JournalPath(cfg, "gmail"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(JournalPath(cfg, "gmail"), []byte("run r_abc 2026-08-13T02:00:00Z\n/a.md\n/b.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hdr, n, present := ScanJournal(JournalPath(cfg, "gmail"))
	if hdr != "2026-08-13T02:00:00Z" || n != 2 || !present {
		t.Fatalf("scan=(%q,%d,%v)", hdr, n, present)
	}
	if err := os.MkdirAll(filepath.Dir(JournalPath(cfg, "calendar")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(JournalPath(cfg, "calendar"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, paths, oldest, err := JournalStatus(cfg)
	if err != nil || !dirty || paths != 2 || oldest != "2026-08-13T02:00:00Z" {
		t.Fatalf("status=(%v,%d,%q,%v)", dirty, paths, oldest, err)
	}
}
func TestMissingJournalIsClean(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	dirty, n, oldest, err := JournalStatus(cfg)
	if err != nil || dirty || n != 0 || oldest != "" {
		t.Fatalf("status=(%v,%d,%q,%v)", dirty, n, oldest, err)
	}
	if _, _, present := ScanJournal(filepath.Join(t.TempDir(), "missing")); present {
		t.Fatal("missing scan present")
	}
}
func TestHeaderRunID(t *testing.T) {
	valid := func(s string) bool { return strings.HasPrefix(s, "r_") }
	if got := HeaderRunID("run r_abc 2026-01-01T00:00:00Z", valid); got != "r_abc" {
		t.Fatalf("got=%q", got)
	}
	for _, h := range []string{"", "run bad now", "broken r_a now"} {
		if got := HeaderRunID(h, valid); got != "" {
			t.Errorf("HeaderRunID(%q)=%q", h, got)
		}
	}
}

func TestAppendDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "journal.log")
	if err := AppendDurable(path, "one\n"); err != nil {
		t.Fatal(err)
	}
	if err := AppendDurable(path, "two\n"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one\ntwo\n" {
		t.Fatalf("body=%q", body)
	}
}
