package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// These tests pin the four defects behind "index rebuild STALLED" and "a fresh
// write is not searchable until some later rebuild" on a vault with many
// concurrent agent sessions (each one a `mora mcp serve` process).

// liveReconciler swaps the test's inert reconciler for the real one and counts
// how many runs are in flight, so a test can wait for every scheduled
// background reconcile to finish before it asserts.
func liveReconciler(t *testing.T) (wait func(), calls *atomic.Int64) {
	t.Helper()
	env := lookupTestEnv(t)
	if env == nil {
		t.Fatal("liveReconciler needs a bound test env")
	}
	var wg sync.WaitGroup
	calls = new(atomic.Int64)
	env.reconciler = func(ctx context.Context, cfg Config) error {
		wg.Add(1)
		defer wg.Done()
		calls.Add(1)
		return reconcileAuthoredWrites(ctx, cfg)
	}
	return wg.Wait, calls
}

// waitForReconciles polls until no reconcile is running and none has started
// for a short quiet window. Scheduling is asynchronous, so a WaitGroup alone
// can return before the last scheduled goroutine has called Add.
func waitForReconciles(t *testing.T, wait func(), calls *atomic.Int64) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		before := calls.Load()
		time.Sleep(300 * time.Millisecond)
		wait()
		if calls.Load() == before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background reconciles never went quiet")
		}
	}
}

// seedForeignWriteMarker publishes a write marker whose path lies outside the
// vault, the shape found in the live vault on 2026-10-28: a process run with
// MORA_VAULT pointed at a temp vault while state_dir still named the real one.
func seedForeignWriteMarker(t *testing.T, cfg Config) string {
	t.Helper()
	foreign := filepath.Join(t.TempDir(), "v", "memories", "personal", "mem_foreign.md")
	op := pendingOp{OpID: "mem_foreign_marker", Kind: opKindWrite, Path: foreign, MarkedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}
	body, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "pending"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := atomicio.WriteDurable(filepath.Join(cfg.StateDir, "pending", op.OpID+".json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return op.OpID
}

func pendingIDs(t *testing.T, cfg Config) []string {
	t.Helper()
	ops, err := listPendingOps(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, op := range ops {
		ids = append(ids, op.OpID+"("+op.Kind+")")
	}
	return ids
}

// TestConcurrentWritersStayFreshWithoutManualRebuild runs many MCP writers and
// searchers at once against a vault that carries a foreign pending marker, and
// checks that (1) each write is searchable the moment write_memory returns and
// (2) once the background reconciles finish, nothing is left pending — no
// manual `mora index rebuild` needed. Before the fix the foreign marker could
// never be covered by any rebuild, so the index read dirty forever and every
// reconcile ran its full three passes.
func TestConcurrentWritersStayFreshWithoutManualRebuild(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	seedForeignWriteMarker(t, cfg)
	wait, calls := liveReconciler(t)
	ctx := testCtx(t)

	const writers, perWriter = 8, 3
	errs := make(chan error, writers*perWriter*2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				token := fmt.Sprintf("freshtok%dx%d", w, i)
				id, err := mcpWrite(ctx, "global", "fresh "+token, token+" body")
				if err != nil {
					errs <- fmt.Errorf("writer %d/%d: %w", w, i, err)
					continue
				}
				var out bytes.Buffer
				if err := Run(ctx, []string{"search", token, "--json"}, &out, &out, strings.NewReader("")); err != nil {
					errs <- fmt.Errorf("search %s: %w", token, err)
					continue
				}
				if !strings.Contains(out.String(), id) {
					errs <- fmt.Errorf("write %s (%s) not searchable right after write_memory returned: %s", id, token, out.String())
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	waitForReconciles(t, wait, calls)
	if left := pendingIDs(t, cfg); len(left) != 0 {
		t.Fatalf("index still dirty after every background reconcile finished; pending markers: %v", left)
	}
}

// TestMCPWriteSchedulesReconcileWhenUpsertFails: when the FTS upsert cannot
// commit (for example it waited out a long full rebuild), the write is saved
// but not searchable. The write must still schedule the background reconcile;
// before the fix only a successful upsert did, so the memory stayed invisible
// until an unrelated rebuild happened to run.
func TestMCPWriteSchedulesReconcileWhenUpsertFails(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	env := lookupTestEnv(t)
	scheduled := make(chan struct{}, 4)
	env.reconciler = func(context.Context, Config) error {
		scheduled <- struct{}{}
		return nil
	}
	poisonInserts(t, cfg)

	res, err := callMCPTool(testCtx(t), "write_memory", map[string]any{"title": "Blocked", "text": "upsert fails"})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := res.(map[string]any); m == nil || m["index_stale"] != true {
		t.Fatalf("precondition: want a degraded index_stale write, got %+v", res)
	}
	select {
	case <-scheduled:
	case <-time.After(5 * time.Second):
		t.Fatal("a write whose index update failed scheduled no reconcile; it stays unsearchable until some unrelated rebuild")
	}
}

// TestMCPServeWaitsForInFlightReconcile: a session that writes and then exits
// closes stdin at once. The server used to return immediately, so the process
// died in the middle of the background rebuild, leaving a running receipt with
// a dead owner (the "index rebuild operation STALLED" banner) and the write's
// projections uncovered.
func TestMCPServeWaitsForInFlightReconcile(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	env := lookupTestEnv(t)
	release := make(chan struct{})
	var finished atomic.Bool
	env.reconciler = func(context.Context, Config) error {
		<-release
		finished.Store(true)
		return nil
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_memory","arguments":{"title":"exit soon","text":"session ends right after this write"}}}`,
	}, "\n") + "\n"
	done := make(chan error, 1)
	go func() { done <- serveMCP(testCtx(t), &bytes.Buffer{}, strings.NewReader(in)) }()
	select {
	case err := <-done:
		t.Fatalf("serve returned (err=%v) while its reconcile was still running; the process would exit and kill the rebuild", err)
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after its reconcile finished")
	}
	if !finished.Load() {
		t.Fatal("serve returned before the reconcile finished")
	}
}

// TestReconcileReapsDeadOwnerLease: the reconcile election lease left by a
// killed process must not block catch-up for its whole TTL. Before the fix
// reaping looked only at age, so a fresh lease from a dead process made every
// new write skip the reconcile.
func TestReconcileReapsDeadOwnerLease(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := cmd.Process.Pid
	body, err := json.Marshal(loopLockBody{PID: deadPID, AcquiredAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(authoredReconcileLockPath(cfg)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authoredReconcileLockPath(cfg), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpWrite(ctx, "global", "after a crash", "deadlease body"); err != nil {
		t.Fatal(err)
	}
	if err := reconcileAuthoredWrites(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if left := pendingIDs(t, cfg); len(left) != 0 {
		t.Fatalf("a dead process's lease blocked the reconcile; pending markers: %v", left)
	}
}
