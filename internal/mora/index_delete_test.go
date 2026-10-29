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
		if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "t", Source: "manual", CreatedAt: nowRFC3339(), Text: "zebrafinch " + id}); err != nil {
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
