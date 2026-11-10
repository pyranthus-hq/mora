package mora

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func proposalViaFixture(t *testing.T) (Config, mcpWriteProposal) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	result, err := stageMCPWriteProposal(cfg, map[string]any{
		"scope": "project:demo", "type": "note", "title": "Popup approval", "text": "The text shown to the owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["proposal"].(map[string]any)["id"].(string)
	p, _, err := readMCPWriteProposal(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, p
}

func TestProposalViaApprovals(t *testing.T) {
	for _, via := range []string{"", "popup"} {
		t.Run("via="+via, func(t *testing.T) {
			cfg, p := proposalViaFixture(t)
			// AC-001.1: digest is exactly SHA256(json.Marshal(args)), including the stored decision.
			body, err := json.Marshal(p.Arguments)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := mcpProposalArgumentsSHA256(p.Arguments)
			if err != nil || digest != fmt.Sprintf("%x", sha256.Sum256(body)) {
				t.Fatalf("digest=%q err=%v", digest, err)
			}
			var id string
			// AC-001.2: legacy wrapper approves and omits decided_via.
			// AC-002.1: popup approves once, records its origin, and logs its caller.
			if via == "" {
				id, err = decideMCPWriteProposal(testCtx(t), cfg, p.ID, "approve")
			} else {
				id, err = decideMCPWriteProposalVia(testCtx(t), cfg, p.ID, "approve", via, digest)
			}
			if err != nil {
				t.Fatal(err)
			}
			files, err := allMemoryFiles(cfg)
			if err != nil || len(files) != 1 {
				t.Fatalf("memories=%v err=%v", files, err)
			}
			m, err := parseMemory(files[0])
			if err != nil || m.ID != id {
				t.Fatalf("memory=%+v err=%v", m, err)
			}
			d, err := readMCPProposalDecision(cfg, p.ID)
			if err != nil || d == nil {
				t.Fatalf("decision=%+v err=%v", d, err)
			}
			if d.Status != "approved" || d.Action != "approve" || d.MemoryID != id || d.DecidedVia != via || d.ArgumentsSHA256 != digest {
				t.Fatalf("decision=%+v", d)
			}
			raw, err := os.ReadFile(mcpProposalDecisionPath(cfg, p.ID))
			if err != nil {
				t.Fatal(err)
			}
			if via == "" && bytes.Contains(raw, []byte("decided_via")) {
				t.Fatalf("legacy record contains decided_via: %s", raw)
			}
			if _, err := os.Stat(mustProposalPath(t, cfg, p.ID)); !os.IsNotExist(err) {
				t.Fatalf("pending proposal: %v", err)
			}
			// AC-002.4: CLI caller remains approved-proposal without the popup suffix.
			log, err := os.ReadFile(filepath.Join(cfg.VaultDir, agentWriteLogName))
			if err != nil {
				t.Fatal(err)
			}
			caller := "approved-proposal"
			if via == "popup" {
				caller += "/popup"
			}
			found := false
			for _, line := range strings.Split(string(log), "\n") {
				if strings.Contains(line, id) && strings.Contains(line, "caller="+caller) {
					found = true
				}
			}
			if !found || (via == "" && strings.Contains(string(log), "caller=approved-proposal/popup")) {
				t.Fatalf("unexpected audit log: %s", log)
			}
		})
	}
}

func TestProposalViaRefusals(t *testing.T) {
	for _, tc := range []struct{ name, via, action, digest, wantErr string }{
		// AC-001.3: unsupported via refuses without changing the pending proposal.
		{"AC-001.3", "cli", "approve", "", "invalid proposal decision via"},
		// AC-001.4: popup rejects are refused before IO.
		{"AC-001.4", "popup", "reject", "", "popup proposal decisions must approve"},
		// AC-002.2: stale popup text cannot publish or claim a decision.
		{"AC-002.2", "popup", "approve", strings.Repeat("0", 64), "proposal arguments changed"},
		// AC-002.3: digest binding also applies without a via.
		{"AC-002.3", "", "approve", strings.Repeat("0", 64), "proposal arguments changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, p := proposalViaFixture(t)
			path := mustProposalPath(t, cfg, p.ID)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			id, err := decideMCPWriteProposalVia(testCtx(t), cfg, p.ID, tc.action, tc.via, tc.digest)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || id != "" {
				t.Fatalf("id=%q err=%v", id, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("pending proposal changed: %v", err)
			}
			if _, err := os.Stat(mcpProposalDecisionPath(cfg, p.ID)); !os.IsNotExist(err) {
				t.Fatalf("decision created: %v", err)
			}
			files, err := allMemoryFiles(cfg)
			if err != nil || len(files) != 0 {
				t.Fatalf("published=%v err=%v", files, err)
			}
			if log, err := os.ReadFile(filepath.Join(cfg.VaultDir, agentWriteLogName)); !os.IsNotExist(err) {
				t.Fatalf("unexpected audit publication: %s %v", log, err)
			}
			if tc.digest == "" {
				// Invalid via/action must win even over an invalid ID and unusable config.
				_, err := decideMCPWriteProposalVia(testCtx(t), Config{}, "../invalid", tc.action, tc.via, "")
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("validation did not precede IO: %v", err)
				}
				if _, err := os.Stat(filepath.Dir(mcpProposalDecisionPath(cfg, p.ID))); !os.IsNotExist(err) {
					t.Fatalf("invalid via created bookkeeping: %v", err)
				}
			}
		})
	}
}

func TestProposalViaDecisionReadAndRecover(t *testing.T) {
	cfg, p := proposalViaFixture(t)
	digest, err := mcpProposalArgumentsSHA256(p.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	for _, via := range []string{"agent", "popup", ""} {
		d := &mcpProposalDecision{Version: 1, ProposalID: p.ID, Action: "approve", Status: "attempted", MemoryID: "mem_interrupted", ArgumentsSHA256: digest, AttemptedAt: p.ProposedAt, DecidedVia: via}
		if err := writeMCPProposalDecision(cfg, d); err != nil {
			t.Fatal(err)
		}
		got, err := readMCPProposalDecision(cfg, p.ID)
		// AC-003.1: unrecognized origin fails closed.
		if via == "agent" {
			if err == nil {
				t.Fatal("accepted agent origin")
			}
		} else {
			// AC-003.2: popup and legacy records are both readable.
			if err != nil || got == nil || got.DecidedVia != via {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
		}
	}
	// AC-003.3: recover --close preserves popup provenance on its rejection.
	d, err := readMCPProposalDecision(cfg, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.DecidedVia = "popup"
	if err := writeMCPProposalDecision(cfg, d); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := cmdMCPProposals(testCtx(t), []string{"recover", p.ID, "--close"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	d, err = readMCPProposalDecision(cfg, p.ID)
	if err != nil || d == nil {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
	if d.Action != "reject" || d.Status != "rejected" || d.DecidedVia != "popup" || d.MemoryID != "" {
		t.Fatalf("closed decision=%+v", d)
	}
}
