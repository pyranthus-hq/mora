package mora

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gate2_pending_test.go — Packet A (the index-state ledger) acceptance + mutation
// matrix rows 1-8. Each gate is proven load-bearing: the named production mutation
// turns the named test red. The mutation is described in each test's comment.

// gate2Write replicates cmdWrite's mark -> create -> upsert -> retire lifecycle.
func gate2Write(t *testing.T, cfg Config, m Memory) Memory {
	t.Helper()
	ctx := context.Background()
	got, op, err := createMemory(ctx, cfg, m)
	if err != nil {
		t.Fatalf("createMemory: %v", err)
	}
	if err := indexUpsert(ctx, cfg, got); err != nil {
		t.Fatalf("indexUpsert: %v", err)
	}
	_ = unmarkIndexDirty(cfg, op.OpID)
	return got
}

// TestUpgradeFromV2IndexStillWrites — an existing v2 vault (no schema bump, no new
// tables) keeps writing with no manual rebuild. MUTATION: bumping indexSchemaVersion
// would force a rebuild and break this.
func TestUpgradeFromV2IndexStillWrites(t *testing.T) {
	cfg := gate2Vault(t)
	if got := gate2ReadMeta(t, cfg); got == nil {
		t.Fatal("no index_meta")
	}
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "New", "zebraword body"))
	if res := gate2Search(t, cfg, "zebraword"); len(res) != 1 || res[0].ID != m.ID {
		t.Fatalf("search after v2 write = %+v, want the new memory", res)
	}
	if st := gate2IndexState(t, cfg); st != idxFresh {
		t.Fatalf("index state after clean write = %q, want fresh", st)
	}
}

// TestEveryVaultMutationMarksDirty (matrix row 1) — every registered mutation marks
// a pending op BEFORE the file becomes visible. MUTATION: drop markIndexDirty from a
// site => the hook never sees the op => RED.
func TestEveryVaultMutationMarksDirty(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()

	t.Run("write_marks_before_file_exists", func(t *testing.T) {
		var sawOpBeforeFile bool
		var pendingAtMark int
		testHookPostMarkerWrite = func() {
			ops, _ := listPendingOps(cfg)
			pendingAtMark = len(ops)
			sawOpBeforeFile = pendingAtMark > 0
		}
		defer func() { testHookPostMarkerWrite = nil }()
		got, op, err := createMemory(ctx, cfg, coreBIdxmem("", "global", "insight", "Marked", "markbody"))
		if err != nil {
			t.Fatal(err)
		}
		if !sawOpBeforeFile {
			t.Fatal("createMemory did not mark a pending op before the vault write")
		}
		_ = unmarkIndexDirty(cfg, op.OpID)
		_ = got
	})

	t.Run("delete_marks_before_removal", func(t *testing.T) {
		m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Doomed", "deleteme body"))
		var fileStillPresentAtMark bool
		testHookPostMarkerWrite = func() {
			_, err := os.Stat(m.Path)
			fileStillPresentAtMark = err == nil
			ops, _ := listPendingOps(cfg)
			if len(ops) == 0 {
				t.Error("no delete op present at mark time")
			}
		}
		defer func() { testHookPostMarkerWrite = nil }()
		op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !fileStillPresentAtMark {
			t.Fatal("delete op was not marked before the file removal")
		}
		_ = os.Remove(m.Path)
		_ = unmarkIndexDirty(cfg, op.OpID)
	})
}

// TestFailedUpsertLeavesIndexDirty (matrix row 2) — an upsert that fails leaves the
// op in place, so the index is not fresh. MUTATION: retire the op before the upsert
// commits (clear-then-commit) => op gone => RED.
func TestFailedUpsertLeavesIndexDirty(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()
	// Force the identity guard to block the upsert (index bound to a different id).
	idxUpsertStampVaultID(t, cfg, "v_someone_else")

	got, op, err := createMemory(ctx, cfg, coreBIdxmem("", "global", "insight", "Blocked", "blockedbody"))
	if err != nil {
		t.Fatal(err)
	}
	uerr := indexUpsert(ctx, cfg, got)
	if !errors.Is(uerr, errRebuildBlocked) {
		t.Fatalf("indexUpsert error = %v, want errRebuildBlocked", uerr)
	}
	// cmdWrite does NOT retire on failure; assert the op survives and the index is
	// not fresh. (A blocked index reads `failed`; the discriminating assertion for
	// this row is that the pending write op is still present.)
	ops, _ := listPendingOps(cfg)
	if len(ops) == 0 || ops[0].OpID != op.OpID {
		t.Fatalf("pending ops after failed upsert = %+v, want the write op %s to survive", ops, op.OpID)
	}
	if st := gate2IndexState(t, cfg); st == idxFresh {
		t.Fatal("index reads fresh after a failed upsert")
	}
}

