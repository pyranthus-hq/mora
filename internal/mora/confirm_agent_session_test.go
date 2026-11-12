package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// An agent session must not be able to clear its own tentative tag or approve
// its own held proposal. Reading and listing stay available.
func TestConfirmAndApproveRefuseInAnAgentSession(t *testing.T) {
	for _, key := range rulesAgentEnv {
		subRun(t, key, func(t *testing.T) {
			cfg := reviewSetup(t)
			note := reviewSaved(t, "Saved note")
			held := reviewHeld(t, "Held note")
			t.Setenv(key, "1")

			_, _, err := mineRun(t, nil, "confirm", note)
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "own terminal") {
				t.Fatalf("confirm in an agent session: %v", err)
			}
			_, _, err = mineRun(t, nil, "mcp", "proposals", "approve", held)
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "own terminal") {
				t.Fatalf("approve in an agent session: %v", err)
			}
			if !reviewTentative(t, cfg, note) {
				t.Fatal("a refused confirm cleared the tag")
			}
			if d, derr := readMCPProposalDecision(cfg, held); derr != nil || d != nil {
				t.Fatalf("a refused approve decided the proposal: %v %v", d, derr)
			}
			if logged, err := os.ReadFile(agentWriteLogPath(cfg)); err == nil && strings.Contains(string(logged), " confirm ") {
				t.Fatalf("a refused confirm left an audit line:\n%s", logged)
			}
			// Read-only forms still work.
			if out, _, err := mineRun(t, nil, "confirm", "--list"); err != nil || !strings.Contains(out, note) {
				t.Fatalf("confirm --list: %q %v", out, err)
			}
			if out, _, err := mineRun(t, nil, "mcp", "proposals", "list"); err != nil || !strings.Contains(out, held) {
				t.Fatalf("proposals list: %q %v", out, err)
			}
		})
	}
}

func TestConfirmAndApproveWithoutAnAgentSessionAreUnchanged(t *testing.T) {
	cfg := reviewSetup(t)
	note := reviewSaved(t, "Saved note")
	held := reviewHeld(t, "Held note")
	run(t, "confirm", note)
	if reviewTentative(t, cfg, note) {
		t.Fatal("confirm did not clear the tag")
	}
	run(t, "mcp", "proposals", "approve", held)
	if d, err := readMCPProposalDecision(cfg, held); err != nil || d == nil || d.Status != "approved" {
		t.Fatalf("approve: %v %v", d, err)
	}
}

// A queue that cannot be read is a warning in doctor, never "0 pending".
func TestDoctorSurfacesAnUnreadableProposalQueue(t *testing.T) {
	cfg := reviewSetup(t)
	dir := mcppkg.ProposalDir(cfg)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "doctor"); !strings.Contains(out, "MCP proposal queue unreadable") {
		t.Fatalf("doctor text hides the failed read:\n%s", out)
	}
	out := run(t, "doctor", "--json")
	if !strings.Contains(out, `"mcp_proposals_readable"`) {
		t.Fatalf("doctor --json has no mcp_proposals_readable check:\n%s", out)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "doctor", "--json"); strings.Contains(out, "mcp_proposals_readable") {
		t.Fatalf("a readable queue produced the check:\n%s", out)
	}
}
