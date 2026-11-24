package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

func saveApproveProposals(t *testing.T, cfg Config, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("p_approve%d", i)
		p := mcpWriteProposal{ID: ids[i], ProposedAt: time.Now().Format(time.RFC3339),
			Arguments: map[string]any{"scope": "project:t", "type": "note", "title": fmt.Sprintf("Approve %d", i), "text": fmt.Sprintf("body %d", i)}}
		if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

// blockedApprove runs `approve ids...` with a reconciler parked until release.
func blockedApprove(t *testing.T, ids func(Config) []string, limit time.Duration) (done chan error, release func(), stderr *bytes.Buffer, cfg Config) {
	t.Helper()
	withTestVerifiedOwnerAction(t)
	cfg = coreBIngestInitCfg(t)
	started := make(chan struct{}, 8)
	gate := make(chan struct{})
	setAuthoredReconcileRunnerForTest(t, func(context.Context, Config) error {
		started <- struct{}{}
		<-gate
		return nil
	})
	var once bool
	release = func() {
		if !once {
			once = true
			close(gate)
		}
	}
	t.Cleanup(release)
	old := approveDrainLimit
	approveDrainLimit = limit
	t.Cleanup(func() { approveDrainLimit = old })
	args := append([]string{"approve"}, ids(cfg)...)
	stderr = &bytes.Buffer{}
	done = make(chan error, 1)
	go func() { var out bytes.Buffer; done <- cmdMCPProposals(testCtx(t), args, &out, stderr) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("reconciler never started")
	}
	return
}

func TestApproveBatchDrainsOnce(t *testing.T) {
	done, release, _, cfg := blockedApprove(t, func(cfg Config) []string { return saveApproveProposals(t, cfg, 2) }, time.Minute)
	select {
	case err := <-done:
		t.Fatalf("approve returned while the rebuild was still running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("approve did not return after the rebuild finished")
	}
	for _, id := range []string{"p_approve0", "p_approve1"} {
		d, err := readMCPProposalDecision(cfg, id)
		if err != nil || d == nil || d.Status != "approved" {
			t.Fatalf("decision for %s = %+v, %v", id, d, err)
		}
	}
}

func TestApproveWarnsOnDrainTimeout(t *testing.T) {
	done, _, stderr, _ := blockedApprove(t, func(cfg Config) []string { return saveApproveProposals(t, cfg, 1) }, 100*time.Millisecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("approve did not stop waiting at the drain limit")
	}
	if !strings.Contains(stderr.String(), "rebuild is unfinished") {
		t.Fatalf("no timeout warning: %q", stderr.String())
	}
}

func TestApproveBatchFailureKeepsReceiptAndRetries(t *testing.T) {
	withTestVerifiedOwnerAction(t)
	cfg := coreBIngestInitCfg(t)
	ids := saveApproveProposals(t, cfg, 2) // p_approve0, p_approve1
	var out, errOut bytes.Buffer
	err := cmdMCPProposals(testCtx(t), []string{"approve", "--json", ids[0], "p_missing", ids[1]}, &out, &errOut)
	if err == nil {
		t.Fatal("batch with a missing id succeeded")
	}
	var rec mcpProposalsPayload
	if jerr := json.Unmarshal(out.Bytes(), &rec); jerr != nil {
		t.Fatalf("no JSON receipt for a failed batch: %v\n%s", jerr, out.String())
	}
	rows := rec.Proposals
	if len(rows) != 2 || rows[0].ID != ids[0] || rows[0].Status != "approved" || rows[0].MemoryID == "" || rows[1].ID != "p_missing" || rows[1].Status != "failed" {
		t.Fatalf("rows = %+v", rows)
	}
	// Retry: the approved id is done, the rest continue.
	out.Reset()
	if err := cmdMCPProposals(testCtx(t), []string{"approve", ids[0], ids[1]}, &out, &errOut); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !strings.Contains(out.String(), "Already approved "+ids[0]) || !strings.Contains(out.String(), "Approved "+ids[1]) {
		t.Fatalf("retry output: %s", out.String())
	}
}
