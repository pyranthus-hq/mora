package mora

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	indexstore "github.com/pyranthus-hq/mora/internal/index"
	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
)

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func hasOp(t *testing.T, cfg Config, opID string) bool {
	t.Helper()
	for _, o := range mustListOps(t, cfg) {
		if o.OpID == opID {
			return true
		}
	}
	return false
}

func afterCommit(t *testing.T, hook func(Config)) {
	t.Helper()
	testHookRebuildCommitted = hook
	t.Cleanup(func() { testHookRebuildCommitted = nil })
}

// The live bug: rebuild A commits, B marks a delete and removes the file, and
// A's cleanup must not retire B's marker just because A parsed that path. If B
// then dies, only that marker keeps the indexed content suppressed.
func TestDeleteAfterCommitKeepsSuppression(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Late delete", "latedeletebody"))
	var op pendingOp
	afterCommit(t, func(c Config) {
		testHookRebuildCommitted = nil
		var err error
		if op, err = markIndexDirty(ctx, c, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID}); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(m.Path); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !hasOp(t, cfg, op.OpID) || !indexstore.PendingDeleteIDs(cfg)[m.ID] {
		t.Fatal("a delete marked after the commit lost its suppression")
	}
	if res := gate2Search(t, cfg, "latedeletebody"); len(res) != 0 {
		t.Fatalf("deleted content is searchable: %+v", res)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, op.OpID) {
		t.Fatal("the next rebuild did not retire the delete marker")
	}
}

func TestMarkerAfterListingSurvives(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Late write", "latewritebody"))
	var op pendingOp
	orig := listRebuildFiles
	t.Cleanup(func() { listRebuildFiles = orig })
	listRebuildFiles = func(c Config) ([]string, error) {
		files, err := orig(c)
		if err == nil {
			op, err = markIndexDirty(ctx, c, pendingOp{Kind: opKindWrite, Path: m.Path})
		}
		return files, err
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	listRebuildFiles = orig
	if !hasOp(t, cfg, op.OpID) {
		t.Fatal("a marker made after listing was retired by that rebuild")
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, op.OpID) {
		t.Fatal("the next rebuild did not retire the marker")
	}
}

func TestRewriteDuringRebuildKeepsMarker(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Rewrite", "rewritebody"))
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	next := bytes.Replace(raw, []byte("rewritebody"), []byte("rewrittenbody"), 1)
	op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: m.Path, ContentSHA256: hashOf(next), OwnerPID: deadPID(t)})
	if err != nil {
		t.Fatal(err)
	}
	// The rebuild parsed the old bytes; the new bytes land before cleanup. A dead
	// owner must not matter: the file proves the write landed unindexed.
	afterCommit(t, func(Config) {
		testHookRebuildCommitted = nil
		if err := os.WriteFile(m.Path, next, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !hasOp(t, cfg, op.OpID) {
		t.Fatal("a rewrite that landed after parsing lost its marker")
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, op.OpID) {
		t.Fatal("the rebuild that parsed the marker's hash did not retire it")
	}
}

func TestOrphanWriteMarkerRetires(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	never := filepath.Join(memoriesRoot(cfg), "global", "never.md")
	dead, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: never, ContentSHA256: hashOf([]byte("x")), OwnerPID: deadPID(t)})
	if err != nil {
		t.Fatal(err)
	}
	live, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: never, ContentSHA256: hashOf([]byte("x")), OwnerPID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, dead.OpID) {
		t.Fatal("an orphan marker with a dead owner did not retire")
	}
	if !hasOp(t, cfg, live.OpID) {
		t.Fatal("a marker whose owner may still publish was retired")
	}
}

func TestUnparseableFileKeepsMarker(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	broken := filepath.Join(memoriesRoot(cfg), "global", "broken.md")
	body := []byte("this is not valid frontmatter\n")
	if err := os.WriteFile(broken, body, 0o644); err != nil {
		t.Fatal(err)
	}
	op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: broken, ContentSHA256: hashOf([]byte("other")), OwnerPID: deadPID(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !hasOp(t, cfg, op.OpID) {
		t.Fatal("a listed file that failed to parse lost its marker")
	}
}

func TestLegacyMarkerStillClears(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Legacy", "legacybody"))
	write, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: m.Path})
	if err != nil {
		t.Fatal(err)
	}
	del, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, write.OpID) || hasOp(t, cfg, del.OpID) {
		t.Fatal("hashless markers must keep today's clear-if-parsed rule")
	}
}

