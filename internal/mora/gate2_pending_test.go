package mora

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
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

// govFingerprint captures a snapshot of the governance ledger — total entries and
// how many are revoked — so a test can prove the ledger had NOT yet been mutated at a
// given instant. An append changes the entry count; a revoke changes the revoked
// count. Every A5-row-7/8 governance mutation is an index input via writeGraph.
func govFingerprint(t *testing.T, cfg Config) (entries, revoked int) {
	t.Helper()
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatalf("loadGovernance: %v", err)
	}
	for _, e := range g.Entries {
		entries++
		if e.revoked() {
			revoked++
		}
	}
	return entries, revoked
}

// TestEveryVaultMutationMarksDirty (matrix row 1 + the A5 registry completeness
// meta-test) — every registered mutation marks the index dirty BEFORE its mutation
// becomes visible, driven through the REAL dispatcher (not a hand-run helper). Each
// subtest reddens when markIndexDirty is dropped from that site: the mark-before-
// visible observation is captured at the FIRST durable-marker write, so removing a
// site's own mark leaves only the rebuild's A4 self-mark, which fires AFTER the
// vault/ledger byte already changed.
func TestEveryVaultMutationMarksDirty(t *testing.T) {
	// captureFirstMark installs a testHookPostMarkerWrite that runs `record` exactly
	// once, at the first markIndexDirty of the dispatch. Returns a restore func.
	captureFirstMark := func(record func()) func() {
		fired := false
		testHookPostMarkerWrite = func() {
			if fired {
				return
			}
			fired = true
			record()
		}
		return func() { testHookPostMarkerWrite = nil }
	}

	t.Run("write_marks_before_file_exists", func(t *testing.T) {
		cfg := gate2Vault(t)
		ctx := context.Background()
		var sawOpBeforeFile bool
		restore := captureFirstMark(func() {
			ops, _ := listPendingOps(cfg)
			sawOpBeforeFile = len(ops) > 0
		})
		defer restore()
		_, op, err := createMemory(ctx, cfg, coreBIdxmem("", "global", "insight", "Marked", "markbody"))
		if err != nil {
			t.Fatal(err)
		}
		if !sawOpBeforeFile {
			t.Fatal("createMemory did not mark a pending op before the vault write (row 1 / A5 row 1)")
		}
		_ = unmarkIndexDirty(cfg, op.OpID)
	})

	t.Run("delete_marks_before_removal", func(t *testing.T) {
		// Drives the REAL cmdDelete. MUTATION: drop cmdDelete's markIndexDirty => the
		// first mark is then the rebuild's A4 self-mark, which fires AFTER os.Remove =>
		// the file is already gone at capture => RED. (A5 row 4.)
		cfg := gate2Vault(t, coreBIdxmem("mem_del", "global", "insight", "Doomed", "deletemebody"))
		target := filepath.Join(memoriesRoot(cfg), "global", "mem_del.md")
		var fileStillPresentAtMark, sawDeleteOp bool
		restore := captureFirstMark(func() {
			_, statErr := os.Stat(target)
			fileStillPresentAtMark = statErr == nil
			for _, o := range mustListOps(t, cfg) {
				if o.Kind == opKindDelete && o.MemoryID == "mem_del" {
					sawDeleteOp = true
				}
			}
		})
		defer restore()
		var buf bytes.Buffer
		if err := cmdDelete(context.Background(), []string{"--yes", "mem_del"}, &buf); err != nil {
			t.Fatal(err)
		}
		if !fileStillPresentAtMark || !sawDeleteOp {
			t.Fatalf("cmdDelete did not mark a delete op before the file removal (filePresent=%v, deleteOp=%v)", fileStillPresentAtMark, sawDeleteOp)
		}
	})

	t.Run("unforget_marks_before_ledger_change", func(t *testing.T) {
		// Drives the REAL cmdUnforget. It revokes a governance entry (an index input),
		// so it must mark BEFORE the revoke. MUTATION: drop cmdUnforget's markIndexDirty
		// => the first mark is the rebuild's A4 self-mark AFTER the revoke => the ledger
		// fingerprint already changed at capture => RED. (A5 row 7.)
		cfg := gate2Vault(t)
		seed, err := appendGovernanceEntry(cfg, govEntry{Kind: govKindForget, Action: govActionSuppress, Atom: govAtom{Kind: atomStableID, Value: "gmail_thread_z"}, Reason: "seed"})
		if err != nil {
			t.Fatal(err)
		}
		baseE, baseR := govFingerprint(t, cfg)
		var ledgerUnchangedAtMark bool
		restore := captureFirstMark(func() {
			e, r := govFingerprint(t, cfg)
			ledgerUnchangedAtMark = e == baseE && r == baseR
		})
		defer restore()
		var buf bytes.Buffer
		if err := cmdUnforget(context.Background(), []string{"--yes", seed.ID}, &buf); err != nil {
			t.Fatal(err)
		}
		if !ledgerUnchangedAtMark {
			t.Fatal("cmdUnforget mutated the governance ledger before marking the index dirty (A5 row 7)")
		}
	})

	t.Run("brief_correct_marks_before_ledger_change", func(t *testing.T) {
		// Drives the REAL cmdBriefCorrect (A5 row 7). MUTATION: drop its markIndexDirty
		// => the append lands before the first (A4) mark => RED.
		cfg := gate2Vault(t, coreBIdxmem("mem_cite", "global", "insight", "Cited", "citebody"))
		baseE, baseR := govFingerprint(t, cfg)
		var ledgerUnchangedAtMark bool
		restore := captureFirstMark(func() {
			e, r := govFingerprint(t, cfg)
			ledgerUnchangedAtMark = e == baseE && r == baseR
		})
		defer restore()
		var buf bytes.Buffer
		if err := cmdBriefCorrect(context.Background(), []string{"--memory-id", "mem_cite", "--attendee", "person@example.com", "--confirm"}, &buf); err != nil {
			t.Fatal(err)
		}
		if !ledgerUnchangedAtMark {
			t.Fatal("cmdBriefCorrect appended to the governance ledger before marking the index dirty (A5 row 7)")
		}
	})

	t.Run("merge_marks_before_ledger_change", func(t *testing.T) {
		// Drives the REAL mergeDecide (A5 row 8). MUTATION: drop its markIndexDirty =>
		// the append lands before the first (A4) mark => RED.
		cfg := gate2Vault(t)
		baseE, baseR := govFingerprint(t, cfg)
		var ledgerUnchangedAtMark bool
		restore := captureFirstMark(func() {
			e, r := govFingerprint(t, cfg)
			ledgerUnchangedAtMark = e == baseE && r == baseR
		})
		defer restore()
		var buf bytes.Buffer
		if err := mergeDecide(context.Background(), []string{"--handle", "+14155550123", "--email", "person@example.com", "--yes"}, &buf, mergeDecisionConfirm); err != nil {
			t.Fatal(err)
		}
		if !ledgerUnchangedAtMark {
			t.Fatal("mergeDecide appended to the governance ledger before marking the index dirty (A5 row 8)")
		}
	})

	t.Run("connector_ingest_journals_before_publish", func(t *testing.T) {
		// Drives the REAL writeMappedMemory (A5 row 2). The durable journal header is
		// the mark-before-visible for a connector publish; it must exist BEFORE the file
		// is published. MUTATION: drop ensureIngestJournalHeader from writeMappedMemory
		// => at publish time no journal exists => RED.
		cfg := gate2Vault(t)
		var journalPresentAtPublish bool
		testHookPostConnectorPublish = func() {
			dirty, _, _, _ := ingestJournalStatus(cfg)
			journalPresentAtPublish = dirty
		}
		defer func() { testHookPostConnectorPublish = nil }()
		mm := memory.MappedMemory{
			StableID: "gmail_thread_mark", Scope: "global", Type: "email", Title: "M",
			Body: "markbody", Source: "gmail", Provider: "gmail",
			CreatedAt: nowRFC3339(), ContentHash: "hash_mark",
		}
		if err := writeMappedMemory(cfg, mm); err != nil {
			t.Fatal(err)
		}
		if !journalPresentAtPublish {
			t.Fatal("writeMappedMemory published a connector memory before writing the durable journal header (A5 row 2)")
		}
	})
}