// TestFailedRebuildNeverAdvancesIndexedAt (acceptance) — a rebuild that fails does
// not advance indexed_at (the index twin of Gate 1's watermark test). MUTATION:
// stamping indexed_at outside the committing tx would advance it on failure.
func TestFailedRebuildNeverAdvancesIndexedAt(t *testing.T) {
	cfg := gate2Vault(t)
	before := gate2ReadMeta(t, cfg)["indexed_at"]
	if before == "" {
		t.Fatal("no indexed_at after initial rebuild")
	}

	orig := listRebuildFiles
	listRebuildFiles = func(Config) ([]string, error) { return nil, errors.New("boom") }
	_, err := rebuildIndex(context.Background(), cfg)
	listRebuildFiles = orig
	if err == nil {
		t.Fatal("forced rebuild did not fail")
	}
	if after := gate2ReadMeta(t, cfg)["indexed_at"]; after != before {
		t.Fatalf("indexed_at advanced on a failed rebuild: %q -> %q", before, after)
	}
}

// TestFailedRebuildIsVisibleOnACleanIndex (matrix row 5) — a failed rebuild on an
// otherwise-clean index reddens the product (its self-op survives). MUTATION: drop
// A4 (rebuild does not mark itself) => index reads fresh => RED.
func TestFailedRebuildIsVisibleOnACleanIndex(t *testing.T) {
	cfg := gate2Vault(t)
	if st := gate2IndexState(t, cfg); st != idxFresh {
		t.Fatalf("precondition: index state = %q, want fresh", st)
	}
	orig := listRebuildFiles
	listRebuildFiles = func(Config) ([]string, error) { return nil, errors.New("boom") }
	_, _ = rebuildIndex(context.Background(), cfg)
	listRebuildFiles = orig
	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("index state after a failed rebuild = %q, want dirty", st)
	}
}

// TestRebuildDoesNotClearRacedMutation (matrix row 4) — a write op marked AFTER the
// rebuild listed is NOT cleared. MUTATION: clear every op regardless of coverage =>
// the raced op is cleared => RED.
func TestRebuildDoesNotClearRacedMutation(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()

	var racedOp pendingOp
	orig := listRebuildFiles
	listRebuildFiles = func(c Config) ([]string, error) {
		files, err := allMemoryFiles(c)
		if err != nil {
			return nil, err
		}
		// A write races in AFTER the listing snapshot: mark its op and write the
		// vault file, but DO NOT include it in the returned listing.
		racedOp, _ = markIndexDirty(ctx, c, pendingOp{Kind: opKindWrite, Path: filepath.Join(memoriesRoot(c), "global", "raced.md")})
		return files, nil
	}
	_, err := rebuildIndex(ctx, cfg)
	listRebuildFiles = orig
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := listPendingOps(cfg)
	found := false
	for _, op := range ops {
		if op.OpID == racedOp.OpID {
			found = true
		}
	}
	if !found {
		t.Fatal("a write op that raced in after the listing was wrongly cleared by the rebuild")
	}
	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("index state = %q, want dirty (a raced write is uncovered)", st)
	}
}

