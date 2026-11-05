package mora

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

func TestMCPProposalConcurrentApprovalsPublishOnce(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	p := mcpWriteProposal{ID: "p_parallel", ProposedAt: "2026-10-29T00:00:00Z", Arguments: map[string]any{"title": "Use pnpm", "text": "Always install pnpm", "scope": "project:demo", "type": "decision"}}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	oldClock := mcpWriteClock
	mcpWriteClock = func() time.Time {
		time.Sleep(25 * time.Millisecond)
		return time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	}
	t.Cleanup(func() { mcpWriteClock = oldClock })
	ctx := testCtx(t)
	start := make(chan struct{})
	results := make(chan error, 8)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			var out, errout bytes.Buffer
			results <- cmdMCP(ctx, []string{"proposals", "approve", p.ID}, &out, &errout, strings.NewReader(""))
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "conflict") {
			conflicts++
		} else {
			t.Logf("other refusal: %v", err)
		}
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || successes != 1 || conflicts != 7 {
		t.Fatalf("parallel approvals: memories=%d successes=%d conflicts=%d; want 1,1,7", len(files), successes, conflicts)
	}
	if _, err := os.Stat(filepath.Join(mcpProposalDir(cfg), p.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("successful proposal remains pending: %v", err)
	}
}

func decisionFixture(t *testing.T) (Config, mcpWriteProposal) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	p := mcpWriteProposal{ID: "p_failure", ProposedAt: "2026-10-29T00:00:00Z", Arguments: map[string]any{"scope": "project:demo", "type": "decision", "title": "Package manager", "text": "Use pnpm"}}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	return cfg, p
}

func TestMCPProposalCleanupFailureCannotReplayOrResurrect(t *testing.T) {
	cfg, p := decisionFixture(t)
	old := removeDecidedMCPProposal
	removeDecidedMCPProposal = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { removeDecidedMCPProposal = old })
	id, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve")
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("want cleanup failure, got %v", err)
	}
	d, err := readMCPProposalDecision(cfg, p.ID)
	if err != nil || d == nil || d.Status != "approved" || d.MemoryID != id {
		t.Fatalf("decision: %+v %v", d, err)
	}
	var shown bytes.Buffer
	if err := showMCPWriteProposal(cfg, p.ID, &shown); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown.String(), `"status": "approved"`) || !strings.Contains(shown.String(), id) {
		t.Fatalf("misleading inspection: %s", shown.String())
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 1 {
		t.Fatalf("memories %v %v", files, err)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	// Changed scope/content and retracted/deleted memory cannot reset this identity.
	p.Arguments["scope"] = "project:other"
	p.Arguments["text"] = "Different content"
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"approve", "reject"} {
		if _, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, action); err == nil || !strings.Contains(err.Error(), "conflict") {
			t.Fatalf("%s replay: %v", action, err)
		}
	}
	files, err = allMemoryFiles(cfg)
	if err != nil || len(files) != 0 {
		t.Fatalf("resurrected memories %v %v", files, err)
	}
}