// The deleter is paused after marking hash A while a connector publishes B and a
// rebuild indexes B. If that rebuild clears the marker, the deleter's later unlink
// leaves B searchable with nothing suppressing it.
func TestLiveDeleteKeepsMarkerUntilUnlinked(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Paused delete", "pausedbody"))
	op, err := markIndexDirty(ctx, cfg, deleteOp(m))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, bytes.Replace(raw, []byte("pausedbody"), []byte("reingestedbody"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !hasOp(t, cfg, op.OpID) {
		t.Fatal("a rebuild retired a delete whose owner is alive and has not unlinked")
	}
	if err := os.Remove(m.Path); err != nil {
		t.Fatal(err)
	}
	if err := markFTSCommitted(cfg, op); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, op.OpID) {
		t.Fatal("a rebuild after the recorded unlink did not retire the marker")
	}
}

// The unlink is recorded, then identical bytes come back. A live owner must not
// hide the restored memory until that process exits.
func TestRestoredFileAfterUnlinkRetires(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Restored", "restoredbody"))
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	op, err := markIndexDirty(ctx, cfg, deleteOp(m))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.Path); err != nil {
		t.Fatal(err)
	}
	if err := markFTSCommitted(cfg, op); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, op.OpID) {
		t.Fatal("a restored file stayed suppressed behind a live owner's marker")
	}
	if res := gate2Search(t, cfg, "restoredbody"); len(res) != 1 {
		t.Fatalf("the restored memory is not searchable: %+v", res)
	}
}

// Dead owner, file never removed, committed parse and disk both equal the marker:
// the delete never happened, so the marker retires and the memory returns.
func TestAbandonedDeleteRetires(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Abandoned", "abandonedbody"))
	stuck := deleteOp(m)
	stuck.OwnerPID = deadPID(t)
	op, err := markIndexDirty(ctx, cfg, stuck)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if hasOp(t, cfg, op.OpID) {
		t.Fatal("an abandoned delete marker pinned the index dirty")
	}
	if res := gate2Search(t, cfg, "abandonedbody"); len(res) != 1 {
		t.Fatalf("the memory did not return to search: %+v", res)
	}
}

// The real delete paths record the unlink, or a live server's marker never clears.
func TestDeletePathsRecordUnlink(t *testing.T) {
	for name, del := range map[string]func(*testing.T, Config, string) error{
		"delete_memory": func(t *testing.T, _ Config, id string) error {
			_, err := callMCPTool(testCtx(t), "delete_memory", map[string]any{"id": id})
			return err
		},
		"cmdDelete": func(t *testing.T, _ Config, id string) error {
			var buf bytes.Buffer
			return cmdDelete(testCtx(t), []string{"--yes", id}, &buf, testStderr)
		},
		"forget": func(t *testing.T, _ Config, id string) error {
			var buf bytes.Buffer
			return cmdForget(testCtx(t), []string{"--yes", "--chat", id}, &buf, testStderr)
		},
	} {
		subRun(t, name, func(t *testing.T) {
			cfg := gate2Vault(t, coreBIdxmem("mem_del", "global", "insight", "Doomed", "doomedbody"), coreBIdxmem("mem_keep", "global", "insight", "Kept", "keptbody"))
			if err := del(t, cfg, "mem_del"); err != nil {
				t.Fatal(err)
			}
			waitForAuthoredReconciles(5 * time.Second)
			for _, o := range mustListOps(t, cfg) {
				if o.Kind == opKindDelete && !o.Unlinked {
					t.Fatalf("delete marker has no recorded unlink: %+v", o)
				}
			}
			if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
				t.Fatal(err)
			}
			if ops := mustListOps(t, cfg); len(ops) != 0 {
				t.Fatalf("markers left after a covering rebuild: %+v", ops)
			}
		})
	}
}

// One rebuild reads one ledger: a commitments pass that reloads would fail here.
func TestWriteCommitmentsUsesPassedGovernance(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := governanceStore(cfg).Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a ledger"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := writeCommitments(ctx, tx, "gen", nil, cfg, time.Unix(0, 0), g); err != nil {
		t.Fatalf("writeCommitments reloaded the ledger: %v", err)
	}
}

