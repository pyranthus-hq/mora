package mora

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		noFile   bool           // proposal file absent
		vault    string         // "", "match", "differs", "differs-source", "differs-meta", "corrupt"
		pargs    map[string]any // extra proposal arguments
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
		{name: "match, decision type", action: "approve", status: "attempted", vault: "match", pargs: map[string]any{"type": "decision", "durability": "durable", "flip_conditions": "a;b"}, want: want{status: "approved", vaultFile: true}, reconcil: true},
		{name: "match, correction meta", action: "approve", status: "attempted", vault: "match", pargs: map[string]any{"type": "correction", "target": "mem_x", "disposition": "keep"}, want: want{status: "approved", vaultFile: true}, reconcil: true},
		{name: "vault differs in source", action: "approve", status: "attempted", vault: "differs-source", want: want{err: "collision", proposal: true, status: "attempted", vaultFile: true}},
		{name: "vault differs in meta", action: "approve", status: "attempted", vault: "differs-meta", pargs: map[string]any{"type": "correction", "target": "mem_x", "disposition": "keep"}, want: want{err: "collision", proposal: true, status: "attempted", vaultFile: true}},
		{name: "corrupt vault file blocks retry", action: "approve", status: "attempted", vault: "corrupt", args: []string{"--retry"}, want: want{err: "cannot tell", proposal: true, status: "attempted", vaultFile: false}},
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
			for k, v := range r.pargs {
				p.Arguments[k] = v
			}
			if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
				t.Fatal(err)
			}
			if r.vault != "" {
				m, err := mcpMemoryFromArgs(p.Arguments, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				switch r.vault {
				case "differs":
					m.Text = "someone else's body"
				case "differs-source":
					m.Source = "other"
				case "differs-meta":
					m.Meta = map[string]any{"target": "mem_y", "disposition": "retract"}
				}
				if r.vault == "corrupt" {
					m.ID = memID
					path, err := memoryPath(cfg, m)
					if err != nil {
						t.Fatal(err)
					}
					_ = os.MkdirAll(filepath.Dir(path), 0o755)
					if err := os.WriteFile(path, []byte("---\nnot: [valid\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else if _, _, err := createMemory(testCtx(t), cfg, m, withExplicitMemoryID(memID)); err != nil {
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

func TestProposalRecoverJSONReceipt(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	p := mcpWriteProposal{ID: "p_recoverjson", ProposedAt: time.Now().Format(time.RFC3339),
		Arguments: map[string]any{"scope": "project:t", "title": "T", "text": "b"}}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(p.Arguments)
	d := &mcpProposalDecision{Version: 1, ProposalID: p.ID, Action: "reject", Status: "attempted",
		ArgumentsSHA256: fmt.Sprintf("%x", sha256.Sum256(args)), AttemptedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeMCPProposalDecision(cfg, d); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := cmdMCPProposals(testCtx(t), []string{"recover", p.ID, "--json"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var rec mcpProposalsPayload
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil || rec.Decision != "recover" || len(rec.Proposals) != 1 || rec.Proposals[0].Status != "rejected" {
		t.Fatalf("receipt = %s (%v)", out.String(), err)
	}
}