func TestMCPProposalInterruptedClaimAndCorruptDecisionFailClosed(t *testing.T) {
	cfg, p := decisionFixture(t)
	// A durable attempted claim represents a process that stopped before it could
	// record the outcome. Neither absence of the memory nor pending data is proof
	// that it is safe to publish again.
	d := &mcpProposalDecision{Version: 1, ProposalID: p.ID, Action: "approve", Status: "attempted", MemoryID: "mem_interrupted", ArgumentsSHA256: strings.Repeat("a", 64), AttemptedAt: p.ProposedAt}
	if err := writeMCPProposalDecision(cfg, d); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"approve", "reject"} {
		if _, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, action); err == nil || !strings.Contains(err.Error(), "conflict") {
			t.Fatalf("%s: %v", action, err)
		}
	}
	path := mcpProposalDecisionPath(cfg, p.ID)
	if err := os.WriteFile(path, []byte(`{"corrupt":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("corrupt record: %v", err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 0 {
		t.Fatalf("unexpected publication: %v %v", files, err)
	}
}

func TestMCPProposalClaimFailureDoesNotPublish(t *testing.T) {
	cfg, p := decisionFixture(t)
	// A non-directory prevents durable bookkeeping before any vault write.
	dir := filepath.Dir(mcpProposalDecisionPath(cfg, p.ID))
	if err := os.WriteFile(dir, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve"); err == nil {
		t.Fatal("expected claim refusal")
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 0 {
		t.Fatalf("published without claim: %v %v", files, err)
	}
}

func TestMCPProposalRejectAndApproveShareDecision(t *testing.T) {
	cfg, p := decisionFixture(t)
	ctx := testCtx(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, action := range []string{"approve", "reject"} {
		go func(action string) { <-start; _, err := decideMCPWriteProposal(ctx, cfg, p.ID, action); results <- err }(action)
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "conflict") {
			conflict++
		} else {
			t.Errorf("unexpected: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	d, err := readMCPProposalDecision(cfg, p.ID)
	if err != nil || d == nil {
		t.Fatalf("decision %v %v", d, err)
	}
	files, err := allMemoryFiles(cfg)
	want := 0
	if d.Action == "approve" {
		want = 1
	}
	if err != nil || len(files) != want {
		t.Fatalf("%s decision memories=%v err=%v", d.Action, files, err)
	}
}

func TestMCPProposalPinnedCollisionRefused(t *testing.T) {
	cfg, p := decisionFixture(t)
	old := newIDFn
	newIDFn = func() string { return "mem_collision" }
	t.Cleanup(func() { newIDFn = old })
	if _, err := mcpWriteMemory(testCtx(t), cfg, p.Arguments); err != nil {
		t.Fatal(err)
	}
	if _, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve"); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("collision: %v", err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 1 {
		t.Fatalf("collision wrote duplicate: %v %v", files, err)
	}
	d, err := readMCPProposalDecision(cfg, p.ID)
	if err != nil || d.Status != "attempted" {
		t.Fatalf("decision %v %v", d, err)
	}
}

func TestMCPProposalDurabilityBarriersPrecedePublication(t *testing.T) {
	for _, barrier := range []string{"parent", "data", "directory", "outcome"} {
		t.Run(barrier, func(t *testing.T) {
			cfg, p := decisionFixture(t)
			dir := filepath.Dir(mcpProposalDecisionPath(cfg, p.ID))
			oldData, oldDir := atomicio.MarkerSyncFn, atomicio.SyncDirFn
			t.Cleanup(func() { atomicio.MarkerSyncFn = oldData; atomicio.SyncDirFn = oldDir })
			calls := 0
			atomicio.MarkerSyncFn = func(f *os.File) error {
				if filepath.Dir(f.Name()) == dir {
					calls++
					if barrier == "data" || barrier == "outcome" && calls == 2 {
						return os.ErrPermission
					}
				}
				return oldData(f)
			}
			atomicio.SyncDirFn = func(path string) error {
				if path == cfg.StateDir && barrier == "parent" || path == dir && barrier == "directory" {
					return os.ErrPermission
				}
				return oldDir(path)
			}
			_, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve")
			if err == nil {
				t.Fatal("expected durable write failure")
			}
			files, err := allMemoryFiles(cfg)
			want := 0
			if barrier == "outcome" {
				want = 1
			}
			if err != nil || len(files) != want {
				t.Fatalf("%s: memories=%v error=%v", barrier, files, err)
			}
			// A failed directory sync or outcome update can leave an attempted claim;
			// those attempts must never be automatically retried.
			if barrier == "directory" || barrier == "outcome" {
				if _, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve"); err == nil || !strings.Contains(err.Error(), "conflict") {
					t.Fatalf("retry: %v", err)
				}
				var shown bytes.Buffer
				if err := showMCPWriteProposal(cfg, p.ID, &shown); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(shown.String(), `"status": "attempted"`) {
					t.Fatalf("misleading status: %s", shown.String())
				}
			}
		})
	}
}

func TestMCPProposalTeachRetractionDoesNotPermitReapproval(t *testing.T) {
	cfg, p := decisionFixture(t)
	id, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve")
	if err != nil {
		t.Fatal(err)
	}
	run(t, "teach", "memory", "retract", "--id", id, "--yes")
	g, err := loadGovernance(cfg)
	if err != nil || g.memoryVisible(id) {
		t.Fatalf("memory remains current: %v", err)
	}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	if _, err := decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("retracted proposal replay: %v", err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 1 {
		t.Fatalf("retraction replay wrote another memory: %v %v", files, err)
	}
}
