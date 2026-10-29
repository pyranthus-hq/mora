package mora

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	configstore "github.com/pyranthus-hq/mora/internal/config"
)

// deleteFixture seeds a bound, rebuilt index with n memories and returns their
// ids plus a counter of scheduled authored reconciles (the seam is inert).
func deleteFixture(t *testing.T, n int) (Config, []string, *atomic.Int32) {
	t.Helper()
	var scheduled atomic.Int32
	env := &testEnv{configRoot: t.TempDir(), reconciler: func(context.Context, Config) error { scheduled.Add(1); return nil }}
	bindTestEnv(t, env)
	pinOperationClockForTest(t, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, err := configstore.LoadFrom(env.ctx())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cfg.VaultDir, cfg.DataDir, cfg.StateDir, cfg.ConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for i := 0; i < n; i++ {
		id := newID()
		ids = append(ids, id)
		if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "t", Source: "manual", CreatedAt: nowRFC3339(), Text: "zebrafinch [[Acme Corp]] " + id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, ids, &scheduled
}

func indexScalar(t *testing.T, cfg Config, q string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// plantRebuildSentinel adds a row only a whole-vault rebuild would wipe
// (rebuildIndex runs DELETE FROM mem_vectors), so its survival proves the
// delete did not rebuild.
func plantRebuildSentinel(t *testing.T, cfg Config) {
	t.Helper()
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO mem_vectors(memory_id, dim, model, vec) VALUES('sentinel', 1, 'x', x'00')`); err != nil {
		t.Fatal(err)
	}
}

func assertGone(t *testing.T, cfg Config, id string) {
	t.Helper()
	if n := indexScalar(t, cfg, `SELECT COUNT(*) FROM memories WHERE id=?`, id); n != 0 {
		t.Fatalf("memories still has a row for %s", id)
	}
	if n := indexScalar(t, cfg, `SELECT COUNT(*) FROM memories_fts WHERE id=?`, id); n != 0 {
		t.Fatalf("memories_fts still has a row for %s", id)
	}
	// Concurrent searches right after the delete must never see it.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := searchMemories(context.Background(), cfg, "zebrafinch", "", 20)
			if err != nil {
				t.Errorf("search: %v", err)
				return
			}
			for _, m := range res {
				if m.ID == id {
					t.Errorf("search returned deleted memory %s", id)
				}
			}
		}()
	}
	wg.Wait()
	if _, err := findMemory(cfg, id); err == nil {
		t.Fatalf("read path still finds deleted memory %s", id)
	}
}

// TestMCPDeleteIsTargetedNotFullRebuild pins V3: delete_memory must update the
// index with one small transaction and schedule the deferred reconcile, never
// run the whole-vault rebuild inline.
func TestMCPDeleteIsTargetedNotFullRebuild(t *testing.T) {
	cfg, ids, scheduled := deleteFixture(t, 3)
	plantRebuildSentinel(t, cfg)

	res, err := callMCPTool(testCtx(t), "delete_memory", map[string]any{"id": ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.(map[string]any)["deleted"]; got != ids[0] {
		t.Fatalf("deleted = %v, want %s", got, ids[0])
	}
	if n := indexScalar(t, cfg, `SELECT COUNT(*) FROM mem_vectors WHERE memory_id='sentinel'`); n != 1 {
		t.Fatal("sentinel wiped: delete_memory ran a whole-vault rebuild inline")
	}
	assertGone(t, cfg, ids[0])
	if got := indexCount(t, cfg); got != 2 {
		t.Fatalf("index count = %d, want 2 survivors", got)
	}
	// Reconcile scheduling runs on a goroutine; wait for it.
	waitForAuthoredReconciles(5 * time.Second)
	if scheduled.Load() != 1 {
		t.Fatalf("scheduled reconciles = %d, want 1", scheduled.Load())
	}
	// The marker stays until the reconcile's rebuild covers the graph/vector
	// lag, and keeps suppressing the id meanwhile.
	if !PendingDeleteSuppresses(cfg, ids[0]) {
		t.Fatal("pending-delete marker was dropped before the covering reconcile")
	}
}

// PendingDeleteSuppresses reports whether the pending-delete ledger suppresses id.
func PendingDeleteSuppresses(cfg Config, id string) bool {
	return len(suppressPendingDeletes(cfg, []Memory{{ID: id}})) == 0
}

func TestCLIDeleteRemovesRowAndKeepsReceipt(t *testing.T) {
	cfg, ids, _ := deleteFixture(t, 2)
	var out, errb bytes.Buffer
	if err := cmdDelete(testCtx(t), []string{"--yes", "--json", ids[0]}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"deleted": true`) || !strings.Contains(out.String(), ids[0]) {
		t.Fatalf("receipt = %q", out.String())
	}
	assertGone(t, cfg, ids[0])
}

// TestDeleteIndexFailureStaysLoud: if the targeted delete fails, the error is
// loud, the file is gone, and the pending-delete marker remains so reads keep
// suppressing the id.
func TestDeleteIndexFailureStaysLoud(t *testing.T) {
	cfg, ids, _ := deleteFixture(t, 2)
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER no_del BEFORE DELETE ON memories BEGIN SELECT RAISE(FAIL, 'forced delete failure'); END;`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = callMCPTool(testCtx(t), "delete_memory", map[string]any{"id": ids[0]})
	if err == nil || !strings.Contains(err.Error(), "mora index rebuild") {
		t.Fatalf("want loud error naming `mora index rebuild`, got %v", err)
	}
	if !PendingDeleteSuppresses(cfg, ids[0]) {
		t.Fatal("marker must remain after a failed index delete")
	}
	if res := gate2Search(t, cfg, "zebrafinch"); len(res) != 1 || res[0].ID != ids[1] {
		t.Fatalf("search must suppress the pending-delete id, got %+v", res)
	}
}

func indexExec(t *testing.T, cfg Config, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// TestMCPDeleteDropsDerivedProjections: a deleted memory must leave no graph,
// vector or commitment trace, because those reads (list_entities, get_entity,
// think's commitment snapshot) do not consult the file or the pending ledger.
func TestMCPDeleteDropsDerivedProjections(t *testing.T) {
	cfg, ids, _ := deleteFixture(t, 2)
	doomed := ids[0]
	// A real commitment is hard to provoke from prose; seed a row under the live
	// generation exactly as writeCommitments shapes it.
	indexExec(t, cfg, `INSERT INTO commitments(generation, row_key, commitment_id, memory_id, payload)
		SELECT value, 'rk_'||?, NULL, ?, '{}' FROM index_meta WHERE key='commitments_generation'`, doomed, doomed)
	ctx := testCtx(t)
	if snap, err := readCommitmentSnapshot(ctx, cfg); err != nil || len(snap.Commitments) != 1 {
		t.Fatalf("seed: snapshot = %+v, err %v", snap, err)
	}
	if n := indexScalar(t, cfg, `SELECT COUNT(*) FROM edges WHERE evidence_id=?`, doomed); n == 0 {
		t.Fatal("seed: expected graph edges for the memory")
	}
	if n := indexScalar(t, cfg, `SELECT COUNT(*) FROM mem_vectors WHERE memory_id=?`, doomed); n != 1 {
		t.Fatalf("seed: expected a vector, got %d", n)
	}

	if _, err := callMCPTool(ctx, "delete_memory", map[string]any{"id": doomed}); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{
		`SELECT COUNT(*) FROM edges WHERE evidence_id=? OR src='memory:'||?`,
		`SELECT COUNT(*) FROM entities WHERE id='memory:'||?`,
		`SELECT COUNT(*) FROM mem_vectors WHERE memory_id=?`,
		`SELECT COUNT(*) FROM commitments WHERE memory_id=?`,
	} {
		args := []any{doomed}
		if strings.Count(q, "?") == 2 {
			args = append(args, doomed)
		}
		if n := indexScalar(t, cfg, q, args...); n != 0 {
			t.Fatalf("%s left %d rows", q, n)
		}
	}
	if snap, err := readCommitmentSnapshot(ctx, cfg); err != nil || len(snap.Commitments) != 0 {
		t.Fatalf("commitment snapshot still shows the deleted memory: %+v, err %v", snap, err)
	}
	ents, err := graphListEntities(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		for _, id := range e.MemoryIDs {
			if id == doomed {
				t.Fatalf("list_entities entity %q still names the deleted memory", e.Name)
			}
		}
	}
	got, err := graphGetEntity(ctx, cfg, "Acme Corp")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got["memories"].([]Memory) {
		if m.ID == doomed {
			t.Fatal("get_entity still returns the deleted memory")
		}
	}
	for _, e := range got["edges"].([]map[string]any) {
		if e["evidence_id"] == doomed {
			t.Fatal("get_entity edges still cite the deleted memory")
		}
	}
}

// TestDeleteMarkersAreReconciled: a delete leaves a delete marker plus a
// rebuild marker stamped after the file went away; the reconciler retires both,
// and a rebuild that listed before the removal cannot.
func TestDeleteMarkersAreReconciled(t *testing.T) {
	cfg, ids, scheduled := deleteFixture(t, 3)
	ctx := testCtx(t)
	gonePath := mustMemoryPathByID(t, cfg, ids[0])
	if _, err := callMCPTool(ctx, "delete_memory", map[string]any{"id": ids[0]}); err != nil {
		t.Fatal(err)
	}
	waitForAuthoredReconciles(5 * time.Second)
	if scheduled.Load() != 1 {
		t.Fatalf("scheduled reconciles = %d, want 1", scheduled.Load())
	}
	ops, _ := listPendingOps(cfg)
	kinds := map[string]bool{}
	for _, op := range ops {
		kinds[op.Kind] = true
	}
	if !kinds[opKindDelete] || !kinds[opKindRebuild] {
		t.Fatalf("want delete + rebuild markers pending, got %+v", ops)
	}
	// A stale rebuild: listed before the file went away, so it parsed the path.
	stale := time.Now().Add(-time.Minute)
	if err := clearCoveredPendingOps(cfg, stale, []string{gonePath}, []string{gonePath}); err != nil {
		t.Fatal(err)
	}
	ops, _ = listPendingOps(cfg)
	if len(ops) != 1 || ops[0].Kind != opKindRebuild {
		t.Fatalf("a stale rebuild must retire only the delete marker, got %+v", ops)
	}
	// Reconciling (the real one, not the inert seam) retires what is left.
	if err := reconcileAuthoredWrites(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if ops, _ = listPendingOps(cfg); len(ops) != 0 {
		t.Fatalf("reconcile left markers pending: %+v", ops)
	}
	assertGone(t, cfg, ids[0])
}

// TestReconcileRetiresBareDeleteMarker: a delete marker alone (e.g. a racing
// delete whose worker lost the lease) is picked up by the reconciler.
func TestReconcileRetiresBareDeleteMarker(t *testing.T) {
	cfg, ids, _ := deleteFixture(t, 2)
	ctx := testCtx(t)
	p := mustMemoryPathByID(t, cfg, ids[0])
	if _, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: p, MemoryID: ids[0]}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := reconcileAuthoredWrites(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if ops, _ := listPendingOps(cfg); len(ops) != 0 {
		t.Fatalf("delete marker not reconciled: %+v", ops)
	}
	assertGone(t, cfg, ids[0])
}

func mustMemoryPathByID(t *testing.T, cfg Config, id string) string {
	t.Helper()
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	return m.Path
}