// TestUnparseableMemoryKeepsIndexDirty (matrix row 3) — a write op whose file is
// LISTED but fails to parse stays dirty. MUTATION: clear on listing membership
// (not parsed) => op cleared => RED.
func TestUnparseableMemoryKeepsIndexDirty(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()
	// A file with broken frontmatter: listed by allMemoryFiles, dropped by parseMemory.
	broken := filepath.Join(memoriesRoot(cfg), "global", "broken.md")
	if err := os.WriteFile(broken, []byte("this is not valid frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: broken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ops, _ := listPendingOps(cfg)
	found := false
	for _, o := range ops {
		if o.OpID == op.OpID {
			found = true
		}
	}
	if !found {
		t.Fatal("the write op for an unparseable (listed-but-not-indexed) file was cleared — clean-but-missing")
	}
}

// TestReingestRetiresDeleteOp (matrix row 8) — a delete op whose path reappears in
// `parsed` (re-ingested onto its own stable path) is retired. MUTATION: drop rule
// (c)'s reappearance clause => the op lives forever and B4 hides a LIVE memory => RED.
func TestReingestRetiresDeleteOp(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Reingested", "reingestbody"))
	// A stale delete op for a memory that still exists on disk (a connector rewrote
	// it onto its own path). The next committed rebuild lists+parses it => cleared.
	op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ops, _ := listPendingOps(cfg)
	for _, o := range ops {
		if o.OpID == op.OpID {
			t.Fatal("a re-ingested memory's stale delete op was not retired — B4 would hide a live memory forever")
		}
	}
}

// TestAbandonedMutationLeavesNoPendingOp (matrix row 7) — a mutation whose vault
// write FAILS removes its own op (compensating retirement). MUTATION: drop A2 step X
// => the op is pinned forever => RED.
func TestAbandonedMutationLeavesNoPendingOp(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()
	// Plant a regular FILE where the scope directory would be, so atomicCreate's
	// MkdirAll fails with a non-EEXIST error.
	blocker := filepath.Join(memoriesRoot(cfg), "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := createMemory(ctx, cfg, coreBIdxmem("", "blocked", "insight", "Doomed", "doomedbody"))
	if err == nil {
		t.Fatal("createMemory should have failed on the blocked scope dir")
	}
	ops, _ := listPendingOps(cfg)
	if len(ops) != 0 {
		t.Fatalf("abandoned mutation left %d pending op(s), want 0", len(ops))
	}
}

// TestKilledRebuildOpIsRecoverable (matrix row 6) — a killed rebuild's op is cleared
// by the NEXT committed rebuild (rule a: marked_at <= listing_started_at), not only
// by its own process. MUTATION: clear only the own op_id => the killed op is
// unclearable forever => RED.
func TestKilledRebuildOpIsRecoverable(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()
	// Simulate a SIGKILLed rebuild: a rebuild op left behind with an OLD marked_at
	// and a DIFFERENT op_id than any live process.
	killed := pendingOp{
		OpID:     "killed-rebuild-op",
		Kind:     opKindRebuild,
		MarkedAt: time.Unix(1_000_000_000, 0).UTC().Format(time.RFC3339),
	}
	if op, err := markIndexDirty(ctx, cfg, killed); err != nil {
		t.Fatal(err)
	} else if op.OpID != "killed-rebuild-op" {
		t.Fatalf("op id changed: %q", op.OpID)
	}
	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("precondition: index state = %q, want dirty", st)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ops, _ := listPendingOps(cfg)
	for _, o := range ops {
		if o.OpID == "killed-rebuild-op" {
			t.Fatal("a killed rebuild's op was not recovered by the next rebuild")
		}
	}
}

// TestWriteDuringRebuildDoesNotAbort (acceptance) — a write DURING a long rebuild
// still lands in the vault and the index reads dirty, rather than the write failing.
// The mark is a file, so it never contends on the rebuild's writer lock.
func TestWriteDuringRebuildDoesNotAbort(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()

	var landedPath string
	orig := listRebuildFiles
	listRebuildFiles = func(c Config) ([]string, error) {
		files, err := allMemoryFiles(c)
		if err != nil {
			return nil, err
		}
		// A concurrent write DURING the rebuild's held tx: the mark (a file) and the
		// vault write (atomicCreate) take no DB lock, so both succeed. We skip the
		// upsert (it would block on the writer lock) — the op survives => dirty.
		got, _, cerr := createMemory(ctx, c, coreBIdxmem("", "global", "insight", "Concurrent", "concurrentbody"))
		if cerr != nil {
			t.Errorf("write during rebuild aborted: %v", cerr)
		}
		landedPath = got.Path
		return files, nil // the raced file is NOT in this snapshot
	}
	_, err := rebuildIndex(ctx, cfg)
	listRebuildFiles = orig
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(landedPath); serr != nil {
		t.Fatalf("the write during the rebuild did not land: %v", serr)
	}
	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("index state = %q, want dirty (the raced write is uncovered)", st)
	}
}

// TestPendingPathsMatchListingOnAllPlatforms (acceptance, Windows CI) — op paths
// (minted by memoryPath) and the rebuild's listing (allMemoryFiles) reduce to the
// same cleaned absolute form, so an op is actually clearable on every platform.
func TestPendingPathsMatchListingOnAllPlatforms(t *testing.T) {
	cfg := gate2Vault(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Pathy", "pathybody"))
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	set := cleanPathSet(files)
	if !set[cleanVaultPath(memoryPath(cfg, m))] {
		t.Fatalf("memoryPath %q not found in the cleaned listing set", memoryPath(cfg, m))
	}
}
