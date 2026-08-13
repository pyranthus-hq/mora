package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

func recoverySeams(held bool) RecoverySeams {
	return RecoverySeams{CleanPathSet: func(paths []string) map[string]bool {
		out := map[string]bool{}
		for _, p := range paths {
			abs, _ := filepath.Abs(p)
			out[filepath.Clean(abs)] = true
		}
		return out
	}, CleanPath: func(p string) string { abs, _ := filepath.Abs(p); return filepath.Clean(abs) }, LeaseHeld: func(config.Config, string) bool { return held }, Remove: os.Remove, ValidToken: func(s string) bool { return strings.HasPrefix(s, "r_") }}
}
func writeJournal(t *testing.T, cfg config.Config, key, body string) {
	t.Helper()
	path := JournalPath(cfg, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func TestRecoverRetiresCoveredJournalAndRun(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	file := filepath.Join(t.TempDir(), "m.md")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, cfg, "gmail", "run r_abc 2026-01-01T00:00:00Z\n"+file+"\n")
	got, err := RecoverJournals(cfg, []string{file}, recoverySeams(false))
	if err != nil || len(got.RetiredRunIDs) != 1 || got.RetiredRunIDs[0] != "r_abc" {
		t.Fatalf("recovery=(%+v,%v)", got, err)
	}
	if _, err = os.Stat(JournalPath(cfg, "gmail")); !os.IsNotExist(err) {
		t.Fatalf("journal remains: %v", err)
	}
}
func TestCompactKeepsHeaderWhileLeaseHeld(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	writeJournal(t, cfg, "gmail", "run r_abc 2026-01-01T00:00:00Z\n/gone\n")
	retired, err := CompactJournal(cfg, "gmail", map[string]bool{}, recoverySeams(true))
	if err != nil || retired != "" {
		t.Fatalf("compact=(%q,%v)", retired, err)
	}
	body, _ := os.ReadFile(JournalPath(cfg, "gmail"))
	if string(body) != "run r_abc 2026-01-01T00:00:00Z\n" {
		t.Fatalf("body=%q", body)
	}
}
func TestCompactKeepsUncoveredExistingPaths(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	file := filepath.Join(t.TempDir(), "later.md")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, cfg, "gmail", "run r_abc now\n"+file+"\n/gone\n")
	retired, err := CompactJournal(cfg, "gmail", map[string]bool{}, recoverySeams(false))
	if err != nil || retired != "" {
		t.Fatalf("compact=(%q,%v)", retired, err)
	}
	body, _ := os.ReadFile(JournalPath(cfg, "gmail"))
	if !strings.Contains(string(body), file) || strings.Contains(string(body), "/gone") {
		t.Fatalf("body=%q", body)
	}
}
func TestRecoveryMissingAndRemovalError(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	got, err := RecoverJournals(cfg, nil, recoverySeams(false))
	if err != nil || len(got.RetiredRunIDs) != 0 {
		t.Fatalf("missing=(%+v,%v)", got, err)
	}
	writeJournal(t, cfg, "gmail", "run r_abc now\n")
	seams := recoverySeams(false)
	seams.Remove = func(string) error { return errors.New("remove") }
	if _, err = RecoverJournals(cfg, nil, seams); err == nil || !strings.Contains(err.Error(), "retiring ingest journal") {
		t.Fatalf("error=%v", err)
	}
}
