package mora

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"testing"
)

// persistedJournalMode opens index.db with a NEUTRAL DSN (no journal_mode pragma,
// so the open cannot itself convert the file) and reads the mode stored in the
// database header. `PRAGMA journal_mode` with no `=` only reports; it never
// changes the mode.
func persistedJournalMode(t *testing.T, cfg Config) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	return mode
}

// TestIndexIsWAL guards the multi-process lock-contention bug: in the default
// rollback-journal (delete) mode a writer's EXCLUSIVE lock is incompatible with
// every reader's SHARED lock, so under N long-lived `mora mcp serve` reader
// processes a write blows past busy_timeout and surfaces "database is locked".
// WAL lets concurrent readers and the single writer proceed without mutual
// exclusion. A rebuilt index MUST persist WAL mode.
func TestIndexIsWAL(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if mode := persistedJournalMode(t, cfg); mode != "wal" {
		t.Fatalf("index.db journal_mode = %q, want \"wal\" (delete mode causes multi-process lock contention → 'database is locked')", mode)
	}
}

// TestIndexUpsertKeepsWAL ensures the incremental user-write path (indexUpsert)
// also opens the live index in WAL, so a plain `mora write` never regresses the
// file back to a mode that mutually excludes readers.
func TestIndexUpsertKeepsWAL(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// A user write goes through the incremental upsert path, not a full rebuild.
	if err := Run(context.Background(), []string{"write", "--title", "t", "--text", "hello wal"}, &nopW{}, &nopW{}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if mode := persistedJournalMode(t, cfg); mode != "wal" {
		t.Fatalf("after user write, index.db journal_mode = %q, want \"wal\"", mode)
	}
}

// TestReadOnlyIndexNeedsNoDirectoryWriteAccess pins the sandboxed-agent case:
// callers may read index.db while the containing data directory is not writable.
// The reader must not request journal-mode changes or sidecar creation.
func TestReadOnlyIndexNeedsNoDirectoryWriteAccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory mode bits do not model Windows ACL write denial")
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if err := os.Chmod(cfg.DataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.DataDir, 0o700) })

	prevHeal := indexAutoHeal
	indexAutoHeal = func(Config) bool { return false }
	t.Cleanup(func() { indexAutoHeal = prevHeal })

	db, err := openIndexRO(context.Background(), cfg)
	if err != nil {
		t.Fatalf("read-only open required directory write access: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("read from index in a non-writable directory: %v", err)
	}
}

type nopW struct{}

func (nopW) Write(p []byte) (int, error) { return len(p), nil }
