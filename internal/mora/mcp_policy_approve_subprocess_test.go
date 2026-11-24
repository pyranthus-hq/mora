package mora

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	configstore "github.com/pyranthus-hq/mora/internal/config"
)

// TestApproveSubprocessWorker is the child of TestApproveDrainsReconcile. Its
// reconciler holds a real index_rebuild receipt (the one a rebuild opens at its
// first phase), parks until released, then retires the receipt.
func TestApproveSubprocessWorker(t *testing.T) {
	home := os.Getenv("MORA_APPROVE_HOME")
	if home == "" {
		t.Skip("not the subprocess worker")
	}
	withTestVerifiedOwnerAction(t)
	parked, resume := os.Getenv("MORA_APPROVE_PARKED"), os.Getenv("MORA_APPROVE_RESUME")
	ctx := configstore.WithHomeRoot(context.Background(), home)
	ctx = configstore.WithAuthoredReconciler(ctx, func(_ context.Context, cfg Config) error {
		h, err := beginOperation(cfg, operationKindIndexRebuild, "listing", time.Now())
		if err != nil {
			return err
		}
		_ = os.WriteFile(parked, []byte("1"), 0o644)
		for {
			if _, err := os.Stat(resume); err == nil {
				return finishOperation(cfg, h, operationCompleted, "retired", operationCounts{}, "", time.Now())
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	var out, errOut discardBuf
	if err := cmdMCPProposals(ctx, []string{"approve", os.Getenv("MORA_APPROVE_ID")}, &out, &errOut); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

type discardBuf struct{}

func (*discardBuf) Write(p []byte) (int, error) { return len(p), nil }

func spawnApproveWorker(t *testing.T, home, id, parked, resume string) (*exec.Cmd, chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestApproveSubprocessWorker", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), "MORA_TEST_SUBPROCESS=1", "HOME="+home, "USERPROFILE="+home, "MORA_CONFIG_DIR=",
		"MORA_APPROVE_HOME="+home, "MORA_APPROVE_ID="+id, "MORA_APPROVE_PARKED="+parked, "MORA_APPROVE_RESUME="+resume)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done
}

func rebuildReceipts(cfg Config) (running, stalled int) {
	for _, a := range operationActivities(cfg, time.Now(), operationProcessAlive) {
		if a.Kind != operationKindIndexRebuild {
			continue
		}
		switch a.State {
		case operationRunning:
			running++
		case operationStalled:
			stalled++
		}
	}
	return
}

func TestApproveDrainsReconcile(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	home := cfg.HomeDir()
	ids := saveApproveProposals(t, cfg, 2)
	dir := t.TempDir()

	// Run 1: the process must stay alive until the rebuild finishes.
	parked, resume := filepath.Join(dir, "parked1"), filepath.Join(dir, "resume1")
	_, done := spawnApproveWorker(t, home, ids[0], parked, resume)
	waitForFile(t, parked, 60*time.Second)
	select {
	case err := <-done:
		t.Fatalf("approve exited while the rebuild was running: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := os.WriteFile(resume, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("approve failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("approve did not exit after the rebuild finished")
	}
	rctx := configstore.WithHomeRoot(context.Background(), home)
	rcfg, err := loadConfigFor(rctx)
	if err != nil {
		t.Fatal(err)
	}
	if r, s := rebuildReceipts(rcfg); r+s != 0 {
		t.Fatalf("receipts after a clean approve: running=%d stalled=%d", r, s)
	}

	// Run 2: kill the process mid-rebuild. The dead receipt shows as stalled,
	// and the next rebuild clears it at once.
	parked2 := filepath.Join(dir, "parked2")
	cmd, done2 := spawnApproveWorker(t, home, ids[1], parked2, filepath.Join(dir, "never"))
	waitForFile(t, parked2, 60*time.Second)
	_ = cmd.Process.Kill()
	<-done2
	if _, s := rebuildReceipts(rcfg); s != 1 {
		t.Fatalf("expected one stalled receipt after the kill, got %d", s)
	}
	if _, err := rebuildIndex(rctx, rcfg); err != nil {
		t.Fatal(err)
	}
	if r, s := rebuildReceipts(rcfg); r+s != 0 {
		t.Fatalf("dead rebuild receipt survived the next rebuild: running=%d stalled=%d", r, s)
	}
}
