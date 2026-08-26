package mora

import (
	"database/sql"
	"testing"
	"time"
)

// TestReadOnlyIndexWaitsOnWriteLock pins busy_timeout on the READ side: the
// hourly rebuild (or an MCP write_memory) briefly holds the writer lock, and a
// zero-timeout read-only connection surfaces a raw "database is locked" to the
// user/agent instead of waiting out the window. The writer DSN already carries
// busy_timeout(5000); this holds the read path to the same standard.
func TestReadOnlyIndexWaitsOnWriteLock(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	// Disable the auto-heal fallback: without a busy_timeout on the ro DSN the
	// schema probe's SQLITE_BUSY is misread as "stale index" and openIndexRO
	// quietly launches a FULL rebuild (whose writer DSN waits out the lock) —
	// masking the missing timeout here and wasting a rebuild in production.
	// With heal off, the raw read path must survive the lock on its own.
	prevHeal := indexAutoHeal
	indexAutoHeal = func(Config) bool { return false }
	t.Cleanup(func() { indexAutoHeal = prevHeal })

	// Hold an EXCLUSIVE transaction on a pinned connection (sql.DB pooling
	// would otherwise COMMIT on a different connection), then release it after
	// a beat — far shorter than the 5s a reader should be willing to wait.
	w, err := sql.Open("sqlite", dbPath(cfg)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	conn, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	release := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, cerr := conn.ExecContext(ctx, "COMMIT")
		release <- cerr
	}()

	// openIndexRO's schema probe (PRAGMA user_version) runs under the held
	// lock: without a busy_timeout it fails immediately with SQLITE_BUSY.
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		t.Fatalf("read-only open under a brief write lock failed (busy_timeout missing on the ro DSN?): %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("read after lock release: %v", err)
	}
	if err := <-release; err != nil {
		t.Fatalf("releasing the write lock: %v", err)
	}
}
