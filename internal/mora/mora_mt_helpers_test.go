package mora

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// mtBreakIndex stales the on-disk index (user_version=0) AND pins the auto-heal
// policy OFF, so any subsequent ensureIndexDB/openIndexRO/hybridSearchTrace call
// fails loudly instead of silently self-healing.
func mtBreakIndex(t *testing.T, cfg Config) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatalf("open index for staling: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		db.Close()
		t.Fatalf("stale index user_version: %v", err)
	}
	db.Close()
	prev := indexAutoHeal
	indexAutoHeal = func(Config) bool { return false }
	t.Cleanup(func() { indexAutoHeal = prev })
}

// mtScratchDB opens a fresh, empty temp-file sqlite DB (no mora schema) the caller
// can shape with hand-crafted rows to drive Scan/type-mismatch error paths.
func mtScratchDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mt-scratch.db"))
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mtClosedDB returns a closed sqlite handle for deterministic query errors.
func mtClosedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mt-closed.db"))
	if err != nil {
		t.Fatalf("open closed db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping before close: %v", err)
	}
	db.Close()
	return db
}

// mtMakeMemoriesUnreadable creates an unreadable subdirectory under the vault's
// memories tree so allMemoryFiles surfaces a permission error.
func mtMakeMemoriesUnreadable(t *testing.T, cfg Config) {
	t.Helper()
	skipOnWindows(t, "chmod 0000 does not block WalkDir on Windows; the unreadable-memories walk error can't be provoked")
	if os.Geteuid() == 0 {
		t.Skip("runs as root — 0000 perms are bypassed, so the walk error can't be provoked")
	}
	bad := filepath.Join(memoriesRoot(cfg), "mtbad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatalf("mkdir bad dir: %v", err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod 0000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o755) })
}
