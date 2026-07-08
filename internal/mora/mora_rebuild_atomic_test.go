package mora

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestRebuildIndexIsAtomic proves that a mid-rebuild INSERT failure leaves the
// previously-committed index fully intact rather than half-emptied (Codex
// BLOCKER #5). A SQLite BEFORE INSERT trigger forces a deterministic failure on
// a poison memory id, hitting the same sql.Open(dbPath) path rebuildIndex uses,
// so the whole rebuild (memories + FTS) must roll back as one unit.
func TestRebuildIndexIsAtomic(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Seed + build the "old" index.
	if err := writeMemory(cfg, Memory{ID: "aa_keep", Scope: "personal", Title: "Keep", Text: "alphakeeptoken"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Mutate the vault after the old index exists: aa_keep changes, and a poison
	// memory is added whose insert the trigger will reject.
	if err := writeMemory(cfg, Memory{ID: "aa_keep", Scope: "personal", Title: "Keep", Text: "betaupdatetoken"}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, Memory{ID: "zz_fail", Scope: "personal", Title: "Boom", Text: "gammafailtoken"}); err != nil {
		t.Fatal(err)
	}

	// Install a trigger that fails the INSERT of the poison row mid-rebuild.
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_rebuild BEFORE INSERT ON memories
		WHEN NEW.id = 'zz_fail'
		BEGIN SELECT RAISE(FAIL, 'forced rebuild failure'); END;`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// The rebuild must fail because of the trigger.
	if _, err := rebuildIndex(ctx, cfg); err == nil {
		t.Fatal("expected rebuildIndex to fail due to the forced-failure trigger")
	}

	// ...and the prior committed index must survive intact (all-or-nothing).
	assertCount := func(query string, want int) {
		t.Helper()
		got, err := searchMemories(ctx, cfg, query, "", 10)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(got) != want {
			t.Fatalf("after a failed rebuild, search %q = %d results, want %d (rebuild was not atomic)", query, len(got), want)
		}
	}
	assertCount("alphakeeptoken", 1)  // old committed row preserved by rollback
	assertCount("betaupdatetoken", 0) // partial new write must have rolled back
	assertCount("gammafailtoken", 0)  // poison row never committed
}

// TestRebuildIsAtomicWhenGraphWriteFails proves the entity-graph materialization
// runs inside the SAME rebuild transaction: a failure while writing edges rolls
// back the memories + FTS writes too, preserving the prior index.
func TestRebuildIsAtomicWhenGraphWriteFails(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{ID: "keep", Scope: "personal", Title: "K", Text: "deltakeeptoken [[Anchor]]", CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Mutate the vault, then make any edge write fail mid-rebuild.
	if err := writeMemory(cfg, Memory{ID: "keep", Scope: "personal", Title: "K", Text: "epsilonnewtoken [[Anchor]]", CreatedAt: "2026-01-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_graph BEFORE INSERT ON edges
		BEGIN SELECT RAISE(FAIL, 'forced graph failure'); END;`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := rebuildIndex(ctx, cfg); err == nil {
		t.Fatal("expected rebuildIndex to fail while writing the graph")
	}

	got, err := searchMemories(ctx, cfg, "deltakeeptoken", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a graph-write failure must roll back memories too; deltakeeptoken=%d, want 1", len(got))
	}
	got, _ = searchMemories(ctx, cfg, "epsilonnewtoken", "", 10)
	if len(got) != 0 {
		t.Fatalf("epsilonnewtoken=%d, want 0 (the partial new write must have rolled back)", len(got))
	}
}

// TestRebuildListsVaultInsideWriteLock proves the vault directory listing runs
// AFTER the immediate write transaction has acquired the writer lock, never
// before (P1 snapshot-before-lock race). Were the listing snapshotted before
// BeginTx, two concurrent rebuilds could interleave so the one that COMMITS LAST
// indexed the OLDER file list — silently dropping a memory that is on disk until
// some later rebuild. Because the immediate lock serializes rebuilds, listing
// inside the transaction guarantees the later-committing rebuild also listed
// later, so it always indexes a superset.
//
// Deterministic — no sleeps, no goroutine races. The rebuild's listing is routed
// through the listRebuildFiles seam; the instant it fires we probe from a SECOND
// connection with a competing `BEGIN IMMEDIATE` under busy_timeout(0). That probe
// fails fast with SQLITE_BUSY iff the rebuild already holds the writer lock,
// which is true only when the listing happens inside the transaction.
func TestRebuildListsVaultInsideWriteLock(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{ID: "aa_one", Scope: "personal", Title: "One", Text: "onetoken"}); err != nil {
		t.Fatal(err)
	}

	orig := listRebuildFiles
	defer func() { listRebuildFiles = orig }()

	var fired, lockHeld bool
	listRebuildFiles = func(c Config) ([]string, error) {
		fired = true
		// A second connection with busy_timeout(0) so a contended BEGIN IMMEDIATE
		// returns immediately instead of waiting — keeps the probe deterministic.
		db2, err := sql.Open("sqlite", dbPath(c)+"?_txlock=immediate&_pragma=busy_timeout(0)")
		if err != nil {
			t.Errorf("probe open: %v", err)
			return orig(c)
		}
		defer db2.Close()
		if tx2, err := db2.BeginTx(context.Background(), nil); err != nil {
			// Could not take the writer lock -> the rebuild already holds it.
			lockHeld = strings.Contains(err.Error(), "SQLITE_BUSY")
		} else {
			_ = tx2.Rollback()
		}
		return orig(c)
	}

	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("listRebuildFiles seam never fired; the rebuild listing path moved")
	}
	if !lockHeld {
		t.Fatal("vault listing ran BEFORE the write lock was acquired: snapshot-before-lock " +
			"race (P1). Move the listRebuildFiles call inside the immediate transaction.")
	}
}