// mustListOps is a fatal-on-error listPendingOps for tests.
func mustListOps(t *testing.T, cfg Config) []pendingOp {
	t.Helper()
	ops, err := listPendingOps(cfg)
	if err != nil {
		t.Fatalf("listPendingOps: %v", err)
	}
	return ops
}

// TestFailedUpsertLeavesIndexDirty (matrix row 2) — an upsert that fails leaves the
// op in place, so the index is not fresh. This drives the REAL cmdWrite dispatcher
// (not a hand-run createMemory+indexUpsert), so MUTATION: moving cmdWrite's
// unmarkIndexDirty BEFORE indexUpsert (clear-then-commit) retires the op before the
// blocked upsert => 0 surviving ops => index reads fresh => RED.
func TestFailedUpsertLeavesIndexDirty(t *testing.T) {
	cfg := gate2Vault(t)
	// Force the identity guard to block the upsert: bind the index to a DIFFERENT id
	// than the marker/config, so cmdWrite's indexUpsert returns errRebuildBlocked and
	// takes its degraded-success path (warn, exit 0, op REMAINS).
	idxUpsertStampVaultID(t, cfg, "v_someone_else")

	var buf bytes.Buffer
	if err := cmdWrite(context.Background(), []string{"--title", "Blocked", "--text", "blockedbody"}, &buf); err != nil {
		t.Fatalf("cmdWrite should degrade-succeed on a blocked upsert, got %v", err)
	}
	ops, _ := listPendingOps(cfg)
	writeOps := 0
	for _, o := range ops {
		if o.Kind == opKindWrite {
			writeOps++
		}
	}
	if writeOps != 1 {
		t.Fatalf("pending write ops after a failed upsert = %d, want 1 (the op must survive a failed upsert; MUTATION clears it before commit)", writeOps)
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