// Each publishing path must stamp the bytes it publishes or removes, or every
// marker silently takes the hashless rule.
func TestMarkersCarryPublishedHash(t *testing.T) {
	captureOp := func(t *testing.T, cfg Config, kind string) *pendingOp {
		t.Helper()
		var got pendingOp
		testHookPostMarkerWrite = func() {
			for _, o := range mustListOps(t, cfg) {
				if o.Kind == kind && got.OpID == "" {
					got = o
				}
			}
		}
		t.Cleanup(func() { testHookPostMarkerWrite = nil })
		return &got
	}
	check := func(t *testing.T, op *pendingOp, want []byte) {
		t.Helper()
		if op.ContentSHA256 != hashOf(want) || op.OwnerPID != os.Getpid() {
			t.Fatalf("marker %+v, want hash %s and pid %d", op, hashOf(want), os.Getpid())
		}
	}
	subRun(t, "createMemory", func(t *testing.T) {
		cfg := gate2Vault(t)
		op := captureOp(t, cfg, opKindWrite)
		m, _, err := createMemory(testCtx(t), cfg, coreBIdxmem("", "global", "insight", "Hashed", "hashedbody"))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(m.Path)
		check(t, op, b)
	})
	subRun(t, "confirm", func(t *testing.T) {
		cfg := gate2Vault(t)
		m, _, err := createMemory(testCtx(t), cfg, coreBIdxmem("", "project:x", "insight", "Tent", "tentbody"), func(o *createOptions) { o.tentative = true })
		if err != nil {
			t.Fatal(err)
		}
		op := captureOp(t, cfg, opKindWrite)
		for _, o := range mustListOps(t, cfg) {
			_ = unmarkIndexDirty(cfg, o.OpID)
		}
		var buf bytes.Buffer
		if err := cmdConfirm(testCtx(t), []string{m.ID}, &buf, testStderr); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(m.Path)
		check(t, op, b)
	})
	deletes := map[string]func(t *testing.T, id string){
		"cmdDelete": func(t *testing.T, id string) {
			var buf bytes.Buffer
			if err := cmdDelete(testCtx(t), []string{"--yes", id}, &buf, testStderr); err != nil {
				t.Fatal(err)
			}
		},
		"delete_memory": func(t *testing.T, id string) {
			if _, err := callMCPTool(testCtx(t), "delete_memory", map[string]any{"id": id}); err != nil {
				t.Fatal(err)
			}
			waitForAuthoredReconciles(5 * time.Second)
		},
		"forget": func(t *testing.T, id string) {
			var buf bytes.Buffer
			if err := cmdForget(testCtx(t), []string{"--yes", "--chat", id}, &buf, testStderr); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, del := range deletes {
		subRun(t, name, func(t *testing.T) {
			cfg := gate2Vault(t, coreBIdxmem("mem_del", "global", "insight", "Doomed", "doomedbody"), coreBIdxmem("mem_keep", "global", "insight", "Kept", "keptbody"))
			path := filepath.Join(memoriesRoot(cfg), "global", "mem_del.md")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			op := captureOp(t, cfg, opKindDelete)
			del(t, "mem_del")
			check(t, op, want)
		})
	}
}

// A connector publishes after the rebuild listed: its journal line is the only
// record, so it must outlive this rebuild even though the path was listed.
func TestJournalLineAfterListingSurvivesRebuild(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := testCtx(t)
	m := gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Journaled", "journalbody"))
	jp := ingestpkg.JournalPath(cfg, "gmail")
	if err := os.MkdirAll(filepath.Dir(jp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jp, []byte("run r_"+"abc 2026-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := listRebuildFiles
	t.Cleanup(func() { listRebuildFiles = orig })
	listRebuildFiles = func(c Config) ([]string, error) {
		files, err := orig(c)
		if err == nil {
			f, oerr := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0o644)
			if oerr != nil {
				return nil, oerr
			}
			defer f.Close()
			_, err = f.WriteString(cleanVaultPath(m.Path) + "\n")
		}
		return files, err
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	listRebuildFiles = orig
	if _, n, present := ingestpkg.ScanJournal(jp); !present || n != 1 {
		t.Fatalf("journal after rebuild: present=%v lines=%d, want the late line kept", present, n)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, _, present := ingestpkg.ScanJournal(jp); present {
		t.Fatal("the next rebuild did not retire the covered journal")
	}
}
