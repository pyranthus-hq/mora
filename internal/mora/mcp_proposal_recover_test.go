package mora

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

func TestProposalRecoverMatrix(t *testing.T) {
	const memID = "mem_20260101_000000_abcd1234"
	type want struct {
		err       string // substring of the refusal; "" means success
		proposal  bool   // proposal file still present
		status    string // decision status after; "" means no decision file
		vaultFile bool
	}
	rows := []struct {
		name     string
		action   string // decision action; "" means no decision
		status   string
		noFile   bool   // proposal file absent
		vault    string // "", "match", "differs"
		args     []string
		want     want
		reconcil bool
	}{
		{name: "no decision", args: nil, want: want{err: "nothing to recover", proposal: true}},
		{name: "approved, file left", action: "approve", status: "approved", vault: "match", want: want{status: "approved", vaultFile: true}},
		{name: "rejected, file left", action: "reject", status: "rejected", want: want{status: "rejected"}},
		{name: "attempted reject", action: "reject", status: "attempted", want: want{status: "rejected"}},
		{name: "attempted approve, vault matches", action: "approve", status: "attempted", vault: "match", want: want{status: "approved", vaultFile: true}, reconcil: true},
		{name: "attempted approve, vault differs", action: "approve", status: "attempted", vault: "differs", want: want{err: "collision", proposal: true, status: "attempted", vaultFile: true}},
		{name: "attempted approve, no vault file", action: "approve", status: "attempted", want: want{err: "choose --retry", proposal: true, status: "attempted"}},
		{name: "attempted approve, no vault file, retry", action: "approve", status: "attempted", args: []string{"--retry"}, want: want{proposal: true}},
		{name: "attempted approve, no vault file, close", action: "approve", status: "attempted", args: []string{"--close"}, want: want{status: "rejected"}},
		{name: "retry flag on a safe row", action: "reject", status: "attempted", args: []string{"--retry"}, want: want{err: "only apply", proposal: true, status: "attempted"}},
		{name: "both flags", action: "approve", status: "attempted", args: []string{"--retry", "--close"}, want: want{err: "not both", proposal: true, status: "attempted"}},
	}
	for _, r := range rows {
		subRun(t, r.name, func(t *testing.T) {
			cfg := coreBIngestInitCfg(t)
			reconciled := make(chan struct{}, 4)
			setAuthoredReconcileRunnerForTest(t, func(context.Context, Config) error { reconciled <- struct{}{}; return nil })
			id := "p_recover"
			p := mcpWriteProposal{ID: id, ProposedAt: time.Now().Format(time.RFC3339),
				Arguments: map[string]any{"scope": "project:t", "type": "note", "title": "Recover me", "text": "original body"}}
			if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
				t.Fatal(err)
			}
			if r.vault != "" {
				text := "original body"
				if r.vault == "differs" {
					text = "someone else's body"
				}
				if _, _, err := createMemory(testCtx(t), cfg, Memory{Scope: "project:t", Type: "note", Title: "Recover me", Text: text}, withExplicitMemoryID(memID)); err != nil {
					t.Fatal(err)
				}
			}
			if r.action != "" {
				args, _ := json.Marshal(p.Arguments)
				d := &mcpProposalDecision{Version: 1, ProposalID: id, Action: r.action, Status: r.status,
					ArgumentsSHA256: fmt.Sprintf("%x", sha256.Sum256(args)), AttemptedAt: time.Now().UTC().Format(time.RFC3339Nano)}
				if r.action == "approve" {
					d.MemoryID = memID
				}
				if err := writeMCPProposalDecision(cfg, d); err != nil {
					t.Fatal(err)
				}
			}
			var out, errOut bytes.Buffer
			err := cmdMCPProposals(testCtx(t), append([]string{"recover", id}, r.args...), &out, &errOut)
			if r.want.err == "" && err != nil {
				t.Fatalf("recover: %v\n%s", err, out.String())
			}
			if r.want.err != "" && (err == nil || !strings.Contains(err.Error(), r.want.err)) {
				t.Fatalf("recover error = %v, want %q", err, r.want.err)
			}
			if out.Len() == 0 && r.want.err == "" {
				t.Fatal("recover printed nothing")
			}
			if _, statErr := os.Stat(mustProposalPath(t, cfg, id)); (statErr == nil) != r.want.proposal {
				t.Fatalf("proposal file present = %v, want %v", statErr == nil, r.want.proposal)
			}
			d, derr := readMCPProposalDecision(cfg, id)
			if derr != nil {
				t.Fatal(derr)
			}
			got := ""
			if d != nil {
				got = d.Status
			}
			if got != r.want.status {
				t.Fatalf("decision status = %q, want %q", got, r.want.status)
			}
			if _, ferr := findMemoryRaw(cfg, memID); (ferr == nil) != r.want.vaultFile {
				t.Fatalf("vault file present = %v, want %v", ferr == nil, r.want.vaultFile)
			}
			if r.reconcil {
				select {
				case <-reconciled:
				default:
					t.Fatal("recover did not run the reconcile")
				}
			}
		})
	}
}

func mustProposalPath(t *testing.T, cfg Config, id string) string {
	t.Helper()
	path, err := mcppkg.ProposalPath(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
