package mora

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openloops_mt_cover_test.go covers the error/empty branches of openloops.go the
// happy-path suite (openloops_test.go) leaves out: the listTasks error forks, the
// gazetteer/index failures, the Blocker join, the query-person-with-no-loops skip,
// and the entity-display-name fallback.

// TestMt_OpenLoopsByPersonMissingLedger: a vault with NO live-tasks.md yields an
// empty map, not an error (the os.IsNotExist recovery).
func TestMt_OpenLoopsByPersonMissingLedger(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "sam@a.com", "Sam Rivera", time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Ensure the ledger is truly absent so listTasks returns os.ErrNotExist.
	_ = os.Remove(filepath.Join(cfg.VaultDir, "live-tasks.md"))

	db := openRO(t, cfg)
	defer db.Close()
	byPerson, err := openLoopsByPerson(ctx, cfg, db)
	if err != nil {
		t.Fatalf("missing ledger must not error, got: %v", err)
	}
	if len(byPerson) != 0 {
		t.Fatalf("missing ledger should yield an empty map, got %+v", byPerson)
	}
}

// TestMt_OpenLoopsByPersonLedgerReadError: a live-tasks.md that is a DIRECTORY
// makes listTasks fail with a non-NotExist error, which propagates.
func TestMt_OpenLoopsByPersonLedgerReadError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(cfg.VaultDir, "live-tasks.md")
	_ = os.Remove(live)
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatalf("mkdir live-tasks.md-as-dir: %v", err)
	}
	db := openRO(t, cfg)
	defer db.Close()
	if _, err := openLoopsByPerson(ctx, cfg, db); err == nil {
		t.Fatal("a directory live-tasks.md should surface a read error")
	}
}

// TestMt_OpenLoopsByPersonGazetteerError: a broken (closed) DB fails the gazetteer
// load AFTER listTasks succeeds, and the error propagates.
func TestMt_OpenLoopsByPersonGazetteerError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	// A real ledger row so listTasks succeeds and we reach loadPersonGazetteer.
	writeLiveTasks(t, cfg, "| Ship the thing | work | you | P1 | active | — | wk | 2026-06-10 |")
	if _, err := openLoopsByPerson(ctx, cfg, mtClosedDB(t)); err == nil {
		t.Fatal("openLoopsByPerson with a closed db should surface the gazetteer error")
	}
}

// TestMt_OpenLoopsByPersonBlockerJoin: a task whose Blocker column (not its Task
// text) names a person still joins to that person.
func TestMt_OpenLoopsByPersonBlockerJoin(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "sam@a.com", "Sam Rivera", time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Task text names nobody; the Blocker column (col 6) carries "Sam Rivera".
	writeLiveTasks(t, cfg, "| Unblock the launch | work | you | P1 | active | Sam Rivera | wk | 2026-06-10 |")

	db := openRO(t, cfg)
	defer db.Close()
	byPerson, err := openLoopsByPerson(ctx, cfg, db)
	if err != nil {
		t.Fatalf("openLoopsByPerson: %v", err)
	}
	loops := byPerson["person:sam@a.com"]
	if len(loops) != 1 || loops[0].Task != "Unblock the launch" {
		t.Fatalf("Blocker-column join failed: %+v", byPerson)
	}
}

// TestMt_OpenLoopsForQueryEnsureIndexError: a broken index fails openLoopsForQuery
// at its own ensureIndexDB.
func TestMt_OpenLoopsForQueryEnsureIndexError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "sam@a.com", "Sam Rivera", time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mtBreakIndex(t, cfg)
	if _, err := openLoopsForQuery(ctx, cfg, "what is open with Sam Rivera"); err == nil {
		t.Fatal("openLoopsForQuery over a broken index should error")
	}
}

// TestMt_OpenLoopsForQueryNamedPersonNoLoops: a query that names a person WITHOUT
// open loops (while another person DOES have loops) yields no block for the named
// person — the len(loops)==0 skip.
func TestMt_OpenLoopsForQueryNamedPersonNoLoops(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, personMemNamed("s1", "gmail", "sam@a.com", "Sam Rivera", time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMemNamed("n1", "gmail", "neil@a.com", "Neil Patel", time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Only Sam has an open loop; the query names Neil (in the gazetteer, no loops).
	writeLiveTasks(t, cfg, "| Send Sam Rivera the contract | work | you | P1 | active | — | wk | 2026-06-10 |")

	out, err := openLoopsForQuery(ctx, cfg, "what is open with Neil Patel")
	if err != nil {
		t.Fatalf("openLoopsForQuery: %v", err)
	}
	for _, pl := range out {
		if pl.Person == "Neil Patel" {
			t.Fatalf("Neil (no open loops) should not surface a block: %+v", out)
		}
	}
}

// TestMt_EntityDisplayNameFallback: an id absent from the entities table falls back
// to the bare identity (person: prefix stripped).
func TestMt_EntityDisplayNameFallback(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "sam@a.com", "Sam Rivera", time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	db := openRO(t, cfg)
	defer db.Close()
	if got := entityDisplayName(ctx, db, "person:ghost@nowhere.com"); got != "ghost@nowhere.com" {
		t.Fatalf("entityDisplayName fallback = %q, want the bare identity", got)
	}
}
